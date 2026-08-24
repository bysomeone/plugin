//! The sidecar's core: wraps the rgb-ops `Stock` (RGB consensus state) + the persistent
//! application ledger + the single-script Bitcoin wallet, and exposes the operations behind
//! the gRPC API.
//!
//! Watch-only / single-script: every seal UTXO lives under `wpkh(<TSS pubkey>)`; the engine
//! never holds a private key (signing is external — TSS in production, a test key in E2E).

use std::collections::HashMap;
use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::absolute::LockTime;
use bitcoin::{
    Address, OutPoint, Psbt, ScriptBuf, Sequence, Transaction, TxIn, TxOut, Witness,
};
use electrum_client::ElectrumApi;
use psrgbt::{RgbOutExt, RgbPsbtExt};
use rand::Rng;
use bitcoin::Network;
use rgbinvoice::Precision;
use rgbcore::validation::{ResolveWitness, ValidationConfig, WitnessResolverError, WitnessStatus};
use rgbcore::vm::WitnessOrd;
use rgbcore::{ChainNet, ContractId, Opout, TxoSeal, Txid};
use rgbstd::containers::{Consignment, ConsignmentExt};
use rgbstd::contract::{AllocatedState, ContractBuilder, IssuerWrapper, TransitionBuilder};
use rgbstd::invoice::Amount;
use rgbstd::persistence::fs::FsBinStore;
use rgbstd::persistence::{ContractStateRead, Stock};
use rgbstd::stl::{AssetSpec, ContractTerms, RicardianContract};
use rgbstd::txout::{BlindSeal, CloseMethod, TxPtr};
use rgbstd::{Identity, OutputSeal};
use schemata::InflatableFungibleAsset;

use crate::config::Config;
use crate::invoice::{
    consignment_from_bytes, consignment_to_bytes, network_to_chainnet, parse_invoice,
};
use crate::ledger::{AssetRec, Ledger, ReceiveRec};
use crate::types::{recv_status, SealStatus, SealTxOut};
use crate::wallet::{BtcWallet, WalletUtxo};

/// A `ResolveWitness` that resolves witness transactions from the Electrum indexer.
struct ElectrumResolver {
    electrum: Arc<electrum_client::Client>,
    chain_net: ChainNet,
}

impl ResolveWitness for ElectrumResolver {
    fn resolve_witness(&self, witness_id: Txid) -> Result<WitnessStatus, WitnessResolverError> {
        match self.electrum.transaction_get(&witness_id) {
            Ok(tx) => Ok(WitnessStatus::Resolved(tx, WitnessOrd::Tentative)),
            Err(_) => Ok(WitnessStatus::Unresolved),
        }
    }
    fn check_chain_net(&self, chain_net: ChainNet) -> Result<(), WitnessResolverError> {
        if chain_net == self.chain_net {
            Ok(())
        } else {
            Err(WitnessResolverError::WrongChainNet)
        }
    }
}

/// Result of a consignment inspection.
#[derive(Clone, Debug)]
pub struct ConsignmentInspection {
    pub asset_id: String,
    pub schema_id: String,
    pub valid: bool,
    pub error: Option<String>,
    /// seal outpoints closed by the transition (its inputs), resolved from the Stock.
    pub closed_seals: Vec<String>,
    /// outpoints opened by the transition + amounts (witness-txid:vout).
    pub opened_seals: Vec<OpenedSealInfo>,
    /// the opened seal at the TSS script (recipient), if any.
    pub recipient_seal: Option<String>,
    pub recipient_amount: i64,
}

#[derive(Clone, Debug)]
pub struct OpenedSealInfo {
    pub outpoint: String,
    pub amount: i64,
    pub asset_id: String,
}

#[derive(Clone, Debug)]
pub struct BuildTransferOutcome {
    pub psbt: Psbt,
    pub input_amounts: Vec<i64>,
    pub input_btc_values: Vec<u64>,
    pub consignment: Vec<u8>,
    pub txid: Txid,
    /// vout of the recipient output (always 1).
    pub recipient_vout: u32,
    /// vout of the change output, if any (always 2).
    pub change_vout: Option<u32>,
    pub change_amount: i64,
}

