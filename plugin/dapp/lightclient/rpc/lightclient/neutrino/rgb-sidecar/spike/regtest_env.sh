#!/usr/bin/env bash
# Spike 2/3 regtest environment bootstrap.
#
# Sets up:
#   - bitcoind 27.2 regtest   (data dir $DATADIR, RPC 127.0.0.1:18443, cookie auth)
#   - electrs 0.11.1 (Electrum indexer on 127.0.0.1:60401), built from source
#   - funds the single-script P2WPKH address used by src/bin/spike2_utxo.rs
#
# Requirements:
#   - bitcoind & bitcoin-cli on PATH (or /opt/bitcoin-27.2/bin)
#   - electrs source already built (see ELECTRS_BIN below) with
#     LIBCLANG_PATH=/Library/Developer/CommandLineTools/usr/lib
#
# Usage:  ./spike/regtest_env.sh
# After this, run:
#   cargo run --bin spike2_utxo
#   cargo run --bin spike3_rgb

set -euo pipefail

BITCOIND="${BITCOIND:-/opt/bitcoin-27.2/bin/bitcoind}"
BITCOIN_CLI="${BITCOIN_CLI:-/opt/bitcoin-27.2/bin/bitcoin-cli}"
ELECTRS_BIN="${ELECTRS_BIN:-/tmp/rgb-spike/electrs-src/target/release/electrs}"

DATADIR="${DATADIR:-/tmp/rgb-spike/bitcoin}"
CONF="$DATADIR/bitcoin.conf"
ELECTRS_DB="${ELECTRS_DB:-/tmp/rgb-spike/electrs-db}"

bc() { "$BITCOIN_CLI" -datadir="$DATADIR" -rpcport=18443 -regtest "$@"; }

mkdir -p "$DATADIR"
if [[ ! -f "$CONF" ]]; then
  cat > "$CONF" <<EOF
regtest=1
server=1
txindex=1
fallbackfee=0.0001
[regtest]
rpcport=18443
rpcbind=127.0.0.1
EOF
fi

# bitcoind (cookie auth only — electrs needs the .cookie file)
if ! bc getblockchaininfo >/dev/null 2>&1; then
  echo "== starting bitcoind regtest =="
  "$BITCOIND" -datadir="$DATADIR" -daemon
  sleep 3
fi

# wallet for mining
bc loadwallet spike >/dev/null 2>&1 || bc createwallet spike >/dev/null 2>&1 || true

# mine enough for mature coinbase + electrs indexing
if [[ "$(bc getblockcount)" -lt 150 ]]; then
  echo "== mining 150 regtest blocks =="
  ADDR=$(bc -rpcwallet=spike getnewaddress)
  bc generatetoaddress 150 "$ADDR" >/dev/null
fi

# electrs
if ! nc -z 127.0.0.1 60401 >/dev/null 2>&1; then
  echo "== starting electrs 0.11.1 (Electrum on 127.0.0.1:60401) =="
  "$ELECTRS_BIN" --network regtest --db-dir "$ELECTRS_DB" \
    --daemon-dir "$DATADIR" --electrum-rpc-addr 127.0.0.1:60401 \
    --log-filters info &
  # wait for electrum port
  for _ in $(seq 1 30); do
    nc -z 127.0.0.1 60401 >/dev/null 2>&1 && break
    sleep 1
  done
fi
echo "== electrs reachable on 127.0.0.1:60401 =="

# Fund the single-script address used by spike2 (descriptor wpkh(<G compressed>)).
TSS_ADDR=bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080
if [[ "$(bc -rpcwallet=spike getbalance)" == "0.00000000" ]] || true; then
  echo "== funding single-script address $TSS_ADDR =="
  bc -rpcwallet=spike sendtoaddress "$TSS_ADDR" 1.5 >/dev/null
  bc generatetoaddress 3 "$(bc -rpcwallet=spike getnewaddress)" >/dev/null
fi

echo
echo "Environment ready."
echo "  cargo run --bin spike2_utxo"
echo "  cargo run --bin spike3_rgb"
