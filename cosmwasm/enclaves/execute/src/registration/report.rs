// Apache Teaclave (incubating)
// Copyright 2019-2020 The Apache Software Foundation
//
// This product includes software developed at
// The Apache Software Foundation (http://www.apache.org/).
//! Types that contain information about attestation report.
//! The implementation is based on Attestation Service API version 4.
//! https://api.trustedservices.intel.com/documents/sgx-attestation-api-spec.pdf

use std::array::TryFromSliceError;
use std::collections::HashMap;
use std::convert::TryFrom;

use log::*;

use lazy_static::lazy_static;
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::Value;
use uuid::Uuid;

use enclave_ffi_types::NodeAuthResult;

use super::cert::{get_ias_auth_config, get_netscape_comment};

use sgx_types::sgx_quote_t;

#[derive(Debug)]
pub enum Error {
    ReportParseError,
    ReportValidationError,
}

impl From<std::array::TryFromSliceError> for Error {
    fn from(_: TryFromSliceError) -> Self {
        Error::ReportParseError
    }
}

impl From<serde_json::error::Error> for Error {
    fn from(_: serde_json::error::Error) -> Self {
        Error::ReportParseError
    }
}

/// AttestationReport can be endorsed by either the Intel Attestation Service
/// using EPID or Data Center Attestation
/// Service (platform dependent) using ECDSA.
#[derive(Default, Serialize, Deserialize)]
pub struct EndorsedAttestationReport {
    /// Attestation report generated by the hardware
    #[serde(serialize_with = "as_base64", deserialize_with = "from_base64")]
    pub report: Vec<u8>,
    /// Singature of the report
    #[serde(serialize_with = "as_base64", deserialize_with = "from_base64")]
    pub signature: Vec<u8>,
    /// Certificate matching the signing key of the signature
    #[serde(serialize_with = "as_base64", deserialize_with = "from_base64")]
    pub signing_cert: Vec<u8>,
}

fn as_base64<S>(key: &[u8], serializer: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    serializer.serialize_str(&base64::encode(key))
}

fn from_base64<'de, D>(deserializer: D) -> Result<Vec<u8>, D::Error>
where
    D: Deserializer<'de>,
{
    struct Base64Visitor;

    impl<'de> serde::de::Visitor<'de> for Base64Visitor {
        type Value = Vec<u8>;

        fn expecting(&self, formatter: &mut ::std::fmt::Formatter) -> std::fmt::Result {
            write!(formatter, "base64 ASCII text")
        }

        fn visit_str<E>(self, v: &str) -> Result<Self::Value, E>
        where
            E: serde::de::Error,
        {
            base64::decode(v).map_err(E::custom)
        }
    }
    deserializer.deserialize_str(Base64Visitor)
}

type SignatureAlgorithms = &'static [&'static webpki::SignatureAlgorithm];
static SUPPORTED_SIG_ALGS: SignatureAlgorithms = &[
    &webpki::ECDSA_P256_SHA256,
    &webpki::ECDSA_P256_SHA384,
    &webpki::ECDSA_P384_SHA256,
    &webpki::ECDSA_P384_SHA384,
    &webpki::RSA_PSS_2048_8192_SHA256_LEGACY_KEY,
    &webpki::RSA_PSS_2048_8192_SHA384_LEGACY_KEY,
    &webpki::RSA_PSS_2048_8192_SHA512_LEGACY_KEY,
    &webpki::RSA_PKCS1_2048_8192_SHA256,
    &webpki::RSA_PKCS1_2048_8192_SHA384,
    &webpki::RSA_PKCS1_2048_8192_SHA512,
    &webpki::RSA_PKCS1_3072_8192_SHA384,
];

/// A report generated by an enclave that contains measurement, identity and
/// other data related to enclave.
///
/// # Note
///
/// Do not confuse `SgxEnclaveReport` with `AttestationReport`.
/// `SgxEnclaveReport` is generated by SGX hardware and endorsed by Quoting
/// Enclave through local attestation. The endorsed `SgxEnclaveReport` is an
/// `SgxQuote`. The quote is then sent to some attestation service (IAS or
/// DCAP-based AS). The endorsed `SgxQuote` is an attestation report signed by
/// attestation service's private key, a.k.a., `EndorsedAttestationReport`.
pub struct SgxEnclaveReport {
    /// Security version number of host system's CPU
    pub cpu_svn: [u8; 16],
    /// Misc select bits for the target enclave. Reserved for future function
    /// extension.
    pub misc_select: u32,
    /// Attributes of the enclave, for example, whether the enclave is running
    /// in debug mode.
    pub attributes: [u8; 16],
    /// Measurement value of the enclave. See
    /// [`EnclaveMeasurement`](../types/struct.EnclaveMeasurement.html)
    pub mr_enclave: [u8; 32],
    /// Measurement value of the public key that verified the enclave. See
    /// [`EnclaveMeasurement`](../types/struct.EnclaveMeasurement.html)
    pub mr_signer: [u8; 32],
    /// Product ID of the enclave
    pub isv_prod_id: u16,
    /// Security version number of the enclave
    pub isv_svn: u16,
    /// Set of data used for communication between enclave and target enclave
    pub report_data: [u8; 64],
}

