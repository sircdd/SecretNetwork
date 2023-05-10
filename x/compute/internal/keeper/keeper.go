package keeper

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	capabilitykeeper "github.com/cosmos/cosmos-sdk/x/capability/keeper"
	channelkeeper "github.com/cosmos/ibc-go/v4/modules/core/04-channel/keeper"
	portkeeper "github.com/cosmos/ibc-go/v4/modules/core/05-port/keeper"
	wasmTypes "github.com/scrtlabs/SecretNetwork/go-cosmwasm/types"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck

	"github.com/cosmos/cosmos-sdk/telemetry"

	"github.com/cosmos/cosmos-sdk/baseapp"
	codedctypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/x/auth/ante"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	govkeeper "github.com/cosmos/cosmos-sdk/x/gov/keeper"
	mintkeeper "github.com/cosmos/cosmos-sdk/x/mint/keeper"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	sdktxsigning "github.com/cosmos/cosmos-sdk/types/tx/signing"
	wasm "github.com/scrtlabs/SecretNetwork/go-cosmwasm"

	v010wasmTypes "github.com/scrtlabs/SecretNetwork/go-cosmwasm/types/v010"
	v1wasmTypes "github.com/scrtlabs/SecretNetwork/go-cosmwasm/types/v1"

	"github.com/scrtlabs/SecretNetwork/x/compute/internal/types"
)

type ResponseHandler interface {
	// Handle processes the data returned by a contract invocation.
	Handle(
		ctx sdk.Context,
		contractAddr sdk.AccAddress,
		ibcPort string,
		messages []v1wasmTypes.SubMsg,
		origRspData []byte,
		ogTx []byte,
		sigInfo wasmTypes.VerificationInfo,
	) ([]byte, error)
}

// Keeper will have a reference to Wasmer with it's own data directory.
type Keeper struct {
	storeKey         sdk.StoreKey
	cdc              codec.BinaryCodec
	legacyAmino      codec.LegacyAmino
	accountKeeper    authkeeper.AccountKeeper
	bankKeeper       bankkeeper.Keeper
	portKeeper       portkeeper.Keeper
	capabilityKeeper capabilitykeeper.ScopedKeeper
	wasmer           wasm.Wasmer
	queryPlugins     QueryPlugins
	messenger        Messenger
	// queryGasLimit is the max wasm gas that can be spent on executing a query with a contract
	queryGasLimit uint64
	HomeDir       string
	// authZPolicy   AuthorizationPolicy
	// paramSpace    subspace.Subspace
	LastMsgManager *baseapp.LastMsgMarkerContainer
}

func moduleLogger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

// MessageRouter ADR 031 request type routing
type MessageRouter interface {
	Handler(msg sdk.Msg) baseapp.MsgServiceHandler
}

func NewKeeper(
	cdc codec.Codec,
	legacyAmino codec.LegacyAmino,
	storeKey sdk.StoreKey,
	accountKeeper authkeeper.AccountKeeper,
	bankKeeper bankkeeper.Keeper,
	govKeeper govkeeper.Keeper,
	distKeeper distrkeeper.Keeper,
	mintKeeper mintkeeper.Keeper,
	stakingKeeper stakingkeeper.Keeper,
	capabilityKeeper capabilitykeeper.ScopedKeeper,
	portKeeper portkeeper.Keeper,
	portSource types.ICS20TransferPortSource,
	channelKeeper channelkeeper.Keeper,
	legacyMsgRouter sdk.Router,
	msgRouter MessageRouter,
	queryRouter GRPCQueryRouter,
	homeDir string,
	wasmConfig *types.WasmConfig,
	supportedFeatures string,
	customEncoders *MessageEncoders,
	customPlugins *QueryPlugins,
	lastMsgManager *baseapp.LastMsgMarkerContainer,
) Keeper {
	wasmer, err := wasm.NewWasmer(filepath.Join(homeDir, "wasm"), supportedFeatures, wasmConfig.CacheSize, wasmConfig.EnclaveCacheSize)
	if err != nil {
		panic(err)
	}

	keeper := Keeper{
		storeKey:         storeKey,
		cdc:              cdc,
		legacyAmino:      legacyAmino,
		wasmer:           *wasmer,
		accountKeeper:    accountKeeper,
		bankKeeper:       bankKeeper,
		portKeeper:       portKeeper,
		capabilityKeeper: capabilityKeeper,
		messenger:        NewMessageHandler(msgRouter, legacyMsgRouter, customEncoders, channelKeeper, capabilityKeeper, portSource, cdc),
		queryGasLimit:    wasmConfig.SmartQueryGasLimit,
		HomeDir:          homeDir,
		LastMsgManager:   lastMsgManager,
	}
	keeper.queryPlugins = DefaultQueryPlugins(govKeeper, distKeeper, mintKeeper, bankKeeper, stakingKeeper, queryRouter, &keeper, channelKeeper).Merge(customPlugins)

	return keeper
}

func (k Keeper) GetLastMsgMarkerContainer() *baseapp.LastMsgMarkerContainer {
	return k.LastMsgManager
}

