//! Sidecar application ledger: persistent bookkeeping of assets, RGB seals and receives.
//!
//! The RGB consensus state lives in the rgb-ops `Stock` (persisted as bin files by
//! `FsBinStore`). This JSON ledger is the *sidecar's own* bookkeeping that the Stock does
//! not track: symbol→asset mapping, seal lifecycle, receive_id↔SecretSeal mapping and the
//! incoming-transfer (receive) records surfaced by the gRPC API.

use std::collections::BTreeMap;
use std::fs;
use std::io::Write;
use std::path::Path;

use anyhow::{anyhow, Context, Result};
use serde::{Deserialize, Serialize};

use crate::types::{recv_status, SealStatus, SealTxOut};

/// An RGB20/IFA asset known to the sidecar.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AssetRec {
    pub asset_id: String,
    pub symbol: String,
    pub schema: String,
    pub precision: u8,
    pub issued_supply: i64,
    /// Hex of the serialized genesis contract consignment (so the contract can be
    /// re-imported if the Stock is rebuilt).
    pub genesis_consignment_hex: String,
}

/// An incoming RGB transfer (a receive) tracked by the sidecar.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ReceiveRec {
    pub receive_id: String,
    pub invoice: String,
    pub asset_symbol: String,
    pub asset_id: String,
    pub amount_requested: i64,
    pub status: String,
    pub settled_amount: i64,
    pub txid: String,
    pub vout: u32,
    /// Hex of the `SecretSeal` used for a blinded receive (None for witness receives).
    pub secret_seal_hex: Option<String>,
}

/// The whole persistent bookkeeping state.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Ledger {
    pub assets: BTreeMap<String, AssetRec>,
    pub seals: BTreeMap<String, SealTxOut>,
    pub receives: BTreeMap<String, ReceiveRec>,
    /// index receive_id -> outpoint for settled receives (reverse lookup for attribution).
    pub settled_by_outpoint: BTreeMap<String, String>,
}

impl Ledger {
    pub fn load(path: &Path) -> Result<Self> {
        if !path.exists() {
            return Ok(Self::default());
        }
        let data = fs::read(path)
            .with_context(|| format!("read ledger {}", path.display()))?;
        serde_json::from_slice(&data)
            .with_context(|| format!("parse ledger {}", path.display()))
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir).ok();
        }
        let tmp = path.with_extension("json.tmp");
        let mut f = fs::File::create(&tmp)
            .with_context(|| format!("create {}", tmp.display()))?;
        let json = serde_json::to_vec_pretty(self)?;
        f.write_all(&json)?;
        f.sync_all().ok();
        fs::rename(&tmp, path)
            .with_context(|| format!("rename to {}", path.display()))?;
        Ok(())
    }

    // ---- assets ----

    pub fn asset(&self, symbol: &str) -> Option<&AssetRec> {
        self.assets.values().find(|a| a.symbol == symbol)
    }

    pub fn upsert_asset(&mut self, rec: AssetRec) {
        self.assets.insert(rec.asset_id.clone(), rec);
    }

    // ---- seals ----

    pub fn seal(&self, outpoint: &str) -> Option<&SealTxOut> {
        self.seals.get(outpoint)
    }

    pub fn upsert_seal(&mut self, seal: SealTxOut) {
        self.seals.insert(seal.outpoint.clone(), seal);
    }

    pub fn seals_for<'a>(&'a self, symbol: &'a str) -> impl Iterator<Item = &'a SealTxOut> + 'a {
        self.seals
            .values()
            .filter(move |s| s.asset_symbol == symbol)
    }

    /// Select spendable (minted, not consumed) seals for `symbol` covering at least `need`.
    /// Returns (chosen seals, total amount, excess=change).
    pub fn select_seals<'a>(&'a self, symbol: &'a str, need: i64) -> Result<(Vec<&'a SealTxOut>, i64)> {
        let mut chosen = Vec::new();
        let mut total = 0i64;
        for s in self.seals_for(symbol) {
            if s.status == SealStatus::Minted {
                chosen.push(s);
                total = total.saturating_add(s.amount);
                if total >= need {
                    break;
                }
            }
        }
        if total < need {
            return Err(anyhow!(
                "insufficient {}: need {need}, have {total} (across {} minted seals)",
                symbol,
                chosen.len()
            ));
        }
        Ok((chosen, total))
    }

    // ---- receives ----

    pub fn upsert_receive(&mut self, rec: ReceiveRec) {
        self.receives.insert(rec.receive_id.clone(), rec);
    }

    pub fn receive(&self, id: &str) -> Option<&ReceiveRec> {
        self.receives.get(id)
    }

    pub fn mark_settled(&mut self, id: &str, amount: i64, asset_id: &str, txid: &str, vout: u32) -> Result<()> {
        let rec = self
            .receives
            .get_mut(id)
            .ok_or_else(|| anyhow!("receive {id} not found"))?;
        rec.status = recv_status::SETTLED.to_string();
        rec.settled_amount = amount;
        rec.asset_id = asset_id.to_string();
        rec.txid = txid.to_string();
        rec.vout = vout;
        self.settled_by_outpoint
            .insert(format!("{txid}:{vout}"), id.to_string());
        Ok(())
    }

    /// Reverse lookup: which receive_id settled the given outpoint.
    pub fn receive_for_outpoint(&self, outpoint: &str) -> Option<&str> {
        self.settled_by_outpoint.get(outpoint).map(String::as_str)
    }
}