impl std::fmt::Debug for SgxEnclaveReport {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        writeln!(f, "cpu_svn: {:?}", self.cpu_svn)?;
        writeln!(f, "misc_select: {:?}", self.misc_select)?;
        writeln!(f, "attributes: {:?}", self.attributes)?;
        writeln!(f, "mr_enclave: {:?}", self.mr_enclave)?;
        writeln!(f, "mr_signer: {:?}", self.mr_signer)?;
        writeln!(f, "isv_prod_id: {}", self.isv_prod_id)?;
        writeln!(f, "isv_svn: {}", self.isv_svn)?;
        writeln!(f, "report_data: {:?}", &self.report_data.to_vec())
    }
}

impl SgxEnclaveReport {
    /// Parse bytes of report into `SgxEnclaveReport`.
    pub fn parse_from<'a>(bytes: &'a [u8]) -> Result<Self, Error> {
        let mut pos: usize = 0;
        let mut take = |n: usize| -> Result<&'a [u8], Error> {
            if n > 0 && bytes.len() >= pos + n {
                let ret = &bytes[pos..pos + n];
                pos += n;
                Ok(ret)
            } else {
                error!("Enclave report parsing error - bad report size");
                Err(Error::ReportParseError)
            }
        };

        // Start parsing report by bytes following specifications. Don't
        // transmute directly, since there may cause endianness issue.
        // off 48, size 16
        let cpu_svn = <[u8; 16]>::try_from(take(16)?)?;

        // off 64, size 4
        let misc_select = u32::from_le_bytes(<[u8; 4]>::try_from(take(4)?)?);

        // off 68, size 28
        let _reserved = take(28)?;

        // off 96, size 16
        let attributes = <[u8; 16]>::try_from(take(16)?)?;

        // off 112, size 32
        let mr_enclave = <[u8; 32]>::try_from(take(32)?)?;

        // off 144, size 32
        let _reserved = take(32)?;

        // off 176, size 32
        let mr_signer = <[u8; 32]>::try_from(take(32)?)?;

        // off 208, size 96
        let _reserved = take(96)?;

        // off 304, size 2
        let isv_prod_id = u16::from_le_bytes(<[u8; 2]>::try_from(take(2)?)?);

        // off 306, size 2
        let isv_svn = u16::from_le_bytes(<[u8; 2]>::try_from(take(2)?)?);

        // off 308, size 60
        let _reserved = take(60)?;

        // off 368, size 64
        let mut report_data = [0u8; 64];
        let _report_data = take(64)?;
        let mut _it = _report_data.iter();
        for i in report_data.iter_mut() {
            *i = *_it.next().ok_or(Error::ReportParseError)?;
        }

        if pos != bytes.len() {
            warn!("Enclave report parsing error.");
            return Err(Error::ReportParseError);
        };

        Ok(SgxEnclaveReport {
            cpu_svn,
            misc_select,
            attributes,
            mr_enclave,
            mr_signer,
            isv_prod_id,
            isv_svn,
            report_data,
        })
    }
}

/// SGX Quote structure version
#[derive(Debug, PartialEq)]
#[allow(dead_code)]
pub enum SgxQuoteVersion {
    /// EPID quote version
    V1(SgxEpidQuoteSigType),
    /// EPID quote version
    V2(SgxEpidQuoteSigType),
    /// ECDSA quote version
    V3(SgxEcdsaQuoteAkType),
}

/// Intel EPID attestation signature type
#[derive(Debug, PartialEq)]
#[allow(dead_code)]
pub enum SgxEpidQuoteSigType {
    Unlinkable,
    Linkable,
}

/// ECDSA attestation key type
#[derive(Debug, PartialEq)]
pub enum SgxEcdsaQuoteAkType {
    /// ECDSA-256-with-P-256 curve
    P256_256,
    /// ECDSA-384-with-P-384 curve
    P384_384,
}

