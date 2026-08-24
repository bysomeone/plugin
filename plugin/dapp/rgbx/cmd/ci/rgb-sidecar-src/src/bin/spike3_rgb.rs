//! Spike 3: RGB consensus layer — drive the split RGB crates DIRECTLY
//! (rgb-consensus 0.11.1-rc.11 + rgb-ops 0.11.1-rc.11 + rgb-schemas 0.11.1-rc.11 +
//! rgb-psbt-utils 0.11.1-rc.11), NOT rgb-lib.
//!
//! Verifies (with real compiled code, no wallet state / no private keys in the RGB path):
//!   (a) stateless consignment validation via `Consignment::validate(resolver, config)`
//!       (the planned `ValidateConsignment` gRPC);
//!   (b) construct an IFA state transition + opret anchor and build an UNSIGNED Bitcoin PSBT
//!       with EXPLICIT inputs (the planned `BuildWithdrawal` path);
//!   (c) issue an RGB20 (IFA) fungible asset and build a send between two "addresses";
//!   (d) is analysed in SPIKE.md (receive attribution is our own design).
//! Goal 4: the resulting PSBT is signed by an EXTERNAL private key (test key only — the real
//! signer is the TSS group) and the RGB layer still accepts it (segwit txid is signature-independent).

use std::collections::BTreeMap;
use std::str::FromStr;

use anyhow::{anyhow, Result};
use amplify::confinement::U24;
// rgb-ops is published under the lib name `rgbstd`; rgb-consensus as `rgbcore`;
// rgb-invoicing as `rgbinvoice`; rgb-schemas as `schemata`; rgb-psbt-utils as `psrgbt`.
use psrgbt::{RgbOutExt, RgbPsbtExt};
use rgbinvoice::Precision;
use rgbcore::validation::{ResolveWitness, ValidationConfig, WitnessResolverError, WitnessStatus};
use rgbcore::vm::WitnessOrd;
use rgbcore::{ChainNet, Opout, Operation, Txid};
use rgbstd::containers::{Consignment, ConsignmentExt};
use rgbstd::contract::{AllocatedState, ContractBuilder, IssuerWrapper, TransitionBuilder};
use rgbstd::invoice::Amount;
use rgbstd::persistence::Stock;
use rgbstd::stl::{AssetSpec, ContractTerms, RicardianContract};
use rgbstd::txout::{BlindSeal, CloseMethod, TxPtr};
use rgbstd::{Identity, OutputSeal};
use schemata::{InflatableFungibleAsset, OS_ASSET};
use strict_encoding::{StrictDeserialize, StrictSerialize};

use bitcoin::absolute::LockTime;
use bitcoin::sighash::{EcdsaSighashType, SighashCache};
use bitcoin::{
    Address, CompressedPublicKey, Network, OutPoint, Psbt, ScriptBuf, Sequence, Transaction, TxIn,
    TxOut, Witness,
};

/// Genesis seal "UTXO" — a dummy outpoint owned by the TSS script (shared P2WPKH).
/// In the real sidecar this is a real funded UTXO at the TSS script.
const GENESIS_TXID: &str = "1111111111111111111111111111111111111111111111111111111111111111";

/// A `ResolveWitness` implementation backed by a map of known txs.
/// The sidecar will point this at bitcoind/electrs (its own chain view).
struct MapResolver {
    txs: BTreeMap<Txid, bitcoin::Transaction>,
}
impl ResolveWitness for MapResolver {
    fn resolve_witness(&self, witness_id: Txid) -> Result<WitnessStatus, WitnessResolverError> {
        match self.txs.get(&witness_id) {
            Some(tx) => Ok(WitnessStatus::Resolved(tx.clone(), WitnessOrd::Tentative)),
            None => Ok(WitnessStatus::Unresolved),
        }
    }
    fn check_chain_net(&self, chain_net: ChainNet) -> Result<(), WitnessResolverError> {
        if chain_net == ChainNet::BitcoinRegtest {
            Ok(())
        } else {
            Err(WitnessResolverError::WrongChainNet)
        }
    }
}

