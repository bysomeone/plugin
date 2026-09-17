//! The sidecar's core: wraps the rgb-ops `Stock` (RGB consensus state) + the persistent
//! application ledger + the single-script Bitcoin wallet, and exposes the operations behind
//! the gRPC API.
//!
//! Watch-only / single-script: every seal UTXO lives under `wpkh(<TSS pubkey>)`; the engine
//! never holds a private key (signing is external — TSS in production, a test key in E2E).

use std::collections::HashMap;
use std::str::FromStr;
use std::sync::{Arc, Mutex};

use anyhow::{anyhow, Result};
use bitcoin::absolute::LockTime;
use bitcoin::{
    Address, OutPoint, Psbt, ScriptBuf, Sequence, Transaction, TxIn, TxOut, Witness,
};
use psrgbt::{RgbOutExt, RgbPsbtExt};
use rand::Rng;
use bitcoin::Network;
use rgbinvoice::Precision;
use rgbcore::validation::{
    ResolveWitness, ValidationConfig, WitnessOrdProvider, WitnessResolverError, WitnessStatus,
};
use rgbcore::vm::WitnessOrd;
use rgbcore::{ChainNet, ContractId, Opout, TxoSeal, Txid};
use rgbstd::containers::{Consignment, ConsignmentExt, Fascia};
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
use crate::ledger::{AssetRec, FinalizedWithdrawalRec, Ledger, ReceiveRec, RecordedWithdrawal};
use crate::rpc::BtcdRpc;
use crate::types::{recv_status, SealStatus, SealTxOut};
use crate::wallet::{BtcWallet, WalletUtxo};

/// A `ResolveWitness` that resolves witness transactions from the btcd node.
///
/// Withdrawal consignments are validated *before* their anchor tx is broadcast (the signers need
/// to agree before the tx is sent), so the anchor is not yet on btcd. Such anchors are resolved
/// from `local_anchors` — a shared cache of the txs this engine just built — before falling back
/// to the node. Deposit/user-pay anchors are broadcast & mined first, so they resolve from btcd.
struct BtcdResolver {
    rpc: Arc<BtcdRpc>,
    chain_net: ChainNet,
    local_anchors: Arc<Mutex<HashMap<Txid, Transaction>>>,
}

impl ResolveWitness for BtcdResolver {
    fn resolve_witness(&self, witness_id: Txid) -> Result<WitnessStatus, WitnessResolverError> {
        if let Ok(guard) = self.local_anchors.lock() {
            if let Some(tx) = guard.get(&witness_id) {
                return Ok(WitnessStatus::Resolved(tx.clone(), WitnessOrd::Tentative));
            }
        }
        match self.rpc.get_transaction(&witness_id) {
            Ok(Some(tx)) => Ok(WitnessStatus::Resolved(tx, WitnessOrd::Tentative)),
            _ => Ok(WitnessStatus::Unresolved),
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

impl WitnessOrdProvider for BtcdResolver {
    /// Witness ordering view of the same resolver: used when merging a locally built fascia
    /// (`Stock::consume_fascia`), where the anchor tx is typically not yet mined and resolves
    /// from the `local_anchors` cache as [`WitnessOrd::Tentative`].
    fn witness_ord(&self, witness_id: Txid) -> Result<WitnessOrd, WitnessResolverError> {
        Ok(self.resolve_witness(witness_id)?.witness_ord())
    }
}

/// An error that retrying can never turn into a success, because the request targets state this
/// sidecar does not hold any more (an asset that was re-issued, an invoice written for a contract
/// that no longer exists, ...). The gRPC layer reports it as `FAILED_PRECONDITION` so the bridge
/// stops retrying and surfaces it, instead of hammering the sidecar once per second forever.
#[derive(Debug)]
pub struct PermanentError(pub String);

impl std::fmt::Display for PermanentError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.0)
    }
}

impl std::error::Error for PermanentError {}

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
    /// The RGB fascia exported from the PSBT: merging it into the Stock is what registers
    /// this transfer's output seals (incl. the change seal) as spendable RGB state.
    pub fascia: Fascia,
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
    /// Kept until `finalize_withdrawal` so the transition can be merged into the Stock once the
    /// withdrawal is signed (and therefore certain to be broadcast).
    pub fascia: Fascia,
}