// Create uploads and compiles a WASM contract, returning a short identifier for the contract
func (k Keeper) Create(ctx sdk.Context, creator sdk.AccAddress, wasmCode []byte, source string, builder string) (codeID uint64, err error) {
	wasmCode, err = uncompress(wasmCode)
	if err != nil {
		return 0, sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	ctx.GasMeter().ConsumeGas(types.CompileCost*uint64(len(wasmCode)), "Compiling WASM Bytecode")

	codeHash, err := k.wasmer.Create(wasmCode)
	if err != nil {
		return 0, sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	store := ctx.KVStore(k.storeKey)
	codeID = k.autoIncrementID(ctx, types.KeyLastCodeID)

	codeInfo := types.NewCodeInfo(codeHash, creator, source, builder)
	// 0x01 | codeID (uint64) -> ContractInfo
	store.Set(types.GetCodeKey(codeID), k.cdc.MustMarshal(&codeInfo))

	return codeID, nil
}

func (k Keeper) importCode(ctx sdk.Context, codeID uint64, codeInfo types.CodeInfo, wasmCode []byte) error {
	wasmCode, err := uncompress(wasmCode)
	if err != nil {
		return sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	newCodeHash, err := k.wasmer.Create(wasmCode)
	if err != nil {
		return sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	if !bytes.Equal(codeInfo.CodeHash, newCodeHash) {
		return sdkerrors.Wrap(types.ErrInvalid, "code hashes not same")
	}

	store := ctx.KVStore(k.storeKey)
	key := types.GetCodeKey(codeID)
	if store.Has(key) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "duplicate code: %d", codeID)
	}
	// 0x01 | codeID (uint64) -> ContractInfo
	store.Set(key, k.cdc.MustMarshal(&codeInfo))
	return nil
}

func (k Keeper) GetTxInfo(ctx sdk.Context, sender sdk.AccAddress) ([]byte, sdktxsigning.SignMode, []byte, []byte, []byte, error) {
	var rawTx sdktx.TxRaw
	err := k.cdc.Unmarshal(ctx.TxBytes(), &rawTx)
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to decode raw transaction from bytes: %s", err.Error()))
	}

	var txAuthInfo sdktx.AuthInfo
	err = k.cdc.Unmarshal(rawTx.AuthInfoBytes, &txAuthInfo)
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to decode transaction auth info from bytes: %s", err.Error()))
	}

	// Assaf:
	// We're not decoding Body as it is unnecessary in this context,
	// and it fails decoding IBC messages with the error "no concrete type registered for type URL /ibc.core.channel.v1.MsgChannelOpenInit against interface *types.Msg".
	// I think that's because the core IBC messages don't support Amino encoding,
	// and a "concrete type" is used to refer to the mapping between the Go struct and the Amino type string (e.g. "cosmos-sdk/MsgSend")
	// Therefore we'll ignore the Body here.
	// Plus this probably saves CPU cycles to not decode the body.
	tx := authtx.WrapTx(&sdktx.Tx{
		Body:       nil,
		AuthInfo:   &txAuthInfo,
		Signatures: rawTx.Signatures,
	}).GetTx()

	pubKeys, err := tx.GetPubKeys()
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to get public keys for instantiate: %s", err.Error()))
	}

	pkIndex := -1
	if sender == nil {
		// Assaf:
		// We are in a situation where the contract gets a null msg.sender,
		// however we still need to get the sign bytes for verification against the wasm input msg inside the enclave.
		// There can be multiple signers on the tx, for example one can be the msg.sender and the another can be the gas fee payer. Another example is if this tx also contains MsgMultiSend which supports multiple msg.senders thus requiring multiple signers.
		// Not sure if we should support this or if this even matters here, as we're most likely here because it's an incoming IBC tx and the signer is the relayer.
		// For now we will just take the first signer.
		// Also, because we're not decoding the tx body anymore, we can't use tx.GetSigners() here. Therefore we'll convert the pubkey into an address.

		pubkeys, err := tx.GetPubKeys()
		if err != nil {
			return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to retrieve pubkeys from tx: %s", err.Error()))
		}

		pkIndex = 0
		sender = sdk.AccAddress(pubkeys[pkIndex].Address())
	} else {
		var _signers [][]byte // This is just used for the error message below
		for index, pubKey := range pubKeys {
			thisSigner := pubKey.Address().Bytes()
			_signers = append(_signers, thisSigner)
			if bytes.Equal(thisSigner, sender.Bytes()) {
				pkIndex = index
			}
		}
		if pkIndex == -1 {
			return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Message sender: %v is not found in the tx signer set: %v, callback signature not provided", sender, _signers))
		}
	}

	signatures, err := tx.GetSignaturesV2()
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to get signatures: %s", err.Error()))
	}
	var signMode sdktxsigning.SignMode
	switch signData := signatures[pkIndex].Data.(type) {
	case *sdktxsigning.SingleSignatureData:
		signMode = signData.SignMode
	case *sdktxsigning.MultiSignatureData:
		signMode = sdktxsigning.SignMode_SIGN_MODE_LEGACY_AMINO_JSON
	}

	signerAcc, err := ante.GetSignerAcc(ctx, k.accountKeeper, sender)
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to retrieve account by address: %s", err.Error()))
	}

	signBytes, err := authtx.DirectSignBytes(rawTx.BodyBytes, rawTx.AuthInfoBytes, ctx.ChainID(), signerAcc.GetAccountNumber())
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, fmt.Sprintf("Unable to recreate sign bytes for the tx: %s", err.Error()))
	}

	modeInfoBytes, err := sdktxsigning.SignatureDataToProto(signatures[pkIndex].Data).Marshal()
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, "couldn't marshal mode info")
	}

	var pkBytes []byte
	pubKey := pubKeys[pkIndex]
	anyPubKey, err := codedctypes.NewAnyWithValue(pubKey)
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, "couldn't turn public key into Any")
	}
	pkBytes, err = k.cdc.Marshal(anyPubKey)
	if err != nil {
		return nil, 0, nil, nil, nil, sdkerrors.Wrap(types.ErrSigFailed, "couldn't marshal public key")
	}
	return signBytes, signMode, modeInfoBytes, pkBytes, rawTx.Signatures[pkIndex], nil
}

