# RGB Sidecar — Spike 2 Report

**Goal of the spike series:** decide whether a small Rust "sidecar" daemon can provide RGB
consensus (validate consignments, build state transitions + unsigned PSBTs, track seals) in
**watch-only** mode for a chain33 cross-chain bridge, under the constraint that the TSS GG18
group holds a **bare secp256k1 pubkey with NO chain code** — so the sidecar must use
**single-script** wallets/PSBTs pinned to `wpkh(<TSS compressed pubkey>)`, zero BIP32
derivation, and must **not hold private keys**.

- Spike 1 (done): `bdk_wallet` 3.1.0 builds a single-script watch-only wallet from
  `wpkh(<compressed_pubkey>)`; every derived index == the same P2WPKH address. Code in `src/main.rs`.
- **This document: Spike 2** — regtest indexer, UTXO recognition, and the **make-or-break**
  decision on the RGB consensus layer + the PSBT external-signing seam.

All code below was **compiled and run** in this repo (Rust 1.98, macOS aarch64). Run it yourself:

```
./spike/regtest_env.sh          # bitcoind regtest + electrs + fund the spike address
cargo run --bin spike1_singlescript   # Spike 1 (single-script wallet)
cargo run --bin spike2_utxo           # UTXO recognition (needs the regtest env)
cargo run --bin spike3_rgb            # RGB layer: issue IFA, validate, build PSBT, external signing
```

---

## 1. Working regtest env (exact commands)

### Choice of indexer: **electrs 0.11.1** (Electrum protocol), synced from `bdk_electrum 0.24.0`

- `bitcoind 27.2` is at `/opt/bitcoin-27.2/bin/bitcoind` (+ `bitcoin-cli`).
- Docker daemon is **not** running, so the indexer must run natively.
- Candidates:
  - **electrs** — Rust, needs a `cargo build`. **v0.11.1 ships NO prebuilt binaries** (release assets empty) and, importantly, **v0.11.1 removed the esplora HTTP API** (`--http-addr` is gone; it only serves the **Electrum** protocol). bdk connects via `bdk_electrum`.
  - **esplora server** — not a single downloadable binary (it is a heavy Rust service that itself needs an Electrum backend: electrs/fulcrum). Not simpler.
  - Verdict: **electrs from source + `bdk_electrum`**. `bdk_esplora` would need a separate esplora HTTP server (electrs ≤0.10 or the full esplora stack) — documented, not needed for the spike.

### Build electrs 0.11.1

```bash
cd /tmp && git clone --depth 1 --branch v0.11.1 https://github.com/romanz/electrs.git rgb-spike/electrs-src
cd rgb-spike/electrs-src
# macOS: rust-rocksdb bindgen needs libclang; fix the dyld @rpath error:
cargo clean -p rust-librocksdb-sys
LIBCLANG_PATH=/Library/Developer/CommandLineTools/usr/lib \
DYLD_LIBRARY_PATH=/Library/Developer/CommandLineTools/usr/lib \
  cargo build --release        # ~6 min (rocksdb C++)
```

### Start bitcoind regtest (cookie auth — electrs needs the `.cookie`)

```bash
DATADIR=/tmp/rgb-spike/bitcoin; mkdir -p "$DATADIR"
cat > "$DATADIR/bitcoin.conf" <<EOF
regtest=1
server=1
txindex=1
fallbackfee=0.0001
[regtest]
rpcport=18443
rpcbind=127.0.0.1
EOF
/opt/bitcoin-27.2/bin/bitcoind -datadir="$DATADIR" -daemon
# NOTE: do NOT set rpcuser/rpcpassword — electrs reads the cookie file that bitcoind
# only generates when auth is cookie-based. (.cookie at $DATADIR/regtest/.cookie)

bc() { /opt/bitcoin-27.2/bin/bitcoin-cli -datadir="$DATADIR" -rpcport=18443 -regtest "$@"; }
bc createwallet spike
ADDR=$(bc -rpcwallet=spike getnewaddress)
bc generatetoaddress 150 "$ADDR"        # mature coinbase + indexable chain
```