/// A built (unsigned) withdrawal awaiting external signature.
#[derive(Clone, Debug)]
struct PendingWithdrawal {
    pub txid: Txid,
    pub asset_id: String,
    pub asset_symbol: String,
    pub input_outpoints: Vec<OutPoint>,
    pub change_vout: Option<u32>,
    pub change_amount: i64,
}

pub struct RgbEngine {
    cfg: Config,
    pub stock: Stock,
    pub ledger: Ledger,
    wallet: BtcWallet,
    electrum: Arc<electrum_client::Client>,
    chain_net: ChainNet,
    tss_script: ScriptBuf,
    tss_address: Address,
    resolver: ElectrumResolver,
    pending_withdrawals: HashMap<String, PendingWithdrawal>,
}

impl RgbEngine {
    pub fn open(cfg: Config) -> Result<Self> {
        std::fs::create_dir_all(&cfg.data_dir)?;
        std::fs::create_dir_all(&cfg.stock_dir())?;

        let store = FsBinStore::new(cfg.stock_dir())?;
        let stock = match Stock::load(store.clone(), true) {
            Ok(s) => s,
            Err(_) => {
                let mut s = Stock::in_memory();
                s.make_persistent(store, true)
                    .map_err(|e| anyhow!("make stock persistent: {e:?}"))?;
                s
            }
        };

        let ledger = Ledger::load(&cfg.ledger_path())?;
        let wallet = BtcWallet::new(&cfg.electrum_url, &cfg.tss_pubkey_hex, cfg.network)?;
        let tss_address = wallet.address().clone();
        let tss_script = wallet.script().clone();
        let chain_net = network_to_chainnet(cfg.network);
        let electrum = Arc::new(
            electrum_client::Client::new(&cfg.electrum_url)
                .map_err(|e| anyhow!("electrum connect {}: {}", cfg.electrum_url, e))?,
        );
        let resolver = ElectrumResolver {
            electrum: electrum.clone(),
            chain_net,
        };

        Ok(Self {
            cfg,
            stock,
            ledger,
            wallet,
            electrum,
            chain_net,
            tss_script,
            tss_address,
            resolver,
            pending_withdrawals: HashMap::new(),
        })
    }

    pub fn save(&mut self) -> Result<()> {
        self.stock
            .store()
            .map_err(|e| anyhow!("stock store: {e:?}"))?;
        self.ledger.save(&self.cfg.ledger_path())?;
        Ok(())
    }

    pub fn tss_address(&self) -> &Address {
        &self.tss_address
    }

    pub fn tss_script(&self) -> &ScriptBuf {
        &self.tss_script
    }

    /// Bitcoin network (regtest/testnet/mainnet).
    pub fn network(&self) -> Network {
        self.cfg.network
    }

    /// Height of the indexer tip.
    pub fn synced_height(&self) -> u64 {
        self.electrum
            .block_headers_subscribe()
            .map(|h| h.height as u64)
            .unwrap_or(0)
    }

    // ===================================================================
    // Asset issuance (out-of-band bootstrap; not part of the gRPC contract)
    // ===================================================================

    /// Issue an RGB20 (IFA) fungible asset. The genesis seal is a real TSS UTXO.
    pub fn issue_asset(
        &mut self,
        symbol: &str,
        name: &str,
        precision: u8,
        issued_supply: u64,
    ) -> Result<AssetRec> {
        if let Some(a) = self.ledger.asset(symbol) {
            return Ok(a.clone());
        }
        let genesis_utxo = self.pick_genesis_utxo()?;
        self.issue_asset_at(symbol, name, precision, issued_supply, genesis_utxo)
    }

