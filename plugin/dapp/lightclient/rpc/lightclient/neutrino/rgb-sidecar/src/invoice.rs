//! RGB invoice build/parse helpers (rgb-invoicing 0.11.1-rc.11).
//!
//! The sidecar issues **WitnessVout** (address-based) invoices so an external RGB wallet can
//! pay them without any out-of-band vout coordination. Blinded-receive invoices are a v1
//! limitation (documented in SPIKE.md Phase 2a).

use std::str::FromStr;

use anyhow::{anyhow, Result};
use bitcoin::Network;
use rgbcore::{ChainNet, ContractId, SchemaId, SecretSeal};
use rgbstd::containers::Consignment;
use rgbstd::invoice::{Amount, RgbInvoice};
use rgbinvoice::{AddressPayload, Beneficiary, Pay2Vout, RgbInvoiceBuilder, XChainNet};

/// Build an address-based RGB invoice for receiving `amount` (asset min units) of
/// `contract_id` at `script`.
///
/// `transport_endpoint` is written into the invoice's standard `transports` list when non-empty
/// (e.g. `rpcs://gw.example.com/0.2/json-rpc`), telling the payer where to deliver the
/// consignment. An empty/absent endpoint leaves the list empty.
pub fn build_address_invoice(
    network: Network,
    script: &bitcoin::ScriptBuf,
    contract_id: ContractId,
    schema_id: SchemaId,
    amount: u64,
    transport_endpoint: Option<&str>,
) -> Result<String> {
    let chain_net = network_to_chainnet(network);
    let payload = AddressPayload::from_script(script)
        .map_err(|e| anyhow!("script to address payload: {e}"))?;
    let beneficiary = XChainNet::with(chain_net, Beneficiary::WitnessVout(Pay2Vout::new(payload), None));
    let mut builder = RgbInvoiceBuilder::new(beneficiary)
        .set_contract(contract_id)
        .set_schema(schema_id)
        .set_amount_raw(amount)
        .set_assignment_name("assetOwner");
    if let Some(endpoint) = transport_endpoint.filter(|endpoint| !endpoint.is_empty()) {
        builder = builder
            .add_transport(endpoint)
            .map_err(|(_builder, e)| anyhow!("invalid transport endpoint {endpoint:?}: {e}"))?;
    }
    Ok(builder.finish().to_string())
}

/// The standard RGB HTTP JSON-RPC `recipient_id` of an invoice: the beneficiary identifier
/// (`<chain-net>:<beneficiary>`), exactly as it appears in the invoice.
///
/// The sidecar returns it to the Go bridge after `CreateReceive`, so the bridge never has to
/// parse RGB. Parsing the invoice (rather than re-deriving the beneficiary) keeps the value
/// tied to the bytes actually handed out.
pub fn recipient_id(invoice: &str) -> Result<String> {
    let invoice = RgbInvoice::from_str(invoice).map_err(|e| anyhow!("parse invoice: {e}"))?;
    Ok(invoice.beneficiary.to_string())
}

/// The IFA/RGB20 schema id (used by the test-sim to build user invoices).
pub fn ifa_schema_id() -> SchemaId {
    use rgbstd::contract::IssuerWrapper;
    use schemata::InflatableFungibleAsset;
    InflatableFungibleAsset::schema().schema_id()
}

/// A parsed invoice's essential data for building a transfer to it.
#[derive(Clone, Debug)]
pub struct ParsedInvoice {
    pub contract_id: Option<ContractId>,
    pub schema_id: Option<SchemaId>,
    pub amount: Option<Amount>,
    /// For `WitnessVout` beneficiaries: the output script the sender must pay.
    pub witness_script: Option<bitcoin::ScriptBuf>,
    /// For `BlindedSeal` beneficiaries: the concealed seal to spend to.
    pub secret_seal: Option<SecretSeal>,
    /// Transport endpoints the sender may use to deliver the consignment.
    pub transports: Vec<String>,
    /// UTC unix timestamp after which the invoice is no longer valid.
    pub expiry: Option<i64>,
    /// Assignment name the transfer must target (e.g. `assetOwner`).
    pub assignment_name: Option<String>,
}

pub fn parse_invoice(s: &str) -> Result<ParsedInvoice> {
    let invoice = RgbInvoice::from_str(s).map_err(|e| anyhow!("parse invoice: {e}"))?;
    let beneficiary = invoice.beneficiary.clone().into_inner();
    let (witness_script, secret_seal) = match beneficiary {
        Beneficiary::WitnessVout(pay2vout, _) => (Some(pay2vout.to_script()), None),
        Beneficiary::BlindedSeal(secret) => (None, Some(secret)),
    };
    let amount = match invoice.assignment_state {
        Some(rgbstd::invoice::InvoiceState::Amount(a)) => Some(a),
        _ => None,
    };
    Ok(ParsedInvoice {
        contract_id: invoice.contract,
        schema_id: invoice.schema,
        amount,
        witness_script,
        secret_seal,
        transports: invoice.transports.iter().map(ToString::to_string).collect(),
        expiry: invoice.expiry,
        assignment_name: invoice.assignment_name.as_ref().map(ToString::to_string),
    })
}

