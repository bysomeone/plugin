//! RGB sidecar gRPC server (tonic).

use std::path::PathBuf;

use bitcoin::Network;
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::pb::rgb_sidecar_server::RgbSidecarServer;
use rgb_sidecar::service::RgbSidecarService;

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn parse_network(s: &str) -> Network {
    match s.to_lowercase().as_str() {
        "mainnet" | "bitcoin" => Network::Bitcoin,
        "testnet" => Network::Testnet,
        "testnet4" => Network::Testnet4,
        "signet" => Network::Signet,
        _ => Network::Regtest,
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let data_dir = PathBuf::from(env_or("RGB_SIDECAR_DATA_DIR", "./sidecar-data"));
    let electrum_url = env_or("RGB_SIDECAR_ELECTRUM", "127.0.0.1:60401");
    let network = parse_network(&env_or("RGB_SIDECAR_NETWORK", "regtest"));
    let tss_pubkey_hex = env_or(
        "RGB_SIDECAR_TSS_PUBKEY",
        // Dev default = test key 0x11... (E2E key); production MUST set the real TSS pubkey.
        "034f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa",
    );
    let grpc_listen = env_or("RGB_SIDECAR_LISTEN", "0.0.0.0:50061");

    let cfg = Config {
        data_dir,
        electrum_url,
        network,
        tss_pubkey_hex,
        grpc_listen: grpc_listen.clone(),
    };
    let engine = RgbEngine::open(cfg)?;
    let service = RgbSidecarService::new(engine);

    // Test-only simulation HTTP server (E2E driver), disabled unless RGB_SIDECAR_TEST_LISTEN is set.
    let test_listen = env_or("RGB_SIDECAR_TEST_LISTEN", "");
    if !test_listen.is_empty() {
        let sim_engine = service.engine_handle();
        rgb_sidecar::test_sim::spawn(sim_engine, &test_listen)?;
        println!("RGB test-sim HTTP enabled on {test_listen}");
    }

    println!("RGB sidecar listening on gRPC {grpc_listen}");
    tonic::transport::Server::builder()
        .add_service(RgbSidecarServer::new(service))
        .serve(grpc_listen.parse()?)
        .await?;
    Ok(())
}