    /// Issue an asset at an explicit genesis outpoint (used by E2E / tests).
    pub fn issue_asset_at(
        &mut self,
        symbol: &str,
        name: &str,
        precision: u8,
        issued_supply: u64,
        genesis_utxo: OutPoint,
    ) -> Result<AssetRec> {
        let chain_net = self.chain_net;
        let schema = InflatableFungibleAsset::schema();
        let types = InflatableFungibleAsset::types();
        let scripts = InflatableFungibleAsset::scripts();

        let spec = AssetSpec::with(symbol, name, precision_of(precision)?, None)?;
        let terms = ContractTerms { text: RicardianContract::default(), media: None };
        let genesis_seal = BlindSeal::<Txid>::new_random(genesis_utxo.txid, genesis_utxo.vout);

        let builder = ContractBuilder::with(
            Identity::default(),
            schema,
            types.clone(),
            scripts,
            chain_net,
        )
        .add_global_state("spec", spec)?
        .add_global_state("terms", terms)?
        .add_global_state("issuedSupply", Amount::from(issued_supply))?
        .add_global_state("maxSupply", Amount::from(issued_supply))?
        .add_fungible_state("assetOwner", genesis_seal, Amount::from(issued_supply))?;

        let valid_contract = builder.issue_contract()?;
        let asset_id = valid_contract.contract_id();
        let contract: rgbstd::containers::Contract =
            valid_contract.clone().into_consignment().into_contract();
        let genesis_bytes = consignment_to_bytes(&contract)?;
        let resolver = ElectrumResolver {
            electrum: self.electrum.clone(),
            chain_net,
        };
        self.stock
            .import_contract(valid_contract, resolver)
            .map_err(|e| anyhow!("import contract: {e:?}"))?;

        let asset = AssetRec {
            asset_id: asset_id.to_string(),
            symbol: symbol.to_string(),
            schema: "IFA/RGB20".to_string(),
            precision,
            issued_supply: issued_supply as i64,
            genesis_consignment_hex: bitcoin::hex::DisplayHex::to_hex_string(
                &genesis_bytes,
                bitcoin::hex::Case::Lower,
            ),
        };
        let btc_value = self
            .wallet
            .list_unspent()
            .into_iter()
            .find(|u| u.outpoint == genesis_utxo)
            .map(|u| u.value)
            .unwrap_or(0);
        self.ledger.upsert_seal(SealTxOut {
            outpoint: format!("{genesis_utxo}"),
            asset_id: asset_id.to_string(),
            asset_symbol: symbol.to_string(),
            amount: issued_supply as i64,
            btc_value: btc_value as i64,
            maturity_height: self.synced_height() as u32,
            status: SealStatus::Minted,
            secret_seal_hex: None,
        });
        self.ledger.upsert_asset(asset.clone());
        self.save()?;
        Ok(asset)
    }

    fn pick_genesis_utxo(&mut self) -> Result<OutPoint> {
        self.wallet.sync()?;
        let unspents = self.wallet.list_unspent();
        unspents
            .iter()
            .filter(|u| u.outpoint != OutPoint::null())
            .max_by_key(|u| u.value)
            .map(|u| u.outpoint)
            .ok_or_else(|| {
                anyhow!("no TSS UTXO available for genesis seal; fund the TSS address first")
            })
    }

    // ===================================================================
    // Receives (deposits)
    // ===================================================================

    /// Create a WitnessVout receive: returns an address-based invoice.
    pub fn create_receive(&mut self, asset_symbol: &str, amount: i64) -> Result<(String, String)> {
        let asset = self
            .ledger
            .asset(asset_symbol)
            .ok_or_else(|| anyhow!("asset {asset_symbol} not issued"))?;
        let contract_id = ContractId::from_str(&asset.asset_id)?;
        let schema_id = InflatableFungibleAsset::schema().schema_id();
        let invoice = crate::invoice::build_address_invoice(
            self.cfg.network,
            &self.tss_script,
            contract_id,
            schema_id,
            amount as u64,
        )?;

        let receive_id = format!("rcv-{}", rand_hex(8));
        self.ledger.upsert_receive(ReceiveRec {
            receive_id: receive_id.clone(),
            invoice: invoice.clone(),
            asset_symbol: asset_symbol.to_string(),
            asset_id: asset.asset_id.clone(),
            amount_requested: amount,
            status: recv_status::WAITING_COUNTERPARTY.to_string(),
            settled_amount: 0,
            txid: String::new(),
            vout: 0,
            secret_seal_hex: None,
        });
        self.save()?;
        Ok((invoice, receive_id))
    }

    // ===================================================================
    // Consignment validation / settlement
    // ===================================================================

