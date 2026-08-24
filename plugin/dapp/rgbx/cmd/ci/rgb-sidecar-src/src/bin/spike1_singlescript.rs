//! Spike 1: prove bdk_wallet can build a single-script watch-only wallet from
//! `wpkh(<compressed pubkey>)` — zero BIP32 derivation, every address == P2WPKH(hash160(pubkey)).
//!
//! Verify: (1) descriptor parses; (2) wallet address == P2WPKH(pubkey); (3) single-script
//! (any "derived" index returns the same address).

use std::collections::BTreeSet;
use std::str::FromStr;

use bdk_wallet::bitcoin::{Address, CompressedPublicKey, Network};
use bdk_wallet::{KeychainKind, Wallet};

fn main() {
    let pubkey_hex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798";
    let pubkey = CompressedPublicKey::from_str(pubkey_hex).expect("valid compressed pubkey");

    let descriptor_str = format!("wpkh({})", pubkey_hex);

    let mut wallet = Wallet::create_single(descriptor_str.clone())
        .network(Network::Regtest)
        .create_wallet_no_persist()
        .expect("create_single");
    println!("descriptor: {}", descriptor_str);

    let addr = wallet.peek_address(KeychainKind::External, 0);
    let expected = Address::p2wpkh(&pubkey, Network::Regtest);
    println!("wallet address:   {}", addr.address);
    println!("expected address: {}", expected);
    assert_eq!(addr.address, expected, "address must equal the TSS P2WPKH script");

    let revealed: Vec<_> = wallet
        .reveal_addresses_to(KeychainKind::External, 10)
        .collect();
    let distinct: BTreeSet<_> = revealed.iter().map(|a| a.address.clone()).collect();
    println!("derived addresses (10 indexes): {:?}", distinct);
    assert_eq!(distinct.len(), 1, "zero derivation: all indexes same address");

    println!("SPIKE1-PASS: single-script watch-only wallet OK");
}