func V010MsgToV1SubMsg(contractAddress string, msg v010wasmTypes.CosmosMsg) (v1wasmTypes.SubMsg, error) {
	if !isValidV010Msg(msg) {
		return v1wasmTypes.SubMsg{}, fmt.Errorf("exactly one message type is supported: %+v", msg)
	}

	subMsg := v1wasmTypes.SubMsg{
		ID:       0,   // https://github.com/CosmWasm/cosmwasm/blob/v1.0.0/packages/std/src/results/submessages.rs#L40-L41
		GasLimit: nil, // New v1 submessages module handles nil as unlimited, in v010 the gas was not limited for messages
		ReplyOn:  v1wasmTypes.ReplyNever,
	}

	if msg.Bank != nil { //nolint:gocritic
		if msg.Bank.Send.FromAddress != contractAddress {
			return v1wasmTypes.SubMsg{}, fmt.Errorf("contract doesn't have permission to send funds from another account (using BankMsg)")
		}
		subMsg.Msg = v1wasmTypes.CosmosMsg{
			Bank: &v1wasmTypes.BankMsg{
				Send: &v1wasmTypes.SendMsg{ToAddress: msg.Bank.Send.ToAddress, Amount: msg.Bank.Send.Amount},
			},
		}
	} else if msg.Custom != nil {
		subMsg.Msg.Custom = msg.Custom
	} else if msg.Staking != nil {
		subMsg.Msg = v1wasmTypes.CosmosMsg{
			Staking: &v1wasmTypes.StakingMsg{
				Delegate:   msg.Staking.Delegate,
				Undelegate: msg.Staking.Undelegate,
				Redelegate: msg.Staking.Redelegate,
				Withdraw:   msg.Staking.Withdraw,
			},
		}
	} else if msg.Wasm != nil {
		subMsg.Msg = v1wasmTypes.CosmosMsg{
			Wasm: &v1wasmTypes.WasmMsg{
				Execute:     msg.Wasm.Execute,
				Instantiate: msg.Wasm.Instantiate,
			},
		}
	} else if msg.Gov != nil {
		subMsg.Msg = v1wasmTypes.CosmosMsg{
			Gov: &v1wasmTypes.GovMsg{
				Vote: &v1wasmTypes.VoteMsg{ProposalId: msg.Gov.Vote.Proposal, Vote: v1wasmTypes.ToVoteOption[msg.Gov.Vote.VoteOption]},
			},
		}
	}

	return subMsg, nil
}

func V010MsgsToV1SubMsgs(contractAddr string, msgs []v010wasmTypes.CosmosMsg) ([]v1wasmTypes.SubMsg, error) {
	subMsgs := []v1wasmTypes.SubMsg{}
	for _, msg := range msgs {
		v1SubMsg, err := V010MsgToV1SubMsg(contractAddr, msg)
		if err != nil {
			return nil, err
		}
		subMsgs = append(subMsgs, v1SubMsg)
	}

	return subMsgs, nil
}