    /// Deterministic consignment validation (no wallet / private-key state).
    pub fn validate_consignment(&mut self, bytes: &[u8]) -> Result<ConsignmentInspection> {
        let consignment = consignment_from_bytes(bytes)?;
        let config = ValidationConfig {
            chain_net: self.chain_net,
            trusted_typesystem: InflatableFungibleAsset::types(),
            ..Default::default()
        };
        let mut inspection = self.inspect(&consignment);
        match consignment.validate(&self.resolver, &config) {
            Ok(_) => inspection.valid = true,
            Err(e) => {
                inspection.valid = false;
                inspection.error = Some(format!("{e}"));
                inspection.opened_seals.clear();
            }
        }
        Ok(inspection)
    }

    /// Validate + settle a consignment: accept it into the Stock (updating the RGB state),
    /// attribute it to a pending receive, update the ledger.
    pub fn provide_consignment(
        &mut self,
        bytes: &[u8],
        receive_id_hint: Option<&str>,
    ) -> Result<ReceiveRec> {
        let consignment = consignment_from_bytes(bytes)?;
        let config = ValidationConfig {
            chain_net: self.chain_net,
            trusted_typesystem: InflatableFungibleAsset::types(),
            ..Default::default()
        };
        let inspection = self.inspect(&consignment);
        let valid = consignment
            .clone()
            .validate(&self.resolver, &config)
            .map_err(|e| anyhow!("consignment invalid: {e}"))?;

        let mut candidate = receive_id_hint
            .and_then(|h| self.ledger.receive(h))
            .filter(|r| r.status == recv_status::WAITING_COUNTERPARTY)
            .cloned();
        if candidate.is_none() {
            candidate = self
                .ledger
                .receives
                .values()
                .find(|r| {
                    r.status == recv_status::WAITING_COUNTERPARTY
                        && r.asset_id == inspection.asset_id
                        && r.amount_requested == inspection.recipient_amount
                })
                .cloned();
        }
        let rec = candidate.ok_or_else(|| {
            anyhow!(
                "no pending receive matches asset {} amount {}",
                inspection.asset_id,
                inspection.recipient_amount
            )
        })?;

        let recipient_outpoint = inspection
            .recipient_seal
            .clone()
            .ok_or_else(|| anyhow!("consignment opens no seal at the TSS script"))?;
        let (txid, vout) = split_outpoint(&recipient_outpoint)?;

        // Accept into the RGB consensus state BEFORE updating the ledger, so the new seal's
        // assignment (opout) is queryable by later transfers/withdrawals.
        self.stock
            .accept_transfer(valid, &self.resolver)
            .map_err(|e| anyhow!("accept_transfer: {e:?}"))?;

        self.ledger.mark_settled(
            &rec.receive_id,
            inspection.recipient_amount,
            &inspection.asset_id,
            &txid,
            vout,
        )?;
        // BTC value of the receive output, from the witness tx (when available).
        let btc_value = Txid::from_str(&txid)
            .ok()
            .and_then(|tid| self.electrum.transaction_get(&tid).ok())
            .and_then(|tx| tx.output.get(vout as usize).cloned())
            .map(|o| o.value.to_sat() as i64)
            .unwrap_or(0);
        self.ledger.upsert_seal(SealTxOut {
            outpoint: recipient_outpoint.clone(),
            asset_id: inspection.asset_id.clone(),
            asset_symbol: rec.asset_symbol.clone(),
            amount: inspection.recipient_amount,
            btc_value,
            maturity_height: 0,
            status: SealStatus::PendingMint,
            secret_seal_hex: None,
        });
        for closed in &inspection.closed_seals {
            if let Some(s) = self.ledger.seals.get_mut(closed) {
                s.status = SealStatus::Consumed;
            }
        }

        self.save()?;
        Ok(self.ledger.receive(&rec.receive_id).cloned().expect("just settled"))
    }

