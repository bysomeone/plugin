//! Watch-only Bitcoin wallet backed by the btcd JSON-RPC node.
//!
//! The main pool is the descriptor `wpkh(<TSS compressed pubkey>)` — zero BIP32 derivation, every
//! index resolves to the same P2WPKH script. On top of it the wallet keeps a registry of the
//! *per-user P2WSH deposit scripts* the bridge has handed out (C2): those UTXOs are spendable by
//! the same TSS group key (the script's `<pubkey>` is the group key) but they hang off their own
//! script, so each one needs its own `witnessScript` (the BIP143 scriptCode) to be signed.
//! The wallet holds NO private keys; it discovers UTXOs by querying btcd's
//! `searchrawtransactions` (address index) + `gettxout`.

use std::collections::HashMap;
use std::path::PathBuf;
use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::opcodes::all::{OP_CHECKSIG, OP_DROP};
use bitcoin::script::{Builder, PushBytesBuf};
use bitcoin::{Address, CompressedPublicKey, Network, OutPoint, ScriptBuf};
use serde::{Deserialize, Serialize};

use crate::rpc::BtcdRpc;

/// 派生 `witnessScript = <push userID> OP_DROP <push tssPub> OP_CHECKSIG`（规格 §1.2 冻结形态）。
///
/// 与 Go 侧 `rgbx/types/p2wsh_deposit.go` 的那份实现必须**逐字节相同**（同一组冻结向量
/// `types/testdata/p2wsh_deposit_vectors.json` 是两边共同的判据）。push 必须是最小 push：
/// `Builder::push_slice` 对 ≤75 字节走 `OP_PUSHBYTES_n` —— 手工拼长度前缀一旦拼错，地址会静默
/// 指向一个没人能签的脚本。
///
/// userID 只影响 program（脚本自推后立刻 `OP_DROP`），花费时的 witness 栈只需
/// `[sig||sighashType, witnessScript]`，不需要花费方提供 userID。
pub fn deposit_witness_script(user_id: &str, tss_pubkey: &CompressedPublicKey) -> Result<ScriptBuf> {
    if user_id.is_empty() {
        return Err(anyhow!("deposit witness script: empty user id"));
    }
    // 与 Go 侧 `DeriveDepositWitnessScript` 同界：>75 字节的 push 不再是单字节长度前缀的最小 push，
    // 两边对"超长 userID"必须同样拒绝（否则一方产出的脚本另一方重建不出来）。
    if user_id.len() > 75 {
        return Err(anyhow!(
            "deposit witness script: user id is {} bytes (max 75)",
            user_id.len()
        ));
    }
    // push_slice 只接受 ≤75 字节的 PushBytes（正是上面那条界）；它负责最小 push 编码，
    // 手写长度前缀是被规格明令禁止的（错一个字节 = 地址静默指向无人能签的脚本）。
    let user_push = PushBytesBuf::try_from(user_id.as_bytes().to_vec())
        .map_err(|e| anyhow!("deposit witness script: user id push: {e}"))?;
    Ok(Builder::new()
        .push_slice(user_push)
        .push_opcode(OP_DROP)
        .push_slice(tss_pubkey.to_bytes())
        .push_opcode(OP_CHECKSIG)
        .into_script())
}

/// Test-only: derive the E2E "user" P2WPKH address from a fixed test secret ([0x22;32]).
/// Used by the test-sim driver to build user-side RGB invoices; the sidecar still holds NO keys.
pub fn user_test_address(network: Network) -> Result<Address> {
    use bitcoin::secp256k1::Secp256k1;
    let secp = Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(&[0x22u8; 32])?;
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    let compressed = CompressedPublicKey(pk);
    Ok(Address::p2wpkh(&compressed, network))
}

/// A confirmed/unconfirmed UTXO of a watched script.
#[derive(Clone, Debug)]
pub struct WalletUtxo {
    pub outpoint: OutPoint,
    pub value: u64,
    pub script_pubkey: ScriptBuf,
    pub height: Option<u32>,
}