// Instantiate creates an instance of a WASM contract
func (k Keeper) Instantiate(ctx sdk.Context, codeID uint64, creator sdk.AccAddress, initMsg []byte, label string, deposit sdk.Coins, callbackSig []byte) (sdk.AccAddress, []byte, error) {
	defer telemetry.MeasureSince(time.Now(), "compute", "keeper", "instantiate")

	ctx.GasMeter().ConsumeGas(types.InstanceCost, "Loading CosmWasm module: init")

	signBytes := []byte{}
	signMode := sdktxsigning.SignMode_SIGN_MODE_UNSPECIFIED
	modeInfoBytes := []byte{}
	pkBytes := []byte{}
	signerSig := []byte{}
	var err error

	// If no callback signature - we should send the actual msg sender sign bytes and signature
	if callbackSig == nil {
		signBytes, signMode, modeInfoBytes, pkBytes, signerSig, err = k.GetTxInfo(ctx, creator)
		if err != nil {
			return nil, nil, err
		}
	}

	verificationInfo := types.NewVerificationInfo(signBytes, signMode, modeInfoBytes, pkBytes, signerSig, callbackSig)

	// create contract address

	store := ctx.KVStore(k.storeKey)
	existingAddress := store.Get(types.GetContractLabelPrefix(label))

	if existingAddress != nil {
		return nil, nil, sdkerrors.Wrap(types.ErrAccountExists, label)
	}

	contractAddress := k.generateContractAddress(ctx, codeID, creator)
	existingAcct := k.accountKeeper.GetAccount(ctx, contractAddress)
	if existingAcct != nil {
		return nil, nil, sdkerrors.Wrap(types.ErrAccountExists, existingAcct.GetAddress().String())
	}

	// deposit initial contract funds
	if !deposit.IsZero() {
		if k.bankKeeper.BlockedAddr(creator) {
			return nil, nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, "blocked address can not be used")
		}
		sdkerr := k.bankKeeper.SendCoins(ctx, creator, contractAddress, deposit)
		if sdkerr != nil {
			return nil, nil, sdkerr
		}
	} else {
		// create an empty account (so we don't have issues later)
		// TODO: can we remove this?
		contractAccount := k.accountKeeper.NewAccountWithAddress(ctx, contractAddress)
		k.accountKeeper.SetAccount(ctx, contractAccount)
	}

	// get contact info
	bz := store.Get(types.GetCodeKey(codeID))
	if bz == nil {
		return nil, nil, sdkerrors.Wrap(types.ErrNotFound, "code")
	}
	var codeInfo types.CodeInfo
	k.cdc.MustUnmarshal(bz, &codeInfo)

	random := k.GetRandomSeed(ctx, ctx.BlockHeight())

	// prepare env for contract instantiate call
	env := types.NewEnv(ctx, creator, deposit, contractAddress, nil, random)

	// create prefixed data store
	// 0x03 | contractAddress (sdk.AccAddress)
	prefixStoreKey := types.GetContractStorePrefixKey(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)

	// prepare querier
	querier := QueryHandler{
		Ctx:     ctx,
		Plugins: k.queryPlugins,
		Caller:  contractAddress,
	}

	// instantiate wasm contract
	gas := gasForContract(ctx)
	response, key, gasUsed, err := k.wasmer.Instantiate(codeInfo.CodeHash, env, initMsg, prefixStore, cosmwasmAPI, querier, ctx.GasMeter(), gas, verificationInfo, contractAddress)
	consumeGas(ctx, gasUsed)

	if err != nil {
		switch res := response.(type) { //nolint:gocritic
		case v1wasmTypes.DataWithInternalReplyInfo:
			result, e := json.Marshal(res)
			if e != nil {
				return nil, nil, sdkerrors.Wrap(e, "couldn't marshal internal reply info")
			}

			return contractAddress, result, sdkerrors.Wrap(types.ErrInstantiateFailed, err.Error())
		}

		return contractAddress, nil, sdkerrors.Wrap(types.ErrInstantiateFailed, err.Error())
	}

	switch res := response.(type) {
	case *v010wasmTypes.InitResponse:
		// emit all events from this contract itself

		// persist instance
		createdAt := types.NewAbsoluteTxPosition(ctx)
		contractInfo := types.NewContractInfo(codeID, creator, label, createdAt)
		store.Set(types.GetContractAddressKey(contractAddress), k.cdc.MustMarshal(&contractInfo))

		store.Set(types.GetContractEnclaveKey(contractAddress), key)
		store.Set(types.GetContractLabelPrefix(label), contractAddress)

		subMessages, err := V010MsgsToV1SubMsgs(contractAddress.String(), res.Messages)
		if err != nil {
			return nil, nil, sdkerrors.Wrap(err, "couldn't convert v0.10 messages to v1 messages")
		}

		data, err := k.handleContractResponse(ctx, contractAddress, contractInfo.IBCPortID, subMessages, res.Log, []v1wasmTypes.Event{}, res.Data, initMsg, verificationInfo, wasmTypes.CosmosMsgVersionV010)
		if err != nil {
			return nil, nil, sdkerrors.Wrap(err, "dispatch")
		}

		return contractAddress, data, nil
	case *v1wasmTypes.Response:
		// persist instance first
		createdAt := types.NewAbsoluteTxPosition(ctx)
		contractInfo := types.NewContractInfo(codeID, creator, label, createdAt)

		// check for IBC flag
		report, err := k.wasmer.AnalyzeCode(codeInfo.CodeHash)
		if err != nil {
			return contractAddress, nil, sdkerrors.Wrap(types.ErrInstantiateFailed, err.Error())
		}
		if report.HasIBCEntryPoints {
			// register IBC port
			ibcPort, err := k.ensureIbcPort(ctx, contractAddress)
			if err != nil {
				return nil, nil, err
			}
			contractInfo.IBCPortID = ibcPort
		}

		ctx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeInstantiate,
			sdk.NewAttribute(types.AttributeKeyContractAddr, contractAddress.String()),
			sdk.NewAttribute(types.AttributeKeyCodeID, strconv.FormatUint(codeID, 10)),
		))

		// persist instance
		store.Set(types.GetContractAddressKey(contractAddress), k.cdc.MustMarshal(&contractInfo))
		store.Set(types.GetContractEnclaveKey(contractAddress), key)

		store.Set(types.GetContractLabelPrefix(label), contractAddress)

		data, err := k.handleContractResponse(ctx, contractAddress, contractInfo.IBCPortID, res.Messages, res.Attributes, res.Events, res.Data, initMsg, verificationInfo, wasmTypes.CosmosMsgVersionV1)
		if err != nil {
			return nil, nil, sdkerrors.Wrap(err, "dispatch")
		}

		return contractAddress, data, nil
	default:
		return nil, nil, sdkerrors.Wrap(types.ErrInstantiateFailed, fmt.Sprintf("cannot detect response type: %+v", res))
	}
}