/// Convenience used by the E2E: serialize/deserialize consignment bytes.
pub fn consignment_to_bytes<const TRANSFER: bool>(c: &Consignment<TRANSFER>) -> Result<Vec<u8>> {
    use amplify::confinement::U24;
    use strict_encoding::StrictSerialize;
    Ok(c.to_strict_serialized::<U24>()?.release())
}

pub fn consignment_from_bytes(bytes: &[u8]) -> Result<Consignment<true>> {
    deserialize_consignment::<true>(bytes)
}

/// Parse a **genesis** (non-transfer) contract consignment — the form an issuer publishes so
/// that somebody else can adopt the contract. [`consignment_from_bytes`] only accepts the
/// transfer form, so adoption needs its own entry point.
pub fn contract_from_bytes(bytes: &[u8]) -> Result<Consignment<false>> {
    deserialize_consignment::<false>(bytes)
}

fn deserialize_consignment<const TRANSFER: bool>(bytes: &[u8]) -> Result<Consignment<TRANSFER>> {
    use amplify::confinement::{Confined, U24};
    use strict_encoding::StrictDeserialize;
    let confined: Confined<Vec<u8>, 0, U24> = Confined::try_from(bytes.to_vec())
        .map_err(|e| anyhow!("consignment too large: {e}"))?;
    Consignment::<TRANSFER>::from_strict_serialized::<U24>(confined)
        .map_err(|e| anyhow!("deserialize consignment: {e}"))
}

pub fn network_to_chainnet(network: Network) -> ChainNet {
    match network {
        Network::Bitcoin => ChainNet::BitcoinMainnet,
        Network::Testnet => ChainNet::BitcoinTestnet3,
        Network::Testnet4 => ChainNet::BitcoinTestnet4,
        Network::Signet => ChainNet::BitcoinSignet,
        Network::Regtest => ChainNet::BitcoinRegtest,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn script() -> bitcoin::ScriptBuf {
        // A syntactically valid P2WPKH output: OP_0 <20-byte hash>.
        let mut bytes = vec![0x00u8, 0x14];
        bytes.extend([7u8; 20]);
        bitcoin::ScriptBuf::from_bytes(bytes)
    }

    fn invoice(endpoint: Option<&str>) -> String {
        build_address_invoice(
            Network::Regtest,
            &script(),
            ContractId::from([1u8; 32]),
            SchemaId::from([2u8; 32]),
            42,
            endpoint,
        )
        .unwrap()
    }

    #[test]
    fn endpoint_is_embedded_in_transports_and_parses_back() {
        let endpoint = "rpcs://gw.example.com/0.2/json-rpc";
        let s = invoice(Some(endpoint));
        let parsed = parse_invoice(&s).unwrap();
        assert_eq!(parsed.transports, vec![endpoint.to_string()]);
        assert_eq!(parsed.amount.unwrap().value(), 42);
        assert_eq!(parsed.assignment_name.as_deref(), Some("assetOwner"));
        assert_eq!(parsed.expiry, None);
        assert!(parsed.witness_script.is_some());
    }

    #[test]
    fn no_endpoint_means_no_transports() {
        let parsed = parse_invoice(&invoice(None)).unwrap();
        assert!(parsed.transports.is_empty());
        assert!(parse_invoice(&invoice(Some(""))).unwrap().transports.is_empty());
    }

    #[test]
    fn invalid_endpoint_is_rejected() {
        let err = build_address_invoice(
            Network::Regtest,
            &script(),
            ContractId::from([1u8; 32]),
            SchemaId::from([2u8; 32]),
            42,
            Some("not-a-transport"),
        )
        .unwrap_err();
        assert!(format!("{err}").contains("invalid transport endpoint"));
    }

    #[test]
    fn recipient_id_is_the_beneficiary_identifier() {
        let s = invoice(Some("rpcs://gw.example.com/0.2/json-rpc"));
        let recipient = recipient_id(&s).unwrap();
        // The identifier carries the chain-net prefix and the beneficiary payload.
        assert!(recipient.starts_with("bcrt:"), "unexpected recipient id: {recipient}");
        // Stable across parses and equal to the invoice's own beneficiary rendering.
        assert_eq!(recipient, recipient_id(&s).unwrap());
        let parsed = parse_invoice(&s).unwrap();
        let raw = RgbInvoice::from_str(&s).unwrap();
        assert_eq!(recipient, raw.beneficiary.to_string());
        assert!(parsed.witness_script.is_some());
    }

    #[test]
    fn recipient_id_refuses_garbage() {
        assert!(recipient_id("not-an-invoice").is_err());
    }
}