/// A user's per-user P2WSH deposit script, as handed out by the bridge (`rgbx/types/p2wsh_deposit.go`).
///
/// Native P2WSH keeps the witnessScript **off-chain**: the chain only ever shows the 34-byte
/// `OP_0 <sha256(witnessScript)>` program, and the script itself only shows up in the witness of
/// the tx that *spends* it. So the original script has to be re-derived from `(userID, tssPub)`
/// and kept here — without it the UTXO cannot be spent (there is no scriptCode to sign against).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct UserDepositScript {
    /// The chain33 address string that is the derivation's `userID` (the `OP_DROP` label in the script).
    pub user_id: String,
    /// `<push userID> OP_DROP <push tssPub> OP_CHECKSIG`, byte for byte as the Go side derives it.
    pub witness_script: ScriptBuf,
}

/// Persisted form of the registry (hex strings: `ScriptBuf` has no serde impl without the
/// `serde` feature, and the on-disk form should be readable without knowing rust-bitcoin).
#[derive(Serialize, Deserialize, Default)]
struct UserScriptsFile {
    scripts: Vec<UserScriptRec>,
}

#[derive(Serialize, Deserialize)]
struct UserScriptRec {
    user_id: String,
    pk_script: String,
    witness_script: String,
}

pub struct BtcWallet {
    rpc: Arc<BtcdRpc>,
    address: Address,
    script: ScriptBuf,
    /// 该 symbol 的 TSS 群公钥（33 字节压缩）。用户充值脚本必须由它派生 —— 见
    /// [`deposit_witness_script`]，登记时用它把桥下发的条目重新推导一遍再比对。
    tss_pubkey: CompressedPublicKey,
    /// programme(pkScript) → 该用户充值脚本的原文与 userID（主池之外的 watch 集）。
    user_scripts: HashMap<ScriptBuf, UserDepositScript>,
    /// 注册表落盘路径（`None` = 纯内存，测试用）。注册**必须**持久化：重启后 watch 集一丢，
    /// 已经打进用户 P2WSH 的 BTC 就变成"看得见但花不掉"（witnessScript 只能从 (userID, tssPub)
    /// 重新派生，而 userID 只在桥的注册表里）。
    user_scripts_path: Option<PathBuf>,
}

impl BtcWallet {
    pub fn new(rpc: Arc<BtcdRpc>, tss_pubkey_hex: &str, network: Network) -> Result<Self> {
        // chain33 getCrossChainInfo returns pubkey with a "0x" prefix; strip it before hex parsing.
        let hex = tss_pubkey_hex.strip_prefix("0x").unwrap_or(tss_pubkey_hex);
        let pubkey = CompressedPublicKey::from_str(hex)
            .map_err(|e| anyhow!("invalid TSS pubkey {tss_pubkey_hex}: {e}"))?;
        let address = Address::p2wpkh(&pubkey, network);
        let script = ScriptBuf::from(address.script_pubkey());
        Ok(Self {
            rpc,
            address,
            script,
            tss_pubkey: pubkey,
            user_scripts: HashMap::new(),
            user_scripts_path: None,
        })
    }

    /// 该 symbol 的 TSS 群公钥（33 字节压缩）。
    pub fn tss_pubkey(&self) -> &CompressedPublicKey {
        &self.tss_pubkey
    }

    /// 按 `(userID, 本侧车的 tssPub)` 重新派生某个 userID 的充值脚本原文。
    pub fn derive_user_witness_script(&self, user_id: &str) -> Result<ScriptBuf> {
        deposit_witness_script(user_id, &self.tss_pubkey)
    }

    /// 指定用户充值脚本注册表的落盘路径（`data_dir/user_scripts.json`）。
    pub fn with_user_scripts_path(mut self, path: PathBuf) -> Self {
        self.user_scripts_path = Some(path);
        self
    }

    pub fn address(&self) -> &Address {
        &self.address
    }

    pub fn script(&self) -> &ScriptBuf {
        &self.script
    }