// Execute executes the contract instance
func (k Keeper) Execute(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, msg []byte, coins sdk.Coins, callbackSig []byte, handleType wasmTypes.HandleType) (*sdk.Result, error) {
	defer telemetry.MeasureSince(time.Now(), "compute", "keeper", "execute")

	ctx.GasMeter().ConsumeGas(types.InstanceCost, "Loading Compute module: execute")

	signBytes := []byte{}
	signMode := sdktxsigning.SignMode_SIGN_MODE_UNSPECIFIED
	modeInfoBytes := []byte{}
	pkBytes := []byte{}
	signerSig := []byte{}
	var err error

	// If no callback signature - we should send the actual msg sender sign bytes and signature
	if callbackSig == nil {
		signBytes, signMode, modeInfoBytes, pkBytes, signerSig, err = k.GetTxInfo(ctx, caller)
		if err != nil {
			return nil, err
		}
	}

	verificationInfo := types.NewVerificationInfo(signBytes, signMode, modeInfoBytes, pkBytes, signerSig, callbackSig)

	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}

	store := ctx.KVStore(k.storeKey)

	// add more funds
	if !coins.IsZero() {
		if k.bankKeeper.BlockedAddr(caller) {
			return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, "blocked address can not be used")
		}

		sdkerr := k.bankKeeper.SendCoins(ctx, caller, contractAddress, coins)
		if sdkerr != nil {
			return nil, sdkerr
		}
	}

	random := k.GetRandomSeed(ctx, ctx.BlockHeight())
	contractKey := store.Get(types.GetContractEnclaveKey(contractAddress))
	env := types.NewEnv(ctx, caller, coins, contractAddress, contractKey, random)

	// prepare querier
	querier := QueryHandler{
		Ctx:     ctx,
		Plugins: k.queryPlugins,
		Caller:  contractAddress,
	}

	gas := gasForContract(ctx)
	response, gasUsed, execErr := k.wasmer.Execute(codeInfo.CodeHash, env, msg, prefixStore, cosmwasmAPI, querier, gasMeter(ctx), gas, verificationInfo, handleType)
	consumeGas(ctx, gasUsed)

	if execErr != nil {
		var result sdk.Result
		switch res := response.(type) { //nolint:gocritic
		case v1wasmTypes.DataWithInternalReplyInfo:
			result.Data, err = json.Marshal(res)
			if err != nil {
				return nil, sdkerrors.Wrap(err, "couldn't marshal internal reply info")
			}
		}

		return &result, sdkerrors.Wrap(types.ErrExecuteFailed, execErr.Error())
	}

	switch res := response.(type) {
	case *v010wasmTypes.HandleResponse:
		subMessages, err := V010MsgsToV1SubMsgs(contractAddress.String(), res.Messages)
		if err != nil {
			return nil, sdkerrors.Wrap(err, "couldn't convert v0.10 messages to v1 messages")
		}

		data, err := k.handleContractResponse(ctx, contractAddress, contractInfo.IBCPortID, subMessages, res.Log, []v1wasmTypes.Event{}, res.Data, msg, verificationInfo, wasmTypes.CosmosMsgVersionV010)
		if err != nil {
			return nil, sdkerrors.Wrap(err, "dispatch")
		}

		return &sdk.Result{
			Data: data,
		}, nil
	case *v1wasmTypes.Response:
		ctx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeExecute,
			sdk.NewAttribute(types.AttributeKeyContractAddr, contractAddress.String()),
		))

		data, err := k.handleContractResponse(ctx, contractAddress, contractInfo.IBCPortID, res.Messages, res.Attributes, res.Events, res.Data, msg, verificationInfo, wasmTypes.CosmosMsgVersionV1)
		if err != nil {
			return nil, sdkerrors.Wrap(err, "dispatch")
		}

		return &sdk.Result{
			Data: data,
		}, nil
	default:
		return nil, sdkerrors.Wrap(types.ErrExecuteFailed, fmt.Sprintf("cannot detect response type: %+v", res))
	}
}

// QuerySmart queries the smart contract itself.
func (k Keeper) QuerySmart(ctx sdk.Context, contractAddr sdk.AccAddress, req []byte, useDefaultGasLimit bool) ([]byte, error) {
	return k.querySmartImpl(ctx, contractAddr, req, useDefaultGasLimit, 1)
}

// QuerySmartRecursive queries the smart contract itself. This should only be called when running inside another query recursively.
func (k Keeper) querySmartRecursive(ctx sdk.Context, contractAddr sdk.AccAddress, req []byte, queryDepth uint32, useDefaultGasLimit bool) ([]byte, error) {
	return k.querySmartImpl(ctx, contractAddr, req, useDefaultGasLimit, queryDepth)
}

func (k Keeper) querySmartImpl(ctx sdk.Context, contractAddress sdk.AccAddress, req []byte, useDefaultGasLimit bool, queryDepth uint32) ([]byte, error) {
	defer telemetry.MeasureSince(time.Now(), "compute", "keeper", "query")

	if useDefaultGasLimit {
		ctx = ctx.WithGasMeter(sdk.NewGasMeter(k.queryGasLimit))
	}

	ctx.GasMeter().ConsumeGas(types.InstanceCost, "Loading CosmWasm module: query")

	_, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}

	// prepare querier
	querier := QueryHandler{
		Ctx:     ctx,
		Plugins: k.queryPlugins,
		Caller:  contractAddress,
	}

	store := ctx.KVStore(k.storeKey)
	// 0x01 | codeID (uint64) -> ContractInfo
	contractKey := store.Get(types.GetContractEnclaveKey(contractAddress))

	params := types.NewEnv(
		ctx,
		sdk.AccAddress{}, /* empty because it's unused in queries */
		sdk.NewCoins(),   /* empty because it's unused in queries */
		contractAddress,
		contractKey,
		[]byte{0}, /* empty because it's unused in queries */
	)
	params.QueryDepth = queryDepth

	queryResult, gasUsed, qErr := k.wasmer.Query(codeInfo.CodeHash, params, req, prefixStore, cosmwasmAPI, querier, gasMeter(ctx), gasForContract(ctx))
	consumeGas(ctx, gasUsed)

	telemetry.SetGauge(float32(gasUsed), "compute", "keeper", "query", contractAddress.String(), "gasUsed")

	if qErr != nil {
		return nil, sdkerrors.Wrap(types.ErrQueryFailed, qErr.Error())
	}
	return queryResult, nil
}