    /// Extract the structural inspection of a consignment (no consensus check).
    fn inspect(&self, consignment: &Consignment<true>) -> ConsignmentInspection {
        let asset_id = consignment.contract_id().to_string();
        let schema_id = consignment.schema_id().to_string();
        let closed_seals = self.resolve_closed_seals(consignment);
        let (opened, recipient, recipient_amount) = self.inspect_opened_seals(consignment);
        ConsignmentInspection {
            asset_id,
            schema_id,
            valid: false,
            error: None,
            closed_seals,
            opened_seals: opened,
            recipient_seal: recipient,
            recipient_amount,
        }
    }

    /// Update seal lifecycles from the TSS script UTXO set.
    pub fn sync(&mut self) -> Result<(bool, Vec<String>)> {
        self.wallet.sync()?;
        let unspents = self.wallet.list_unspent();
        let unspent_outpoints: std::collections::BTreeSet<String> = unspents
            .iter()
            .filter(|u| u.script_pubkey == self.tss_script)
            .map(|u| u.outpoint.to_string())
            .collect();
        let by_outpoint: HashMap<String, &WalletUtxo> = unspents
            .iter()
            .filter(|u| u.script_pubkey == self.tss_script)
            .map(|u| (u.outpoint.to_string(), u))
            .collect();

        let mut changed = false;
        for seal in self.ledger.seals.values_mut() {
            let is_unspent = unspent_outpoints.contains(&seal.outpoint);
            match seal.status {
                SealStatus::PendingMint if is_unspent => {
                    seal.status = SealStatus::Minted;
                    changed = true;
                }
                SealStatus::Minted if !is_unspent => {
                    seal.status = SealStatus::Consumed;
                    changed = true;
                }
                _ => {}
            }
            // Refresh btc_value / maturity from the wallet's view when the UTXO is known.
            if let Some(utxo) = by_outpoint.get(&seal.outpoint) {
                let btc = utxo.value as i64;
                let height = utxo.height.unwrap_or(0);
                if btc != seal.btc_value || height != seal.maturity_height {
                    seal.btc_value = btc;
                    seal.maturity_height = height;
                    changed = true;
                }
            }
        }

        let new_seals: Vec<String> = unspent_outpoints
            .iter()
            .filter(|o| !self.ledger.seals.contains_key(*o))
            .cloned()
            .collect();
        if !new_seals.is_empty() {
            changed = true;
        }
        if changed {
            self.save()?;
        }
        Ok((changed, new_seals))
    }

    pub fn list_seals(&self, symbol: &str) -> Vec<SealTxOut> {
        self.ledger.seals_for(symbol).cloned().collect()
    }

    pub fn get_balance(&self, symbol: &str) -> (i64, i64) {
        let mut settled = 0i64;
        let mut pending = 0i64;
        for s in self.ledger.seals_for(symbol) {
            match s.status {
                SealStatus::Minted => settled += s.amount,
                SealStatus::PendingMint => pending += s.amount,
                SealStatus::Consumed => {}
            }
        }
        (settled, pending)
    }

    pub fn list_assets(&self) -> Vec<AssetRec> {
        self.ledger.assets.values().cloned().collect()
    }

    // ===================================================================
    // Transfers (BuildWithdrawal and E2E "user" simulation share this)
    // ===================================================================

