package neutrino

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightninglabs/neutrino"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

/*
 * C2（桥侧 P2WSH 配套）的测试。
 *
 * 派生向量与 C1 **同源**：直接读 plugin/dapp/rgbx/types/testdata/p2wsh_deposit_vectors.json
 * （C1 的冻结向量，Go 桥/Go 合约/Rust 侧车共用）。桥侧不许另造一组向量 —— 否则"三方一致"
 * 这件事就没有共同的锚了。
 */

const depositVectorsPath = "../../../../rgbx/types/testdata/p2wsh_deposit_vectors.json"

type depositVector struct {
	Name          string            `json:"name"`
	UserID        string            `json:"userID"`
	TssPubKey     string            `json:"tssPubKey"`
	WitnessScript string            `json:"witnessScript"`
	Program       string            `json:"programSHA256"`
	PkScript      string            `json:"pkScript"`
	Addresses     map[string]string `json:"addresses"`
}

func loadDepositVectors(t *testing.T) []depositVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(depositVectorsPath))
	require.NoError(t, err, "读 C1 的冻结向量失败（路径变了就要同步改这里）")
	var doc struct {
		Spec    string          `json:"spec"`
		Vectors []depositVector `json:"vectors"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, rtypes.P2WSHDepositSpecV1, doc.Spec, "向量文件的规格标签必须与链上实现一致")
	require.NotEmpty(t, doc.Vectors)
	return doc.Vectors
}

func vectorPub(t *testing.T, v depositVector) []byte {
	t.Helper()
	pub, err := hex.DecodeString(v.TssPubKey)
	require.NoError(t, err)
	return pub
}

// ---- 阶段 2a：按需 import + 地址与冻结向量同源 ----

func newTestWallet(t *testing.T, pub *btcec.PublicKey, cfg config) (*btcWallet, walletdb.DB, walletdb.DB) {
	t.Helper()
	params := chaincfg.RegressionNetParams
	dir := t.TempDir()
	_, walletDB, err := openWalletDB(dir, "btcwallet.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = walletDB.Close() })
	pubPass := []byte("hello")
	require.NoError(t, wallet.CreateWatchingOnly(walletDB, pubPass, &params, time.Now()))
	w, err := wallet.Open(walletDB, pubPass, nil, &params, 0)
	require.NoError(t, err)
	_, neutrinoDB, err := openWalletDB(dir, "neutrino.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = neutrinoDB.Close() })

	b := &btcWallet{
		Wallet:         w,
		db:             walletDB,
		chainParams:    params,
		depositScripts: newDepositScriptSet(),
		client: &neutrinoClient{
			tss:         &tssService{tssPublicKey: pub},
			cfg:         cfg,
			neutrinoCfg: neutrino.Config{Database: neutrinoDB},
		},
	}
	return b, walletDB, neutrinoDB
}

// TestEnsureUserDepositScript_matchesFrozenVector 桥发放的地址必须逐字节等于 C1 冻结向量里
// 同一 (userID, tssPub) 的 regtest 地址，且钱包真的把它导入并订阅了。
func TestEnsureUserDepositScript_matchesFrozenVector(t *testing.T) {
	vectors := loadDepositVectors(t)
	v := vectors[0]
	pub, err := btcec.ParsePubKey(vectorPub(t, v))
	require.NoError(t, err)

	var notified []btcutil.Address
	b, _, _ := newTestWallet(t, pub, config{})
	b.notifyFn = func(addrs []btcutil.Address) error {
		notified = append(notified, addrs...)
		return nil
	}

	addr, err := b.ensureUserDepositScript(v.UserID)
	require.NoError(t, err)
	assert.Equal(t, v.Addresses["regtest"], addr, "桥发放的地址必须与冻结向量一致（与 C1 同源）")

	// 钱包确实"看见"了这条脚本：地址管理器里有它，且它的 pkScript 就是链上会比对的 program。
	info, err := b.Wallet.AddressInfo(mustWitnessAddr(t, addr))
	require.NoError(t, err, "地址必须已导入钱包地址管理器")
	assert.Equal(t, waddrmgr.WitnessScript, info.AddrType())
	assert.Equal(t, v.PkScript, hex.EncodeToString(mustPkScript(info.Address())),
		"导入的脚本必须派生出向量里的 program（导入错脚本=收不到钱）")
	require.Len(t, notified, 1, "新脚本必须订阅给链客户端")
	assert.Equal(t, addr, notified[0].String())

	// 幂等：重复请求不会再 import/订阅一次，watch 集也不增长。
	addrAgain, err := b.ensureUserDepositScript(v.UserID)
	require.NoError(t, err)
	assert.Equal(t, addr, addrAgain)
	assert.Len(t, notified, 1, "重复请求不得重复订阅")
	assert.Equal(t, 1, b.depositScripts.size())
}

// TestEnsureUserDepositScript_persistsAndReloads 重启后 watch 集从 neutrino.db 载入并重新导入。
func TestEnsureUserDepositScript_persistsAndReloads(t *testing.T) {
	vectors := loadDepositVectors(t)
	v1, v2 := vectors[0], vectors[1]
	pub, err := btcec.ParsePubKey(vectorPub(t, v1))
	require.NoError(t, err)
	params := chaincfg.RegressionNetParams

	b, walletDB, neutrinoDB := newTestWallet(t, pub, config{})
	b.notifyFn = func([]btcutil.Address) error { return nil }
	addr1, err := b.ensureUserDepositScript(v1.UserID)
	require.NoError(t, err)
	addr2, err := b.ensureUserDepositScript(v2.UserID)
	require.NoError(t, err)
	// 第二个 userID 用的是**同一个**群公钥（钱包只有一把），所以与向量的地址不相等
	// （向量 v2 配的是它自己的 tssPub）—— 这里比的是同一份派生函数的输出。
	want2, err := rtypes.DeriveDepositAddress(v2.UserID, pub.SerializeCompressed(), &params)
	require.NoError(t, err)
	require.Equal(t, want2, addr2)
	require.NotEqual(t, addr1, addr2, "不同 userID 必须得到不同地址")
	require.Equal(t, 2, b.depositScripts.size())

	// 模拟重启：同一批 DB 上新建一个 btcWallet，载入 watch 集。
	w, err := wallet.Open(walletDB, []byte("hello"), nil, &params, 0)
	require.NoError(t, err)
	reloaded := &btcWallet{
		Wallet:         w,
		db:             walletDB,
		chainParams:    params,
		depositScripts: newDepositScriptSet(),
		client: &neutrinoClient{
			tss:         &tssService{tssPublicKey: pub},
			cfg:         config{},
			neutrinoCfg: neutrino.Config{Database: neutrinoDB},
		},
	}
	reloaded.notifyFn = func([]btcutil.Address) error { return nil }
	reloaded.loadDepositScripts()
	assert.Equal(t, 2, reloaded.depositScripts.size(), "重启后 watch 集必须从 neutrino.db 恢复")
	userID, ok := reloaded.depositScripts.lookupUser(mustPkScriptHex(t, addr1, &params))
	assert.True(t, ok)
	assert.Equal(t, v1.UserID, userID)

	// 换成另一把群公钥（re-DKG 后）：旧条目全部作废，载入时必须跳过而不是继续 watch。
	otherPriv, _ := btcec.PrivKeyFromBytes([]byte{0x77})
	reloaded.client.tss.tssPublicKey = otherPriv.PubKey()
	reloaded.depositScripts = newDepositScriptSet()
	reloaded.loadDepositScripts()
	assert.Equal(t, 0, reloaded.depositScripts.size(), "旧世代的 watch 条目必须被跳过（地址已作废）")
}

// TestEnsureUserDepositScript_rejectsBadRequests 输入校验与上限：拿不到地址就必须报错，
// 绝不能返回一个"没被 watch 的地址"。
func TestEnsureUserDepositScript_rejectsBadRequests(t *testing.T) {
	vectors := loadDepositVectors(t)
	pub, err := btcec.ParsePubKey(vectorPub(t, vectors[0]))
	require.NoError(t, err)

	b, _, _ := newTestWallet(t, pub, config{MaxWatchedDepositScripts: 1})
	b.notifyFn = func([]btcutil.Address) error { return nil }

	_, err = b.ensureUserDepositScript("")
	assert.Error(t, err, "空 userID")
	_, err = b.ensureUserDepositScript("0000000000000000000000000000000000000000000000000000000000000000:0")
	assert.Error(t, err, "UTXO 形态不是合法充值 userID（链上同样拒）")
	_, err = b.ensureUserDepositScript("not-a-chain33-address")
	assert.Error(t, err, "非法 chain33 地址")

	// userID 超过最小 push 上限（75 字节）时派生失败，不得发出地址。
	_, err = b.ensureUserDepositScript(string(make([]byte, rtypes.MaxDepositUserIDLen+1)))
	assert.Error(t, err, "超长 userID 无法进入 P2WSH 脚本，必须拒发")

	// 达到 watch 集上限：拒发新地址（明确失败优于静默漏认）。
	_, err = b.ensureUserDepositScript(vectors[0].UserID)
	require.NoError(t, err)
	_, err = b.ensureUserDepositScript(vectors[1].UserID)
	require.Error(t, err, "watch 集满时必须拒发新地址")
	assert.Contains(t, err.Error(), "maxWatchedDepositScripts")

	// DKG 未完成（没有群公钥）时不发地址。
	b2, _, _ := newTestWallet(t, pub, config{})
	b2.notifyFn = func([]btcutil.Address) error { return nil }
	b2.client.tss.tssPublicKey = nil
	_, err = b2.ensureUserDepositScript(vectors[0].UserID)
	assert.Error(t, err)
}

// ---- 阶段 2b：归因（按派生 program 反解 userID，不依赖 OP_RETURN）----

func newAttributionWallet(t *testing.T, vectors []depositVector) (*btcWallet, []byte, []byte) {
	t.Helper()
	pub, err := btcec.ParsePubKey(vectorPub(t, vectors[0]))
	require.NoError(t, err)
	params := chaincfg.RegressionNetParams
	b := &btcWallet{
		chainParams:    params,
		depositScripts: newDepositScriptSet(),
		client:         &neutrinoClient{},
	}
	for _, v := range vectors[:2] {
		script, derr := rtypes.DeriveDepositPkScript(v.UserID, pub.SerializeCompressed())
		require.NoError(t, derr)
		b.depositScripts.add(v.UserID, script)
	}
	return b, pub.SerializeCompressed(), pub.SerializeUncompressed()
}

func depositPkScript(t *testing.T, userID string, pub []byte) []byte {
	t.Helper()
	script, err := rtypes.DeriveDepositPkScript(userID, pub)
	require.NoError(t, err)
	return script
}

// TestAnalyzeTransaction_attributesDepositByDerivedScript 充值归因只看"付给了谁的派生脚本"：
// 不带任何 OP_RETURN 也必须归到正确的 userID（这是硬切后唯一的绑定）。
func TestAnalyzeTransaction_attributesDepositByDerivedScript(t *testing.T) {
	vectors := loadDepositVectors(t)
	b, pub, _ := newAttributionWallet(t, vectors)
	user1, user2 := vectors[0].UserID, vectors[1].UserID

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x01}, Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(120_000, depositPkScript(t, user1, pub))) // 充值输出
	tx.AddTxOut(wire.NewTxOut(50_000, mustPkScriptOfAddr(t, "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080", &b.chainParams)))

	hash := tx.TxHash()
	pending := b.analyzeTransaction(&hash, tx)
	require.NotNil(t, pending, "付给用户充值脚本的交易必须被认成充值")
	assert.Equal(t, transactionTypeDeposit, pending.txType)
	assert.Equal(t, user1, pending.chain33DepositAddress, "必须归到派生脚本对应的 userID")
	assert.Equal(t, btcutil.Amount(120_000), pending.depositAmount)

	// 付给 user2 的脚本 → 只能归到 user2（换人不能错认）。
	tx2 := wire.NewMsgTx(wire.TxVersion)
	tx2.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x02}, Index: 0}, nil, nil))
	tx2.AddTxOut(wire.NewTxOut(7_000, depositPkScript(t, user2, pub)))
	hash2 := tx2.TxHash()
	pending2 := b.analyzeTransaction(&hash2, tx2)
	require.NotNil(t, pending2)
	assert.Equal(t, user2, pending2.chain33DepositAddress)
	assert.Equal(t, btcutil.Amount(7_000), pending2.depositAmount)
}

// TestAnalyzeTransaction_depositDoesNotDependOnOpReturn 归因不依赖 OP_RETURN：
//   - 有派生脚本输出、没有 OP_RETURN → 充值（上一条用例已覆盖，这里再显式断言一次）；
//   - 有 `rgbx:deposit:` OP_RETURN 但钱付给主池脚本 → **不是充值**（OP_RETURN 不再是承诺）；
//   - 有派生脚本输出、同时又带 deposit OP_RETURN → 仍然按脚本归因。
func TestAnalyzeTransaction_depositDoesNotDependOnOpReturn(t *testing.T) {
	vectors := loadDepositVectors(t)
	b, pub, _ := newAttributionWallet(t, vectors)
	user1 := vectors[0].UserID
	// 主池脚本（P2WPKH）：硬切后不再是充值地址。
	poolAddr, err := btcutil.NewAddressWitnessPubKeyHash(make([]byte, 20), &b.chainParams)
	require.NoError(t, err)
	b.tssPkScript = mustPkScript(poolAddr)

	depositOpReturn, err := txscript.NullDataScript([]byte("rgbx:deposit:" + user1))
	require.NoError(t, err)

	// ① OP_RETURN 冒充充值承诺，但钱付给了主池 → 不是充值。
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x03}, Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(9_000, b.tssPkScript))
	tx.AddTxOut(wire.NewTxOut(0, depositOpReturn))
	hash := tx.TxHash()
	assert.Nil(t, b.analyzeTransaction(&hash, tx),
		"OP_RETURN 不再是充值承诺：钱没付给派生脚本就不能算充值")

	// ② 钱付给了派生脚本、同时带 deposit OP_RETURN → 归因仍来自脚本（不是 OP_RETURN）。
	tx2 := wire.NewMsgTx(wire.TxVersion)
	tx2.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x04}, Index: 0}, nil, nil))
	tx2.AddTxOut(wire.NewTxOut(9_000, depositPkScript(t, user1, pub)))
	tx2.AddTxOut(wire.NewTxOut(0, depositOpReturn))
	hash2 := tx2.TxHash()
	pending := b.analyzeTransaction(&hash2, tx2)
	require.NotNil(t, pending)
	assert.Equal(t, user1, pending.chain33DepositAddress)
	assert.Equal(t, btcutil.Amount(9_000), pending.depositAmount)
}

// TestAnalyzeTransaction_multiUserDeposit 一笔 tx 付给多个用户的充值脚本：链上按 btc txid 去重，
// 只能有一个用户拿到账，桥取金额最大的那个并告警。
func TestAnalyzeTransaction_multiUserDeposit(t *testing.T) {
	vectors := loadDepositVectors(t)
	b, pub, _ := newAttributionWallet(t, vectors)
	user1, user2 := vectors[0].UserID, vectors[1].UserID

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x05}, Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1_000, depositPkScript(t, user1, pub)))
	tx.AddTxOut(wire.NewTxOut(5_000, depositPkScript(t, user2, pub)))
	hash := tx.TxHash()
	pending := b.analyzeTransaction(&hash, tx)
	require.NotNil(t, pending)
	assert.Equal(t, user2, pending.chain33DepositAddress, "取金额最大的那个用户")
	assert.Equal(t, btcutil.Amount(5_000), pending.depositAmount)
}

// TestAnalyzeTransaction_withdrawStillUsesCommitment 提现路径不变：主池输入 + 非主池输出 +
// `rgbx:withdraw:<txhash>` 承诺才认提现；OP_RETURN 格式不符时不给链上提现哈希。
func TestAnalyzeTransaction_withdrawStillUsesCommitment(t *testing.T) {
	vectors := loadDepositVectors(t)
	b, _, _ := newAttributionWallet(t, vectors)
	priv, _ := btcec.PrivKeyFromBytes([]byte{0x31})
	b.tssPubKey = priv.PubKey()

	withdrawHash := make([]byte, 32)
	for i := range withdrawHash {
		withdrawHash[i] = byte(i)
	}
	commitment, err := txscript.NullDataScript(append([]byte(withdrawOpReturnPrefix), withdrawHash...))
	require.NoError(t, err)
	dest := mustPkScriptOfAddr(t, "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080", &b.chainParams)

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x06}, Index: 0},
		Witness:          [][]byte{{0x01}, b.tssPubKey.SerializeCompressed()},
	})
	tx.AddTxOut(wire.NewTxOut(30_000, dest))
	tx.AddTxOut(wire.NewTxOut(0, commitment))
	hash := tx.TxHash()
	pending := b.analyzeTransaction(&hash, tx)
	require.NotNil(t, pending)
	assert.Equal(t, transactionTypeWithdraw, pending.txType)
	assert.Equal(t, withdrawHash, pending.chain33WithdrawTxHash)

	// 同一笔但 OP_RETURN 不是提现承诺（充值前缀）→ 不能当成提现哈希。
	badCommitment, err := txscript.NullDataScript([]byte("rgbx:deposit:" + vectors[0].UserID))
	require.NoError(t, err)
	tx2 := wire.NewMsgTx(wire.TxVersion)
	tx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x07}, Index: 0},
		Witness:          [][]byte{{0x01}, b.tssPubKey.SerializeCompressed()},
	})
	tx2.AddTxOut(wire.NewTxOut(30_000, dest))
	tx2.AddTxOut(wire.NewTxOut(0, badCommitment))
	hash2 := tx2.TxHash()
	pending2 := b.analyzeTransaction(&hash2, tx2)
	require.NotNil(t, pending2)
	assert.Equal(t, transactionTypeWithdraw, pending2.txType)
	assert.Empty(t, pending2.chain33WithdrawTxHash, "非提现承诺的 OP_RETURN 不得被当成提现哈希")
}

// TestValidatePendingDeposit 待提交充值的门控：归属必须是派生反解出来的 chain33 地址。
func TestValidatePendingDeposit(t *testing.T) {
	vectors := loadDepositVectors(t)
	valid := &btcPendingTx{
		tx:                    wire.NewMsgTx(wire.TxVersion),
		depositAmount:         1_000,
		chain33DepositAddress: vectors[0].UserID,
	}
	require.NoError(t, validatePendingDeposit(valid))

	cases := []struct {
		name string
		tx   *btcPendingTx
	}{
		{"nil", nil},
		{"no tx", &btcPendingTx{depositAmount: 1, chain33DepositAddress: vectors[0].UserID}},
		{"zero amount", &btcPendingTx{tx: valid.tx, chain33DepositAddress: vectors[0].UserID}},
		{"no attribution", &btcPendingTx{tx: valid.tx, depositAmount: 1}},
		{"utxo form attribution", &btcPendingTx{tx: valid.tx, depositAmount: 1,
			chain33DepositAddress: "0000000000000000000000000000000000000000000000000000000000000000:0"}},
		{"garbage attribution", &btcPendingTx{tx: valid.tx, depositAmount: 1, chain33DepositAddress: "garbage"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, validatePendingDeposit(tc.tx))
		})
	}
}

// TestBuildWithdrawTx_refusesUserDepositScript 冻结不变式：桥的自有付款不得落到用户充值脚本上
// （否则收款用户能把那笔付款当充值证明再铸一次）。提现侧按 watch 集直接拒付。
func TestBuildWithdrawTx_refusesUserDepositScript(t *testing.T) {
	vectors := loadDepositVectors(t)
	v := vectors[0]
	pub, err := btcec.ParsePubKey(vectorPub(t, v))
	require.NoError(t, err)
	b, _, _ := newTestWallet(t, pub, config{})
	b.notifyFn = func([]btcutil.Address) error { return nil }
	addr, err := b.ensureUserDepositScript(v.UserID)
	require.NoError(t, err)

	depositAddr := addr
	_, _, _, err = b.buildWithdrawTx(&withdrawRequest{toAddress: depositAddr, amount: 10_000, feeRate: 1})
	require.Error(t, err, "付给用户充值脚本的提现必须被拒")
	assert.Contains(t, err.Error(), "deposit script")

	// 普通地址（非 watch 集）不会被这条护栏挡住 —— 走到"钱不够"而不是"护栏"。
	_, _, _, err = b.buildWithdrawTx(&withdrawRequest{
		toAddress: "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080", amount: 10_000, feeRate: 1})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "deposit script")
}

// ---- 小工具 ----

func mustPkScriptOfAddr(t *testing.T, addrStr string, params *chaincfg.Params) []byte {
	t.Helper()
	addr, err := btcutil.DecodeAddress(addrStr, params)
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	return script
}

func mustWitnessAddr(t *testing.T, addrStr string) btcutil.Address {
	t.Helper()
	addr, err := btcutil.DecodeAddress(addrStr, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	return addr
}

func mustPkScriptHex(t *testing.T, addrStr string, params *chaincfg.Params) []byte {
	t.Helper()
	return mustPkScriptOfAddr(t, addrStr, params)
}

// TestHandleDepositAddressRequest 充值地址发放 HTTP 的对外契约（E2E 与运维要对着它写脚本）：
// GET/POST 两种形态、返回体字段、以及"拿不到地址就必须是错误码而不是 200"。
func TestHandleDepositAddressRequest(t *testing.T) {
	vectors := loadDepositVectors(t)
	v := vectors[0]
	pub, err := btcec.ParsePubKey(vectorPub(t, v))
	require.NoError(t, err)
	b, _, _ := newTestWallet(t, pub, config{})
	b.notifyFn = func([]btcutil.Address) error { return nil }
	n := &neutrinoClient{bw: b, cfg: config{}}

	decode := func(t *testing.T, rec *httptest.ResponseRecorder) depositAddressHTTPResponse {
		t.Helper()
		var resp depositAddressHTTPResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	// GET ?chain33Addr=
	rec := httptest.NewRecorder()
	n.handleDepositAddressRequest(rec, httptest.NewRequest(http.MethodGet,
		"/rgbx/v1/btc-deposit-address?chain33Addr="+v.UserID, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decode(t, rec)
	info, ok := resp.Data.(map[string]interface{})
	require.True(t, ok, "data 必须是结构化的对象：%v", resp.Data)
	assert.Equal(t, v.Addresses["regtest"], info["address"], "发放的地址必须与冻结向量一致")
	assert.Equal(t, v.PkScript, info["pkScript"], "返回的 program 就是执行器会比对的字节")
	assert.Equal(t, v.UserID, info["userID"])
	assert.Equal(t, rtypes.P2WSHDepositSpecV1, info["spec"])
	assert.Equal(t, float64(1), info["watchSize"])

	// POST JSON 等价（幂等：同一个地址，watch 集不增长）
	rec = httptest.NewRecorder()
	body := `{"chain33Addr":"` + v.UserID + `"}`
	n.handleDepositAddressRequest(rec, httptest.NewRequest(http.MethodPost,
		"/rgbx/v1/btc-deposit-address", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, v.Addresses["regtest"], decode(t, rec).Data.(map[string]interface{})["address"])
	assert.Equal(t, 1, b.depositScripts.size(), "重复请求不得让 watch 集增长")

	// 缺参数 / 非法地址 / 方法不对：都必须是明确失败，绝不返回一个没被 watch 的地址。
	rec = httptest.NewRecorder()
	n.handleDepositAddressRequest(rec, httptest.NewRequest(http.MethodGet, "/rgbx/v1/btc-deposit-address", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	n.handleDepositAddressRequest(rec, httptest.NewRequest(http.MethodGet,
		"/rgbx/v1/btc-deposit-address?chain33Addr=not-an-address", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	n.handleDepositAddressRequest(rec, httptest.NewRequest(http.MethodDelete, "/rgbx/v1/btc-deposit-address", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, 1, b.depositScripts.size(), "失败请求不得改变 watch 集")
}