// We don't use this function since we have an encrypted state. It's here for upstream compatibility
// QueryRaw returns the contract's state for give key. For a `nil` key a empty slice result is returned.
func (k Keeper) QueryRaw(ctx sdk.Context, contractAddress sdk.AccAddress, key []byte) []types.Model {
	result := make([]types.Model, 0)
	if key == nil {
		return result
	}
	prefixStoreKey := types.GetContractStorePrefixKey(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)

	if val := prefixStore.Get(key); val != nil {
		return append(result, types.Model{
			Key:   key,
			Value: val,
		})
	}
	return result
}

func (k Keeper) contractInstance(ctx sdk.Context, contractAddress sdk.AccAddress) (types.ContractInfo, types.CodeInfo, prefix.Store, error) {
	store := ctx.KVStore(k.storeKey)

	contractBz := store.Get(types.GetContractAddressKey(contractAddress))
	if contractBz == nil {
		return types.ContractInfo{}, types.CodeInfo{}, prefix.Store{}, sdkerrors.Wrap(types.ErrNotFound, "contract")
	}
	var contract types.ContractInfo
	k.cdc.MustUnmarshal(contractBz, &contract)

	contractInfoBz := store.Get(types.GetCodeKey(contract.CodeID))
	if contractInfoBz == nil {
		return types.ContractInfo{}, types.CodeInfo{}, prefix.Store{}, sdkerrors.Wrap(types.ErrNotFound, "contract info")
	}
	var codeInfo types.CodeInfo
	k.cdc.MustUnmarshal(contractInfoBz, &codeInfo)
	prefixStoreKey := types.GetContractStorePrefixKey(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	return contract, codeInfo, prefixStore, nil
}

func (k Keeper) GetContractKey(ctx sdk.Context, contractAddress sdk.AccAddress) []byte {
	store := ctx.KVStore(k.storeKey)

	contractKey := store.Get(types.GetContractEnclaveKey(contractAddress))

	return contractKey
}

func (k Keeper) GetRandomSeed(ctx sdk.Context, height int64) []byte {
	store := ctx.KVStore(k.storeKey)

	random := store.Get(types.GetRandomKey(height))

	return random
}

func (k Keeper) SetRandomSeed(ctx sdk.Context, random []byte) {
	store := ctx.KVStore(k.storeKey)

	ctx.Logger().Info(fmt.Sprintf("Setting random: %s", hex.EncodeToString(random)))

	store.Set(types.GetRandomKey(ctx.BlockHeight()), random)
}

func (k Keeper) GetContractAddress(ctx sdk.Context, label string) sdk.AccAddress {
	store := ctx.KVStore(k.storeKey)

	contractAddress := store.Get(types.GetContractLabelPrefix(label))

	return contractAddress
}

func (k Keeper) GetContractHash(ctx sdk.Context, contractAddress sdk.AccAddress) ([]byte, error) {
	contractInfo := k.GetContractInfo(ctx, contractAddress)

	if contractInfo == nil {
		return nil, fmt.Errorf("failed to get contract info for the following address: %s", contractAddress.String())
	}

	codeId := contractInfo.CodeID

	codeInfo, err := k.GetCodeInfo(ctx, codeId)
	if err != nil {
		return nil, err
	}

	return codeInfo.CodeHash, nil
}

func (k Keeper) GetContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress) *types.ContractInfo {
	store := ctx.KVStore(k.storeKey)
	var contract types.ContractInfo
	contractBz := store.Get(types.GetContractAddressKey(contractAddress))
	if contractBz == nil {
		return nil
	}
	k.cdc.MustUnmarshal(contractBz, &contract)
	return &contract
}

func (k Keeper) containsContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetContractAddressKey(contractAddress))
}

func (k Keeper) setContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress, contract *types.ContractInfo) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetContractAddressKey(contractAddress), k.cdc.MustMarshal(contract))
}

func (k Keeper) setContractCustomInfo(ctx sdk.Context, contractAddress sdk.AccAddress, contract *types.ContractCustomInfo) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetContractEnclaveKey(contractAddress), contract.EnclaveKey)
	// println(fmt.Sprintf("Setting enclave key: %x: %x\n", types.GetContractEnclaveKey(contractAddress), contract.EnclaveKey))
	store.Set(types.GetContractLabelPrefix(contract.Label), contractAddress)
	// println(fmt.Sprintf("Setting label: %x: %x\n", types.GetContractLabelPrefix(contract.Label), contractAddress))
}

func (k Keeper) IterateContractInfo(ctx sdk.Context, cb func(sdk.AccAddress, types.ContractInfo, types.ContractCustomInfo) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.ContractKeyPrefix)
	iter := prefixStore.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		var contract types.ContractInfo
		k.cdc.MustUnmarshal(iter.Value(), &contract)

		enclaveId := ctx.KVStore(k.storeKey).Get(types.GetContractEnclaveKey(iter.Key()))
		// println(fmt.Sprintf("Setting enclave key: %x: %x\n", types.GetContractEnclaveKey(iter.Key()), enclaveId))
		// println(fmt.Sprintf("Setting label: %x: %x\n", types.GetContractLabelPrefix(contract.Label), contract.Label))

		contractCustomInfo := types.ContractCustomInfo{
			EnclaveKey: enclaveId,
			Label:      contract.Label,
		}

		// cb returns true to stop early
		if cb(iter.Key(), contract, contractCustomInfo) {
			break
		}
	}
}