    /// Build an unsigned PSBT + consignment spending `input_outpoints` (carrying `symbol`
    /// state) to the `recipient_invoice` beneficiary, with change to `change_script`.
    pub fn build_transfer(
        &mut self,
        symbol: &str,
        input_outpoints: &[OutPoint],
        recipient_invoice: &str,
        change_script: &ScriptBuf,
        fee_rate: u64,
        recipient_btc: u64,
    ) -> Result<BuildTransferOutcome> {
        let asset = self
            .ledger
            .asset(symbol)
            .ok_or_else(|| anyhow!("asset {symbol} not issued"))?;
        let contract_id = ContractId::from_str(&asset.asset_id)?;
        let invoice = parse_invoice(recipient_invoice)?;
        if let Some(cid) = invoice.contract_id {
            if cid != contract_id {
                return Err(anyhow!("invoice contract {cid} != asset contract {contract_id}"));
            }
        }
        let recipient_script = invoice
            .witness_script
            .clone()
            .ok_or_else(|| anyhow!("blinded-receive invoices not supported for transfers (v1)"))?;
        let invoice_amount = invoice.amount.map(|a| a.value() as i64).unwrap_or(0);

        // Gather input opouts + states from the Stock.
        let mut builder: TransitionBuilder = self
            .stock
            .transition_builder(contract_id, "transfer")
            .map_err(|e| anyhow!("transition_builder: {e:?}"))?;
        let mut input_amounts: Vec<i64> = Vec::new();
        let mut input_btc: Vec<u64> = Vec::new();
        for outpoint in input_outpoints {
            let assignments = self
                .stock
                .contract_assignments_for(contract_id, [*outpoint])
                .map_err(|e| anyhow!("assignments: {e:?}"))?;
            let entry = assignments
                .get(&OutputSeal::new(*outpoint))
                .ok_or_else(|| anyhow!("outpoint {outpoint} has no {} state", asset.asset_id))?;
            let (opout, state) = entry
                .iter()
                .next()
                .ok_or_else(|| anyhow!("outpoint {outpoint} has no assignment"))?;
            builder = builder.add_input(*opout, state.clone())?;
            let amount: Amount = match state {
                AllocatedState::Amount(rv) => (*rv).into(),
                _ => return Err(anyhow!("input {outpoint} not fungible")),
            };
            input_amounts.push(amount.value() as i64);
            let btc = self
                .wallet
                .list_unspent()
                .into_iter()
                .find(|u| u.outpoint == *outpoint)
                .map(|u| u.value)
                .unwrap_or(0);
            input_btc.push(btc);
        }

        let total_input = input_amounts.iter().sum::<i64>();
        let send_amount = if invoice_amount > 0 {
            invoice_amount
        } else {
            total_input
        };
        if send_amount > total_input {
            return Err(anyhow!("invoice amount {send_amount} exceeds input total {total_input}"));
        }
        let change_amount = total_input - send_amount;

        let recipient_seal = BlindSeal::<TxPtr>::new_random_vout(1);
        builder =
            builder.add_fungible_state("assetOwner", recipient_seal, Amount::from(send_amount as u64))?;
        if change_amount > 0 {
            let change_seal = BlindSeal::<TxPtr>::new_random_vout(2);
            builder = builder
                .add_fungible_state("assetOwner", change_seal, Amount::from(change_amount as u64))?;
        }
        let transition = builder.complete_transition()?;

        // Build the unsigned PSBT with explicit inputs.
        let total_btc = input_btc.iter().sum::<u64>();
        let est_vbytes = 10 + 41 * input_outpoints.len() + 31 * 3 + 68 * input_outpoints.len();
        let fee = est_vbytes as u64 * fee_rate;
        if total_btc < recipient_btc + fee {
            return Err(anyhow!("BTC inputs ({total_btc}) cannot cover output+fee"));
        }
        let change_btc = total_btc - recipient_btc - fee;

        let mut tx = Transaction {
            version: bitcoin::transaction::Version::TWO,
            lock_time: LockTime::ZERO,
            input: input_outpoints
                .iter()
                .map(|o| TxIn {
                    previous_output: *o,
                    script_sig: ScriptBuf::new(),
                    sequence: Sequence::MAX,
                    witness: Witness::new(),
                })
                .collect(),
            output: vec![
                TxOut { value: bitcoin::Amount::from_sat(0), script_pubkey: ScriptBuf::new_op_return([]) },
                TxOut { value: bitcoin::Amount::from_sat(recipient_btc), script_pubkey: recipient_script.clone() },
                TxOut { value: bitcoin::Amount::from_sat(change_btc), script_pubkey: change_script.clone() },
            ],
        };
        if change_amount == 0 {
            tx.output.truncate(2);
        }
        let mut psbt = Psbt::from_unsigned_tx(tx)?;
        for (i, _outpoint) in input_outpoints.iter().enumerate() {
            psbt.inputs[i].witness_utxo = Some(TxOut {
                value: bitcoin::Amount::from_sat(input_btc[i]),
                script_pubkey: self.tss_script.clone(),
            });
        }

        psbt.push_rgb_transition(transition.clone())?;
        psbt.outputs[0].set_opret_host();
        psbt.outputs[0].set_mpc_entropy(rand::rng().random::<u64>())?;
        psbt.set_rgb_close_method(CloseMethod::OpretFirst);
        let fascia = psbt.rgb_commit()?;
        let txid = psbt.get_txid();

        let recipient_out = OutputSeal::with(txid, 1);
        let mut outputs = vec![recipient_out];
        if change_amount > 0 {
            outputs.push(OutputSeal::with(txid, 2));
        }
        let transfer: Consignment<true> = self
            .stock
            .transfer_from_fascia(contract_id, outputs, [], [], &fascia)
            .map_err(|e| anyhow!("transfer_from_fascia: {e:?}"))?;
        let consignment = consignment_to_bytes(&transfer)?;

        Ok(BuildTransferOutcome {
            psbt,
            input_amounts,
            input_btc_values: input_btc,
            consignment,
            txid,
            recipient_vout: 1,
            change_vout: if change_amount > 0 { Some(2) } else { None },
            change_amount,
        })
    }