/// SGX Quote status
#[derive(PartialEq, Debug)]
pub enum SgxQuoteStatus {
    /// EPID signature of the ISV enclave QUOTE was verified correctly and the
    /// TCB level of the SGX platform is up-to-date.
    OK,
    /// EPID signature of the ISV enclave QUOTE was invalid. The content of the
    /// QUOTE is not trustworthy.
    ///
    /// For DCAP, the signature over the application report is invalid.
    SignatureInvalid,
    /// The EPID group has been revoked. When this value is returned, the
    /// revocation Reason field of the Attestation Verification Report will
    /// contain revocation reason code for this EPID group as reported in the
    /// EPID Group CRL. The content of the QUOTE is not trustworthy.
    GroupRevoked,
    /// The EPID private key used to sign the QUOTE has been revoked by
    /// signature. The content of the QUOTE is not trustworthy.
    SignatureRevoked,
    /// The EPID private key used to sign the QUOTE has been directly revoked
    /// (not by signature). The content of the QUOTE is not trustworthy.
    ///
    /// For DCAP, the attestation key or platform has been revoked.
    KeyRevoked,
    /// SigRL version in ISV enclave QUOTE does not match the most recent
    /// version of the SigRL. In rare situations, after SP retrieved the SigRL
    /// from IAS and provided it to the platform, a newer version of the SigRL
    /// is madeavailable. As a result, the Attestation Verification Report will
    /// indicate SIGRL_VERSION_MISMATCH. SP can retrieve the most recent version
    /// of SigRL from the IAS and request the platform to perform remote
    /// attestation again with the most recent version of SigRL. If the platform
    /// keeps failing to provide a valid QUOTE matching with the most recent
    /// version of the SigRL, the content of the QUOTE is not trustworthy.
    SigrlVersionMismatch,
    /// The EPID signature of the ISV enclave QUOTE has been verified correctly,
    /// but the TCB level of SGX platform is outdated (for further details see
    /// Advisory IDs). The platform has not been identified as compromised and
    /// thus it is not revoked. It is up to the Service Provider to decide
    /// whether or not to trust the content of the QUOTE, andwhether or not to
    /// trust the platform performing the attestation to protect specific
    /// sensitive information.
    GroupOutOfDate,
    /// The EPID signature of the ISV enclave QUOTE has been verified correctly,
    /// but additional configuration of SGX platform may be needed(for further
    /// details see Advisory IDs). The platform has not been identified as
    /// compromised and thus it is not revoked. It is up to the Service Provider
    /// to decide whether or not to trust the content of the QUOTE, and whether
    /// or not to trust the platform performing the attestation to protect
    /// specific sensitive information.
    ///
    /// For DCAP, The Quote verification passed and the platform is patched to
    /// the latest TCB level but additional configuration of the SGX
    /// platform may be needed.
    ConfigurationNeeded,
    /// The EPID signature of the ISV enclave QUOTE has been verified correctly
    /// but due to certain issues affecting the platform, additional SW
    /// Hardening in the attesting SGX enclaves may be needed.The relying party
    /// should evaluate the potential risk of an attack leveraging the relevant
    /// issues on the attesting enclave, and whether the attesting enclave
    /// employs adequate software hardening to mitigate the risk.
    SwHardeningNeeded,
    /// The EPID signature of the ISV enclave QUOTE has been verified correctly
    /// but additional configuration for the platform and SW Hardening in the
    /// attesting SGX enclaves may be needed. The platform has not been
    /// identified as compromised and thus it is not revoked. It is up to the
    /// Service Provider to decide whether or not to trust the content of the
    /// QUOTE. The relying party should also evaluate the potential risk of an
    /// attack leveraging the relevant issues on the attestation enclave, and
    /// whether the attesting enclave employs adequate software hardening to
    /// mitigate the risk.
    ConfigurationAndSwHardeningNeeded,
    /// DCAP specific quote status. The Quote is good but TCB level of the
    /// platform is out of date. The platform needs patching to be at the latest
    /// TCB level.
    OutOfDate,
    /// DCAP specific quote status. The Quote is good but the TCB level of the
    /// platform is out of date and additional configuration of the SGX Platform
    /// at its current patching level may be needed. The platform needs patching
    /// to be at the latest TCB level.
    OutOfDateConfigurationNeeded,
    /// Other unknown bad status.
    UnknownBadStatus,
}

impl From<&SgxQuoteStatus> for NodeAuthResult {
    fn from(status: &SgxQuoteStatus) -> Self {
        match status {
            SgxQuoteStatus::ConfigurationAndSwHardeningNeeded => {
                NodeAuthResult::SwHardeningAndConfigurationNeeded
            }
            SgxQuoteStatus::ConfigurationNeeded => NodeAuthResult::ConfigurationNeeded,
            SgxQuoteStatus::GroupOutOfDate => NodeAuthResult::GroupOutOfDate,
            SgxQuoteStatus::KeyRevoked => NodeAuthResult::KeyRevoked,
            SgxQuoteStatus::SigrlVersionMismatch => NodeAuthResult::SigrlVersionMismatch,
            SgxQuoteStatus::SignatureRevoked => NodeAuthResult::SignatureRevoked,
            SgxQuoteStatus::GroupRevoked => NodeAuthResult::GroupRevoked,
            _ => NodeAuthResult::BadQuoteStatus,
        }
    }
}