    /// btcd is queried live on `list_unspent`, so there is nothing to sync. Kept for API parity.
    pub fn sync(&mut self) -> Result<()> {
        Ok(())
    }

    // ------------------------------------------------------------------
    // 用户 P2WSH 充值脚本注册表
    // ------------------------------------------------------------------

    /// 登记一个用户的 P2WSH 充值脚本（幂等）。`pk_script` 必须**正好**是 `witness_script` 的
    /// P2WSH program —— 这条绑定校验是必须的：witnessScript 是 BIP143 的 scriptCode，一份
    /// 与 prevout 对不上的 witnessScript 会让签名者对着一个不属于这笔 UTXO 的脚本出签名
    /// （Go 侧 `resolvePsbtInputScriptCode` 有同一条校验，两边都不能少）。
    pub fn register_user_script(
        &mut self,
        pk_script: ScriptBuf,
        user_id: String,
        witness_script: ScriptBuf,
    ) -> Result<()> {
        if user_id.is_empty() {
            return Err(anyhow!("user deposit script: empty user id"));
        }
        if pk_script != ScriptBuf::new_p2wsh(&witness_script.wscript_hash()) {
            return Err(anyhow!(
                "user deposit script for {user_id}: pkScript {} is not the p2wsh program of the given witnessScript",
                hex_of(pk_script.as_bytes())
            ));
        }
        let entry = UserDepositScript {
            user_id,
            witness_script,
        };
        if self.user_scripts.get(&pk_script) == Some(&entry) {
            return Ok(()); // 幂等：同一条目不重复落盘
        }
        self.user_scripts.insert(pk_script, entry);
        self.save_user_scripts()
    }

    /// 登记桥下发的用户充值脚本条目：**不采信**下发的 witnessScript（根本没有这个字段），
    /// 而是按 `(user_id, 本侧车的 tssPub)` 自己重新派生，并要求它的 program 与桥给的
    /// `pk_script` 逐字节一致。
    ///
    /// 为什么必须核对而不是照收：桥与侧车各自持有一份群公钥（桥取运行时 `tssPublicKey`，侧车取
    /// 配置），两者不一致时桥发的是**另一个群**的地址 —— 侧车把它当"自己的充值脚本"收下，就会
    /// 对着一笔自己无权花费的 UTXO 出签名（签出来的必然无效），而故障要到提现时才暴露。这里
    /// 直接对不上就报错，把不一致顶到登记那一刻。
    ///
    /// 返回 `true` = 本次新增，`false` = 已在集合里（幂等）。
    pub fn register_deposit_script(&mut self, user_id: &str, pk_script: &ScriptBuf) -> Result<bool> {
        let witness_script = self.derive_user_witness_script(user_id)?;
        let expected = ScriptBuf::new_p2wsh(&witness_script.wscript_hash());
        if expected != *pk_script {
            return Err(anyhow!(
                "deposit script for {user_id}: bridge pkScript {} is not the p2wsh program of the script \
                 this sidecar derives from (userID, its own tss pubkey {}); the two derivations disagree \
                 (different group key, or the frozen template drifted on one side)",
                hex_of(pk_script.as_bytes()),
                hex_of(&self.tss_pubkey.to_bytes())
            ));
        }
        if self.user_scripts.contains_key(pk_script) {
            return Ok(false);
        }
        self.register_user_script(pk_script.clone(), user_id.to_string(), witness_script)?;
        Ok(true)
    }

    /// 该脚本的 witnessScript（= BIP143 scriptCode）。非注册的用户脚本返回 `None`
    /// —— 调用方据此 fail-closed，绝不拿 prevout 的 output program 去顶替。
    pub fn witness_script_for(&self, script: &ScriptBuf) -> Option<ScriptBuf> {
        self.user_scripts.get(script).map(|e| e.witness_script.clone())
    }

    /// 该脚本对应的 userID（充值归因/对账用）。非注册的用户脚本返回 `None`。
    pub fn user_id_for(&self, script: &ScriptBuf) -> Option<String> {
        self.user_scripts.get(script).map(|e| e.user_id.clone())
    }