    pub fn broadcast(&self, tx: &Transaction) -> Result<Txid> {
        self.electrum
            .transaction_broadcast(tx)
            .map_err(|e| anyhow!("broadcast: {e}"))
    }

    /// Wait until the tx is visible to the indexer (electrs polls every few seconds).
    pub fn wait_tx(&self, txid: Txid, timeout: std::time::Duration) -> Result<()> {
        let start = std::time::Instant::now();
        while start.elapsed() < timeout {
            if self.electrum.transaction_get(&txid).is_ok() {
                return Ok(());
            }
            std::thread::sleep(std::time::Duration::from_millis(250));
        }
        anyhow::bail!("tx {txid} not visible to indexer within {timeout:?}")
    }

    // ===================================================================
    // Withdrawals
    // ===================================================================

    pub fn build_withdrawal(
        &mut self,
        symbol: &str,
        amount: i64,
        recipient_invoice: &str,
        change_address: &str,
        fee_rate: u64,
    ) -> Result<BuildTransferOutcome> {
        let (seals, _total) = self.ledger.select_seals(symbol, amount)?;
        let input_outpoints: Vec<OutPoint> = seals
            .iter()
            .map(|s| OutPoint::from_str(&s.outpoint))
            .collect::<Result<_, _>>()?;
        let change_script = Address::from_str(change_address)?
            .require_network(self.cfg.network)
            .map_err(|e| anyhow!("change address network: {e}"))?
            .script_pubkey();
        let outcome = self.build_transfer(
            symbol,
            &input_outpoints,
            recipient_invoice,
            &change_script,
            fee_rate,
            546, // dust to the withdrawal recipient
        )?;
        let asset_id = self
            .ledger
            .asset(symbol)
            .map(|a| a.asset_id.clone())
            .unwrap_or_default();
        self.pending_withdrawals.insert(
            outcome.txid.to_string(),
            PendingWithdrawal {
                txid: outcome.txid,
                asset_id,
                asset_symbol: symbol.to_string(),
                input_outpoints,
                change_vout: outcome.change_vout,
                change_amount: outcome.change_amount,
            },
        );
        Ok(outcome)
    }

    /// Complete a withdrawal: verify the signed PSBT (segwit signing keeps the txid),
    /// mark input seals consumed, record the change seal, return (txid, recipient, change).
    pub fn finalize_withdrawal(
        &mut self,
        signed_psbt: &Psbt,
    ) -> Result<(Txid, String, Option<String>)> {
        let tx = signed_psbt.clone().extract_tx()?;
        let txid = tx.compute_txid();

        let pending = self
            .pending_withdrawals
            .remove(&txid.to_string())
            .ok_or_else(|| anyhow!("no pending withdrawal for txid {txid}"))?;
        assert_eq!(pending.txid, txid, "pending withdrawal txid mismatch");

        for o in &pending.input_outpoints {
            let key = o.to_string();
            if let Some(s) = self.ledger.seals.get_mut(&key) {
                s.status = SealStatus::Consumed;
            }
        }
        if let Some(vout) = pending.change_vout {
            let outpoint = format!("{txid}:{vout}");
            self.ledger.upsert_seal(SealTxOut {
                outpoint: outpoint.clone(),
                asset_id: pending.asset_id.clone(),
                asset_symbol: pending.asset_symbol.clone(),
                amount: pending.change_amount,
                btc_value: tx
                    .output
                    .get(vout as usize)
                    .map(|o| o.value.to_sat() as i64)
                    .unwrap_or(0),
                maturity_height: 0,
                status: SealStatus::PendingMint,
                secret_seal_hex: None,
            });
        }
        self.save()?;
        let recipient_outpoint = format!("{txid}:1");
        let change_outpoint = pending.change_vout.map(|v| format!("{txid}:{v}"));
        Ok((txid, recipient_outpoint, change_outpoint))
    }