impl From<&str> for SgxQuoteStatus {
    /// Convert from str status from the report to enum.
    fn from(status: &str) -> Self {
        match status {
            "OK" => SgxQuoteStatus::OK,
            "SIGNATURE_INVALID" => SgxQuoteStatus::SignatureInvalid,
            "GROUP_REVOKED" => SgxQuoteStatus::GroupRevoked,
            "SIGNATURE_REVOKED" => SgxQuoteStatus::SignatureRevoked,
            "KEY_REVOKED" => SgxQuoteStatus::KeyRevoked,
            "SIGRL_VERSION_MISMATCH" => SgxQuoteStatus::SigrlVersionMismatch,
            "GROUP_OUT_OF_DATE" => SgxQuoteStatus::GroupOutOfDate,
            "OUT_OF_DATE" => SgxQuoteStatus::OutOfDate,
            "OUT_OF_DATE_CONFIGURATION_NEEDED" => SgxQuoteStatus::OutOfDateConfigurationNeeded,
            "CONFIGURATION_NEEDED" => SgxQuoteStatus::ConfigurationNeeded,
            "SW_HARDENING_NEEDED" => SgxQuoteStatus::SwHardeningNeeded,
            "CONFIGURATION_AND_SW_HARDENING_NEEDED" => {
                SgxQuoteStatus::ConfigurationAndSwHardeningNeeded
            }
            _ => SgxQuoteStatus::UnknownBadStatus,
        }
    }
}

/// An application that hosts an enclave can ask the enclave to produce a report
/// (`SgxEnclaveReport`) and then pass this report to a platform service
/// (Quoting Enclave) to produce a type of credential that reflects the enclave
/// and platform state. The quote can be passed to entities off the platform,
/// and verified using Intel EPID signature verification techniques.
pub struct SgxQuote {
    /// Version of the quote structure
    pub version: SgxQuoteVersion,
    /// ID of the Intel EPID group of the platform belongs to
    pub gid: u32,
    /// Security version number of Quoting Enclave
    pub isv_svn_qe: u16,
    /// Security version number of PCE
    pub isv_svn_pce: u16,
    /// Vendor ID of Quoting Enclave
    pub qe_vendor_id: Uuid,
    /// User data
    pub user_data: [u8; 20],
    /// Report generated by the enclave
    pub isv_enclave_report: SgxEnclaveReport,
}

impl std::fmt::Debug for SgxQuote {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        writeln!(f, "version: {:?}", self.version)?;
        writeln!(f, "gid: {}", self.gid)?;
        writeln!(f, "isv_svn_qe: {}", self.isv_svn_qe)?;
        writeln!(f, "isv_svn_pce: {}", self.isv_svn_pce)?;
        writeln!(f, "qe_vendor_id: {}", self.qe_vendor_id)?;
        writeln!(f, "user_data: {:?}", &self.user_data)?;
        writeln!(f, "isv_enclave_report: \n{:?}", self.isv_enclave_report)
    }
}