fn main() -> Result<()> {
    let chain_net = ChainNet::BitcoinRegtest;

    // A deterministic TEST signing key (stand-in for the TSS group's bare secp256k1 pubkey).
    // The descriptor/script used below is P2WPKH of THIS pubkey — a real single-script wallet.
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let signer_secret = bitcoin::secp256k1::SecretKey::from_slice(&[0x11u8; 32])?;
    let signer_pubkey = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &signer_secret);
    let tss_pubkey_hex =
        bitcoin::hex::DisplayHex::to_hex_string(&signer_pubkey.serialize(), bitcoin::hex::Case::Lower);
    let tss_script = {
        let pk = CompressedPublicKey::from_slice(&signer_pubkey.serialize())?;
        Address::p2wpkh(&pk, Network::Regtest).script_pubkey()
    };
    println!("SPIKE3: single-script (TSS stand-in) pubkey={tss_pubkey_hex}");
    println!("SPIKE3:   P2WPKH script (shared by every UTXO) = {tss_script}");

    // =====================================================================
    // (c) ISSUE an RGB20 (IFA) fungible asset "USDT" (precision 8) on regtest
    // =====================================================================
    let schema = InflatableFungibleAsset::schema();
    let types = InflatableFungibleAsset::types();
    let scripts = InflatableFungibleAsset::scripts();

    let spec = AssetSpec::new("USDT", "Tether USD", Precision::CentiMicro);
    let terms = ContractTerms { text: RicardianContract::default(), media: None };
    let issued_supply: u64 = 1_000_000_000; // 10.0 USDT (8 decimals)

    let genesis_outpoint = OutPoint::new(Txid::from_str(GENESIS_TXID)?, 0);
    let genesis_seal = BlindSeal::<Txid>::new_random(genesis_outpoint.txid, genesis_outpoint.vout);

    let contract_builder = ContractBuilder::with(
        Identity::default(),
        schema.clone(),
        types.clone(),
        scripts.clone(),
        chain_net,
    )
    .add_global_state("spec", spec)?
    .add_global_state("terms", terms)?
    .add_global_state("issuedSupply", Amount::from(issued_supply))?
    .add_global_state("maxSupply", Amount::from(issued_supply))?
    .add_fungible_state("assetOwner", genesis_seal, Amount::from(issued_supply))?;

    let valid_contract = contract_builder
        .issue_contract()
        .map_err(|e| anyhow!("issue_contract failed: {e:#?}"))?;
    let contract_id = valid_contract.contract_id();
    let genesis_opid = valid_contract.genesis().id();
    println!("SPIKE3: issued IFA contract id={contract_id}");
    println!("SPIKE3:   schema_id={}", valid_contract.schema_id());
    println!("SPIKE3:   genesis_opid={genesis_opid}");
    println!(
        "SPIKE3:   issued_supply={} to genesis seal {}",
        issued_supply, genesis_outpoint
    );

    // =====================================================================
    // (a) STATELESS validation of the genesis consignment
    //     (Consignment::validate needs only a resolver + a ValidationConfig;
    //      no wallet, no Stock). This is the `ValidateConsignment` gRPC.
    // =====================================================================
    let resolver0 = MapResolver { txs: Default::default() };
    let config0 = ValidationConfig {
        chain_net,
        trusted_typesystem: types.clone(),
        ..Default::default()
    };
    let validated_contract = valid_contract
        .clone()
        .into_consignment()
        .validate(&resolver0, &config0)
        .map_err(|e| anyhow!("genesis validate failed: {e:#?}"))?;
    println!(
        "SPIKE3(a): genesis consignment validated statelessly, status={}",
        validated_contract.validation_status().validity()
    );

    // Import into an in-memory Stock (the sidecar's RGB state; not required for (a))
    let mut stock = Stock::in_memory();
    stock
        .import_contract(validated_contract.clone(), &resolver0)
        .map_err(|e| anyhow!("{e:?}"))?;

    // =====================================================================
    // (b) Build a transfer: spend 2.0 USDT of the genesis to a NEW recipient
    //     (the shared TSS script), keeping 8.0 USDT as "change".
    // =====================================================================
    let builder: TransitionBuilder = stock
        .transition_builder(contract_id, "transfer")
        .map_err(|e| anyhow!("{e:?}"))?;
    // input: the genesis OS_ASSET assignment at vout 0
    let input_opout = Opout::new(genesis_opid, OS_ASSET, 0);
    let builder = builder
        .add_input(input_opout, AllocatedState::from(Amount::from(issued_supply)))?;
    // outputs: recipient gets 2.0 USDT at witness-tx vout 1; change 8.0 USDT at vout 2
    let recipient_seal = BlindSeal::<TxPtr>::new_random_vout(1);
    let change_seal = BlindSeal::<TxPtr>::new_random_vout(2);
    let transition = builder
        .add_fungible_state("assetOwner", recipient_seal, Amount::from(200_000_000u64))?
        .add_fungible_state("assetOwner", change_seal, Amount::from(800_000_000u64))?
        .complete_transition()?;
    println!("SPIKE3(b): built transition opid={}", transition.id());
    println!(
        "SPIKE3(b):   inputs={:?}",
        transition.inputs().into_iter().collect::<Vec<_>>()
    );

    // --- Build the UNSIGNED bitcoin PSBT with EXPLICIT inputs ---
    // input[0] = genesis UTXO at the TSS script (value 100_000 sat)
    // output[0] = OP_RETURN (opret host — commitment written by rgb_commit)
    // output[1] = recipient 60_000 sat to TSS script
    // output[2] = change 39_000 sat to TSS script (100_000 - 60_000 - 1_000 fee)
    let tx = Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: LockTime::ZERO,
        input: vec![TxIn {
            previous_output: genesis_outpoint,
            script_sig: ScriptBuf::new(),
            sequence: Sequence::MAX,
            witness: Witness::new(),
        }],
        output: vec![
            TxOut { value: bitcoin::Amount::from_sat(0), script_pubkey: ScriptBuf::new_op_return([]) },
            TxOut { value: bitcoin::Amount::from_sat(60_000), script_pubkey: tss_script.clone() },
            TxOut { value: bitcoin::Amount::from_sat(39_000), script_pubkey: tss_script.clone() },
        ],
    };
    let mut psbt = Psbt::from_unsigned_tx(tx)?;
    psbt.inputs[0].witness_utxo = Some(TxOut {
        value: bitcoin::Amount::from_sat(100_000),
        script_pubkey: tss_script.clone(),
    });

    // --- Attach RGB transition + commit the opret anchor ---
    psbt.push_rgb_transition(transition.clone())?;
    let opret_idx = 0;
    psbt.outputs[opret_idx].set_opret_host();
    psbt.outputs[opret_idx].set_mpc_entropy(42)?;
    psbt.set_rgb_close_method(CloseMethod::OpretFirst);
    let fascia = psbt.rgb_commit()?;
    let witness_txid = psbt.get_txid();
    println!("SPIKE3(b): committed opret anchor, witness txid={witness_txid}");
    println!(
        "SPIKE3(b):   opret output script now: {:?}",
        psbt.unsigned_tx.output[opret_idx].script_pubkey
    );
    let unsigned_txid = psbt.unsigned_tx.compute_txid();
    assert_eq!(unsigned_txid, witness_txid);

    // --- Construct the Transfer consignment from the fascia + explicit outputs ---
    let recipient_out = OutputSeal::with(witness_txid, 1);
    let change_out = OutputSeal::with(witness_txid, 2);
    let transfer: Consignment<true> = stock
        .transfer_from_fascia(contract_id, [recipient_out, change_out], [], [], &fascia)
        .map_err(|e| anyhow!("{e:?}"))?;

    // Serialize round-trip (the consignment is fully self-contained)
    let bytes = transfer.clone().to_strict_serialized::<U24>()?;
    let transfer2 = Consignment::<true>::from_strict_serialized(bytes)?;
    assert_eq!(transfer.consignment_id(), transfer2.consignment_id());

    // =====================================================================
    // (a again) Validate the TRANSFER consignment deterministically, with a
    // resolver that supplies the witness tx (the resolver is the authoritative
    // chain view; in the sidecar this points at bitcoind/electrs). No Stock
    // needed — this is the `ValidateConsignment` gRPC for transfers.
    // =====================================================================
    let witness_tx = psbt.unsigned_tx.clone();
    let resolver1 = MapResolver { txs: BTreeMap::from([(witness_txid, witness_tx)]) };
    let config1 = ValidationConfig {
        chain_net,
        trusted_typesystem: types.clone(),
        ..Default::default()
    };
    let validated_transfer = transfer2
        .clone()
        .validate(&resolver1, &config1)
        .map_err(|e| anyhow!("transfer validate failed: {e:#?}"))?;
    println!(
        "SPIKE3(a): transfer consignment validated statelessly, status={}",
        validated_transfer.validation_status().validity()
    );

    // =====================================================================
    // GOAL 4: external-signing seam — sign the PSBT with a TEST private key,
    // finalize, extract; the RGB layer still accepts it (segwit txid unchanged).
    // The real system uses TSS (bare pubkey, no chain code) for the same input.
    // =====================================================================
    let mut psbt_signed = psbt.clone();
    let txout = psbt_signed.inputs[0]
        .witness_utxo
        .clone()
        .expect("witness_utxo must be set");
    let mut sighash_cache = SighashCache::new(&psbt_signed.unsigned_tx);
    let sighash = sighash_cache.p2wpkh_signature_hash(
        0,
        &txout.script_pubkey,
        txout.value,
        EcdsaSighashType::All,
    )?;
    let msg = bitcoin::secp256k1::Message::from(sighash);
    let sig = secp.sign_ecdsa(&msg, &signer_secret);
    let btc_sig = bitcoin::ecdsa::Signature::sighash_all(sig);
    psbt_signed.inputs[0]
        .partial_sigs
        .insert(bitcoin::PublicKey::new(signer_pubkey), btc_sig);
    // Finalize: push sig+pubkey witness
    let mut witness = Witness::new();
    witness.push(btc_sig.to_vec());
    witness.push(signer_pubkey.serialize());
    psbt_signed.inputs[0].final_script_witness = Some(witness);
    psbt_signed.inputs[0].final_script_sig = Some(ScriptBuf::new());

    let signed_tx = psbt_signed.extract_tx()?;
    let signed_txid = signed_tx.compute_txid();
    println!("SPIKE3(g4): signed PSBT extracted, signed txid={signed_txid}");
    assert_eq!(signed_txid, witness_txid, "segwit signing must NOT change the txid");
    assert!(!signed_tx.input[0].witness.is_empty(), "input must carry the segwit signature");

    // Now validate the SAME transfer consignment against the SIGNED tx as witness.
    let resolver2 = MapResolver { txs: BTreeMap::from([(signed_txid, signed_tx)]) };
    let validated_signed = transfer2
        .clone()
        .validate(&resolver2, &config1)
        .map_err(|e| anyhow!("signed transfer validate failed: {e:#?}"))?;
    println!(
        "SPIKE3(g4): transfer re-validated with the SIGNED witness tx, status={}",
        validated_signed.validation_status().validity()
    );

    println!("SPIKE3-PASS: rgb-consensus + rgb-ops + rgb-schemas + rgb-psbt-utils drive OK, single-script compatible");
    Ok(())
}