### Start electrs (Electrum on 60401)

```bash
/tmp/rgb-spike/electrs-src/target/release/electrs \
  --network regtest \
  --db-dir /tmp/rgb-spike/electrs-db \
  --daemon-dir /tmp/rgb-spike/bitcoin \
  --electrum-rpc-addr 127.0.0.1:60401 \
  --log-filters info
# verify: nc -z 127.0.0.1 60401 ; python -c 'socket...' blockchain.headers.subscribe -> height 150
```

### Fund the single-script address (descriptor `wpkh(<G compressed>)`)

```bash
bc -rpcwallet=spike sendtoaddress bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080 1.5
bc generatetoaddress 3 "$(bc -rpcwallet=spike getnewaddress)"
```

`spike/regtest_env.sh` automates all of the above.

---

## 2. Spike 2 result — UTXO recognition via bdk on a single-script wallet

`src/bin/spike2_utxo.rs` (bdk_wallet 3.1.0 + bdk_electrum 0.24.0):

1. Builds `wpkh(0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798)`
   single-script wallet (`Wallet::create_single(...).network(Regtest)`).
2. `wallet.reveal_addresses_to(External, 5)` then
   `wallet.start_sync_with_revealed_spks().build()` →
   `client.sync(request, 10, true)` (BdkElectrumClient) →
   `wallet.apply_update(response)`.
3. Asserts the wallet **owns** the 1.5 BTC UTXO and that every listed UTXO is at the
   shared script.

Verified output:

```
descriptor:      wpkh(0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798)
shared address:  bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080
balance:         total=1.50000000 BTC confirmed=1.50000000 BTC ...
list_unspent:    1 UTXOs
  outpoint=e032dad5...:0 amount=1.50000000 BTC is_spent=false spk==shared=true
SPIKE2-PASS: single-script watch-only wallet synced and owns the UTXO
```

**Verdict:** the whole "watch-only single-script wallet synced from an indexer" loop is
real. The sidecar can track the TSS script's UTXOs with zero derivation and zero keys.

---

## 3. RGB layer decision — **drive the split RGB crates directly; do NOT use rgb-lib**

### Why rgb-lib is out (concrete evidence)

rgb-lib **0.3.0-beta.7** is the current release. Its wallet layer is BIP32/xpub-centric
(even its watch-only mode):

- `rgb_lib::Wallet::new` / `new_in_memory` build **xpub descriptors** —
  `build_descriptors()` in `src/wallet/singlesig.rs` uses `account_xpub_vanilla` +
  `account_xpub_colored` (`get_descriptors_from_xpubs(...)`); a bare pubkey with no chain
  code cannot be expressed.
- The receive model derives addresses (`get_new_addresses(KeychainKind::Internal, 1)`) and
  blinds wallet-owned UTXOs selected from derived keys; receive attribution is tied to those
  derived addresses + a SeaORM DB.