impl SgxQuote {
    /// Parse from bytes to `SgxQuote`.
    // just unused in SW mode
    #[allow(dead_code)]
    fn parse_from<'a>(bytes: &'a [u8]) -> Result<Self, Error> {
        let mut pos: usize = 0;
        let mut take = |n: usize| -> Result<&'a [u8], Error> {
            if n > 0 && bytes.len() >= pos + n {
                let ret = &bytes[pos..pos + n];
                pos += n;
                Ok(ret)
            } else {
                warn!("Quote parsing error.");
                Err(Error::ReportParseError)
            }
        };

        // Parse by bytes according to specifications.
        // off 0, size 2 + 2
        let version = match u16::from_le_bytes(<[u8; 2]>::try_from(take(2)?)?) {
            1 => {
                let signature_type = match u16::from_le_bytes(<[u8; 2]>::try_from(
                    take(2).map_err(|_| Error::ReportParseError)?,
                )?) {
                    0 => SgxEpidQuoteSigType::Unlinkable,
                    1 => SgxEpidQuoteSigType::Linkable,
                    _ => {
                        warn!("Invalid v1 quote signature type");
                        return Err(Error::ReportParseError);
                    }
                };
                SgxQuoteVersion::V1(signature_type)
            }
            2 => {
                let signature_type = match u16::from_le_bytes(<[u8; 2]>::try_from(
                    take(2).map_err(|_| Error::ReportParseError)?,
                )?) {
                    0 => SgxEpidQuoteSigType::Unlinkable,
                    1 => SgxEpidQuoteSigType::Linkable,
                    _ => {
                        warn!("Invalid v2 quote signature type");
                        return Err(Error::ReportParseError);
                    }
                };
                SgxQuoteVersion::V2(signature_type)
            }
            3 => {
                let attestation_key_type = match u16::from_le_bytes(<[u8; 2]>::try_from(
                    take(2).map_err(|_| Error::ReportParseError)?,
                )?) {
                    2 => SgxEcdsaQuoteAkType::P256_256,
                    3 => SgxEcdsaQuoteAkType::P384_384,
                    _ => {
                        warn!("Quote parsing error - ecdsa quote type invalid");
                        return Err(Error::ReportParseError);
                    }
                };
                SgxQuoteVersion::V3(attestation_key_type)
            }
            _ => {
                warn!("Quote parsing error - Unknown quote version");
                return Err(Error::ReportParseError);
            }
        };

        // off 4, size 4
        let gid = u32::from_le_bytes(<[u8; 4]>::try_from(take(4).map_err(|_| {
            warn!("Failed to parse quote gid");
            Error::ReportParseError
        })?)?);

        // off 8, size 2
        let isv_svn_qe = u16::from_le_bytes(<[u8; 2]>::try_from(take(2).map_err(|_| {
            warn!("Failed to parse quote isv svn qe");
            Error::ReportParseError
        })?)?);

        // off 10, size 2
        let isv_svn_pce = u16::from_le_bytes(<[u8; 2]>::try_from(take(2).map_err(|_| {
            warn!("Failed to parse quote isv svn");
            Error::ReportParseError
        })?)?);

        // off 12, size 16
        let qe_vendor_id_raw =
            <[u8; 16]>::try_from(take(16).map_err(|_| Error::ReportParseError)?)?;
        let qe_vendor_id = Uuid::from_slice(&qe_vendor_id_raw).map_err(|_| {
            warn!("Failed to parse quote vendor id");
            Error::ReportParseError
        })?;

        // off 28, size 20
        let user_data = <[u8; 20]>::try_from(take(20).map_err(|_| {
            warn!("Failed to parse quote user data");
            Error::ReportParseError
        })?)?;

        // off 48, size 384
        let isv_enclave_report = SgxEnclaveReport::parse_from(take(384).map_err(|_| {
            warn!("Failed to parse enclave report");
            Error::ReportParseError
        })?)?;

        if pos != bytes.len() {
            warn!("Quote parsing error - Quote size different from expected");
            return Err(Error::ReportParseError);
        };

        Ok(Self {
            version,
            gid,
            isv_svn_qe,
            isv_svn_pce,
            qe_vendor_id,
            user_data,
            isv_enclave_report,
        })
    }
}

#[cfg(all(feature = "SGX_MODE_HW", not(feature = "production")))]
const WHITELISTED_ADVISORIES: &[&str] = &[
    "INTEL-SA-00334",
    "INTEL-SA-00219",
    "INTEL-SA-00615",
    "INTEL-SA-00657",
    "INTEL-SA-00767",
];

#[cfg(all(feature = "SGX_MODE_HW", feature = "production"))]
const WHITELISTED_ADVISORIES: &[&str] = &[
    "INTEL-SA-00334",
    "INTEL-SA-00219",
    "INTEL-SA-00615",
    "INTEL-SA-00657",
    "INTEL-SA-00767",
];

lazy_static! {
    static ref ADVISORY_DESC: HashMap<&'static str, &'static str> = [
        (
            "INTEL-SA-00161",
            "You must disable hyperthreading in the BIOS"
        ),
        (
            "INTEL-SA-00289",
            "You must disable overclocking/undervolting in the BIOS"
        ),
    ]
    .iter()
    .copied()
    .collect();
}

#[derive(Debug)]
pub struct AdvisoryIDs(pub Vec<String>);

#[cfg(feature = "SGX_MODE_HW")]
impl AdvisoryIDs {
    pub(crate) fn vulnerable(&self) -> Vec<String> {
        let mut vulnerable: Vec<String> = vec![];
        for i in self.0.iter() {
            if !WHITELISTED_ADVISORIES.contains(&i.as_str()) {
                vulnerable.push(i.clone());
                if let Some(v) = ADVISORY_DESC.get(&i.as_str()) {
                    vulnerable.push((*v).to_string())
                }
            }
        }
        vulnerable
    }
}

/// A report that can be signed by Intel EPID (which generates
/// `EndorsedAttestationReport`) and then sent off of the platform to be
/// verified by remote client.
#[derive(Debug)]
pub struct AttestationReport {
    /// The freshness of the report, i.e., elapsed time after acquiring the
    /// report in seconds.
    pub timestamp: u64,
    /// Quote status
    pub sgx_quote_status: SgxQuoteStatus,
    /// Content of the quote
    pub sgx_quote_body: SgxQuote,
    pub platform_info_blob: Option<Vec<u8>>,
    pub advisory_ids: AdvisoryIDs,
    pub tcb_eval_data_number: u16,
}

