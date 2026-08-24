//! tonic gRPC service — implements every RPC of `proto/rgb_sidecar.proto`.
//!
//! All state mutations are serialised through a single `tokio::sync::Mutex<RgbEngine>`; the
//! engine holds no private keys (TSS signs externally).

use std::sync::Arc;

use bitcoin::consensus::Decodable;
use bitcoin::psbt::Psbt;
use tokio::sync::Mutex;
use tonic::{Request, Response, Status};

use crate::engine::{ConsignmentInspection, RgbEngine};
use crate::pb::rgb_sidecar_server::RgbSidecar;
use crate::pb::*;

pub struct RgbSidecarService {
    engine: Arc<Mutex<RgbEngine>>,
}

impl RgbSidecarService {
    /// Shared engine handle (used by the test-sim HTTP driver in the same process).
    pub fn engine_handle(&self) -> Arc<Mutex<RgbEngine>> {
        self.engine.clone()
    }

    pub fn new(engine: RgbEngine) -> Self {
        Self { engine: Arc::new(Mutex::new(engine)) }
    }
}

fn err(e: impl std::fmt::Display) -> Status {
    Status::internal(format!("{e}"))
}

fn psbt_to_bytes(psbt: &Psbt) -> Vec<u8> {
    psbt.serialize()
}

fn psbt_from_bytes(bytes: &[u8]) -> Result<Psbt, Status> {
    bitcoin::psbt::Psbt::deserialize(bytes)
        .map_err(|e| Status::invalid_argument(format!("bad PSBT: {e}")))
}

#[tonic::async_trait]
impl RgbSidecar for RgbSidecarService {
    async fn create_receive(
        &self,
        request: Request<CreateReceiveRequest>,
    ) -> Result<Response<ReceiveData>, Status> {
        let req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let (invoice, receive_id) = engine
            .create_receive(&req.asset_symbol, req.amount)
            .map_err(err)?;
        Ok(Response::new(ReceiveData {
            invoice,
            receive_id,
            seal_script: engine.tss_script().to_bytes(),
        }))
    }

    async fn provide_consignment(
        &self,
        request: Request<ProvideConsignmentRequest>,
    ) -> Result<Response<TransferState>, Status> {
        let req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let hint = if req.receive_id_hint.is_empty() {
            None
        } else {
            Some(req.receive_id_hint.as_str())
        };
        let rec = engine
            .provide_consignment(&req.consignment, hint)
            .map_err(err)?;
        Ok(Response::new(TransferState {
            receive_id: rec.receive_id,
            status: rec.status,
            amount: rec.settled_amount,
            asset_id: rec.asset_id,
            txid: rec.txid,
            vout: rec.vout,
        }))
    }

    async fn list_transfers(
        &self,
        request: Request<ListTransfersRequest>,
    ) -> Result<Response<ListTransfersResponse>, Status> {
        let req = request.into_inner();
        let engine = self.engine.lock().await;
        let transfers = engine
            .ledger
            .receives
            .values()
            .filter(|r| req.asset_symbol.is_empty() || r.asset_symbol == req.asset_symbol)
            .filter(|r| req.status_filter.is_empty() || r.status == req.status_filter)
            .map(|r| TransferState {
                receive_id: r.receive_id.clone(),
                status: r.status.clone(),
                amount: r.settled_amount,
                asset_id: r.asset_id.clone(),
                txid: r.txid.clone(),
                vout: r.vout,
            })
            .collect();
        Ok(Response::new(ListTransfersResponse { transfers }))
    }

    async fn validate_consignment(
        &self,
        request: Request<ValidateConsignmentRequest>,
    ) -> Result<Response<ConsignmentValidation>, Status> {
        let req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let insp: ConsignmentInspection = engine
            .validate_consignment(&req.consignment)
            .map_err(err)?;
        let synced_height = engine.synced_height();
        let resp = ConsignmentValidation {
            valid: insp.valid,
            error_message: insp.error.unwrap_or_default(),
            amount: insp.recipient_amount,
            asset_id: insp.asset_id,
            closed_seals: insp.closed_seals,
            opened_seals: insp
                .opened_seals
                .into_iter()
                .map(|o| OpenedSeal {
                    outpoint: o.outpoint,
                    amount: o.amount,
                    asset_id: o.asset_id,
                })
                .collect(),
            recipient_seal: insp.recipient_seal.unwrap_or_default(),
            synced_height,
        };
        Ok(Response::new(resp))
    }