func (k Keeper) GetContractState(ctx sdk.Context, contractAddress sdk.AccAddress) sdk.Iterator {
	prefixStoreKey := types.GetContractStorePrefixKey(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	return prefixStore.Iterator(nil, nil)
}

func (k Keeper) importContractState(ctx sdk.Context, contractAddress sdk.AccAddress, models []types.Model) error {
	prefixStoreKey := types.GetContractStorePrefixKey(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	for _, model := range models {
		if model.Value == nil {
			model.Value = []byte{}
		}

		if prefixStore.Has(model.Key) {
			return sdkerrors.Wrapf(types.ErrDuplicate, "duplicate key: %x", model.Key)
		}
		prefixStore.Set(model.Key, model.Value)

	}
	return nil
}

func (k Keeper) GetCodeInfo(ctx sdk.Context, codeID uint64) (types.CodeInfo, error) {
	store := ctx.KVStore(k.storeKey)
	var codeInfo types.CodeInfo
	codeInfoBz := store.Get(types.GetCodeKey(codeID))
	if codeInfoBz == nil {
		return types.CodeInfo{}, fmt.Errorf("failed to get code info for code id %d", codeID)
	}
	k.cdc.MustUnmarshal(codeInfoBz, &codeInfo)
	return codeInfo, nil
}

func (k Keeper) containsCodeInfo(ctx sdk.Context, codeID uint64) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetCodeKey(codeID))
}

func (k Keeper) IterateCodeInfos(ctx sdk.Context, cb func(uint64, types.CodeInfo) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.CodeKeyPrefix)
	iter := prefixStore.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		var c types.CodeInfo
		k.cdc.MustUnmarshal(iter.Value(), &c)
		// cb returns true to stop early
		if cb(binary.BigEndian.Uint64(iter.Key()), c) {
			return
		}
	}
}

func (k Keeper) GetWasm(ctx sdk.Context, codeID uint64) ([]byte, error) {
	store := ctx.KVStore(k.storeKey)
	var codeInfo types.CodeInfo
	codeInfoBz := store.Get(types.GetCodeKey(codeID))
	if codeInfoBz == nil {
		return nil, nil
	}
	k.cdc.MustUnmarshal(codeInfoBz, &codeInfo)
	return k.wasmer.GetCode(codeInfo.CodeHash)
}

// handleContractResponse processes the contract response data by emitting events and sending sub-/messages.
func (k *Keeper) handleContractResponse(
	ctx sdk.Context,
	contractAddr sdk.AccAddress,
	ibcPort string,
	msgs []v1wasmTypes.SubMsg,
	logs []v010wasmTypes.LogAttribute,
	evts v1wasmTypes.Events,
	data []byte,
	// original TX in order to extract the first 64bytes of signing info
	ogTx []byte,
	// sigInfo of the initial message that triggered the original contract call
	// This is used mainly in replies in order to decrypt their data.
	ogSigInfo wasmTypes.VerificationInfo,
	ogCosmosMessageVersion wasmTypes.CosmosMsgVersion,
) ([]byte, error) {
	events := types.ContractLogsToSdkEvents(logs, contractAddr)

	ctx.EventManager().EmitEvents(events)

	if len(evts) > 0 {

		customEvents, err := types.NewCustomEvents(evts, contractAddr)
		if err != nil {
			return nil, err
		}

		ctx.EventManager().EmitEvents(customEvents)
	}

	responseHandler := NewContractResponseHandler(NewMessageDispatcher(k.messenger, k))
	return responseHandler.Handle(ctx, contractAddr, ibcPort, msgs, data, ogTx, ogSigInfo, ogCosmosMessageVersion)
}

func gasForContract(ctx sdk.Context) uint64 {
	meter := ctx.GasMeter()
	remaining := (meter.Limit() - meter.GasConsumed()) * types.GasMultiplier
	if remaining > types.MaxGas {
		return types.MaxGas
	}
	return remaining
}

func consumeGas(ctx sdk.Context, gas uint64) {
	consumed := (gas / types.GasMultiplier) + 1
	ctx.GasMeter().ConsumeGas(consumed, "wasm contract")
	// throw OutOfGas error if we ran out (got exactly to zero due to better limit enforcing)
	if ctx.GasMeter().IsOutOfGas() {
		panic(sdk.ErrorOutOfGas{Descriptor: "Wasmer function execution"})
	}
}

// generates a contract address from codeID + instanceID
func (k Keeper) generateContractAddress(ctx sdk.Context, codeID uint64, creator sdk.AccAddress) sdk.AccAddress {
	instanceID := k.autoIncrementID(ctx, types.KeyLastInstanceID)
	return contractAddress(codeID, instanceID, creator)
}

func contractAddress(codeID, instanceID uint64, creator sdk.AccAddress) sdk.AccAddress {
	contractId := codeID<<32 + instanceID
	hashSourceBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(hashSourceBytes, contractId)

	hashSourceBytes = append(hashSourceBytes, creator...)

	sha := sha256.Sum256(hashSourceBytes)
	hasherRIPEMD160 := ripemd160.New()
	hasherRIPEMD160.Write(sha[:]) // does not error
	return sdk.AccAddress(hasherRIPEMD160.Sum(nil))
}

func (k Keeper) GetNextCodeID(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.KeyLastCodeID)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	return id
}

func (k Keeper) autoIncrementID(ctx sdk.Context, lastIDKey []byte) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(lastIDKey)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}

	bz = sdk.Uint64ToBigEndian(id + 1)
	store.Set(lastIDKey, bz)

	return id
}

// peekAutoIncrementID reads the current value without incrementing it.
func (k Keeper) peekAutoIncrementID(ctx sdk.Context, lastIDKey []byte) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(lastIDKey)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	return id
}