impl AttestationReport {
    /// Construct a AttestationReport from a X509 certificate and verify
    /// attestation report with the report_ca_cert which is from the attestation
    /// service provider.
    // just unused in SW mode
    #[allow(dead_code)]
    pub fn from_cert(cert: &[u8]) -> Result<Self, Error> {
        let payload = get_netscape_comment(cert).map_err(|_err| {
            error!("Failed to get netscape comment");
            Error::ReportParseError
        })?;

        // Convert to endorsed report
        let report: EndorsedAttestationReport = serde_json::from_slice(&payload)?;

        // Verify report's signature - aka intel's signing cert
        let signing_cert = webpki::EndEntityCert::from(&report.signing_cert).map_err(|_err| {
            error!("Failed to validate signature");
            Error::ReportParseError
        })?;

        let (ias_cert, root_store) = get_ias_auth_config();

        let trust_anchors: Vec<webpki::TrustAnchor> = root_store
            .roots
            .iter()
            .map(|cert| cert.to_trust_anchor())
            .collect();

        let chain: Vec<&[u8]> = vec![&ias_cert];

        // set as 04.11.23(dd.mm.yy) - should be valid for the foreseeable future, and not rely on SystemTime
        let time_stamp = webpki::Time::from_seconds_since_unix_epoch(1723218496);

        // note: there's no way to not validate the time, and we don't want to write this code
        // ourselves. We also can't just ignore the error message, since that means that the rest of
        // the validation didn't happen (time is validated early on)
        match signing_cert.verify_is_valid_tls_server_cert(
            SUPPORTED_SIG_ALGS,
            &webpki::TLSServerTrustAnchors(&trust_anchors),
            &chain,
            time_stamp,
        ) {
            Ok(_) => info!("Certificate verified successfully"),
            Err(e) => {
                error!("Certificate verification error {:?}", e);
                return Err(Error::ReportValidationError);
            }
        };

        // Verify the signature against the signing cert
        match signing_cert.verify_signature(
            &webpki::RSA_PKCS1_2048_8192_SHA256,
            &report.report,
            &report.signature,
        ) {
            Ok(_) => info!("Signature verified successfully"),
            Err(e) => {
                warn!("Signature verification error {:?}", e);
                return Err(Error::ReportParseError);
            }
        }

        // Verify and extract information from attestation report
        let attn_report: Value = serde_json::from_slice(&report.report)?;
        trace!("attn_report: {}", attn_report);

        // Verify API version is supported
        let version = attn_report["version"]
            .as_u64()
            .ok_or(Error::ReportParseError)?;

        if version != 5 {
            warn!("API version incompatible");
            return Err(Error::ReportParseError);
        };

        let mut platform_info_blob = None;
        if let Some(blob) = attn_report["platformInfoBlob"].as_str() {
            let as_binary = hex::decode(blob).map_err(|_| {
                warn!("Error parsing platform info");
                Error::ReportParseError
            })?;
            platform_info_blob = Some(as_binary)
        }

        // Get quote status
        let sgx_quote_status = {
            let status_string = attn_report["isvEnclaveQuoteStatus"]
                .as_str()
                .ok_or_else(|| {
                    warn!("Error parsing enclave quote status");
                    Error::ReportParseError
                })?;
            SgxQuoteStatus::from(status_string)
        };

        // Get quote body
        let sgx_quote_body = {
            let quote_encoded = attn_report["isvEnclaveQuoteBody"].as_str().ok_or_else(|| {
                warn!("Error unpacking enclave quote body");
                Error::ReportParseError
            })?;
            let quote_raw = base64::decode(quote_encoded.as_bytes()).map_err(|_| {
                warn!("Error decoding encoded quote body");
                Error::ReportParseError
            })?;
            SgxQuote::parse_from(quote_raw.as_slice())?
        };

        let advisories: Vec<String> = if let Some(raw) = attn_report.get("advisoryIDs") {
            serde_json::from_value(raw.clone()).map_err(|_| {
                warn!("Failed to decode advisories");
                Error::ReportParseError
            })?
        } else {
            vec![]
        };

        let tcb_eval_data_number = attn_report["tcbEvaluationDataNumber"]
            .as_u64()
            .ok_or(Error::ReportParseError)? as u16;

        let timestamp_str = attn_report["timestamp"]
            .as_str()
            .ok_or(Error::ReportParseError)?;

        let timestamp_rfc = format!("{}Z", timestamp_str);
        let time = chrono::DateTime::parse_from_rfc3339(&timestamp_rfc).map_err(|e| {
            warn!("Failed to decode timestamp: {}", e);
            Error::ReportParseError
        })?;
        let timestamp_since_epoch = time.timestamp();

        // We don't actually validate the public key, since we use ephemeral certificates,
        // and all we really care about that the report is valid and the key that is saved in the
        // report_data field

        Ok(Self {
            timestamp: timestamp_since_epoch as u64,
            sgx_quote_status,
            sgx_quote_body,
            platform_info_blob,
            advisory_ids: AdvisoryIDs(advisories),
            tcb_eval_data_number,
        })
    }
}