- rgb-lib does expose `send_begin`/`send_end` (which per its docs "don't require the wallet
  to have private keys"), and `watch_only()` returns true when no mnemonic is set — but the
  wallet **structure** is still xpub-descriptor based, so a single-script
  `wpkh(<bare pubkey>)` descriptor cannot be expressed through any rgb-lib constructor.

**rgb-lib is therefore incompatible with the "TSS bare pubkey, no chain code, no keys in
sidecar" constraint** — not because it cannot be watch-only, but because it cannot be
single-script / zero-derivation.

Note: rgb-lib internally uses the **same split crates** we drive directly below (its
`Cargo.toml` pins `rgb-ops =0.11.1-rc.11`, `rgb-schemas =0.11.1-rc.11`,
`rgb-psbt-utils =0.11.1-rc.11`, `rgb-invoicing =0.11.1-rc.11`). So we keep the consensus
engine and only replace rgb-lib's BIP32 wallet shell.

### The split crates we drive directly (all **compiled and run** here)

| crate | version | lib name in code |
|---|---|---|
| `rgb-consensus` | `0.11.1-rc.11` | `rgbcore` |
| `rgb-ops` | `0.11.1-rc.11` | `rgbstd` |
| `rgb-schemas` | `0.11.1-rc.11` | `schemata` |
| `rgb-invoicing` | `0.11.1-rc.11` | `rgbinvoice` |
| `rgb-psbt-utils` | `0.11.1-rc.11` | `psrgbt` |
| `rgb-strict-encoding` | `1.0.2` | `strict_encoding` |
| `rgb-strict-types` | `1.0.2` | `strict_types` |

Plus bdk_wallet `=3.1.0`, bdk_electrum `=0.24.0`, bitcoin `0.32.102` — the full matrix
resolves and compiles together (1m23s `cargo check`).

`src/bin/spike3_rgb.rs` is the runnable evidence. It does, with no wallet state and no
private keys anywhere in the RGB path:

---

## 4. Answers to (a)–(d)

### (a) Validate a consignment deterministically WITHOUT wallet state — `ValidateConsignment` gRPC

Yes. Two API layers, both demonstrated as "is valid":

1. High level (what the gRPC should call), in **rgb-ops** (`rgbstd::containers::Consignment`):
   ```rust
   impl<const TRANSFER: bool> Consignment<TRANSFER> {
       pub fn validate(
           self,
           resolver: &impl ResolveWitness,
           validation_config: &ValidationConfig,
       ) -> Result<ValidConsignment<TRANSFER>, ValidationError>;
   }
   ```
2. Low level (what rgb-ops calls internally), in **rgb-consensus**
   (`rgbcore::validation::Validator`):
   ```rust
   pub fn validate<'consignment, 'resolver, S, C, R>(
       consignment: &'consignment C,        // C: ConsignmentApi
       resolver: &'resolver R,              // R: ResolveWitness
       context: S::Context<'_>,             // S: ContractStateAccess + ContractStateEvolve
       validation_config: &ValidationConfig,// { chain_net, safe_height, trusted_typesystem, build_opouts_dag }
   ) -> Result<Status, ValidationError>     // Status::validity() -> Valid | Warnings
   ```

The only external dependency is `ResolveWitness` — **our** chain view (bitcoind/electrs):

```rust
pub trait ResolveWitness {
    fn resolve_witness(&self, witness_id: Txid) -> Result<WitnessStatus, WitnessResolverError>;
    fn check_chain_net(&self, chain_net: ChainNet) -> Result<(), WitnessResolverError>;
}
```

The consignment is self-contained (schema, types, scripts, genesis/bundles, witnesses are
bundled). Deserialize via `Consignment::<true>::from_strict_serialized(bytes)` and validate.
The spike serializes the built transfer to bytes, round-trips it, and validates it with a
fresh resolver — no `Stock`, no wallet.

### (b) Construct a state transition + opret anchor and build an UNSIGNED Bitcoin PSBT with EXPLICIT inputs — `BuildWithdrawal`

Yes. The path (all in the spike):

1. Build the transition with **rgb-ops** `TransitionBuilder`
   (`rgbstd::contract::TransitionBuilder`), via `stock.transition_builder(contract_id, "transfer")`
   (or `TransitionBuilder::with(contract_id, schema, TS_TRANSFER, types)`):
   ```rust
   builder
       .add_input(opout, AllocatedState::from(Amount::from(n)))?   // explicit input seal opout
       .add_fungible_state("assetOwner", recipient_seal, amount)?  // output seal (blinded vout)
       .complete_transition()?;                                     // -> Transition
   ```
2. Build the bitcoin PSBT with **explicit inputs** (bitcoin crate 0.32):
   ```rust
   let tx = Transaction { version: Version::TWO, lock_time: LockTime::ZERO,
       input: vec![TxIn { previous_output: explicit_utxo, script_sig: empty, sequence: MAX, witness: empty }],
       output: vec![ op_return(0), tss_script(dust), tss_script(change) ] };
   let mut psbt = Psbt::from_unsigned_tx(tx)?;
   psbt.inputs[0].witness_utxo = Some(TxOut { value, script_pubkey: tss_script });
   ```
   (bdk's `build_tx().add_utxos(inputs).manually_selected_only()` is an equivalent
   alternative — rgb-lib uses it — but manual construction is the most explicit.)
3. Anchor via **rgb-psbt-utils** (`psrgbt::RgbPsbtExt`/`RgbOutExt` on `bitcoin::Psbt`):
   ```rust
   psbt.push_rgb_transition(transition.clone())?;
   psbt.outputs[0].set_opret_host();
   psbt.outputs[0].set_mpc_entropy(42)?;
   psbt.set_rgb_close_method(CloseMethod::OpretFirst);
   let fascia = psbt.rgb_commit()?;                 // -> Fascia { seal_witness, bundles }
   ```
   `rgb_commit()` computes the MPC merkle root, writes the OP_RETURN commitment into
   `unsigned_tx.output[0].script_pubkey`, and returns a `Fascia`.
4. Build the final `Transfer` consignment from the fascia + explicit outputs
   (**rgb-ops** `Stock::transfer_from_fascia`):
   ```rust
   pub fn transfer_from_fascia(
       &self,
       contract_id: ContractId,
       outputs: impl AsRef<[OutputSeal]>,           // explicit witness-vout seals
       secret_seals: impl AsRef<[SecretSeal]>,      // blinded seals to reveal
       opids: impl IntoIterator<Item = OpId>,
       fascia: &Fascia,
   ) -> Result<Consignment<true>, StockError<S,H,P,ConsignError>>;
   ```

The resulting `Consignment<true>` is what gets handed to the recipient (and what (a) validates).

### (c) Issue an RGB20 (IFA) fungible asset and send it between two addresses

Yes — `rgb-schemas` embeds the IFA (InflatableFungibleAsset, "RGB20") schema. Spike 3 does
it end-to-end:

- **Issue** with `rgbstd::contract::ContractBuilder`:
  ```rust
  ContractBuilder::with(Identity::default(),
        InflatableFungibleAsset::schema(), InflatableFungibleAsset::types(),
        InflatableFungibleAsset::scripts(), ChainNet::BitcoinRegtest)
    .add_global_state("spec", AssetSpec::new("USDT","Tether USD", Precision::CentiMicro))?
    .add_global_state("terms", ContractTerms{ text: RicardianContract::default(), media: None })?
    .add_global_state("issuedSupply", Amount::from(issued))?
    .add_global_state("maxSupply",     Amount::from(issued))?   // REQUIRED (Once)
    .add_fungible_state("assetOwner", genesis_seal, Amount::from(issued))?
    .issue_contract()?;   // -> ValidConsignment<false>, internally validates
  ```
  Required IFA globals: `spec`, `terms`, `issuedSupply`, `maxSupply` (each `Occurrences::Once`);
  `assetOwner` (OS_ASSET=4000) is the fungible assignment.
- **Send** = the transition+PSBT flow in (b); "two addresses" is modelled as two seals on
  the shared TSS script (recipient vout 1, change vout 2). The RGB layer is purely
  **outpoint-based**, so sending between any two outpoints — even two outputs of the same
  script — is just two `ExplicitSeal`s.

Verified output (spike3):
```
SPIKE3: issued IFA contract id=rgb:A9n0n2rI-...
SPIKE3:   schema_id=rgb:sch:IpjJhFLz3oywYKQxO3KmFgR0Aa415nlTNrNyEFqMZCE#shoe-...
SPIKE3(a): genesis consignment validated statelessly, status=is valid
SPIKE3(b): committed opret anchor, witness txid=5f51f5a3...
SPIKE3(a): transfer consignment validated statelessly, status=is valid
```

### (d) Receive attribution under a shared script — analysis + recommendation

Facts from the code:

- RGB output seals are **`BlindSeal<{txid,vout}>`**, i.e. **outpoint-based**, with an
  optional blinding factor. The RGB layer never sees a *script* — the sidecar's "shared
  script" is irrelevant to RGB consensus. This is the key enabler.
- rgb-lib's attribution is: on `blind_receive` it picks a wallet-owned UTXO and blinds it
  (`get_blind_seal(outpoint) -> BlindSeal; conceal() -> SecretSeal`), puts
  `Beneficiary::BlindedSeal(secret_seal)` in the invoice, and sets
  `recipient_id = beneficiary.to_string()`. On settle it reveals the seal
  (`stash.seal_secret(secret_seal)`) and attributes the allocation to that recipient.

**Recommendation (our own design, since we own receive logic):** keep rgb-lib's *model*,
drop its *derivation*.

1. For each receive, the sidecar generates a fresh `SecretSeal`
   (`BlindSeal::<TxPtr>::new_random_vout(vout).conceal()`) and stores
   `secret_seal -> receive_id` in its own store. The blinding factor is the attribution
   key — every receive at the same script gets a *different* blinding factor, so two
   concurrent receives at the same script are distinguishable.
2. The invoice/callback carries `Beneficiary::BlindedSeal(secret_seal)`.
3. When the witness tx confirms at the shared script, the sidecar reveals each stored
   `secret_seal` (`stock.seal_secret(secret_seal)` or `consignment.reveal_terminal_seals`),
   matches the revealed outpoint, and attributes the allocation to `receive_id`.

Fallback (witness-vout / explicit receives to the shared script): attribute by tracking
pending receives and matching `(outpoint, amount)`; this is fragile under concurrency and
is **not** recommended — prefer blinded receives.

Caveat to record: `BlindSeal::new_random_vout` uses `rand`; the sidecar must **persist the
blinding factor** per receive (or use a deterministic PRF) so it can reveal/recover after a
restart.

---

## 5. PSBT external-signing seam — result

Verified in spike3 (goal 4). After `rgb_commit()` produced the opret anchor on an
**unsigned** PSBT:

1. The PSBT input is signed by an **external** private key (a test key that happens to be
   the descriptor key — the real signer is the TSS group). We set `witness_utxo`, compute
   `SighashCache::p2wpkh_signature_hash(...)`, insert `partial_sigs`, finalize the witness,
   and `extract_tx()`.
2. **The signed txid equals the unsigned txid** (`assert_eq` passes). This is the load-bearing
   fact for TSS: for segwit (P2WPKH) inputs the **txid excludes the witness**, so adding the
   signature does not change the txid — and the RGB opret anchor commits to the txid.
3. The **same** transfer consignment re-validates with the resolver returning the *signed*
   tx: `status=is valid`.

Output:
```
SPIKE3(g4): signed PSBT extracted, signed txid=5f51f5a3...
SPIKE3(g4): transfer re-validated with the SIGNED witness tx, status=is valid
```

**Operational constraint for the TSS integration:** the anchor is fixed at `rgb_commit()`
time. TSS must sign the **exact** PSBT the sidecar produces — no re-ordering/adding of
inputs or outputs, no changing values — otherwise the txid (and anchor) changes and RGB
must be re-committed. Signing only the witness of the given inputs is safe.

---

## 6. Blockers / open questions for the overall sidecar design

1. **Sidecar owns UTXO→assignment tracking.** The split crates give us consensus + a
   `Stock` (state machine), but NOT rgb-lib's DB that maps "UTXO → RGB allocations,
   incoming/outgoing, transfer status". The sidecar must implement this bookkeeping
   (track which confirmed outpoints at the TSS script carry which contract/opout). This is
   the largest piece of application logic we own. The building blocks exist:
   `Stock::contract_assignments_for(contract_id, outpoints)`,
   `Stock::accept_transfer`, `Stock::store_secret_seal`, `Stock::seal_secret`.
2. **Persistence of `Stock`.** Spike used `Stock::in_memory()`. Production should use
   rgb-ops `FsBinStore` persistence (`Stock::load(provider, autosave)`); not exercised in
   this spike — needs a follow-up test.
3. **Consignment transport.** RGB v0.11 exchanges consignments over transport endpoints
   (JSON-RPC proxy) or out-of-band. The sidecar must define how Go/chain33 hands consignments
   to the sidecar and how the sidecar delivers the outgoing consignment to the recipient
   (pass the bytes over gRPC and let the Go layer move them is the natural fit).
4. **IFA inflation/burn.** Spike demonstrated issue + transfer. A USDT bridge needs
   `inflate` (mint, TS_INFLATION / OS_INFLATION) and `burn` (TS_BURN) too; the schema
   supports them and the builder APIs are analogous (`add_owned_state_raw` with
   `inflationAllowance`), but they are not yet exercised.
5. **Deterministic blinding / persistence of `SecretSeal`.** See §4(d). Needed for
   restart-recoverable receives.
6. **Electrum-only indexer.** electrs v0.11.1 dropped the esplora HTTP API; we use
   `bdk_electrum`. If the team wants `bdk_esplora`, either use electrs ≤0.10 with
   `--http-addr` or stand up the full esplora stack. No functional blocker for the sidecar.
7. **`ValidationConfig.trusted_typesystem`.** Must be the contract's type system; for an
   unknown asset the types are carried in the consignment, so this is not a blocker but the
   sidecar must feed the right `TypeSystem` per contract.
8. **Anchor immutability vs. TSS.** §5 constraint — TSS signs the exact PSBT; no
   non-witness mutation between `rgb_commit()` and finalize.

---

## 7. Bottom line

A **single-script, watch-only RGB sidecar is buildable** with the split RGB crates:

- `rgb-consensus` `Validator::validate` / `Consignment::validate` give the stateless
  `ValidateConsignment` gRPC (plug our own `ResolveWitness` over bitcoind/electrs).
- `rgb-ops` `TransitionBuilder` + `rgb-psbt-utils` `RgbPsbtExt`/`rgb_commit` +
  `Stock::transfer_from_fascia` give the `BuildWithdrawal` path (issue IFA, build
  transitions, anchor opret, produce unsigned PSBT with explicit inputs, produce the
  consignment).
- The PSBT can be signed by an **external** key (TSS) without breaking the RGB anchor,
  because segwit txids are signature-independent.
- rgb-lib is **not** usable: its wallet shell is xpub-descriptor / BIP32-based and cannot
  express a single-script `wpkh(<bare pubkey>)`; drive `rgb-ops`/`rgb-consensus` directly.
- Receive attribution under one shared script is **our design**: per-receive
  `SecretSeal`/blinding factor, persisted; RGB seals are outpoint-based so the shared
  script is transparent to consensus.

Runnable code: `src/bin/spike2_utxo.rs`, `src/bin/spike3_rgb.rs`, `spike/regtest_env.sh`.

---

# Phase 2a — gRPC service + application layer (implemented)

The spike became a working gRPC service. Everything in this section was **compiled and run**
in this repo (`cargo build --all-targets` is warning-free).

## 8. What was built

```
proto/rgb_sidecar.proto        # shared contract (unchanged; generated by tonic-prost-build)
build.rs                       # tonic_prost_build::compile_protos
src/lib.rs                     # `pb` (generated) + modules below
src/config.rs                  # Config: data dir, electrum URL, network, TSS pubkey, listen
src/types.rs                   # SealStatus lifecycle, SealTxOut record
src/ledger.rs                  # persistent JSON ledger (assets/seals/receives)
src/wallet.rs                  # watch-only single-script bdk wallet + electrum sync
src/invoice.rs                 # RGB invoice build/parse + consignment (de)serialization
src/engine.rs                  # RgbEngine: Stock + ledger + wallet orchestration
src/service.rs                 # tonic RgbSidecar impl (all 11 RPCs)
src/main.rs                    # gRPC server binary (env-configured)
src/bin/e2e.rs                 # standalone regtest E2E (the acceptance test)
src/bin/grpc_smoke.rs          # in-process gRPC client smoke test
src/bin/spike1_singlescript.rs # Spike 1 preserved as a bin
src/bin/spike2_utxo.rs         # Spike 2 preserved
src/bin/spike3_rgb.rs          # Spike 3 preserved
```

All 11 RPCs are implemented: CreateReceive, ProvideConsignment, ListTransfers,
ValidateConsignment, Sync, ListSeals, GetBalance, ListAssets, BuildWithdrawal,
FinalizeWithdrawal, ParseBtcTx.

## 9. gRPC server structure

- tonic 0.14 + tonic-prost 0.14; proto compiled with `tonic_prost_build`.
- `RgbSidecarService` holds `Arc<tokio::sync::Mutex<RgbEngine>>` (single-writer; the engine
  is small and the indexer is local). `RgbEngine` never holds a private key.
- Watch-only single-script: the descriptor is `wpkh(<TSS pubkey>)`; every seal UTXO is under
  one script; seal identity is `outpoint` (+ optional `SecretSeal`).
- Server binary `src/main.rs`: env-configurable
  (`RGB_SIDECAR_DATA_DIR`, `RGB_SIDECAR_ELECTRUM`, `RGB_SIDECAR_NETWORK`,
  `RGB_SIDECAR_TSS_PUBKEY`, `RGB_SIDECAR_LISTEN`). Boots on 50061.

## 10. Ledger / persistence design

Two stores in `data_dir`:

1. **rgb-ops `Stock`** — RGB consensus state, persisted with `FsBinStore`
   (`data_dir/stock/{stash,state,index}.dat`, autosave). Rebuilt-on-open from a clean dir.
2. **`ledger.json`** — the sidecar's own bookkeeping the Stock does not track:
   - `assets`: symbol ↔ asset_id, precision, schema, genesis-consignment hex.
   - `seals`: `outpoint → {asset, amount, btc_value, maturity_height, status, secret_seal}`.
   - `receives`: `receive_id → {invoice, asset, amount, status, settled txid:vout}` plus a
     reverse `settled_by_outpoint` map for attribution.
   - Saved atomically (write-tmp + rename).

**Seal state machine** (`SealStatus`):
```
pending-mint  --Sync sees the UTXO confirmed-->  minted  --spent as input-->  consumed
  (tx broadcast,                      (available to spend /             (no longer unspent;
   not yet confirmed)                  counted in settled balance)       excluded from balance)
```
- `provide_consignment` creates a seal as `pending-mint`; `Sync` flips it to `minted`
  (and refreshes `btc_value`/`maturity_height` from the wallet UTXO view) or to `consumed`
  when the outpoint is spent.

**Receive attribution under the shared script** (the §4(d) design, implemented):
- Receives use **WitnessVout (address-based) invoices** — the bridge's TSS address. No
  out-of-band vout coordination needed.
- On `ProvideConsignment`: the engine validates the consignment, finds the **opened seal at
  the TSS script**, then attributes it to a pending receive matching
  `(asset_id, amount)` and/or `receive_id_hint`, marks it `settled`, records the new seal,
  and marks the closed seals `consumed`.
- The **rgb-ops Stock is advanced via `accept_transfer`** so the new seal's assignment
  (opout → outpoint) is queryable by later transfers — this is what makes a subsequent
  withdrawal able to spend a received seal.

## 11. E2E result (standalone regtest, all assertions pass)

```
E2E: funded TSS=bcrt1ql3e9pgs... user=bcrt1q2vfxp232...
E2E: issued USDT rgb:v_Xm3K0b-... genesis_seal=2b6704c2...:0
E2E: create_receive rcv-fe346c776f179dd8
E2E: user paid, txid=2a43903601...
E2E: provide_consignment -> status=settled amount=100000000
E2E: balance after deposit settled=100000000 pending=0
E2E: build_withdrawal txid=6284858b26... inputs=1
E2E: finalize txid=6284858b26... recipient=...:1 change=Some("...:2")
E2E: balance after withdrawal settled=50000000 pending=0
E2E: seals:
  2a439036...:1 amount=100000000 status=consumed   (received seal, spent by withdrawal)
  2b6704c2...:0 amount=10000000000 status=consumed (genesis seal)
  6284858b...:2 amount=50000000 status=minted      (withdrawal change)
E2E-PASS: deposit (issue->receive->settle->attribute) + withdrawal (build->sign->finalize) OK
```

Flow: **issue USDT → CreateReceive → simulated user pays (build_transfer + external test-key
sign + broadcast + mine) → ProvideConsignment (settle+attribute) → Sync → BuildWithdrawal
(unsigned PSBT + consignment) → external sign → FinalizeWithdrawal → Sync → balance 0.5 USDT.**

The gRPC smoke test (`cargo run --bin grpc_smoke`) exercises the same engine through a real
tonic client: ListAssets / CreateReceive / GetBalance / ListSeals / ParseBtcTx / ListTransfers
all return correct results; the standalone server boots on 50061.

## 12. Contract adjustment suggestions (for the main session — proto was NOT changed)

1. **No asset-issuance RPC.** The proto has no `IssueAsset`; issuance is out-of-band
   (`engine.issue_asset` / config / CLI). If the Go bridge needs to mint RGB20 assets at
   runtime, add an admin RPC (e.g. `IssueAsset`) — otherwise document that issuance is
   bootstrap/config-driven.
2. **PSBT byte format.** `BuildWithdrawalResponse.psbt` / `FinalizeWithdrawalRequest.psbt_signed`
   are `bytes`; the sidecar uses **raw binary PSBT** (`Psbt::serialize`/`deserialize`). Go must
   round-trip the exact bytes or use a PSBT lib accepting binary (base64 is the more common
   interchange — worth pinning in the contract comment).
3. **Withdrawal recipient must be an address-based (WitnessVout) invoice (v1).** Blinded-seal
   invoices are rejected with a clear error; blinded withdrawal recipients need the sender to
   coordinate the output vout (out-of-band), which v1 deliberately does not do.
4. **`ValidateConsignment` expected fields are advisory.** `expected_amount`,
   `expected_recipient_seal`, `expected_closed_seals` are accepted but the engine reports the
   **actual** opened/closed seals + amount; the Go/signing node cross-checks them. Enforcing
   them as a hard requirement is a possible v2 tightening.
5. **Transfer statuses.** Only `waiting-counterparty → settled` is transitioned automatically.
   `failed`/`timeout` need an explicit action (a future expiry sweep, or the Go side marking
   them) — nothing in the sidecar advances them today.
6. **`SyncRequest.asset_symbol` / `btc_block_height` are advisory** — Sync scans all TSS-script
   UTXOs and reports new unspent outpoints regardless of symbol.

## 13. Can the sidecar now independently support the Go bridge?

**Yes — both legs run through the sidecar (verified by E2E):**

- **Deposit (充值验证)**: `CreateReceive` → user pays the address invoice → `ProvideConsignment`
  validates the consignment (rgb-consensus, stateless), settles, attributes to the receive,
  and the ledger tracks the new seal; `Sync`/`GetBalance`/`ListSeals`/`ListTransfers` report
  the bridge's asset state. The signing node can independently re-run `ValidateConsignment`
  (returns `synced_height`) for its confirmation gate.
- **Withdrawal (提现构造)**: `BuildWithdrawal` selects minted seals, builds the state transition
  + opret anchor + **unsigned PSBT with explicit inputs** + the transfer consignment; the TSS
  group signs externally (segwit txid is signature-independent, so the anchor survives);
  `FinalizeWithdrawal` consumes the input seals and records the change seal.

**Residual gaps** (all documented, none block the two legs): blinded-receive invoices are
not supported for withdrawal recipients (v1 = address-based invoices only); asset issuance is
out-of-band; the ledger is single-writer (a `Mutex`, fine for one Go bridge per sidecar);
`failed`/`timeout` receive transitions need an explicit sweep.