    async fn sync(&self, request: Request<SyncRequest>) -> Result<Response<SyncResponse>, Status> {
        let _req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let (changed, new_seals) = engine.sync().map_err(err)?;
        Ok(Response::new(SyncResponse { changed, new_seals }))
    }

    async fn list_seals(
        &self,
        request: Request<ListSealsRequest>,
    ) -> Result<Response<ListSealsResponse>, Status> {
        let req = request.into_inner();
        let engine = self.engine.lock().await;
        let seals = engine
            .list_seals(&req.asset_symbol)
            .into_iter()
            .map(|s| SealInfo {
                outpoint: s.outpoint,
                asset_id: s.asset_id,
                asset_symbol: s.asset_symbol,
                amount: s.amount,
                btc_value: s.btc_value,
                maturity_height: s.maturity_height,
                status: s.status.as_str().to_string(),
            })
            .collect();
        Ok(Response::new(ListSealsResponse { seals }))
    }

    async fn get_balance(
        &self,
        request: Request<GetBalanceRequest>,
    ) -> Result<Response<Balance>, Status> {
        let req = request.into_inner();
        let engine = self.engine.lock().await;
        let (settled, pending) = engine.get_balance(&req.asset_symbol);
        Ok(Response::new(Balance { settled, pending }))
    }

    async fn list_assets(
        &self,
        _request: Request<ListAssetsRequest>,
    ) -> Result<Response<ListAssetsResponse>, Status> {
        let engine = self.engine.lock().await;
        let assets = engine
            .list_assets()
            .into_iter()
            .map(|a| AssetInfo {
                asset_id: a.asset_id,
                asset_symbol: a.symbol,
                schema: a.schema,
                precision: a.precision as u32,
            })
            .collect();
        Ok(Response::new(ListAssetsResponse { assets }))
    }

    async fn build_withdrawal(
        &self,
        request: Request<BuildWithdrawalRequest>,
    ) -> Result<Response<BuildWithdrawalResponse>, Status> {
        let req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let outcome = engine
            .build_withdrawal(
                &req.asset_symbol,
                req.amount,
                &req.recipient_invoice,
                &req.change_address,
                req.fee_rate as u64,
            )
            .map_err(err)?;
        Ok(Response::new(BuildWithdrawalResponse {
            psbt: psbt_to_bytes(&outcome.psbt),
            input_amounts: outcome.input_amounts,
            consignment: outcome.consignment,
        }))
    }

    async fn finalize_withdrawal(
        &self,
        request: Request<FinalizeWithdrawalRequest>,
    ) -> Result<Response<FinalizeWithdrawalResponse>, Status> {
        let req = request.into_inner();
        let mut engine = self.engine.lock().await;
        let psbt = psbt_from_bytes(&req.psbt_signed)?;
        let (txid, recipient_outpoint, change_outpoint) =
            engine.finalize_withdrawal(&psbt).map_err(err)?;
        Ok(Response::new(FinalizeWithdrawalResponse {
            txid: txid.to_string(),
            recipient_seal_outpoint: recipient_outpoint,
            change_seal_outpoint: change_outpoint.unwrap_or_default(),
        }))
    }

    async fn parse_btc_tx(
        &self,
        request: Request<ParseBtcTxRequest>,
    ) -> Result<Response<ParseBtcTxResponse>, Status> {
        let req = request.into_inner();
        let tx = bitcoin::Transaction::consensus_decode(&mut &req.btc_tx[..])
            .map_err(|e| Status::invalid_argument(format!("bad tx: {e}")))?;
        let tss_script = bitcoin::ScriptBuf::from_bytes(req.tss_pk_script);

        let mut has_opret = false;
        let mut seal_output_index = u32::MAX;
        for (i, out) in tx.output.iter().enumerate() {
            if out.script_pubkey.is_op_return() && !out.script_pubkey.is_empty() {
                has_opret = true;
            }
            if out.script_pubkey == tss_script && seal_output_index == u32::MAX {
                seal_output_index = i as u32;
            }
        }
        Ok(Response::new(ParseBtcTxResponse {
            has_rgb_commitment: has_opret,
            commitment_type: if has_opret { "opret".to_string() } else { String::new() },
            asset_id: String::new(),
            seal_output_index: if seal_output_index == u32::MAX { 0 } else { seal_output_index },
        }))
    }
}
