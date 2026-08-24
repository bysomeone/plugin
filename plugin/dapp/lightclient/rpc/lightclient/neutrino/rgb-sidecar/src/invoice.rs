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
pub fn build_address_invoice(
    network: Network,
    script: &bitcoin::ScriptBuf,
    contract_id: ContractId,
    schema_id: SchemaId,
    amount: u64,
) -> Result<String> {
    let chain_net = network_to_chainnet(network);
    let payload = AddressPayload::from_script(script)
        .map_err(|e| anyhow!("script to address payload: {e}"))?;
    let beneficiary = XChainNet::with(chain_net, Beneficiary::WitnessVout(Pay2Vout::new(payload), None));
    let invoice = RgbInvoiceBuilder::new(beneficiary)
        .set_contract(contract_id)
        .set_schema(schema_id)
        .set_amount_raw(amount)
        .set_assignment_name("assetOwner")
        .finish();
    Ok(invoice.to_string())
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
}

pub fn parse_invoice(s: &str) -> Result<ParsedInvoice> {
    let invoice = RgbInvoice::from_str(s).map_err(|e| anyhow!("parse invoice: {e}"))?;
    let beneficiary = invoice.beneficiary.into_inner();
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
    })
}

/// Convenience used by the E2E: serialize/deserialize consignment bytes.
pub fn consignment_to_bytes<const TRANSFER: bool>(c: &Consignment<TRANSFER>) -> Result<Vec<u8>> {
    use amplify::confinement::U24;
    use strict_encoding::StrictSerialize;
    Ok(c.to_strict_serialized::<U24>()?.release())
}

pub fn consignment_from_bytes(bytes: &[u8]) -> Result<Consignment<true>> {
    use amplify::confinement::{Confined, U24};
    use strict_encoding::StrictDeserialize;
    let confined: Confined<Vec<u8>, 0, U24> = Confined::try_from(bytes.to_vec())
        .map_err(|e| anyhow!("consignment too large: {e}"))?;
    Consignment::<true>::from_strict_serialized::<U24>(confined)
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