    /// 该脚本是否在 watch 集里（主池脚本 或 已登记的用户充值脚本）。
    pub fn is_watched(&self, script: &ScriptBuf) -> bool {
        script == &self.script || self.user_scripts.contains_key(script)
    }

    /// 已登记的用户充值脚本（program → 条目）。C4 的扫集/对账与未来的注册 RPC 用。
    pub fn user_scripts(&self) -> Vec<(ScriptBuf, UserDepositScript)> {
        let mut out: Vec<_> = self
            .user_scripts
            .iter()
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect();
        out.sort_by(|a, b| a.1.user_id.cmp(&b.1.user_id));
        out
    }

    /// 从落盘文件载入注册表（不存在视为空；损坏/单条非法一律报错而不静默丢弃 —— watch 集少一条
    /// 就等于那笔充值花不掉，静默降级会让故障延后到"提现时才炸"）。
    pub fn load_user_scripts(&mut self) -> Result<()> {
        let Some(path) = self.user_scripts_path.clone() else {
            return Ok(());
        };
        if !path.exists() {
            return Ok(());
        }
        let raw = std::fs::read_to_string(&path)?;
        let file: UserScriptsFile = serde_json::from_str(&raw)
            .map_err(|e| anyhow!("user scripts {}: {e}", path.display()))?;
        for rec in file.scripts {
            let pk_script = ScriptBuf::from_bytes(decode_hex(&rec.pk_script)?);
            let witness_script = ScriptBuf::from_bytes(decode_hex(&rec.witness_script)?);
            if pk_script != ScriptBuf::new_p2wsh(&witness_script.wscript_hash()) {
                return Err(anyhow!(
                    "user scripts {}: entry for {} does not bind (pkScript != p2wsh(witnessScript))",
                    path.display(),
                    rec.user_id
                ));
            }
            self.user_scripts.insert(
                pk_script,
                UserDepositScript {
                    user_id: rec.user_id,
                    witness_script,
                },
            );
        }
        Ok(())
    }

    fn save_user_scripts(&self) -> Result<()> {
        let Some(path) = self.user_scripts_path.as_ref() else {
            return Ok(());
        };
        let file = UserScriptsFile {
            scripts: self
                .user_scripts
                .iter()
                .map(|(pk, e)| UserScriptRec {
                    user_id: e.user_id.clone(),
                    pk_script: hex_of(pk.as_bytes()),
                    witness_script: hex_of(e.witness_script.as_bytes()),
                })
                .collect(),
        };
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        std::fs::write(path, serde_json::to_vec_pretty(&file)?)
            .map_err(|e| anyhow!("write user scripts {}: {e}", path.display()))
    }

    // ------------------------------------------------------------------
    // UTXO 发现
    // ------------------------------------------------------------------

    /// 主池（TSS P2WPKH）的 UTXO 集。**语义不变**：seal 生命周期推导（`RgbEngine::sync`）、
    /// 创世 seal 选择、`list_unspent_btc` 都只关心主池，用户 P2WSH 的余额不是 seal。
    pub fn list_unspent(&self) -> Vec<WalletUtxo> {
        self.list_unspent_for(&self.script)
    }

    /// 全部 watch 脚本的 UTXO 集（主池 ∪ 已登记的用户充值脚本）。
    ///
    /// 构造交易时用它来解析**每个输入自己的脚本与面额**：用户 P2WSH 的充值 UTXO 可能被当作
    /// RGB seal 花掉（C4 的扫集），那时输入脚本不再是主池脚本，PSBT 必须带上它自己的
    /// witnessScript。代价是每多一个注册脚本就多一次 `searchrawtransactions`
    /// （逐脚本 RPC = O(N)，见规格 §3.2；主路径应改成逐块扫输出，属 C4）。
    pub fn list_unspent_all(&self) -> Vec<WalletUtxo> {
        let mut out = self.list_unspent();
        for script in self.user_scripts.keys() {
            out.extend(self.list_unspent_for(script));
        }
        out
    }