/// Result of a finalized withdrawal, kept so a repeated `finalize_withdrawal` is a no-op
/// returning the same values instead of failing with "no pending withdrawal".
///
/// The record lives in the (persisted) ledger, see [`ledger::FinalizedWithdrawalRec`].
pub struct RgbEngine {
    cfg: Config,
    pub stock: Stock,
    pub ledger: Ledger,
    wallet: BtcWallet,
    rpc: Arc<BtcdRpc>,
    chain_net: ChainNet,
    tss_script: ScriptBuf,
    tss_address: Address,
    resolver: BtcdResolver,
    pending_withdrawals: HashMap<String, PendingWithdrawal>,
    /// Txs built by `build_transfer` that are not yet broadcast (withdrawal anchors awaiting TSS
    /// signature). Shared with `resolver` so pre-broadcast consignment validation can resolve them.
    local_anchors: Arc<Mutex<HashMap<Txid, Transaction>>>,
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
        let rpc = Arc::new(BtcdRpc::connect(
            &cfg.btc_rpc_host,
            &cfg.btc_rpc_user,
            &cfg.btc_rpc_pass,
            cfg.btc_rpc_cert.as_deref(),
            cfg.network,
        )?);
        // 载入用户 P2WSH 充值脚本注册表（不存在 = 空集，不报错）。载入失败必须**中止启动**：
        // 带着半份 watch 集跑起来，那些用户的充值会变成"看得见但花不掉"，而故障要等到提现
        // 时才暴露。
        let mut wallet = BtcWallet::new(rpc.clone(), &cfg.tss_pubkey_hex, cfg.network)?
            .with_user_scripts_path(cfg.user_scripts_path());
        wallet.load_user_scripts()?;
        let tss_address = wallet.address().clone();
        let tss_script = wallet.script().clone();
        let chain_net = network_to_chainnet(cfg.network);
        let local_anchors = Arc::new(Mutex::new(HashMap::new()));
        let resolver = BtcdResolver {
            rpc: rpc.clone(),
            chain_net,
            local_anchors: local_anchors.clone(),
        };