func (k Keeper) importAutoIncrementID(ctx sdk.Context, lastIDKey []byte, val uint64) error {
	store := ctx.KVStore(k.storeKey)
	if store.Has(lastIDKey) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "autoincrement id: %s", string(lastIDKey))
	}
	bz := sdk.Uint64ToBigEndian(val)
	store.Set(lastIDKey, bz)
	return nil
}

func (k Keeper) importContract(ctx sdk.Context, contractAddr sdk.AccAddress, customInfo *types.ContractCustomInfo, c *types.ContractInfo, state []types.Model) error {
	if !k.containsCodeInfo(ctx, c.CodeID) {
		return sdkerrors.Wrapf(types.ErrNotFound, "code id: %d", c.CodeID)
	}
	if k.containsContractInfo(ctx, contractAddr) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "contract: %s", contractAddr)
	}

	k.setContractCustomInfo(ctx, contractAddr, customInfo)
	k.setContractInfo(ctx, contractAddr, c)
	return k.importContractState(ctx, contractAddr, state)
}

// MultipliedGasMeter wraps the GasMeter from context and multiplies all reads by out defined multiplier
type MultipiedGasMeter struct {
	originalMeter sdk.GasMeter
}

var _ wasm.GasMeter = MultipiedGasMeter{}

func (m MultipiedGasMeter) GasConsumed() sdk.Gas {
	return m.originalMeter.GasConsumed() * types.GasMultiplier
}

func gasMeter(ctx sdk.Context) MultipiedGasMeter {
	return MultipiedGasMeter{
		originalMeter: ctx.GasMeter(),
	}
}

type MsgDispatcher interface {
	DispatchSubmessages(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, msgs []v1wasmTypes.SubMsg, ogTx []byte, ogSigInfo wasmTypes.VerificationInfo, ogCosmosMessageVersion wasmTypes.CosmosMsgVersion) ([]byte, error)
}

// ContractResponseHandler default implementation that first dispatches submessage then normal messages.
// The Submessage execution may include an success/failure response handling by the contract that can overwrite the
// original
type ContractResponseHandler struct {
	md MsgDispatcher
}

func NewContractResponseHandler(md MsgDispatcher) *ContractResponseHandler {
	return &ContractResponseHandler{md: md}
}

// Handle processes the data returned by a contract invocation.
func (h ContractResponseHandler) Handle(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, messages []v1wasmTypes.SubMsg, origRspData []byte, ogTx []byte, ogSigInfo wasmTypes.VerificationInfo, ogCosmosMessageVersion wasmTypes.CosmosMsgVersion) ([]byte, error) {
	result := origRspData
	switch rsp, err := h.md.DispatchSubmessages(ctx, contractAddr, ibcPort, messages, ogTx, ogSigInfo, ogCosmosMessageVersion); {
	case err != nil:
		return nil, sdkerrors.Wrap(err, "submessages")
	case rsp != nil:
		result = rsp
	}
	return result, nil
}

// reply is only called from keeper internal functions (dispatchSubmessages) after processing the submessage
func (k Keeper) reply(ctx sdk.Context, contractAddress sdk.AccAddress, reply v1wasmTypes.Reply, ogTx []byte, ogSigInfo wasmTypes.VerificationInfo) ([]byte, error) {
	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}

	// always consider this pinned
	ctx.GasMeter().ConsumeGas(types.InstanceCost, "Loading Compute module: reply")

	store := ctx.KVStore(k.storeKey)
	contractKey := store.Get(types.GetContractEnclaveKey(contractAddress))

	random := k.GetRandomSeed(ctx, ctx.BlockHeight())

	env := types.NewEnv(ctx, contractAddress, sdk.Coins{}, contractAddress, contractKey, random)

	// prepare querier
	querier := QueryHandler{
		Ctx:     ctx,
		Plugins: k.queryPlugins,
		Caller:  contractAddress,
	}

	// instantiate wasm contract
	gas := gasForContract(ctx)
	marshaledReply, err := json.Marshal(reply)
	marshaledReply = append(ogTx[0:64], marshaledReply...)

	if err != nil {
		return nil, err
	}

	response, gasUsed, execErr := k.wasmer.Execute(codeInfo.CodeHash, env, marshaledReply, prefixStore, cosmwasmAPI, querier, ctx.GasMeter(), gas, ogSigInfo, wasmTypes.HandleTypeReply)
	consumeGas(ctx, gasUsed)

	if execErr != nil {
		return nil, sdkerrors.Wrap(types.ErrReplyFailed, execErr.Error())
	}

	switch res := response.(type) {
	case *v010wasmTypes.HandleResponse:
		return nil, sdkerrors.Wrap(types.ErrReplyFailed, fmt.Sprintf("response of reply should always be a CosmWasm v1 response type: %+v", res))
	case *v1wasmTypes.Response:
		consumeGas(ctx, gasUsed)

		ctx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeReply,
			sdk.NewAttribute(types.AttributeKeyContractAddr, contractAddress.String()),
		))

		data, err := k.handleContractResponse(ctx, contractAddress, contractInfo.IBCPortID, res.Messages, res.Attributes, res.Events, res.Data, ogTx, ogSigInfo, wasmTypes.CosmosMsgVersionV1)
		if err != nil {
			return nil, sdkerrors.Wrap(types.ErrReplyFailed, err.Error())
		}

		return data, nil
	default:
		return nil, sdkerrors.Wrap(types.ErrReplyFailed, fmt.Sprintf("cannot detect response type: %+v", res))
	}
}

func (k Keeper) GetStoreKey() sdk.StoreKey {
	return k.storeKey
}