    /// **只有用户 P2WSH 充值脚本**的 UTXO 集（扫集器的输入候选）。
    ///
    /// 与 `list_unspent_all` 分开：扫集要把这些 UTXO 归集回主池，不能把主池自己的 UTXO 当成
    /// 待归集对象（那笔交易等于自己花自己，白付手续费）。
    pub fn list_unspent_user_deposits(&self) -> Vec<WalletUtxo> {
        let mut out = Vec::new();
        for script in self.user_scripts.keys() {
            out.extend(self.list_unspent_for(script));
        }
        out
    }

    /// 单个脚本的 UTXO 集，通过 btcd 的地址索引发现。
    pub fn list_unspent_for(&self, script: &ScriptBuf) -> Vec<WalletUtxo> {
        let Ok(txs) = self.rpc.search_txs_for_script(script) else {
            return Vec::new();
        };
        let best = self.rpc.get_block_count().unwrap_or(0);
        let mut out = Vec::new();
        for tx in txs {
            for (vout, txout) in tx.outputs {
                let outpoint = OutPoint {
                    txid: tx.txid,
                    vout,
                };
                // Still unspent?
                let Ok(Some(live)) = self.rpc.get_txout(&outpoint) else {
                    continue;
                };
                let height = if live.confirmations > 0 {
                    // gettxout confirmations is relative to the chain tip at query time.
                    Some((best.saturating_sub(live.confirmations - 1)) as u32)
                } else {
                    None
                };
                out.push(WalletUtxo {
                    outpoint,
                    value: live.value,
                    script_pubkey: txout.script_pubkey.clone(),
                    height,
                });
            }
        }
        out
    }

}

fn hex_of(bytes: &[u8]) -> String {
    bitcoin::hex::DisplayHex::to_hex_string(bytes, bitcoin::hex::Case::Lower)
}

