//! gRPC smoke test: start the sidecar server in-process, then exercise the client RPCs.
//!
//! Requires bitcoind regtest + electrs (see spike/regtest_env.sh).

use std::process::Command;

use anyhow::{anyhow, Result};
use bitcoin::Network;
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::pb::rgb_sidecar_client::RgbSidecarClient;
use rgb_sidecar::pb::*;
use rgb_sidecar::service::RgbSidecarService;

const ELECTRUM: &str = "127.0.0.1:60401";
const DATA_DIR: &str = "/tmp/rgb-spike/sidecar-smoke";

fn bitcoin_cli(args: &[&str]) -> Result<String> {
    let out = Command::new("/opt/bitcoin-27.2/bin/bitcoin-cli")
        .arg("-datadir=/tmp/rgb-spike/bitcoin")
        .arg("-rpcport=18443")
        .arg("-regtest")
        .args(args)
        .output()?;
    if !out.status.success() {
        return Err(anyhow!("bitcoin-cli failed: {}", String::from_utf8_lossy(&out.stderr)));
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

fn mine(n: u32) -> Result<()> {
    let a = bitcoin_cli(&["-rpcwallet=spike", "getnewaddress"])?;
    bitcoin_cli(&["generatetoaddress", &n.to_string(), &a])?;
    Ok(())
}

fn fund(addr: &str, btc: f64) -> Result<()> {
    bitcoin_cli(&["-rpcwallet=spike", "sendtoaddress", addr, &btc.to_string()])?;
    mine(1)?;
    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    std::fs::remove_dir_all(DATA_DIR).ok();

    // Issue a demo asset into the engine before serving.
    let tss_pubkey_hex = {
        let secp = bitcoin::secp256k1::Secp256k1::new();
        let sk = bitcoin::secp256k1::SecretKey::from_slice(&[0x11u8; 32])?;
        let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
        bitcoin::hex::DisplayHex::to_hex_string(&pk.serialize(), bitcoin::hex::Case::Lower)
    };
    let cfg = Config {
        data_dir: DATA_DIR.into(),
        electrum_url: ELECTRUM.into(),
        network: Network::Regtest,
        tss_pubkey_hex,
        grpc_listen: "0.0.0.0:0".into(),
    };
    let mut engine = RgbEngine::open(cfg)?;
    fund(&engine.tss_address().to_string(), 1.0)?;
    engine.sync()?;
    engine.issue_asset("USDT", "Tether USD", 8, 10_000_000_000)?;

    // Serve on an ephemeral port.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    println!("gRPC smoke: serving on {addr}");
    let service = RgbSidecarService::new(engine);
    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(rgb_sidecar::pb::rgb_sidecar_server::RgbSidecarServer::new(service))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await
            .expect("serve");
    });

    // Client calls.
    let mut client = RgbSidecarClient::connect(format!("http://{addr}")).await?;

    let assets = client.list_assets(ListAssetsRequest {}).await?.into_inner();
    println!("list_assets -> {} assets", assets.assets.len());
    assert_eq!(assets.assets.len(), 1);
    assert_eq!(assets.assets[0].asset_symbol, "USDT");

    let recv = client
        .create_receive(CreateReceiveRequest {
            asset_symbol: "USDT".into(),
            amount: 100_000_000,
            ..Default::default()
        })
        .await?
        .into_inner();
    println!("create_receive -> {} invoice.len={}", recv.receive_id, recv.invoice.len());
    assert!(recv.receive_id.starts_with("rcv-"));

    let bal = client
        .get_balance(GetBalanceRequest { asset_symbol: "USDT".into() })
        .await?
        .into_inner();
    println!("get_balance -> settled={} pending={}", bal.settled, bal.pending);
    assert_eq!(bal.settled, 10_000_000_000); // genesis minted

    let seals = client
        .list_seals(ListSealsRequest { asset_symbol: "USDT".into() })
        .await?
        .into_inner();
    println!("list_seals -> {} seals", seals.seals.len());
    assert_eq!(seals.seals.len(), 1);

    // ParseBtcTx: build a tiny tx and check it round-trips (stateless helper).
    let tx = bitcoin::Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: bitcoin::absolute::LockTime::ZERO,
        input: vec![],
        output: vec![bitcoin::TxOut {
            value: bitcoin::Amount::from_sat(0),
            script_pubkey: bitcoin::ScriptBuf::new_op_return([0x42u8; 32]),
        }],
    };
    let parsed = client
        .parse_btc_tx(ParseBtcTxRequest {
            btc_tx: bitcoin::consensus::encode::serialize(&tx),
            tss_pk_script: vec![],
        })
        .await?
        .into_inner();
    println!("parse_btc_tx -> commitment={} type={}", parsed.has_rgb_commitment, parsed.commitment_type);
    assert!(parsed.has_rgb_commitment);

    let transfers = client
        .list_transfers(ListTransfersRequest { asset_symbol: "USDT".into(), status_filter: "".into() })
        .await?
        .into_inner();
    println!("list_transfers -> {} transfers", transfers.transfers.len());
    assert_eq!(transfers.transfers.len(), 1); // the created receive

    println!("GRPC-SMOKE-PASS");
    Ok(())
}