    // ===================================================================
    // Helpers
    // ===================================================================

    /// Resolve the Bitcoin outpoints closed by a consignment's transition inputs, using the
    /// Stock's assignment state (opout -> seal).
    fn resolve_closed_seals(&self, consignment: &Consignment<true>) -> Vec<String> {
        let contract_id = consignment.contract_id();
        let Ok(state) = self.stock.contract_state(contract_id) else {
            return vec![];
        };
        let mut opout_to_seal: HashMap<Opout, OutPoint> = HashMap::new();
        for a in state.fungible_all() {
            opout_to_seal.insert(a.opout, a.seal.to_outpoint());
        }
        for a in state.data_all() {
            opout_to_seal.insert(a.opout, a.seal.to_outpoint());
        }
        for a in state.rights_all() {
            opout_to_seal.insert(a.opout, a.seal.to_outpoint());
        }
        let mut out = Vec::new();
        for bundle in consignment.bundled_witnesses() {
            for kt in bundle.bundle().known_transitions.iter() {
                for opout in kt.transition.inputs().into_iter() {
                    if let Some(o) = opout_to_seal.get(&opout) {
                        out.push(o.to_string());
                    }
                }
            }
        }
        out
    }

    /// Inspect opened seals; find the recipient (opened seal at the TSS script).
    fn inspect_opened_seals(
        &self,
        consignment: &Consignment<true>,
    ) -> (Vec<OpenedSealInfo>, Option<String>, i64) {
        let asset_id = consignment.contract_id().to_string();
        let mut opened = Vec::new();
        let mut recipient: Option<String> = None;
        let mut recipient_amount = 0i64;
        for bundle in consignment.bundled_witnesses() {
            let wtxid = bundle.witness_id();
            for kt in bundle.bundle().known_transitions.iter() {
                for assigns in kt.transition.assignments.values() {
                    for a in assigns.as_fungible() {
                        if let Some(seal) = a.revealed_seal() {
                            let vout = seal.vout();
                            let amount: Amount = (*a.as_revealed_state()).into();
                            let outpoint = format!("{wtxid}:{vout}");
                            opened.push(OpenedSealInfo {
                                outpoint: outpoint.clone(),
                                amount: amount.value() as i64,
                                asset_id: asset_id.clone(),
                            });
                            if recipient.is_none() {
                                if let Ok(tx) = self.electrum.transaction_get(&wtxid) {
                                    if let Some(o) = tx.output.get(vout.to_u32() as usize) {
                                        if o.script_pubkey == self.tss_script {
                                            recipient = Some(outpoint);
                                            recipient_amount = amount.value() as i64;
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        (opened, recipient, recipient_amount)
    }
}

fn precision_of(p: u8) -> Result<Precision> {
    match p {
        0 => Ok(Precision::Indivisible),
        2 => Ok(Precision::Centi),
        8 => Ok(Precision::CentiMicro),
        _ => Err(anyhow!("unsupported precision {p} (supported: 0, 2, 8)")),
    }
}

fn rand_hex(n: usize) -> String {
    use rand::RngCore;
    let mut buf = vec![0u8; n];
    rand::rng().fill_bytes(&mut buf);
    bitcoin::hex::DisplayHex::to_hex_string(&buf, bitcoin::hex::Case::Lower)
}

fn split_outpoint(s: &str) -> Result<(String, u32)> {
    let (txid, vout) = s
        .rsplit_once(':')
        .ok_or_else(|| anyhow!("bad outpoint {s}"))?;
    Ok((txid.to_string(), vout.parse::<u32>()?))
}