#[cfg(feature = "test")]
pub mod tests {
    use serde_json::json;
    use std::io::Read;
    use std::untrusted::fs::File;

    use crate::registration::attestation::verify_quote_ecdsa;

    use super::*;

    fn tls_ra_cert_der_test() -> Vec<u8> {
        let mut cert = vec![];
        let mut f =
            File::open("../execute/src/registration/fixtures/attestation_cert_hw_invalid_test.der")
                .unwrap();
        f.read_to_end(&mut cert).unwrap();

        cert
    }

    fn tls_ra_cert_der_v3() -> Vec<u8> {
        let mut cert = vec![];
        let mut f = File::open("../execute/src/registration/fixtures/tls_ra_cert_v3.der").unwrap();
        f.read_to_end(&mut cert).unwrap();

        cert
    }

    fn tls_ra_cert_der_v4() -> Vec<u8> {
        let mut cert = vec![];
        let mut f =
            File::open("../execute/src/registration/fixtures/attestation_cert_out_of_date.der")
                .unwrap();
        f.read_to_end(&mut cert).unwrap();

        cert
    }

    fn _test_aes_encrypttls_ra_cert_der_out_of_date() -> Vec<u8> {
        let mut cert = vec![];
        let mut f = File::open(
            "../execute/src/registration/fixtures/attestation_cert_sw_config_needed.der",
        )
        .unwrap();
        f.read_to_end(&mut cert).unwrap();

        cert
    }

    fn _ias_root_ca_cert_der() -> Vec<u8> {
        let mut cert = vec![];
        let mut f =
            File::open("../execute/src/registration/fixtures/ias_root_ca_cert.der").unwrap();
        f.read_to_end(&mut cert).unwrap();

        cert
    }

    fn attesation_report() -> Value {
        let report = json!({
            "version": 3,
            "timestamp": "2020-02-11T22:25:59.682915",
            "platformInfoBlob": "1502006504000900000D0D02040180030000000000000000000\
                                 A00000B000000020000000000000B2FE0AE0F7FD4D552BF7EF4\
                                 C938D44E349F1BD0E76F041362DC52B43B7B25994978D792137\
                                 90362F6DAE91797ACF5BD5072E45F9A60795D1FFB10140421D8\
                                 691FFD",
            "isvEnclaveQuoteStatus": "GROUP_OUT_OF_DATE",
            "isvEnclaveQuoteBody": "AgABAC8LAAAKAAkAAAAAAK1zRQOIpndiP4IhlnW2AkwAAAAA\
                                    AAAAAAAAAAAAAAAABQ4CBf+AAAAAAAAAAAAAAAAAAAAAAAAA\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABwAAAAAAAAAHAAAA\
                                    AAAAADMKqRCjd2eA4gAmrj2sB68OWpMfhPH4MH27hZAvWGlT\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACD1xnn\
                                    ferKFHD2uvYqTXdDA8iZ22kCD5xw7h38CMfOngAAAAAAAAAA\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\
                                    AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\
                                    AAAAAAAAAADYIY9k0MVmCdIDUuFLf/2bGIHAfPjO9nvC7fgz\
                                    rQedeA3WW4dFeI6oe+RCLdV3XYD1n6lEZjITOzPPLWDxulGz",
            "id": "53530608302195762335736519878284384788",
            "epidPseudonym": "NRksaQej8R/SyyHpZXzQGNBXqfrzPy5KCxcmJrEjupXrq3xrm2y2+J\
                              p0IBVtcW15MCekYs9K3UH82fPyj6F5ciJoMsgEMEIvRR+csX9uyd54\
                              p+m+/RVyuGYhWbhUcpJigdI5Q3x04GG/A7EP10j/zypwqhYLQh0qN1\
                              ykYt1N1P0="
        });

        report
    }