fn decode_hex(s: &str) -> Result<Vec<u8>> {
    use bitcoin::hashes::hex::FromHex;
    Vec::<u8>::from_hex(s).map_err(|e| anyhow!("bad hex: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 一个与真实充值脚本同构的 (pkScript, witnessScript)：
    /// `<push userID> OP_DROP <push tssPub> OP_CHECKSIG`，program = sha256(witnessScript)。
    fn deposit_scripts(seed: u8) -> (ScriptBuf, ScriptBuf) {
        let mut ws = vec![0x22u8];
        ws.extend(b"1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u");
        ws.push(0x75); // OP_DROP
        let mut pk = vec![0x02u8];
        pk.extend(std::iter::repeat_n(seed, 32));
        ws.push(pk.len() as u8);
        ws.extend(&pk);
        ws.push(0xac); // OP_CHECKSIG
        let witness_script = ScriptBuf::from_bytes(ws);
        let pk_script = ScriptBuf::new_p2wsh(&witness_script.wscript_hash());
        (pk_script, witness_script)
    }

    /// 不联网的 BtcWallet（btcd agent 是惰性的；任何 RPC 调用失败时 list_unspent 返回空）。
    fn test_wallet_with(pubkey_hex: &str, user_scripts_path: Option<PathBuf>) -> BtcWallet {
        let rpc = Arc::new(
            BtcdRpc::connect("127.0.0.1:1", "", "", None, Network::Regtest).unwrap(),
        );
        let w = BtcWallet::new(rpc, pubkey_hex, Network::Regtest).unwrap();
        match user_scripts_path {
            Some(p) => w.with_user_scripts_path(p),
            None => w,
        }
    }

    fn test_wallet(user_scripts_path: Option<PathBuf>) -> BtcWallet {
        let hex = bitcoin::hex::DisplayHex::to_hex_string(
            &[0x02u8; 33],
            bitcoin::hex::Case::Lower,
        );
        test_wallet_with(&hex, user_scripts_path)
    }

    /// 冻结向量（`plugin/dapp/rgbx/types/testdata/p2wsh_deposit_vectors.json` 的主向量，
    /// 由 `rgbx/types/p2wsh_deposit_test.go` 守着 Go 侧那一份）。三份实现（Go 桥 / Go 执行器 /
    /// Rust 侧车）必须对同一组输入产出同一串字节，这是唯一能防"三份实现漂移"的手段 —— 谁把
    /// 模板改坏了，这里就红。
    const VECTOR_USER_ID: &str = "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u";
    const VECTOR_TSS_PUB: &str = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798";
    const VECTOR_WITNESS_SCRIPT: &str = "2231416d525963555266444778426869614a4176454764526b766b6f4d377a746e317575210279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798ac";
    const VECTOR_PK_SCRIPT: &str =
        "002078d9392c30b67443d288c548adde74e0eff45454048cf3e7cc2f82f43da70465";

    fn vector_pubkey() -> CompressedPublicKey {
        CompressedPublicKey::from_str(VECTOR_TSS_PUB).unwrap()
    }

    #[test]
    fn derivation_matches_the_frozen_vector() {
        let ws = deposit_witness_script(VECTOR_USER_ID, &vector_pubkey()).unwrap();
        assert_eq!(hex_of(ws.as_bytes()), VECTOR_WITNESS_SCRIPT, "witnessScript 漂移了");
        assert_eq!(
            hex_of(ScriptBuf::new_p2wsh(&ws.wscript_hash()).as_bytes()),
            VECTOR_PK_SCRIPT,
            "program(pkScript) 漂移了"
        );
        // 形态自证：最小 push 的 34 字节 userID（0x22 = OP_PUSHBYTES_34）+ OP_DROP + 33 字节
        // 公钥（0x21）+ OP_CHECKSIG（0xac）。手工拼前缀拼错就会在这里露出来。
        let b = ws.as_bytes();
        assert_eq!(b[0], 0x22);
        assert_eq!(b[35], 0x75);
        assert_eq!(b[36], 0x21);
        assert_eq!(b[70], 0xac);
        assert_eq!(b.len(), 71);
    }

    #[test]
    fn derivation_rejects_an_empty_or_oversized_user_id() {
        assert!(deposit_witness_script("", &vector_pubkey()).is_err());
        // 76 字节：不再是单字节长度前缀的最小 push，与 Go 侧同界拒绝。
        assert!(deposit_witness_script(&"a".repeat(76), &vector_pubkey()).is_err());
        assert!(deposit_witness_script(&"a".repeat(75), &vector_pubkey()).is_ok());
    }

    #[test]
    fn register_deposit_script_derives_and_verifies() {
        // 侧车配的就是冻结向量里那把群公钥（真实部署里两边的群公钥必须一致，见下一条测试）。
        let mut w = test_wallet_with(VECTOR_TSS_PUB, None);
        let pk_script = ScriptBuf::from_bytes(decode_hex(VECTOR_PK_SCRIPT).unwrap());
        assert!(w.register_deposit_script(VECTOR_USER_ID, &pk_script).unwrap(), "首次登记 = 新增");
        assert_eq!(
            hex_of(w.witness_script_for(&pk_script).unwrap().as_bytes()),
            VECTOR_WITNESS_SCRIPT,
            "登记的原文必须是自己派生出来的那一份"
        );
        assert_eq!(w.user_id_for(&pk_script).as_deref(), Some(VECTOR_USER_ID));
        // 幂等：重复登记不新增、不报错。
        assert!(!w.register_deposit_script(VECTOR_USER_ID, &pk_script).unwrap());
        assert_eq!(w.user_scripts().len(), 1);
    }

    #[test]
    fn register_deposit_script_refuses_a_program_from_another_group_key() {
        let mut w = test_wallet_with(VECTOR_TSS_PUB, None);
        // 另一个群公钥派出来的 program（这里直接换一个 userID 派生，效果同"桥与侧车的群公钥不一致"）：
        let other = ScriptBuf::new_p2wsh(
            &deposit_witness_script("1AnotherUserAddressForTheSameGroupKey", &vector_pubkey())
                .unwrap()
                .wscript_hash(),
        );
        let err = w.register_deposit_script(VECTOR_USER_ID, &other).unwrap_err();
        assert!(format!("{err}").contains("the two derivations disagree"), "{err}");
        assert!(w.user_scripts().is_empty(), "对不上的条目一律不落地");
    }

    #[test]
    fn register_and_lookup() {
        let mut w = test_wallet(None);
        let (pk_script, ws) = deposit_scripts(2);
        assert!(w.witness_script_for(&pk_script).is_none(), "未登记时查不到");
        assert!(!w.is_watched(&pk_script));
        assert!(w.is_watched(&w.script().clone()), "主池脚本始终在 watch 集里");

        w.register_user_script(pk_script.clone(), "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u".into(), ws.clone())
            .unwrap();
        assert_eq!(w.witness_script_for(&pk_script), Some(ws.clone()));
        assert!(w.is_watched(&pk_script));
        assert_eq!(w.user_scripts().len(), 1);
        assert_eq!(w.user_scripts()[0].1.user_id, "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u");

        // 幂等：重复登记不增条目、不报错。
        w.register_user_script(pk_script.clone(), "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u".into(), ws)
            .unwrap();
        assert_eq!(w.user_scripts().len(), 1);
    }

    #[test]
    fn register_rejects_a_witness_script_that_is_not_the_program_preimage() {
        let mut w = test_wallet(None);
        let (pk_a, _ws_a) = deposit_scripts(2);
        let (_pk_b, ws_b) = deposit_scripts(3);
        // B 的 witnessScript 配 A 的 program：绑定不成立 ⇒ 必须拒（签名者会对着不属于这笔
        // UTXO 的脚本出签名）。
        let err = w
            .register_user_script(pk_a, "user-a".into(), ws_b)
            .unwrap_err();
        assert!(format!("{err}").contains("not the p2wsh program"), "{err}");
        assert!(w.user_scripts().is_empty());
    }

    #[test]
    fn register_rejects_empty_user_id() {
        let mut w = test_wallet(None);
        let (pk, ws) = deposit_scripts(2);
        assert!(w.register_user_script(pk, String::new(), ws).is_err());
    }

    #[test]
    fn registry_survives_a_restart() {
        let dir = std::env::temp_dir().join(format!("rgb-sidecar-wallet-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("user_scripts.json");
        let (pk_script, ws) = deposit_scripts(2);

        {
            let mut w = test_wallet(Some(path.clone()));
            w.register_user_script(
                pk_script.clone(),
                "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u".into(),
                ws.clone(),
            )
            .unwrap();
        }
        // 重启：新实例从同一份文件载入，witnessScript 必须逐字节回来（否则那笔充值花不掉）。
        let mut reloaded = test_wallet(Some(path));
        reloaded.load_user_scripts().unwrap();
        assert_eq!(reloaded.witness_script_for(&pk_script), Some(ws));

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn load_fails_closed_on_a_corrupted_entry() {
        let dir = std::env::temp_dir().join(format!("rgb-sidecar-wallet-bad-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("user_scripts.json");
        let (pk_script, _) = deposit_scripts(2);
        let (_other_pk, other_ws) = deposit_scripts(3);
        // 手工写一条"pkScript 与 witnessScript 不绑定"的记录（文件被改坏/被误写）。
        let bad = serde_json::json!({
            "scripts": [{
                "user_id": "user-a",
                "pk_script": bitcoin::hex::DisplayHex::to_hex_string(pk_script.as_bytes(), bitcoin::hex::Case::Lower),
                "witness_script": bitcoin::hex::DisplayHex::to_hex_string(other_ws.as_bytes(), bitcoin::hex::Case::Lower),
            }]
        });
        std::fs::write(&path, serde_json::to_vec(&bad).unwrap()).unwrap();

        let mut w = test_wallet(Some(path));
        let err = w.load_user_scripts().unwrap_err();
        assert!(format!("{err}").contains("does not bind"), "{err}");
        // 宁可启动失败，也不带着半份 watch 集跑（那会让充值无声地花不掉）。
        assert!(w.user_scripts().is_empty());

        let _ = std::fs::remove_dir_all(&dir);
    }
}