        Ok(Self {
            cfg,
            stock,
            ledger,
            wallet,
            rpc,
            chain_net,
            tss_script,
            tss_address,
            resolver,
            pending_withdrawals: HashMap::new(),
            local_anchors,
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

    /// Height of the chain tip (btcd `getblockcount`).
    pub fn synced_height(&self) -> u64 {
        self.rpc.get_block_count().unwrap_or(0)
    }

    /// Current BTC UTXO set of the single TSS script (watch-only), from btcd.
    pub fn list_unspent_btc(&self) -> Result<Vec<WalletUtxo>> {
        Ok(self.wallet.list_unspent())
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
        let resolver = BtcdResolver {
            rpc: self.rpc.clone(),
            chain_net,
            local_anchors: self.local_anchors.clone(),
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

        // Idempotent re-settle: the Go bridge uploads the consignment AFTER the test-sim already
        // settled the receive. Return the existing settled record instead of erroring so the bridge
        // has a consistent view and can proceed to pollTransfers -> submitDeposit.
        if let Some(h) = receive_id_hint {
            if let Some(r) = self.ledger.receive(h) {
                if r.status == recv_status::SETTLED {
                    return Ok(r.clone());
                }
            }
        }

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
            .and_then(|tid| self.rpc.get_transaction(&tid).ok().flatten())
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
        // 同步 TSS watch-only 钱包：否则 bdk list_unspent 看不到 TSS 地址的 BTC UTXO，
        // 构造 PSBT 时 BTC 输入为 0，报 "BTC inputs (0) cannot cover output+fee"。
        self.wallet.sync()?;
        let asset = self
            .ledger
            .asset(symbol)
            .ok_or_else(|| PermanentError(format!("asset {symbol} not issued")))?;
        let contract_id = ContractId::from_str(&asset.asset_id)?;
        let invoice = parse_invoice(recipient_invoice)?;
        if let Some(cid) = invoice.contract_id {
            if cid != contract_id {
                // The invoice names the contract the payer must be holding. A mismatch means this
                // sidecar's asset was re-issued (its ledger was rebuilt), so the invoice can never
                // be honoured by this sidecar — no amount of retrying changes that.
                return Err(PermanentError(format!(
                    "invoice contract {cid} != asset contract {contract_id}"
                ))
                .into());
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
        // watch 集全集（主池 ∪ 已登记的用户充值脚本）：既要按 outpoint 解析**每个输入自己的
        // 脚本与面额**（用户 P2WSH 的 seal 输入不再是主池脚本），也不能把用户充值 UTXO 的
        // BTC 当成不存在。注意下面的费输入选择仍然只从主池取（见那里的注释）。
        let wallet_utxos = self.wallet.list_unspent_all();
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
            let btc = wallet_utxos
                .iter()
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

        // ---- BTC funding ----
        // RGB receive outputs are dust (546 sats) so a single seal usually cannot cover the
        // recipient dust output + miner fee. When the seal BTC is insufficient, pull additional
        // bridge-owned BTC UTXOs (TSS script, e.g. the deposit change output) as pure-BTC fee
        // inputs. They carry no RGB state and are not part of the transition — they only fund
        // the carrier transaction, and their excess returns to the TSS change output.
        let seal_btc_total = input_btc.iter().sum::<u64>();
        let mut extra_fee_inputs: Vec<(OutPoint, u64)> = Vec::new(); // (outpoint, btc value)
        let mut total_btc = seal_btc_total;
        let mut n_inputs = input_outpoints.len();
        loop {
            // 与既有字节模型保持一致（3 输出上界，P2WPKH 输入 41+68 计费）。
            let est_vbytes = (10 + 41 * n_inputs + 31 * 3 + 68 * n_inputs) as u64;
            let fee = est_vbytes * fee_rate;
            if total_btc >= recipient_btc + fee {
                break;
            }
            // 选一笔桥自有（TSS 脚本）、未作为 RGB seal / 已选费输入 的 BTC UTXO（取最大额优先）。
            //
            // 这里**只从主池取**，用户 P2WSH 的充值 UTXO 不参与费输入选择 —— 这是 C3 刻意的
            // 边界，不是遗漏：签名节点核对输入归属时只认"主池脚本 或 **本节点已登记**的用户
            // 充值脚本"，而 watch 集目前只在发放过地址的那个节点上存在（跨节点分发属 C4）。
            // 现在就允许选它们，会让没有该地址的签名节点拒签（拒签理由正当：它无法核实归属）。
            // 把扫集/选入做出来之后（C4），这里的过滤器与跨节点 watch 集要一起放开。
            let next = wallet_utxos
                .iter()
                .filter(|u| u.script_pubkey == self.tss_script)
                .filter(|u| !self.ledger.seals.contains_key(&u.outpoint.to_string()))
                .filter(|u| !input_outpoints.contains(&u.outpoint))
                .filter(|u| !extra_fee_inputs.iter().any(|(o, _)| o == &u.outpoint))
                .max_by_key(|u| u.value)
                .cloned();
            match next {
                Some(u) => {
                    total_btc += u.value;
                    extra_fee_inputs.push((u.outpoint, u.value));
                    n_inputs += 1;
                }
                None => {
                    return Err(anyhow!(
                        "BTC inputs ({total_btc}) cannot cover output+fee; no bridge-owned BTC fee UTXO available"
                    ));
                }
            }
        }
        let est_vbytes = (10 + 41 * n_inputs + 31 * 3 + 68 * n_inputs) as u64;
        let fee = est_vbytes * fee_rate;
        let change_btc = total_btc - recipient_btc - fee;

        // 交易输入 = RGB seal 输入 + 桥自有费输入。
        let mut all_inputs: Vec<OutPoint> = input_outpoints.to_vec();
        let mut all_input_btc: Vec<u64> = input_btc.clone();
        for (o, v) in &extra_fee_inputs {
            all_inputs.push(*o);
            all_input_btc.push(*v);
        }
        let mut tx = Transaction {
            version: bitcoin::transaction::Version::TWO,
            lock_time: LockTime::ZERO,
            input: all_inputs
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
        // 无 RGB change、也无 BTC 找零时才去掉 change 输出；只要有多余 BTC（来自费输入）
        // 就必须保留找零输出回到 TSS，避免把费输入的币烧成手续费。
        if change_amount == 0 && change_btc == 0 {
            tx.output.truncate(2);
        }
        let mut psbt = Psbt::from_unsigned_tx(tx)?;
        fill_psbt_inputs(
            &mut psbt,
            &all_inputs,
            &wallet_utxos,
            &all_input_btc,
            &self.tss_script,
            &|script| self.wallet.witness_script_for(script),
        )?;

        psbt.push_rgb_transition(transition.clone())?;
        psbt.outputs[0].set_opret_host();
        psbt.outputs[0].set_mpc_entropy(rand::rng().random::<u64>())?;
        psbt.set_rgb_close_method(CloseMethod::OpretFirst);
        let fascia = psbt.rgb_commit()?;
        let txid = psbt.get_txid();
        // Cache the un-broadcast anchor so the withdrawal consignment can be validated (locally by
        // the official node AND by signing nodes via the shared sidecar) before the tx is signed &
        // broadcast. rgb_commit wrote the commitment into unsigned_tx, so its txid == `txid`.
        self.local_anchors
            .lock()
            .unwrap()
            .insert(txid, psbt.unsigned_tx.clone());

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
            input_btc_values: all_input_btc,
            consignment,
            fascia,
            txid,
            recipient_vout: 1,
            change_vout: if change_amount > 0 { Some(2) } else { None },
            change_amount,
        })
    }

    pub fn broadcast(&self, tx: &Transaction) -> Result<Txid> {
        self.rpc
            .send_raw_transaction(tx)
            .map_err(|e| anyhow!("broadcast: {e}"))
    }

    /// Wait until the tx is visible to the indexer (electrs polls every few seconds).
    pub fn wait_tx(&self, txid: Txid, timeout: std::time::Duration) -> Result<()> {
        let start = std::time::Instant::now();
        while start.elapsed() < timeout {
            if self.rpc.get_transaction(&txid).map(|o| o.is_some()).unwrap_or(false) {
                return Ok(());
            }
            std::thread::sleep(std::time::Duration::from_millis(250));
        }
        anyhow::bail!("tx {txid} not visible to btcd within {timeout:?}")
    }

    // ===================================================================
    // Withdrawals
    // ===================================================================

    /// Build an unsigned withdrawal.
    ///
    /// `input_seals` empty ⇒ the usual path: pick spendable (minted) seals covering `amount`, and
    /// record the resulting build.
    ///
    /// `input_seals` non-empty ⇒ **replay** (E9-A): spend exactly these outpoints, ignoring their
    /// lifecycle status, and re-issue the recorded build for them (same PSBT ⇒ same txid). The
    /// bridge uses it to retry a withdrawal whose first attempt already advanced the ledger (spent
    /// seal → `Consumed`, change seal → `Minted`): selecting by status then picks a *different*
    /// seal set, which yields a second, independently valid transfer — the user is paid twice for a
    /// single on-chain burn. If no build is recorded for the given seals (the record is gone), the
    /// build is made from them directly: same seals, but a different transaction.
    ///
    /// The caller is not trusted: each outpoint must actually carry `symbol`'s contract state in
    /// the Stock (the same check `build_transfer` performs per input, made explicit here so the
    /// failure names the seal).
    pub fn build_withdrawal(
        &mut self,
        symbol: &str,
        amount: i64,
        recipient_invoice: &str,
        change_address: &str,
        fee_rate: u64,
        input_seals: &[String],
    ) -> Result<BuildTransferOutcome> {
        // 先从链上对账 seal 生命周期：上一笔提现的 change seal 只有在其锚定 tx 上链后
        // （PendingMint -> Minted，sync 依据 TSS UTXO 集合推导）才可花。否则紧接的下一笔
        // 提现在 select_seals 阶段会报 "0 minted seals"（充值 settle 路径在 test_sim 里
        // 会显式 sync，提现路径漏了这步）。
        self.sync()?;
        let input_outpoints: Vec<OutPoint> = if input_seals.is_empty() {
            let (seals, _total) = self.ledger.select_seals(symbol, amount)?;
            seals
                .iter()
                .map(|s| OutPoint::from_str(&s.outpoint))
                .collect::<Result<_, _>>()?
        } else {
            self.replay_input_seals(symbol, input_seals)?
        };
        let change_script = Address::from_str(change_address)?
            .require_network(self.cfg.network)
            .map_err(|e| anyhow!("change address network: {e}"))?
            .script_pubkey();
        // Replay: hand back the recorded build of this seal set verbatim. Rebuilding instead would
        // produce a *different* transaction — the build draws fresh random blinding factors for the
        // recipient/change seals on every run, so even identical inputs give a different RGB
        // commitment (and txid), i.e. a second, conflicting spend of the same seal rather than a
        // replay. Only a recorded PSBT can be replayed byte for byte.
        if !input_seals.is_empty() {
            if let Some(rec) = self.ledger.withdrawal_builds.get(&seal_set_key(&input_outpoints)) {
                return self.replay_recorded_withdrawal(
                    rec.clone(),
                    symbol,
                    recipient_invoice,
                    change_address,
                );
            }
        }
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
        // Record the build so a retry of this withdrawal (same seals) can replay it verbatim.
        self.ledger.withdrawal_builds.insert(
            seal_set_key(&input_outpoints),
            RecordedWithdrawal {
                txid: outcome.txid.to_string(),
                asset_id: asset_id.clone(),
                asset_symbol: symbol.to_string(),
                input_outpoints: input_outpoints.iter().map(|o| o.to_string()).collect(),
                change_vout: outcome.change_vout,
                change_amount: outcome.change_amount,
                psbt_hex: hex_encode(&outcome.psbt.serialize()),
                consignment_hex: hex_encode(&outcome.consignment),
                input_amounts: outcome.input_amounts.clone(),
                fascia_hex: hex_encode(&fascia_to_bytes(&outcome.fascia)?),
            },
        );
        self.save()?;
        self.pending_withdrawals.insert(
            outcome.txid.to_string(),
            PendingWithdrawal {
                txid: outcome.txid,
                asset_id,
                asset_symbol: symbol.to_string(),
                input_outpoints,
                change_vout: outcome.change_vout,
                change_amount: outcome.change_amount,
                fascia: outcome.fascia.clone(),
            },
        );
        Ok(outcome)
    }

    /// Re-issue the withdrawal build recorded for a seal set (`RecordedWithdrawal`): the same
    /// unsigned PSBT, consignment and fascia, hence the same txid — an idempotent replay of the
    /// first attempt instead of a second payment for the same on-chain burn.
    ///
    /// The record is keyed by the seal set, so a request that happens to name the same seals but is
    /// a *different* withdrawal would otherwise be handed someone else's build. It is therefore
    /// checked against the request (asset, recipient, change script, sent amount) before it is
    /// re-issued; a mismatch is an error, never a quiet substitution.
    fn replay_recorded_withdrawal(
        &mut self,
        rec: RecordedWithdrawal,
        symbol: &str,
        recipient_invoice: &str,
        change_address: &str,
    ) -> Result<BuildTransferOutcome> {
        if rec.asset_symbol != symbol {
            return Err(anyhow!(
                "recorded withdrawal {} is for asset {}, not {symbol}",
                rec.txid,
                rec.asset_symbol
            ));
        }
        let psbt = Psbt::deserialize(&hex_decode(&rec.psbt_hex)?)
            .map_err(|e| anyhow!("recorded withdrawal {}: bad psbt: {e}", rec.txid))?;
        let invoice = parse_invoice(recipient_invoice)?;
        let recipient_script = invoice
            .witness_script
            .clone()
            .ok_or_else(|| anyhow!("blinded-receive invoices not supported for transfers (v1)"))?;
        let change_script = Address::from_str(change_address)?
            .require_network(self.cfg.network)
            .map_err(|e| anyhow!("change address network: {e}"))?
            .script_pubkey();
        let mismatch = |what: &str| anyhow!("recorded withdrawal {}: {what} differs from the request", rec.txid);
        let recipient_out = psbt
            .unsigned_tx
            .output
            .get(1)
            .ok_or_else(|| mismatch("no recipient output"))?;
        if recipient_out.script_pubkey != recipient_script {
            return Err(mismatch("recipient script"));
        }
        if let Some(vout) = rec.change_vout {
            let change_out = psbt
                .unsigned_tx
                .output
                .get(vout as usize)
                .ok_or_else(|| mismatch("no change output"))?;
            if change_out.script_pubkey != change_script {
                return Err(mismatch("change script"));
            }
        }
        let sent: i64 = rec.input_amounts.iter().sum::<i64>() - rec.change_amount;
        if let Some(amount) = invoice.amount {
            if sent != amount.value() as i64 {
                return Err(mismatch("sent amount"));
            }
        }
        let fascia = fascia_from_bytes(&hex_decode(&rec.fascia_hex)?)?;
        let input_outpoints = rec
            .input_outpoints
            .iter()
            .map(|s| OutPoint::from_str(s))
            .collect::<Result<Vec<_>, _>>()?;
        // Re-arm the in-memory pending entry: after a sidecar restart it is empty, and
        // `finalize_withdrawal` needs it to merge the transition. (When the first attempt already
        // finalized, the persisted `finalized_withdrawals` record short-circuits that instead.)
        self.pending_withdrawals.insert(
            rec.txid.clone(),
            PendingWithdrawal {
                txid: psbt.unsigned_tx.compute_txid(),
                asset_id: rec.asset_id,
                asset_symbol: rec.asset_symbol,
                input_outpoints,
                change_vout: rec.change_vout,
                change_amount: rec.change_amount,
                fascia: fascia.clone(),
            },
        );
        let input_btc_values = psbt
            .inputs
            .iter()
            .map(|i| i.witness_utxo.as_ref().map(|u| u.value.to_sat()).unwrap_or(0))
            .collect();
        Ok(BuildTransferOutcome {
            txid: psbt.unsigned_tx.compute_txid(),
            input_amounts: rec.input_amounts,
            input_btc_values,
            consignment: hex_decode(&rec.consignment_hex)?,
            fascia,
            psbt,
            recipient_vout: 1,
            change_vout: rec.change_vout,
            change_amount: rec.change_amount,
        })
    }

    /// Resolve caller-given replay input seals (see `build_withdrawal`).
    ///
    /// The seal *status* is deliberately **not** filtered — a replay spends seals the first
    /// attempt already marked `Consumed`, which is the whole point. What is enforced instead is
    /// that every outpoint really carries this contract's state in the Stock: without that check
    /// a caller could make the sidecar build a transfer out of arbitrary UTXOs.
    fn replay_input_seals(&self, symbol: &str, input_seals: &[String]) -> Result<Vec<OutPoint>> {
        let asset = self
            .ledger
            .asset(symbol)
            .ok_or_else(|| PermanentError(format!("asset {symbol} not issued")))?;
        let contract_id = ContractId::from_str(&asset.asset_id)?;
        let mut out: Vec<OutPoint> = Vec::with_capacity(input_seals.len());
        for s in input_seals {
            let outpoint =
                OutPoint::from_str(s).map_err(|e| anyhow!("invalid input seal {s:?}: {e}"))?;
            let assignments = self
                .stock
                .contract_assignments_for(contract_id, [outpoint])
                .map_err(|e| anyhow!("assignments: {e:?}"))?;
            if !assignments.contains_key(&OutputSeal::new(outpoint)) {
                return Err(anyhow!(
                    "requested input seal {outpoint} has no {} state",
                    asset.asset_id
                ));
            }
            if !out.contains(&outpoint) {
                out.push(outpoint);
            }
        }
        if out.is_empty() {
            return Err(anyhow!("empty input seal list"));
        }
        Ok(out)
    }

    /// Complete a withdrawal: verify the signed PSBT (segwit signing keeps the txid),
    /// mark input seals consumed, record the change seal, return (txid, recipient, change).
    pub fn finalize_withdrawal(
        &mut self,
        signed_psbt: &Psbt,
    ) -> Result<(Txid, String, Option<String>)> {
        let tx = signed_psbt.clone().extract_tx()?;
        let txid = tx.compute_txid();

        // Idempotent re-finalize (e.g. the bridge retrying after its broadcast failed): return the
        // values recorded by the first call instead of double-applying the transition.
        //
        // The record lives in the *persisted* ledger, so it survives a sidecar restart — a retry
        // that rebuilds the very same transaction (same seals ⇒ same txid, see `build_withdrawal`)
        // finds it and returns, instead of failing with "no pending withdrawal" or merging the
        // same fascia into the Stock twice. Either way the on-chain burn would never settle.
        if let Some(fin) = self.ledger.finalized_withdrawals.get(&txid.to_string()) {
            return Ok((txid, fin.recipient_outpoint.clone(), fin.change_outpoint.clone()));
        }

        // Keep the pending entry until the Stock merge succeeded, so a failure here leaves the
        // withdrawal retryable instead of dropping it.
        let pending = self
            .pending_withdrawals
            .get(&txid.to_string())
            .cloned()
            .ok_or_else(|| anyhow!("no pending withdrawal for txid {txid}"))?;
        assert_eq!(pending.txid, txid, "pending withdrawal txid mismatch");

        // Merge our own transfer into the Stock. The bridge builds the withdrawal transition
        // itself, so - unlike the deposit path, where `provide_consignment` -> `accept_transfer`
        // records the received seal - nothing else would ever register this transition's output
        // seals. Without it the RGB state of the change seal is missing and the next withdrawal
        // fails with "outpoint <txid>:<vout> has no <contract> state".
        //
        // Done here (the PSBT is signed, broadcast is imminent) rather than at build time: a
        // built-but-never-signed withdrawal must not leave a tentative transition in the Stock,
        // since tentative witnesses take priority over mined ones. The anchor is still in
        // `local_anchors` (resolved as tentative) because the tx is broadcast only after this
        // call returns.
        let resolver = BtcdResolver {
            rpc: self.rpc.clone(),
            chain_net: self.chain_net,
            local_anchors: self.local_anchors.clone(),
        };
        self.stock
            .consume_fascia(pending.fascia.clone(), resolver)
            .map_err(|e| anyhow!("consume_fascia: {e:?}"))?;

        // Anchor is being broadcast; drop it from the local resolver cache (btcd will own it now).
        self.local_anchors.lock().unwrap().remove(&txid);

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
        let recipient_outpoint = format!("{txid}:1");
        let change_outpoint = pending.change_vout.map(|v| format!("{txid}:{v}"));
        // Recorded before `save()` so the "already finalized" marker is persisted in the same
        // write as the seal statuses it stands for — a crash in between must not leave the
        // transition applied in the Stock without the record that makes the retry a no-op.
        self.ledger.finalized_withdrawals.insert(
            txid.to_string(),
            FinalizedWithdrawalRec {
                txid: txid.to_string(),
                recipient_outpoint: recipient_outpoint.clone(),
                change_outpoint: change_outpoint.clone(),
            },
        );
        self.save()?;
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

    /// The witness transaction behind `wtxid`: this engine's own un-broadcast anchor first (see
    /// `local_anchors`), then the node.
    ///
    /// Withdrawals are validated *before* their anchor tx is broadcast, so btcd cannot resolve it
    /// yet — only the tx this engine just built can. The cache is local construction, never an
    /// external claim: it is filled by `build_transfer` (the anchor whose txid the caller holds as
    /// the PSBT's own txid), so resolving through it cannot vouch for anything the engine did not
    /// build itself.
    fn witness_tx(&self, wtxid: &Txid) -> Option<Transaction> {
        if let Ok(guard) = self.local_anchors.lock() {
            if let Some(tx) = guard.get(wtxid) {
                return Some(tx.clone());
            }
        }
        self.rpc.get_transaction(wtxid).ok().flatten()
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
                                // Resolve through the local anchor cache as well: for a withdrawal
                                // the anchor is still unbroadcast here, and resolving it only from
                                // btcd left `recipient_amount` at 0 — the bridge then rejected the
                                // withdrawal with "consignment=0 expected=…" whenever the bridge
                                // holds nothing but the genesis seal (no earlier mined anchor).
                                if let Some(tx) = self.witness_tx(&wtxid) {
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

/// 逐输入写 PSBT 的 prevout 与 **scriptCode 载体**（BIP143 是逐输入定义的：一笔交易可以同时
/// 花主池 P2WPKH 与用户 P2WSH 的 UTXO，每个输入有自己的 scriptCode 与自己的一条签名轮次）。
///
///   - `witness_utxo.script_pubkey` 必须是**该输入自己的脚本**，不能再统一填主池脚本
///     （统一填的后果：签名者按主池脚本算 sighash，签出来的签名对这笔 UTXO 无效）；
///   - 原生 P2WSH 输入必须带 `PSBT_IN_WITNESS_SCRIPT` = 该输入的 witnessScript，Go 侧 TSS
///     签名器拿它当 BIP143 scriptCode。**不是** output program：34 字节的
///     `OP_0 <sha256(witnessScript)>` 会算出另一个 sighash（见 C1 的可复现证明
///     `rgbx/types/p2wsh_deposit_spend_test.go`）；
///   - P2WPKH 输入**不得**带 witness_script：上游 finalizer 对"带 witness_script 的 P2WPKH"
///     判为不可 finalize（`isFinalizableWitnessInput`），主池输入会因此签不出来。
///
/// 脚本来源是钱包的 watch 集（主池 ∪ 已登记的用户充值脚本）：用户 P2WSH 的 witnessScript 链上
/// 不可见，只能由 `(userID, tssPub)` 重派生后登记到钱包里。**解析不出 witnessScript 的非主池
/// 输入一律失败** —— 猜一个脚本去签等于让 TSS 对不属于这笔 UTXO 的脚本出签名。
fn fill_psbt_inputs(
    psbt: &mut Psbt,
    inputs: &[OutPoint],
    wallet_utxos: &[WalletUtxo],
    fallback_btc: &[u64],
    tss_script: &ScriptBuf,
    witness_script_for: &dyn Fn(&ScriptBuf) -> Option<ScriptBuf>,
) -> Result<()> {
    for (i, outpoint) in inputs.iter().enumerate() {
        let utxo = wallet_utxos.iter().find(|u| u.outpoint == *outpoint);
        // prevout 脚本：优先钱包的 watch 集（它带真实脚本），取不到时退回主池脚本 —— 后者与
        // 改动前的行为一致（seal 的 BTC 面额本来也取自钱包，取不到时按 0 计）。
        let script = utxo
            .map(|u| u.script_pubkey.clone())
            .unwrap_or_else(|| tss_script.clone());
        let value = utxo
            .map(|u| u.value)
            .unwrap_or_else(|| fallback_btc.get(i).copied().unwrap_or(0));
        if script != *tss_script {
            let witness_script = witness_script_for(&script).ok_or_else(|| {
                anyhow!(
                    "input {outpoint} pays script {} which is neither the main pool script nor a \
                     registered user deposit script: its witnessScript (the BIP143 scriptCode) is \
                     unknown, refusing to build an input that cannot be signed",
                    hex_encode(script.as_bytes())
                )
            })?;
            psbt.inputs[i].witness_script = Some(witness_script);
        }
        psbt.inputs[i].witness_utxo = Some(TxOut {
            value: bitcoin::Amount::from_sat(value),
            script_pubkey: script,
        });
    }
    Ok(())
}

fn hex_encode(bytes: &[u8]) -> String {
    bitcoin::hex::DisplayHex::to_hex_string(bytes, bitcoin::hex::Case::Lower)
}

#[cfg(test)]
mod fill_psbt_inputs_tests {
    use super::*;

    const USER_ID: &str = "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u";

    /// 与真实充值脚本同构：`<push userID> OP_DROP <push tssPub> OP_CHECKSIG`，
    /// program = sha256(witnessScript)（`rgbx/types/p2wsh_deposit.go` 的形态）。
    fn deposit_scripts(seed: u8) -> (ScriptBuf, ScriptBuf) {
        let mut ws = vec![0x22u8];
        ws.extend(USER_ID.as_bytes());
        ws.push(0x75); // OP_DROP
        let mut pk = vec![0x02u8];
        pk.extend(vec![seed; 32]);
        ws.push(pk.len() as u8);
        ws.extend(&pk);
        ws.push(0xac); // OP_CHECKSIG
        let witness_script = ScriptBuf::from_bytes(ws);
        let pk_script = ScriptBuf::new_p2wsh(&witness_script.wscript_hash());
        (pk_script, witness_script)
    }

    /// 主池 P2WPKH 形态的脚本（`OP_0 <20 字节>`）。这里只按字节比较脚本，不要求它是真实哈希。
    fn pool_script(seed: u8) -> ScriptBuf {
        let mut b = vec![0x00u8, 0x14];
        b.extend(vec![seed; 20]);
        ScriptBuf::from_bytes(b)
    }

    fn utxo(outpoint: OutPoint, value: u64, script: ScriptBuf) -> WalletUtxo {
        WalletUtxo {
            outpoint,
            value,
            script_pubkey: script,
            height: Some(1),
        }
    }

    fn op(n: u32) -> OutPoint {
        use bitcoin::hashes::Hash;
        OutPoint {
            txid: bitcoin::Txid::from_byte_array([n as u8; 32]),
            vout: n,
        }
    }

    fn unsigned_psbt(inputs: &[OutPoint]) -> Psbt {
        let tx = Transaction {
            version: bitcoin::transaction::Version::TWO,
            lock_time: LockTime::ZERO,
            input: inputs
                .iter()
                .map(|o| TxIn {
                    previous_output: *o,
                    script_sig: ScriptBuf::new(),
                    sequence: Sequence::MAX,
                    witness: Witness::new(),
                })
                .collect(),
            output: vec![TxOut {
                value: bitcoin::Amount::from_sat(1000),
                script_pubkey: ScriptBuf::new_op_return([]),
            }],
        };
        Psbt::from_unsigned_tx(tx).unwrap()
    }

    #[test]
    fn p2wsh_input_carries_its_own_witness_script() {
        let tss = pool_script(1);
        let (user_pk_script, user_ws) = deposit_scripts(2);
        let (o0, o1) = (op(0), op(1));
        let utxos = vec![
            utxo(o0, 5000, user_pk_script.clone()),
            utxo(o1, 7000, tss.clone()),
        ];
        let mut psbt = unsigned_psbt(&[o0, o1]);

        let known: HashMap<ScriptBuf, ScriptBuf> =
            [(user_pk_script.clone(), user_ws.clone())].into_iter().collect();
        fill_psbt_inputs(&mut psbt, &[o0, o1], &utxos, &[5000, 7000], &tss, &|s| {
            known.get(s).cloned()
        })
        .unwrap();

        // 每个输入带**自己的**脚本与面额（不再统一填主池脚本）。
        let in0 = psbt.inputs[0].witness_utxo.as_ref().unwrap();
        assert_eq!(in0.value.to_sat(), 5000);
        assert_eq!(in0.script_pubkey, user_pk_script);
        assert_eq!(
            psbt.inputs[0].witness_script.as_deref(),
            Some(user_ws.as_script())
        );
        let in1 = psbt.inputs[1].witness_utxo.as_ref().unwrap();
        assert_eq!(in1.value.to_sat(), 7000);
        assert_eq!(in1.script_pubkey, tss);
        // 主池输入不得带 witness_script（带了上游 finalizer 会把 P2WPKH 判为不可 finalize）。
        assert!(psbt.inputs[1].witness_script.is_none());

        // 线格式往返：witness_script 必须真的编码进 PSBT（签名节点拿到的是字节）。
        let back = Psbt::deserialize(&psbt.serialize()).unwrap();
        assert_eq!(
            back.inputs[0].witness_script.as_deref(),
            Some(user_ws.as_script())
        );
        assert!(back.inputs[1].witness_script.is_none());
    }

    #[test]
    fn unknown_non_pool_script_is_refused() {
        let tss = pool_script(1);
        let unknown = pool_script(9);
        let o0 = op(0);
        let utxos = vec![utxo(o0, 5000, unknown)];
        let mut psbt = unsigned_psbt(&[o0]);

        // 既不是主池脚本、也没有登记过 witnessScript ⇒ 必须失败：
        // 绝不拿 output program（或任何猜的脚本）去顶替 scriptCode。
        let err = fill_psbt_inputs(&mut psbt, &[o0], &utxos, &[5000], &tss, &|_| None).unwrap_err();
        let msg = format!("{err}");
        assert!(msg.contains("cannot be signed"), "unexpected error: {msg}");
    }

    #[test]
    fn unknown_utxo_falls_back_to_the_main_pool_script() {
        // 既有行为：钱包里查不到该 outpoint 时按 (fallback 面额, 主池脚本) 填。
        let tss = pool_script(1);
        let o0 = op(0);
        let mut psbt = unsigned_psbt(&[o0]);
        fill_psbt_inputs(&mut psbt, &[o0], &[], &[4242], &tss, &|_| None).unwrap();
        let inp = psbt.inputs[0].witness_utxo.as_ref().unwrap();
        assert_eq!(inp.value.to_sat(), 4242);
        assert_eq!(inp.script_pubkey, tss);
        assert!(psbt.inputs[0].witness_script.is_none());
    }
}

fn hex_decode(s: &str) -> Result<Vec<u8>> {
    use bitcoin::hashes::hex::FromHex;
    Vec::<u8>::from_hex(s).map_err(|e| anyhow!("bad hex: {e}"))
}

/// Serialize an RGB fascia for the ledger (same strict encoding the consignment helpers use).
fn fascia_to_bytes(fascia: &Fascia) -> Result<Vec<u8>> {
    use amplify::confinement::U24;
    use strict_encoding::StrictSerialize;
    Ok(fascia.to_strict_serialized::<U24>()?.release())
}

fn fascia_from_bytes(bytes: &[u8]) -> Result<Fascia> {
    use amplify::confinement::{Confined, U24};
    use strict_encoding::StrictDeserialize;
    let confined: Confined<Vec<u8>, 0, U24> =
        Confined::try_from(bytes.to_vec()).map_err(|e| anyhow!("fascia too large: {e}"))?;
    Fascia::from_strict_serialized::<U24>(confined).map_err(|e| anyhow!("deserialize fascia: {e}"))
}

fn split_outpoint(s: &str) -> Result<(String, u32)> {
    let (txid, vout) = s
        .rsplit_once(':')
        .ok_or_else(|| anyhow!("bad outpoint {s}"))?;
    Ok((txid.to_string(), vout.parse::<u32>()?))
}

/// Canonical key of a set of RGB seals: sorted by outpoint, comma-joined. Matches the bridge's
/// own encoding of its sticky-seal record (`rgb20.encodeStickySeals`), so a replay request and the
/// build it replays resolve to the same key.
fn seal_set_key(seals: &[OutPoint]) -> String {
    let mut v: Vec<String> = seals.iter().map(|o| o.to_string()).collect();
    v.sort();
    v.dedup();
    v.join(",")
}