    pub fn test_sgx_quote_parse_from() {
        let attn_report = attesation_report();
        let sgx_quote_body_encoded = attn_report["isvEnclaveQuoteBody"].as_str().unwrap();
        let quote_raw = base64::decode(&sgx_quote_body_encoded.as_bytes()).unwrap();
        let sgx_quote = SgxQuote::parse_from(quote_raw.as_slice()).unwrap();

        assert_eq!(
            sgx_quote.version,
            SgxQuoteVersion::V2(SgxEpidQuoteSigType::Linkable)
        );
        assert_eq!(sgx_quote.gid, 2863);
        assert_eq!(sgx_quote.isv_svn_qe, 10);
        assert_eq!(sgx_quote.isv_svn_pce, 9);
        assert_eq!(
            sgx_quote.qe_vendor_id,
            Uuid::parse_str("00000000-ad73-4503-88a6-77623f822196").unwrap()
        );
        assert_eq!(
            sgx_quote.user_data,
            [117, 182, 2, 76, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
        );

        let isv_enclave_report = sgx_quote.isv_enclave_report;
        assert_eq!(
            isv_enclave_report.cpu_svn,
            [5, 14, 2, 5, 255, 128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
        );
        assert_eq!(isv_enclave_report.misc_select, 0);
        assert_eq!(
            isv_enclave_report.attributes,
            [7, 0, 0, 0, 0, 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0]
        );
        assert_eq!(
            isv_enclave_report.mr_enclave,
            [
                51, 10, 169, 16, 163, 119, 103, 128, 226, 0, 38, 174, 61, 172, 7, 175, 14, 90, 147,
                31, 132, 241, 248, 48, 125, 187, 133, 144, 47, 88, 105, 83
            ]
        );
        assert_eq!(
            isv_enclave_report.mr_signer,
            [
                131, 215, 25, 231, 125, 234, 202, 20, 112, 246, 186, 246, 42, 77, 119, 67, 3, 200,
                153, 219, 105, 2, 15, 156, 112, 238, 29, 252, 8, 199, 206, 158
            ]
        );
        assert_eq!(isv_enclave_report.isv_prod_id, 0);
        assert_eq!(isv_enclave_report.isv_svn, 0);
        assert_eq!(
            isv_enclave_report.report_data.to_vec(),
            [
                216, 33, 143, 100, 208, 197, 102, 9, 210, 3, 82, 225, 75, 127, 253, 155, 24, 129,
                192, 124, 248, 206, 246, 123, 194, 237, 248, 51, 173, 7, 157, 120, 13, 214, 91,
                135, 69, 120, 142, 168, 123, 228, 66, 45, 213, 119, 93, 128, 245, 159, 169, 68,
                102, 50, 19, 59, 51, 207, 45, 96, 241, 186, 81, 179
            ]
            .to_vec()
        );
    }

    pub fn test_attestation_report_from_cert() {
        let tls_ra_cert = tls_ra_cert_der_v4();
        let report = AttestationReport::from_cert(&tls_ra_cert);
        assert!(report.is_ok());

        let report = report.unwrap();
        assert_eq!(report.sgx_quote_status, SgxQuoteStatus::GroupOutOfDate);
    }

    pub fn test_attestation_report_from_cert_invalid() {
        let tls_ra_cert = tls_ra_cert_der_v4();
        let report = AttestationReport::from_cert(&tls_ra_cert);
        assert!(report.is_ok());

        let report = report.unwrap();
        assert_eq!(report.sgx_quote_status, SgxQuoteStatus::GroupOutOfDate);
    }

    pub fn test_attestation_report_from_cert_api_version_not_compatible() {
        let tls_ra_cert = tls_ra_cert_der_v3();
        let report = AttestationReport::from_cert(&tls_ra_cert);
        assert!(report.is_err());
    }

    pub fn test_attestation_report_test() {
        let tls_ra_cert = tls_ra_cert_der_test();
        let report = AttestationReport::from_cert(&tls_ra_cert);

        if report.is_err() {
            println!("err: {:?}", report)
        }

        assert!(report.is_ok());
    }

    fn load_attestation_dcap() -> (Vec<u8>, Vec<u8>, i64) {
        let mut vec_quote = vec![];
        {
            let mut f =
                File::open("../execute/src/registration/fixtures/attestation_dcap.quote").unwrap();
            f.read_to_end(&mut vec_quote).unwrap();
        }

        let mut vec_coll = vec![];
        {
            let mut f = File::open(
                "../execute/src/registration/fixtures/attestation_dcap.quote.collateral",
            )
            .unwrap();
            f.read_to_end(&mut vec_coll).unwrap();
        }
        (vec_quote, vec_coll, 1709649832)
    }

    pub fn test_attestation_dcap() {
        let (vec_quote, vec_coll, time_s) = load_attestation_dcap();

        let res = verify_quote_ecdsa(&vec_quote, &vec_coll, time_s);
        assert!(res.is_ok());
    }

    pub fn test_attestation_dcap_temper() {
        let (vec_quote, vec_coll, time_s) = load_attestation_dcap();

        // tamper with quote
        let mut my_p_quote = vec_quote.as_mut_ptr() as *mut sgx_quote_t;
        unsafe {
            let mut p_report = (*my_p_quote).report_body;
            let mut p_data = p_report.report_data;
            (*p_data).d[6] = (*p_data).d[6] + 4;
        };

        let res = verify_quote_ecdsa(&vec_quote, &vec_coll, time_s);
        assert!(res.is_ok());
    }
}
