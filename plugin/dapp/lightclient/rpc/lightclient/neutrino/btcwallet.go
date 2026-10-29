package neutrino

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

const (
	transactionTypeDeposit      string = "deposit"
	transactionTypeWithdraw     string = "withdraw"
	transactionTypeRgb20Deposit string = "rgb20-deposit"
	_                           string = "mergeBalance"
)

const (
	// 默认确认数
	defaultRequiredConfs = 6
	// 最小找零金额（粉尘限制）
	minChangeAmount = 546
	// UTXO锁定时长
	utxoLeaseDuration = 24 * time.Hour
)

const (
	withdrawOpReturnPrefix    = "rgbx:" + transactionTypeWithdraw + ":"
	withdrawOpReturnDataLen   = len(withdrawOpReturnPrefix) + 32
	withdrawOpReturnScriptLen = 1 + 1 + withdrawOpReturnDataLen
)

const (
	btcwalletMonitorBucket = "rgbx-btcwallet-monitor"
	minPendingHeightKey    = "min-pending-height"
)

// withdrawInputSequence opts every withdrawal input into BIP125 replace-by-fee.
// 0xfffffffd (wire.MaxTxInSequenceNum - 2) is the conventional RBF value used by Bitcoin
// Core's wallet. Locktime stays 0, so this does not enable locktime-based semantics.
//
// nSequence is committed by the sighash and the txid, so this is a network-wide change:
// every bridge node must build and sign the same transaction (fine before launch).
const withdrawInputSequence = wire.MaxTxInSequenceNum - 2

// utxoLockID UTXO锁定ID
var utxoLockID = wtxmgr.LockID{
	'R', 'G', 'B', 'X', '-', 'L', 'O', 'C', 'K',
	'-', 'I', 'D', '-', 'V', '1', '.', '0', '.', '0',
	0, 0, 0, 0,
}

// DepositNotification 充值通知
type DepositNotification struct {
	TxHash       chainhash.Hash
	Amount       btcutil.Amount
	FromAddress  string
	Chain33Addr  string // 绑定的Chain33地址
	OpReturnData string // 原始OP_RETURN数据
}

// WithdrawNotification 提现确认通知
type WithdrawNotification struct {
	TxHash        chainhash.Hash
	ToAddress     string
	Chain33TxHash string // Chain33提现交易哈希
	OpReturnData  string // 原始OP_RETURN数据
}

// withdrawRequest 提现请求
type withdrawRequest struct {
	chain33WithdrawHash []byte
	amount              btcutil.Amount
	feeRate             btcutil.Amount // sat/byte，0表示使用默认
	toAddress           string
	stickyUTXO          *UTXO
	// bump 非 nil 表示这是一次"用户请求加价后的替换交易"。构造、校验、签名、广播仍走
	// processWithdrawRequest 同一条路径，只是费率要按 dust 上限封顶并额外做 RBF 校验。
	bump *withdrawBumpContext
}

// withdrawBumpContext 一笔 burn 的加价替换上下文。
//
// requestedFeeRate 是链上加价请求的费率（用户愿望上限）；effectiveFeeRate 由构造阶段按
// "用户输出 >= dust" 封顶后写回，是真正用于签名/校验的费率。appliedFeeRate 是本地已生效的
// 上一版费率（重启后从 DB 恢复），替换必须严格高于它。
type withdrawBumpContext struct {
	requestTxHash    []byte
	requestedFeeRate btcutil.Amount
	appliedFeeRate   btcutil.Amount
	effectiveFeeRate btcutil.Amount
	prevSignedTx     *wire.MsgTx
	prevInputAmounts []int64
}

type btcWallet struct {
	*wallet.Wallet
	client      *neutrinoClient
	chainParams chaincfg.Params
	chainClient chain.Interface
	rpcClient   *rpcclient.Client
	db          walletdb.DB

	minPendingHeight int32
	processedHeight  int32

	// TSS相关
	tssAddress  btcutil.Address
	tssPubKey   *btcec.PublicKey
	tssPkScript []byte // 预计算的TSS地址脚本

	// notifyFn 可选：注入"订阅地址"的实现（默认走 chainClient.NotifyReceived），仅测试用。
	notifyFn func([]btcutil.Address) error

	// 通知channel
	depositChan       chan *btcPendingTx
	withdrawChan      chan *btcPendingTx
	addPendingChan    chan *btcPendingTx
	removePendingChan chan chainhash.Hash
	// replacePendingChan 用新版本的替换交易顶替同一 burn 的旧 pending 记录（RBF 加价）。
	// 单独一条 channel：先删后加必须是原子的，否则 replay 可能选中已作废的旧版本。
	replacePendingChan chan *btcPendingTx

	// 配置
	requiredConfs int32

	// 交易监控
	pendingTxs map[chainhash.Hash]*btcPendingTx
	rescanDone bool
	// txLock     sync.RWMutex

	// blockCache 按 blockHash 缓存证明所需的块内 txid/时间/高度（见 block_cache.go）。
	// nil 表示不缓存（手工构造的 btcWallet / 测试）；newBtcWallet 会装上默认缓存。
	blockCache *btcBlockCache
	// blockVerboseFn 可选：注入 getblockverbose 实现（默认走 rpcClient），仅测试用。
	blockVerboseFn btcBlockVerboseFetcher
	// selectInputsFn 可选：注入提现输入的选择与锁定（默认走钱包 listUnspent + LeaseOutput），仅测试用。
	selectInputsFn func(target, feeRate btcutil.Amount, sticky *UTXO) ([]*UTXO, btcutil.Amount, error)
	// sendRawTxFn 可选：注入 sendrawtransaction 实现（默认走 rpcClient / chainClient），仅测试用。
	sendRawTxFn func(tx *wire.MsgTx) error
	// walletBirthday is the wallet manager's birthday; walletFreshlyCreated is true when this
	// process created btcwallet.db instead of opening an existing one. Both feed the startup
	// self-check that warns when a fresh wallet cannot see deposits made before it existed.
	walletBirthday       time.Time
	walletFreshlyCreated bool
	// birthdayOnce makes the birthday alert fire at most once, whether it is detected at
	// startup from the persisted header tip or on the first block notification.
	birthdayOnce sync.Once
}

type btcPendingTx struct {
	tx                    *wire.MsgTx
	submitTime            time.Time
	notified              bool
	confirmations         int32
	blockHeight           int32
	blockHash             chainhash.Hash
	txHash                chainhash.Hash
	txType                string // "deposit" or "withdraw"
	depositAmount         btcutil.Amount
	withdrawAmount        btcutil.Amount
	chain33DepositAddress string // Chain33充值地址（= P2WSH 派生里的 userID，由充值脚本反解得到）
	withdrawAddress       string
	chain33WithdrawTxHash []byte // Chain33提现交易哈希
}

func newBtcWallet(n *neutrinoClient) (*btcWallet, error) {
	bw := &btcWallet{
		client:             n,
		chainParams:        n.neutrinoCfg.ChainParams,
		depositChan:        make(chan *btcPendingTx, 100),
		withdrawChan:       make(chan *btcPendingTx, 100),
		addPendingChan:     make(chan *btcPendingTx, 100),
		removePendingChan:  make(chan chainhash.Hash, 100),
		replacePendingChan: make(chan *btcPendingTx, 100),
		requiredConfs:      int32(n.cfg.BlockConfirmations),
		pendingTxs:         make(map[chainhash.Hash]*btcPendingTx),
		blockCache:         newBtcBlockCache(defaultBtcBlockCacheSize),
	}

	if n.cfg.BtcRPC.Host != "" {
		connCfg, err := n.cfg.BtcRPC.toConnConfig()
		if err != nil {
			log.Error("newBtcWallet btc rpc conn config error", "err", err)
			return nil, err
		}
		rpcCli, err := rpcclient.New(connCfg, nil)
		if err != nil {
			log.Error("newBtcWallet create btc rpc client error", "err", err)
			return nil, err
		}
		bw.rpcClient = rpcCli
	}

	exist, db, err := openWalletDB(n.neutrinoCfg.DataDir, "btcwallet.db")
	if err != nil {
		log.Error("newBtcWallet open db error", "err", err)
		return nil, err
	}

	pubPass := []byte("hello")
	if !exist {
		err = wallet.CreateWatchingOnly(db, pubPass, &bw.chainParams, types.Now())
		if err != nil {
			log.Error("newBtcWallet create wallet error", "err", err)
			_ = db.Close()
			return nil, err
		}
	}

	w, err := wallet.Open(db, pubPass, nil, &bw.chainParams, 0)
	if err != nil {
		log.Error("newBtcWallet open wallet error", "err", err)
		_ = db.Close()
		return nil, err
	}
	log.Info("newBtcWallet open wallet success", "wallet birthtime", w.Manager.Birthday().Unix(),
		"freshlyCreated", !exist)
	bw.db = db
	bw.Wallet = w
	bw.walletBirthday = w.Manager.Birthday()
	bw.walletFreshlyCreated = !exist
	bw.chainClient = chain.NewNeutrinoClient(&bw.chainParams, n.neutrinoCS)
	return bw, nil
}

// checkWalletBirthday reports a problem when the wallet was created by this process while the
// chain already had history: the wallet's address manager only starts scanning from its
// birthday, so deposits made before then are absent from the wallet store and cannot be replayed
// by the bridge (its replay reads the wallet store, not the chain). Returns "" when there is
// nothing to report.
func checkWalletBirthday(fresh bool, birthday time.Time, firstChainHeight int32) string {
	if !fresh || firstChainHeight <= 0 {
		return ""
	}
	return "btcwallet.db was created fresh at birthday " + birthday.UTC().Format(time.RFC3339) +
		" while the chain is already at height " + strconv.FormatInt(int64(firstChainHeight), 10) +
		"; deposits made before the wallet existed are not in its store and will be missed. " +
		"Restore btcwallet.db from a backup taken before the loss instead of letting the node " +
		"create a new wallet."
}

// alertFreshWalletBehindChain logs the birthday alert when this process created the wallet
// while the chain already has blocks, at most once per btcWallet. A zero or unknown height
// leaves the check to the first block notification.
func (b *btcWallet) alertFreshWalletBehindChain(chainHeight int32) {
	if !b.walletFreshlyCreated || chainHeight <= 0 {
		return
	}
	b.birthdayOnce.Do(func() {
		if warning := checkWalletBirthday(true, b.walletBirthday, chainHeight); warning != "" {
			log.Error("ALERT wallet birthday cannot cover chain history: " + warning)
		}
	})
}

func (b *btcWallet) start() error {
	if err := b.chainClient.Start(); err != nil {
		log.Error("btcwallet chainclient start error", "err", err)
		return err
	}

	b.Wallet.Start()
	// Check right away when the node already has a header tip: a fresh wallet on a chain
	// with history is already wrong, and the next block notification (which also checks)
	// may not arrive for a long time. On regtest no block arrives unless the harness mines.
	if _, height, err := b.chainClient.GetBestBlock(); err == nil {
		b.alertFreshWalletBehindChain(height)
	}
	// 从这里起钱包才真的在 watch：把"导入 + 订阅"装上，watch 集的载入/补齐（见 deposit_address.go）
	// 会通过它把脚本真正导进钱包。非官方节点不装（它没有可 watch 的钱包）。
	b.client.depositImporter = func(witnessScripts [][]byte) error {
		_, err := b.importDepositWitnessScripts(witnessScripts)
		return err
	}

	// 启动交易监听
	go b.monitorTransactions()

	return nil
}

func (b *btcWallet) stop() {
	b.Wallet.Stop()
	b.chainClient.Stop()
	if b.rpcClient != nil {
		b.rpcClient.Shutdown()
	}
	_ = b.db.Close()
}

// waitAndImportTSSAddress 等待TSS地址生成并导入
func (b *btcWallet) waitAndImportTSSAddress() {

	b.tssPubKey = b.client.tss.tssPublicKey
	b.tssPkScript = b.client.tss.pkScript
	b.tssAddress = b.client.tss.tssAddress
	log.Debug("waitAndImportTSSAddress", "address", b.tssAddress.String())
	b.client.waitUntilDone("waitImportTSSAddress", func() bool {
		// 显式导入TSS公钥到钱包
		if _, err := b.Wallet.AddressInfo(b.tssAddress); err != nil {
			if !waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) {
				log.Error("waitAndImportTSSAddress AddressInfo failed", "err", err)
				return false
			}
			err = b.importTSSPublicKey()
			if err != nil {
				log.Error("waitAndImportTSSAddress ImportPublicKey failed", "err", err)
				return false
			}
		}
		return true
	}, time.Second*3)
	log.Info("waitAndImportTSSAddress success", "address", b.tssAddress.String())
	// 用户充值脚本的 watch 集（P2WSH 每用户地址）：从 neutrino.db 载入并重新导入钱包，
	// 见 deposit_address.go。必须在钱包打开之后（这里）、HTTP 发放地址之前完成。
	b.client.loadDepositScripts()
}

func (b *btcWallet) importTSSPublicKey() error {
	err := b.Wallet.ImportPublicKey(b.tssPubKey, waddrmgr.WitnessPubKey)
	if err == nil {
		return nil
	}
	// Old/external wallet DBs might miss BIP84 scope (m/84'/coin'). Create it
	// on-demand and retry importing the TSS key.
	if !waddrmgr.IsError(err, waddrmgr.ErrScopeNotFound) {
		return err
	}
	if _, err = b.ensureScopedKeyManager(waddrmgr.KeyScopeBIP0084); err != nil {
		return err
	}
	return b.Wallet.ImportPublicKey(b.tssPubKey, waddrmgr.WitnessPubKey)
}

// ensureScopedKeyManager 取指定 scope 的 key manager；旧/外部钱包库可能没有该 scope
// （BIP84 = m/84'/coin'），按需创建后重取。
//
// 为什么用户充值脚本的导入也要走它：钱包（尤其全新 watching-only 库）默认可能没有 BIP84，
// 而导入脚本必须落在某个 scope 的 imported account 上。不依赖"TSS 公钥已经导入过所以 scope 一定在"
// 这种隐含顺序 —— 那种顺序一旦被改动（例如以后 TSS 主池换成别的地址类型）就会变成运行时失败。
func (b *btcWallet) ensureScopedKeyManager(scope waddrmgr.KeyScope) (*waddrmgr.ScopedKeyManager, error) {
	manager, err := b.Wallet.Manager.FetchScopedKeyManager(scope)
	if err == nil {
		return manager, nil
	}
	if !waddrmgr.IsError(err, waddrmgr.ErrScopeNotFound) {
		return nil, err
	}
	schema, ok := waddrmgr.ScopeAddrMap[scope]
	if !ok {
		return nil, err
	}
	if _, addErr := b.Wallet.AddScopeManager(scope, schema); addErr != nil {
		log.Warn("ensureScopedKeyManager AddScopeManager failed, retry fetch anyway",
			"scope", scope, "err", addErr)
	}
	return b.Wallet.Manager.FetchScopedKeyManager(scope)
}

func (b *btcWallet) loadMinPendingHeight() int32 {
	var height int32
	err := walletdb.View(b.client.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(btcwalletMonitorBucket))
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		val := bucket.Get([]byte(minPendingHeightKey))
		if len(val) == 0 {
			return nil
		}
		reply := &types.Int64{}
		if err := types.Decode(val, reply); err != nil {
			return err
		}
		height = int32(reply.GetData())
		return nil
	})
	log.Debug("loadMinPendingHeight", "height", height, "err", err)
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		log.Error("loadMinPendingHeight", "err", err)
	}
	return height
}

func (b *btcWallet) saveMinPendingHeight(height int32) {
	err := walletdb.Update(b.client.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte(btcwalletMonitorBucket))
		if err != nil {
			return err
		}
		if height <= 0 {
			return bucket.Delete([]byte(minPendingHeightKey))
		}
		data := types.Encode(&types.Int64{Data: int64(height)})
		return bucket.Put([]byte(minPendingHeightKey), data)
	})
	log.Debug("saveMinPendingHeight", "height", height, "err", err)
	if err != nil {
		log.Error("saveMinPendingHeight", "err", err, "height", height)
	}
}

func (b *btcWallet) updateMinPendingHeight() {
	minHeight := int32(0)
	for _, pending := range b.pendingTxs {
		if pending.blockHeight <= 0 {
			continue
		}
		if minHeight == 0 || pending.blockHeight < minHeight {
			minHeight = pending.blockHeight
		}
	}
	log.Debug("updateMinPendingHeight", "minHeight", minHeight,
		"processedHeight", b.processedHeight, "bestHeight", b.client.getBestBlockHeight(),
		"b.minPendingHeight", b.minPendingHeight, "pendingTxs", len(b.pendingTxs))
	if minHeight > 0 && minHeight != b.minPendingHeight {
		b.minPendingHeight = minHeight
		b.saveMinPendingHeight(minHeight)
	}
}

func (b *btcWallet) rescanWalletTxs(start, end int32, rescanChan chan *wallet.GetTransactionsResult) error {
	b.client.waitUntilDone("walletTxsRescan", func() bool {
		return b.Wallet.ChainSynced()
	}, time.Second*2)
	log.Debug("rescanWalletTxs", "start", start, "end", end, "bestHeight", b.client.getBestBlockHeight())
	startBlock := wallet.NewBlockIdentifierFromHeight(start)
	endBlock := wallet.NewBlockIdentifierFromHeight(end)
	res, err := b.Wallet.GetTransactions(startBlock, endBlock, "", b.client.ctx.Done())
	if err != nil {
		log.Error("rescanWalletTxs GetTransactions error", "err", err)
		return err
	}
	rescanChan <- res
	return nil

}

func (b *btcWallet) handleNotify(attachedBlocks []wallet.Block, unminedTxs []wallet.TransactionSummary) {

	log.Debug("handleNotify", "attachedBlocks", len(attachedBlocks), "unminedTxs", len(unminedTxs))
	// 处理已确认交易， attachedBlocks是btc链上新添加的区块
	for _, block := range attachedBlocks {
		log.Debug("handleNotify block", "height", block.Height, "hash", block.Hash.String(), "transactions", len(block.Transactions))
		for _, tx := range block.Transactions {
			b.handleTransaction(&tx, block.Height, *block.Hash)
		}
		b.processedHeight = block.Height
	}

	// 处理未确认交易（重置确认数）
	for _, tx := range unminedTxs {
		b.handleUnminedTransaction(*tx.Hash)
	}
}

// monitorTransactions 监听交易通知
func (b *btcWallet) monitorTransactions() {

	client := b.Wallet.NtfnServer.TransactionNotifications()
	b.Wallet.SynchronizeRPC(b.chainClient)
	b.waitAndImportTSSAddress()
	// 读/重放窗口：**有 watermark（min-pending-height）就用 watermark；没有就从钱包自己的最早点读起**
	// （rescanHeight=0 = 不设下限，交给 tx store 自己的游标）。
	//
	// 为什么不留任何"最低高度"（无论来自配置还是锚点+1）：钱包能"看见"哪些 BTC 交易完全由它自己的
	// 生日决定（生日 = btcwallet.db 创建时刻，neutrino 的 rescan 用 StartTime=生日 截断，生日之前的块
	// 根本不会被下载 filter），所以任何最小值都扩大不了"发现历史充值"的能力。而生日 ≥ 桥启动 ≥
	// 锚点+1 意味着这类下限**永远不会 binding**（一个恒为常量的下限没有信息量）；更糟的是钱包从备份
	// 恢复时（历史早于那个常量），下限反而会**截断本该重放的历史**。从 0 读起也不贵：store 读按
	// "有交易的块"走游标，成本与区间长度无关。
	rescanHeight := b.loadMinPendingHeight()
	bestHeight := b.client.getBestBlockHeight()
	if rescanHeight > 0 {
		log.Debug("monitorTransactions resume from the minPendingHeight watermark",
			"height", rescanHeight, "bestHeight", bestHeight)
	} else {
		log.Info("monitorTransactions no minPendingHeight watermark, replay the wallet's own tx store",
			"height", rescanHeight, "bestHeight", bestHeight)
	}
	b.minPendingHeight = rescanHeight
	interval := b.client.cfg.BtcBlockInterval/2 + 1
	ticker := time.NewTicker(time.Second * time.Duration(interval))
	defer ticker.Stop()
	// Replay signed-but-unconfirmed withdrawals. It runs in this goroutine because it reads
	// b.pendingTxs, which is owned here (no lock), and is driven by persisted records so a
	// restarted process still replays.
	replayTicker := time.NewTicker(signedTxReplayInterval)
	defer replayTicker.Stop()
	rescanChan := make(chan *wallet.GetTransactionsResult, 1)
	firstProcessFlag := true
	for {
		select {
		case <-b.client.ctx.Done():
			client.Done()
			return
		case res := <-rescanChan:
			b.handleNotify(res.MinedTransactions, res.UnminedTransactions)
			b.rescanDone = true

		case ntfn := <-client.C:
			if ntfn == nil {
				continue
			}
			// 首次处理检测是否需要重新扫描
			if firstProcessFlag && len(ntfn.AttachedBlocks) > 0 {
				log.Debug("monitorTransactions first process", "height", ntfn.AttachedBlocks[0].Height, "rescanHeight", rescanHeight)
				firstProcessFlag = false
				b.alertFreshWalletBehindChain(ntfn.AttachedBlocks[0].Height)
				if ntfn.AttachedBlocks[0].Height > rescanHeight {
					go b.rescanWalletTxs(rescanHeight, ntfn.AttachedBlocks[0].Height-1, rescanChan)
				} else {
					b.rescanDone = true
				}
			}
			b.handleNotify(ntfn.AttachedBlocks, ntfn.UnminedTransactions)

		case pending := <-b.addPendingChan:
			b.pendingTxs[pending.txHash] = pending
			log.Debug("addPendingTx", "txHash", pending.txHash.String(), "txType", pending.txType)
		case pending := <-b.replacePendingChan:
			// 一笔 burn 的任何时刻只能有一版交易可能在链上确认：先移除同一 burn 的旧版本，
			// 再放入替换版本（两个动作在同一事件里完成，replay 不会看到中间态）。
			for txHash, existing := range b.pendingTxs {
				if existing != nil && existing.txType == transactionTypeWithdraw &&
					bytes.Equal(existing.chain33WithdrawTxHash, pending.chain33WithdrawTxHash) {
					delete(b.pendingTxs, txHash)
				}
			}
			b.pendingTxs[pending.txHash] = pending
			log.Debug("replacePendingTx", "txHash", pending.txHash.String(),
				"burnTxHash", hex.EncodeToString(pending.chain33WithdrawTxHash))
		case txHash := <-b.removePendingChan:
			delete(b.pendingTxs, txHash)
			log.Debug("removePendingTx", "txHash", txHash.String(), "txLen", len(b.pendingTxs),
				"processedHeight", b.processedHeight, "minPendingHeight", b.minPendingHeight)
			if len(b.pendingTxs) > 0 {
				b.updateMinPendingHeight()
			}
		case <-ticker.C:
			if b.rescanDone && len(b.pendingTxs) == 0 && b.processedHeight > b.minPendingHeight {
				b.minPendingHeight = b.processedHeight
				b.saveMinPendingHeight(b.minPendingHeight)
				log.Debug("monitorTransactions update minPendingHeight",
					"minPendingHeight", b.minPendingHeight, "bestHeight", b.client.getBestBlockHeight())
			}
			b.updateTransactionConfirmations()
		case <-replayTicker.C:
			b.replaySignedWithdrawTxs()
		}
	}
}

// handleTransaction 处理单个交易
func (b *btcWallet) handleTransaction(tx *wallet.TransactionSummary, blockHeight int32, blockHash chainhash.Hash) {
	txHash := *tx.Hash
	// 检查提现交易是否已在pending缓存中（避免重复解析），如果存在，则更新区块高度和区块哈希
	pending, exists := b.pendingTxs[txHash]
	if exists {
		pending.blockHeight = blockHeight
		pending.blockHash = blockHash
		log.Debug("handleTransaction already in pending", "txHash", txHash.String())
		return
	}

	log.Debug("handleTransaction processing", "txHash", txHash.String(), "blockHeight", blockHeight,
		"inputs", len(tx.Tx.TxIn), "outputs", len(tx.Tx.TxOut))

	// 分析交易类型和相关信息
	pending = b.analyzeTransaction(tx.Hash, tx.Tx)
	if pending == nil {
		log.Debug("handleTransaction not deposit/withdraw", "txHash", txHash.String())
		return
	}
	pending.tx = tx.Tx
	pending.blockHeight = blockHeight
	pending.blockHash = blockHash
	pending.txHash = txHash
	// 记录关键交易信息
	log.Info("handleTransaction detected "+pending.txType, "blockHeight", blockHeight, "txHash", txHash.String(),
		"depositAmount", pending.depositAmount, "withdrawAmount", pending.withdrawAmount)

	// 添加到待确认列表
	b.pendingTxs[txHash] = pending
}

// handleUnminedTransaction 处理未确认交易（重置确认数）
func (b *btcWallet) handleUnminedTransaction(txHash chainhash.Hash) {
	if pending, exists := b.pendingTxs[txHash]; exists && pending.blockHeight > 0 {
		// 重置确认数和区块高度
		oldConfirmations := pending.confirmations
		oldBlockHeight := pending.blockHeight

		pending.confirmations = 0
		pending.blockHeight = -1
		pending.notified = false

		log.Debug("handleUnminedTransaction reset confirmations", "txHash", txHash.String(),
			"oldConfirmations", oldConfirmations, "oldBlockHeight", oldBlockHeight, "type", pending.txType)
	}
}

// updateTransactionConfirmations 更新已存在交易的确认数
func (b *btcWallet) updateTransactionConfirmations() {
	bestHeight := b.client.getBestBlockHeight()
	log.Debug("updateTransactionConfirmations", "bestBlock", bestHeight, "pendingTxs", len(b.pendingTxs))
	for txHash, pending := range b.pendingTxs {
		if pending.blockHeight > 0 && bestHeight > 0 {
			pending.confirmations = bestHeight - pending.blockHeight + 1
		}
		// 如果达到要求的确认数，发送通知
		if !pending.notified && pending.confirmations >= b.requiredConfs {
			log.Debug("updateTransactionConfirmations ready for notification", "txHash", txHash.String(), "type", pending.txType,
				"confirmations", pending.confirmations, "required", b.requiredConfs)
			b.sendTransactionNotification(txHash, pending)
			pending.notified = true
		}

	}

}

func (b *btcWallet) addPendingTx(pending *btcPendingTx) {
	b.addPendingChan <- pending
}

func (b *btcWallet) removePendingTx(txHash chainhash.Hash) {
	b.removePendingChan <- txHash
}

// replacePendingTx 以新版本交易顶替同一 burn 的旧 pending 记录（RBF 替换）。
func (b *btcWallet) replacePendingTx(pending *btcPendingTx) {
	if b == nil || b.replacePendingChan == nil || pending == nil {
		return
	}
	b.replacePendingChan <- pending
}

// parseWithdrawCommitment 解析提现承诺 OP_RETURN：`rgbx:withdraw:<32 字节 chain33 提现哈希>`。
//
// 只有提现走 OP_RETURN（H4 只对 RGB20 跳过），充值侧已硬切到 P2WSH 派生 —— 归属由
// "付给了谁的派生脚本" 认定，tx 里不需要（也不再接受）任何充值承诺。
// 严格校验前缀与总长：旧实现按 ":" 切分取第 3 段，格式不符时会把任意 OP_RETURN 的
// 中段当成提现哈希。
func parseWithdrawCommitment(pkScript []byte) ([]byte, bool) {
	// OP_RETURN + 长度字节 + 数据（<76 字节走最小 push，前缀 13 + 32 字节哈希）
	if len(pkScript) != 2+withdrawOpReturnDataLen || pkScript[0] != txscript.OP_RETURN ||
		pkScript[1] != byte(withdrawOpReturnDataLen) {
		return nil, false
	}
	payload := pkScript[2:]
	if !bytes.HasPrefix(payload, []byte(withdrawOpReturnPrefix)) {
		return nil, false
	}
	return payload[len(withdrawOpReturnPrefix):], true
}

// analyzeTransaction 分析交易类型
// 返回: ("deposit"|"withdraw"|"", pendingTx)
func (b *btcWallet) analyzeTransaction(hash *chainhash.Hash, tx *wire.MsgTx) *btcPendingTx {
	info := &btcPendingTx{}

	// 分类排除（BL-5/HR-2 定案：侧车状态排除）：
	// 先查"已知 RGB txid"集合（来自侧车 ListTransfers 轮询 + RGB20 提现 txid 映射）。
	// - 是已知 RGB 充值 receive 交易 → 跳过 BTC 充值路径（RGB 铸币走侧车路径，避免双入账）；
	// - 是已知 RGB20 提现交易 → 跳过 BTC 提现路径（否则 getPendingTxBlockIndex("") 死循环）。
	if b.client != nil && b.client.rgb20 != nil && hash != nil && b.client.rgb20.IsKnownRgbTxid(hash.String()) {
		log.Debug("analyzeTransaction skip known rgb tx", "txHash", hash.String())
		return nil
	}

	// 检查输出。三类的判定口径：
	//   - 用户 P2WSH 充值脚本（watch 集反解 userID）→ 充值候选，**归属就是这个 userID**；
	//   - 主池 TSS P2WPKH 脚本 → 提现找零/扫集回池，**不构成充值**（充值已硬切到 P2WSH）；
	//   - 其余 → 提现目标地址候选（取第一个）。
	var withdrawAmount btcutil.Amount
	var firstNonTssOutputAddress string
	var withdrawTxHash []byte
	// deposits 一笔 tx 里各用户的充值额（一笔 tx 可以给多个用户的 P2WSH 付款）。
	deposits := make(map[string]btcutil.Amount)

	for i, output := range tx.TxOut {
		if payload, ok := parseWithdrawCommitment(output.PkScript); ok {
			withdrawTxHash = payload
			log.Debug("analyzeTransaction withdraw commitment found", "txHash", hash.String(),
				"outputIndex", i, "chain33WithdrawHash", hex.EncodeToString(payload))
			continue
		}

		// 充值归因：**只看脚本**。program 是按 (userID, tssPub) 派生的，链上执行器用同一份派生
		// 认定归属，所以"能反解出 userID"就等于"链上会把这笔算给该 userID"。
		if userID, ok := b.client.deposits.lookupUser(output.PkScript); ok {
			deposits[userID] += btcutil.Amount(output.Value)
			log.Debug("analyzeTransaction user deposit script found", "txHash", hash.String(),
				"outputIndex", i, "userID", userID, "amount", btcutil.Amount(output.Value))
			continue
		}

		// 主池脚本：既不是充值（不再有 OP_RETURN 承诺），也不是提现目标。
		if len(b.tssPkScript) > 0 && bytes.Equal(output.PkScript, b.tssPkScript) {
			log.Debug("analyzeTransaction tss pool output found", "txHash", hash.String(),
				"outputIndex", i, "amount", btcutil.Amount(output.Value))
			continue
		}

		withdrawAmount += btcutil.Amount(output.Value)
		if firstNonTssOutputAddress == "" {
			// 提取第一个非TSS地址输出（提现地址）
			_, addrs, _, err := txscript.ExtractPkScriptAddrs(output.PkScript, &b.chainParams)
			if err == nil && len(addrs) > 0 {
				firstNonTssOutputAddress = addrs[0].String()
				log.Debug("analyzeTransaction non-TSS output found", "txHash", hash.String(),
					"outputIndex", i, "address", firstNonTssOutputAddress, "amount", btcutil.Amount(output.Value))
			}
		}
	}

	hasTssInput := false
	// 检查输入：直接从witness解析公钥验证是否来自TSS地址（仅支持 P2WPKH，不支持 Taproot/嵌套 SegWit）
	if len(tx.TxIn) > 0 && b.tssPubKey != nil {
		if witness := tx.TxIn[0].Witness; len(witness) == 2 &&
			bytes.Equal(witness[1], b.tssPubKey.SerializeCompressed()) {
			hasTssInput = true
		}
	}

	// 根据规则判断交易类型。
	// 提现交易特征：有TSS输入，有非TSS输出。顺序不能反：桥的自有付款若落到用户 P2WSH
	// （被 E15-a 与提现护栏挡住，但顺序在这里是第二道），也必须是提现而不是充值。
	if hasTssInput && firstNonTssOutputAddress != "" {
		info.withdrawAddress = firstNonTssOutputAddress
		info.txType = transactionTypeWithdraw
		info.withdrawAmount = withdrawAmount
		info.chain33WithdrawTxHash = withdrawTxHash
		return info
	}

	// 充值交易特征：无TSS输入，且付给了用户 P2WSH 充值脚本（不看 OP_RETURN）。
	if len(deposits) > 0 && !hasTssInput {
		userID, amount := pickDepositUser(hash, deposits)
		info.depositAmount = amount
		info.txType = transactionTypeDeposit
		info.chain33DepositAddress = userID
		return info
	}

	// 不符合充值或提现特征，可能是小额合并交易
	log.Debug("analyzeTransaction not deposit/withdraw", "txHash", hash.String(),
		"hasTssInput", hasTssInput, "depositUsers", len(deposits), "firstNonTssAddress", firstNonTssOutputAddress)
	return nil
}

// pickDepositUser 一笔充值 tx 命中多个用户的 P2WSH 时选一个认领（金额最大者）。
//
// 链上同一笔 btc txid 只能被认领一次（checkDepositDuplicate 按 btc txid 去重，见
// executor/checktx.go），所以"一笔 tx 付给多个用户"这件事本身只能有一个用户拿到账：
// 这里取最大额那笔并告警，剩下的只能等 bridge/C4 侧另想办法（例如按用户分别付款）。
func pickDepositUser(hash *chainhash.Hash, deposits map[string]btcutil.Amount) (string, btcutil.Amount) {
	var bestUser string
	var bestAmount btcutil.Amount
	for userID, amount := range deposits {
		if bestUser == "" || amount > bestAmount {
			bestUser, bestAmount = userID, amount
		}
	}
	if len(deposits) > 1 {
		log.Error("analyzeTransaction one btc tx pays several deposit scripts: only one of them can ever be "+
			"claimed (the chain dedups deposit proofs by btc txid), claiming the largest",
			"txHash", hash.String(), "users", len(deposits), "claimedUser", bestUser, "claimedAmount", bestAmount)
	}
	return bestUser, bestAmount
}

// sendTransactionNotification 发送交易确认通知
func (b *btcWallet) sendTransactionNotification(_ chainhash.Hash, pending *btcPendingTx) {
	if pending.txType == "deposit" {
		b.depositChan <- pending
	} else {
		b.withdrawChan <- pending
	}
}

// buildWithdrawTx 构建提现交易
// 返回: (交易, 输入金额列表, 已锁定的UTXO列表, 错误)
// 注意: 如果返回错误，UTXO锁定会自动释放；如果成功，调用方需要在广播失败时调用releaseUTXOs
func (b *btcWallet) buildWithdrawTx(req *withdrawRequest) (*wire.MsgTx, []int64, []*UTXO, error) {
	outputs, err := b.withdrawOutputs(req)
	if err != nil {
		return nil, nil, nil, err
	}
	// 加价替换与首次提现共用同一套交易构造（见 bump_withdraw_fee.go）；唯一差别是费率要先按
	// "用户输出不得低于 dust" 封顶。
	if req.bump != nil {
		return b.buildWithdrawReplacementTx(req, outputs)
	}

	// 手动选择UTXO并构建交易
	tx, inputAmounts, lockedUTXOs, err := b.buildTransaction(outputs, req.feeRate, req.chain33WithdrawHash, true, req.stickyUTXO)
	if err != nil {
		return nil, nil, nil, err
	}

	log.Debug("buildWithdrawTx", "to", req.toAddress, "amount", req.amount, "feeRate", req.feeRate,
		"inputs", len(tx.TxIn), "outputs", len(tx.TxOut), "lockedUTXOs", len(lockedUTXOs))

	return tx, inputAmounts, lockedUTXOs, nil
}

// withdrawOutputs 构造提现的用户输出，并执行"不得付给用户充值脚本"这一桥侧护栏。
func (b *btcWallet) withdrawOutputs(req *withdrawRequest) ([]*wire.TxOut, error) {
	// 解析目标地址
	toAddr, err := btcutil.DecodeAddress(req.toAddress, &b.chainParams)
	if err != nil {
		return nil, fmt.Errorf("invalid to address: %w", err)
	}

	// 构建输出
	pkScript, err := txscript.PayToAddrScript(toAddr)
	if err != nil {
		return nil, fmt.Errorf("create pk script failed: %w", err)
	}

	// 护栏（不动式）：桥的自有付款不得落到用户 P2WSH 充值脚本上。
	// 违反它的后果见 rgbx/executor/validate_proof.go 的"冻结不变式"：桥自己付出去的那笔交易，
	// 收款用户可以拿回来当**充值证明**再铸一次（链上分不清付款方）。链上只挡得住"发起人自己的
	// 充值地址"（checkWithdraw 的 E15-a），用**另一个自己控制的 chain33 地址**当提现目标那一段
	// 只能由桥侧兜底 —— 就是这里：watch 集里的脚本都是已发放的充值地址，命中即拒。
	// 只拒"我们自己发放过的脚本"，普通 P2WSH（交易所/多签/闪电通道）不受影响。
	if userID, ok := b.client.isWatchedDepositScript(pkScript); ok {
		log.Error("buildWithdrawTx refuse to pay a user deposit script (bridge self-payment invariant)",
			"toAddress", req.toAddress, "depositUserID", userID)
		return nil, fmt.Errorf("withdraw target %s is a user deposit script (userID %s): "+
			"paying it would let the payee re-claim the same btc tx as a deposit (double mint), refused",
			req.toAddress, userID)
	}

	return []*wire.TxOut{{
		Value:    int64(req.amount),
		PkScript: pkScript,
	}}, nil
}

// buildTransaction 手动构建交易
// 返回: (交易, 输入金额列表, 选中的UTXO列表, 错误)
func (b *btcWallet) buildTransaction(outputs []*wire.TxOut, feeRate btcutil.Amount, chain33Hash []byte,
	receiverPaysFee bool, stickyUTXO *UTXO) (*wire.MsgTx, []int64, []*UTXO, error) {
	// 计算输出总额
	var outputTotal btcutil.Amount
	for _, output := range outputs {
		outputTotal += btcutil.Amount(output.Value)
	}

	hashStr := hex.EncodeToString(chain33Hash)
	selectionFeeRate := feeRate
	if receiverPaysFee {
		selectionFeeRate = 0
	}
	selectedUTXOs, inputTotal, err := b.selectWithdrawInputs(outputTotal, selectionFeeRate, stickyUTXO)
	if err != nil {
		log.Error("buildTransaction select withdraw inputs failed", "hash", hashStr, "targetAmount", outputTotal, "err", err)
		return nil, nil, nil, err
	}

	tx, inputAmounts, err := b.buildTransactionFromInputs(outputs, selectedUTXOs, inputTotal, feeRate, receiverPaysFee, chain33Hash)
	if err != nil {
		// 构建失败时释放锁定（sticky 输入按既有口径保留）。
		b.releaseUTXOsExcept(selectedUTXOs, stickyUTXO)
		return nil, nil, nil, err
	}
	return tx, inputAmounts, selectedUTXOs, nil
}

// buildTransactionFromInputs 用**已选定并锁定**的输入组装交易。首次提现与加价替换共用它，
// 保证两条路径的输入顺序、OP_RETURN 承诺、找零与费率计算完全一致。
func (b *btcWallet) buildTransactionFromInputs(outputs []*wire.TxOut, selectedUTXOs []*UTXO,
	inputTotal btcutil.Amount, feeRate btcutil.Amount, receiverPaysFee bool,
	chain33Hash []byte) (*wire.MsgTx, []int64, error) {

	var outputTotal btcutil.Amount
	for _, output := range outputs {
		outputTotal += btcutil.Amount(output.Value)
	}
	hashStr := hex.EncodeToString(chain33Hash)

	// 创建交易
	tx := wire.NewMsgTx(wire.TxVersion)

	// 添加输入（全部 signal BIP125 RBF）
	inputAmounts := make([]int64, 0, len(selectedUTXOs))
	for _, utxo := range selectedUTXOs {
		txIn := wire.NewTxIn(&utxo.OutPoint, nil, nil)
		txIn.Sequence = withdrawInputSequence
		tx.AddTxIn(txIn)
		inputAmounts = append(inputAmounts, int64(utxo.Amount))
	}

	buf := make([]byte, 0, withdrawOpReturnDataLen)
	buf = append(buf, []byte(withdrawOpReturnPrefix)...)
	buf = append(buf, chain33Hash...)
	opScript, err := txscript.NullDataScript(buf)
	if err != nil {
		log.Error("build tx op script failed", "hash", hashStr, "err", err)
		return nil, nil, err
	}
	tx.AddTxOut(wire.NewTxOut(0, opScript))
	// 添加输出
	for _, output := range outputs {
		tx.AddTxOut(output)
	}

	// 计算实际手续费
	fee := estimateBtcFee(tx, feeRate)

	// 计算找零
	change := inputTotal - outputTotal
	if receiverPaysFee {
		// 提现输出金额过小，则不构建交易
		if fee+minChangeAmount >= outputTotal {
			log.Error("buildTransaction withdraw amount too small for fee", "hash", hashStr, "targetAmount", outputTotal,
				"fee", fee, "change", change)
			return nil, nil, fmt.Errorf("withdraw amount too small for fee: amount %d, fee %d", outputTotal, fee)
		}
		outputs[0].Value = int64(outputTotal - fee)
	} else {
		change = inputTotal - outputTotal - fee
	}

	// 注意：如果找零过小，会被用于交易费，由平台承担
	// 还有种方案是将这部分补贴给提现地址，减少用户提现成本
	if change > minChangeAmount {
		// 添加找零输出（使用预计算的脚本）
		tx.AddTxOut(wire.NewTxOut(int64(change), b.tssPkScript))
	} else if change < 0 {
		// 资金不足
		log.Error("buildTransaction insufficient funds", "hash", hashStr, "targetAmount", outputTotal,
			"fee", fee, "change", change)
		return nil, nil, fmt.Errorf("insufficient funds: need %d, have %d", outputTotal+fee, inputTotal)
	}

	log.Debug("buildTransaction success", "inputs", len(selectedUTXOs), "inputTotal", inputTotal,
		"outputTotal", outputTotal, "fee", fee, "change", change)

	return tx, inputAmounts, nil
}

// errWithdrawBumpRateNotHigher 本次加价无法产出严格更高的费率：要么用户请求的费率不高于已生效
// 费率（过期/重复请求），要么按 dust 上限封顶后仍无法比上一版至少高 1 sat/vB（BIP125 的替换要求）。
// 调用方按"本次不签"处理（WARN），不写终态、不报错。
var errWithdrawBumpRateNotHigher = errors.New("withdraw bump cannot produce a higher fee rate")

// buildWithdrawReplacementTx 构造一笔加价替换交易。
//
// 复用首次提现的选择/组装逻辑，只额外保证：
//   - 绑定输入（sticky）必须作为输入出现，保证两版交易互斥，最多一笔能上链；
//   - 费率取 min(用户请求, dust 上限)：用户输出 = 原 pending 金额 - 手续费，必须 >= dust；
//   - 实际费率必须严格高于本地已生效费率（BIP125 要求至少 +1 sat/vB）。
func (b *btcWallet) buildWithdrawReplacementTx(req *withdrawRequest,
	outputs []*wire.TxOut) (*wire.MsgTx, []int64, []*UTXO, error) {

	if req.bump == nil || req.stickyUTXO == nil {
		return nil, nil, nil, fmt.Errorf("withdraw replacement requires a bump context and the bound sticky input")
	}
	requested := int64(req.bump.requestedFeeRate)
	applied := int64(req.bump.appliedFeeRate)
	if requested <= applied {
		return nil, nil, nil, errWithdrawBumpRateNotHigher
	}

	// 选择规则与首次提现完全一致（receiverPaysFee ⇒ 输入选择不预留手续费），sticky 会被追加为
	// 最后一个输入；主池不够时再补其它 UTXO。
	selected, inputTotal, err := b.selectWithdrawInputs(btcutil.Amount(req.amount), 0, req.stickyUTXO)
	if err != nil {
		return nil, nil, nil, err
	}
	release := func() { b.releaseUTXOsExcept(selected, req.stickyUTXO) }

	// 硬要求：sticky 必须真的在输入集合里，否则无法保证与上一版互斥。
	if !containsOutPoint(selected, req.stickyUTXO.OutPoint) {
		release()
		return nil, nil, nil, fmt.Errorf("withdraw replacement did not reuse the bound sticky input %s",
			req.stickyUTXO.OutPoint.String())
	}

	// 手续费按"尚未加入找零输出"的交易大小计算（与 buildTransactionFromInputs 同口径）。
	// 交易结构（输入数量、目标脚本长度）与费率无关，所以先用 0 费率组装一次探测交易，
	// 从它反解出精确的计费大小；这样不必假设收款地址是 P2WPKH。
	probe, _, err := b.buildTransactionFromInputs(outputs, selected, inputTotal, 0, true, req.chain33WithdrawHash)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	baseSize, err := withdrawFeeBaseSize(probe, b.tssPkScript)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	maxFeasible := maxWithdrawFeeRate(int64(req.amount), baseSize)
	if maxFeasible <= 0 {
		release()
		return nil, nil, nil, fmt.Errorf("withdraw %s is too small to pay a replacement fee while keeping the output above dust",
			hex.EncodeToString(req.chain33WithdrawHash))
	}
	effective := requested
	if effective > maxFeasible {
		effective = maxFeasible
	}
	if effective <= applied {
		release()
		return nil, nil, nil, errWithdrawBumpRateNotHigher
	}
	req.bump.effectiveFeeRate = btcutil.Amount(effective)

	tx, inputAmounts, err := b.buildTransactionFromInputs(outputs, selected, inputTotal,
		btcutil.Amount(effective), true, req.chain33WithdrawHash)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	// 签名端还要求"新交易绝对手续费严格高于上一版"。输入集合可能变化（上一版多选了 UTXO），
	// 高费率不必然带来高绝对手续费；在发起 TSS 轮次前先算清楚，避免签名端拒签后干等超时。
	if req.bump.prevSignedTx != nil &&
		len(req.bump.prevInputAmounts) == len(req.bump.prevSignedTx.TxIn) {
		var prevFee int64
		for _, amount := range req.bump.prevInputAmounts {
			prevFee += amount
		}
		for _, out := range req.bump.prevSignedTx.TxOut {
			prevFee -= out.Value
		}
		var newFee int64
		for _, amount := range inputAmounts {
			newFee += amount
		}
		for _, out := range tx.TxOut {
			newFee -= out.Value
		}
		if prevFee > 0 && newFee <= prevFee {
			release()
			return nil, nil, nil, fmt.Errorf("%w: new absolute fee %d is not above the previous fee %d",
				errWithdrawBumpRateNotHigher, newFee, prevFee)
		}
	}
	// 后续 validateWithdrawTx 用同一费率核对手续费。
	req.feeRate = btcutil.Amount(effective)

	log.Info("buildWithdrawReplacementTx", "burnTxHash", hex.EncodeToString(req.chain33WithdrawHash),
		"requestedFeeRate", requested, "effectiveFeeRate", effective, "maxFeasibleFeeRate", maxFeasible,
		"stickyInput", req.stickyUTXO.OutPoint.String(), "inputs", len(selected),
		"btcTxid", tx.TxHash().String())
	return tx, inputAmounts, selected, nil
}

// containsOutPoint 判断输入集合里是否有指定 outpoint。
func containsOutPoint(utxos []*UTXO, op wire.OutPoint) bool {
	for _, utxo := range utxos {
		if utxo != nil && utxo.OutPoint == op {
			return true
		}
	}
	return false
}

// maxWithdrawFeeRate 是"用户输出仍 >= dust"允许的最大费率（sat/vB，向下取整）。
// 与 buildTransactionFromInputs 的边界保持一致：fee + minChangeAmount >= amount 即拒绝。
func maxWithdrawFeeRate(amount int64, baseSize int) int64 {
	if baseSize <= 0 {
		return 0
	}
	maxFee := amount - minChangeAmount - 1
	if maxFee <= 0 {
		return 0
	}
	return maxFee / int64(baseSize)
}

// withdrawFeeBaseSize 返回提现交易的"计费大小"：未签交易的序列化大小 + 每输入 108 字节的
// witness 估算，再扣除找零输出（手续费在加入找零输出之前计算，见 buildTransactionFromInputs）。
// 它让签名节点能从交易本身反解出实际费率，而不需要相信通知里的任何声称值。
func withdrawFeeBaseSize(tx *wire.MsgTx, poolScript []byte) (int, error) {
	if tx == nil || len(tx.TxIn) == 0 {
		return 0, fmt.Errorf("withdraw tx has no inputs")
	}
	size := tx.SerializeSize() + len(tx.TxIn)*108
	if len(poolScript) > 0 {
		for _, out := range tx.TxOut {
			if bytes.Equal(out.PkScript, poolScript) {
				size -= 8 + wire.VarIntSerializeSize(uint64(len(out.PkScript))) + len(out.PkScript)
			}
		}
	}
	if size <= 0 {
		return 0, fmt.Errorf("withdraw tx fee base size is not positive")
	}
	return size, nil
}

// listUnspent 获取可用UTXO
func (b *btcWallet) listUnspent() ([]*UTXO, error) {

	// 获取钱包中的未花费输出
	unspentOutputs, err := b.Wallet.ListUnspent(b.requiredConfs, math.MaxInt32, "")
	if err != nil {
		return nil, fmt.Errorf("list unspent from wallet failed: %w", err)
	}

	var utxos []*UTXO
	totalAmount := btcutil.Amount(0)
	for _, output := range unspentOutputs {
		// 解析OutPoint
		txHash, err := chainhash.NewHashFromStr(output.TxID)
		if err != nil {
			log.Error("listUnspent invalid txid", "txid", output.TxID, "err", err)
			continue
		}

		// 转换金额
		amount, err := btcutil.NewAmount(output.Amount)
		if err != nil {
			log.Error("listUnspent invalid amount", "amount", output.Amount, "err", err)
			continue
		}

		// 解析脚本
		pkScript, err := hex.DecodeString(output.ScriptPubKey)
		if err != nil {
			log.Error("listUnspent invalid script", "script", output.ScriptPubKey, "err", err)
			continue
		}
		if !bytes.Equal(pkScript, b.tssPkScript) {
			log.Debug("listUnspent skip non-tss utxo", "txid", output.TxID, "vout", output.Vout, "amount", amount)
			continue
		}
		// RGB seal 排除（HR-5）：seal（含 pending-mint）不得进 BTC 提现费池，否则会花掉 RGB 资产背书。
		if b.client.rgb20 != nil {
			op := wire.OutPoint{Hash: *txHash, Index: output.Vout}
			if b.client.rgb20.IsSealOutpoint(op.String()) {
				log.Debug("listUnspent skip rgb seal", "outpoint", op.String(), "amount", amount)
				continue
			}
		}

		utxo := &UTXO{
			OutPoint: wire.OutPoint{
				Hash:  *txHash,
				Index: output.Vout,
			},
			Amount:   amount,
			PkScript: pkScript,
		}
		utxos = append(utxos, utxo)
		totalAmount += amount
	}

	log.Debug("listUnspent", "count", len(utxos), "totalAmount", totalAmount.ToBTC())
	return utxos, nil
}

// selectWithdrawInputs lists, selects and leases the inputs of a withdrawal. Tests may
// inject selectInputsFn when no wallet is available.
func (b *btcWallet) selectWithdrawInputs(target, feeRate btcutil.Amount, sticky *UTXO) ([]*UTXO, btcutil.Amount, error) {
	if b.selectInputsFn != nil {
		return b.selectInputsFn(target, feeRate, sticky)
	}
	utxos, err := b.listUnspent()
	if err != nil {
		return nil, 0, fmt.Errorf("list unspent failed: %w", err)
	}
	// 选择并锁定UTXO（防止并发双花）
	return b.selectAndLockUTXOs(utxos, target, feeRate, sticky)
}

// selectUTXOs 找到最少数量的UTXO组合
// 策略：按金额从大到小排序，依次选择直到满足需求
func (b *btcWallet) selectUTXOs(utxos []*UTXO, targetAmount, feeRate btcutil.Amount, stickyCount int) ([]*UTXO, btcutil.Amount, error) {
	// 按金额从小到大排序
	sorted := make([]*UTXO, len(utxos))
	copy(sorted, utxos)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Amount < sorted[j].Amount
	})

	//从小到大，找第一个大于等于amount的UTXO索引，如果没有则返回最大的
	findFirstGreaterOrEqual := func(sorted []*UTXO, amount btcutil.Amount) int {

		for i := 0; i < len(sorted); i++ {
			if sorted[i].Amount >= amount {
				return i
			}
		}
		return len(sorted) - 1
	}

	var selected []*UTXO
	var total btcutil.Amount
	for len(sorted) > 0 {
		// 估算当前手续费
		txSize := b.estimateTxSize(len(selected)+stickyCount+1, 2, withdrawOpReturnScriptLen)
		fee := btcutil.Amount(txSize) * feeRate
		needed := targetAmount + fee - total
		idx := findFirstGreaterOrEqual(sorted, needed)
		selected = append(selected, sorted[idx])
		total += sorted[idx].Amount
		sorted = sorted[:idx]
		// 检查是否满足需求
		if targetAmount+fee <= total {
			log.Debug("selectUTXOs success", "selected", len(selected), "total", total,
				"target", targetAmount, "fee", fee, "waste", total-targetAmount-fee)
			return selected, total, nil
		}
	}

	log.Debug("selectUTXOs insufficient funds", "target", targetAmount, "total", total)
	return nil, 0, fmt.Errorf("insufficient funds: need %d, have %d", targetAmount, total)
}

// estimateTxSize 估算交易大小
// inputCount: 输入数量
// p2wpkhOutputCount: P2WPKH输出数量
// opReturnScriptSize: OP_RETURN脚本长度（字节）
// 返回: 交易大小（字节）
func (b *btcWallet) estimateTxSize(inputCount, p2wpkhOutputCount, opReturnScriptSize int) int {
	// 基本交易大小
	baseSize := 10 // version(4) + locktime(4) + input_count(1) + output_count(1)

	// P2WPKH输入大小
	// - OutPoint: 36字节 (txid:32 + index:4)
	// - ScriptSig: 1字节 (空)
	// - Sequence: 4字节
	// - Witness: ~108字节 (signature:72 + pubkey:33 + witness_count:1 + lengths:2)
	inputSize := inputCount * (36 + 1 + 4 + 108)

	// P2WPKH输出大小
	// - Value: 8字节
	// - ScriptPubKey: 23字节 (OP_0 + 20字节pubkey hash)
	outputSize := p2wpkhOutputCount * (8 + 23)
	if opReturnScriptSize > 0 {
		outputSize += 8 + 1 + opReturnScriptSize // value + script len + script
	}

	return baseSize + inputSize + outputSize
}

// selectAndLockUTXOs 选择并锁定UTXO（用于提现交易）
// 使用wallet.LeaseOutput实现持久化锁定，防止UTXO被重复使用
func (b *btcWallet) selectAndLockUTXOs(utxos []*UTXO, targetAmount, feeRate btcutil.Amount, stickyUTXO *UTXO) ([]*UTXO, btcutil.Amount, error) {

	log.Debug("selectAndLockUTXOs", "total", len(utxos), "targetAmount", targetAmount,
		"feeRate", feeRate, "stickyUTXO", stickyUTXO != nil)

	stickyCount := 0
	if stickyUTXO != nil {
		targetAmount -= stickyUTXO.Amount
		// 如果剩余所需金额为0，且没有其他UTXO，则直接返回指定输入
		if len(utxos) == 0 && targetAmount <= 0 {
			return []*UTXO{stickyUTXO}, stickyUTXO.Amount, nil
		}
		stickyCount = 1
	}

	selected, total, err := b.selectUTXOs(utxos, targetAmount, feeRate, stickyCount)
	if err != nil {
		log.Error("selectAndLockUTXOs selectUTXOs failed", "utxos", len(utxos), "targetAmount", targetAmount,
			"feeRate", feeRate, "err", err)
		return nil, 0, err
	}
	// 如果存在指定输入，则将其添加到选中的UTXO列表中，并累加金额
	if stickyUTXO != nil {
		selected = append(selected, stickyUTXO)
		total += stickyUTXO.Amount
	}

	// 锁定选中的UTXO
	lockedUTXOs := make([]*UTXO, 0, len(selected))
	for _, utxo := range selected {
		// 使用wallet.LeaseOutput锁定UTXO
		expiry, err := b.Wallet.LeaseOutput(utxoLockID, utxo.OutPoint, utxoLeaseDuration)
		if err != nil {
			log.Error("selectAndLockUTXOs lease output failed", "outpoint", utxo.OutPoint.String(), "err", err)
			// 锁定失败，释放已锁定的UTXO
			b.releaseUTXOsExcept(lockedUTXOs, stickyUTXO)
			return nil, 0, fmt.Errorf("lease output failed: %w", err)
		}

		lockedUTXOs = append(lockedUTXOs, utxo)
		log.Debug("selectAndLockUTXOs locked UTXO", "outpoint", utxo.OutPoint.String(), "amount", utxo.Amount, "expiry", expiry)
	}

	log.Debug("selectAndLockUTXOs success", "utxos", len(utxos), "selected", len(lockedUTXOs),
		"totalAmount", total, "targetAmount", targetAmount, "feeRate", feeRate)

	return lockedUTXOs, total, nil
}

// releaseUTXOsExcept 释放UTXO锁定，可选保留指定输入不释放
func (b *btcWallet) releaseUTXOsExcept(utxos []*UTXO, keep *UTXO) {
	if len(utxos) == 0 {
		return
	}

	log.Debug("releaseUTXOs start", "count", len(utxos))

	for _, utxo := range utxos {
		if keep != nil && utxo.OutPoint == keep.OutPoint {
			log.Debug("releaseUTXOsExcept keep sticky UTXO", "outpoint", utxo.OutPoint.String(), "amount", utxo.Amount)
			continue
		}
		err := b.Wallet.ReleaseOutput(utxoLockID, utxo.OutPoint)
		if err != nil {
			log.Error("releaseUTXOs release output failed",
				"outpoint", utxo.OutPoint.String(),
				"err", err)
		}
	}
}

// UTXO 结构
type UTXO struct {
	OutPoint wire.OutPoint
	Amount   btcutil.Amount
	PkScript []byte
}

// sendRawTransaction submits a fully signed transaction without logging. Callers choose
// the log policy (initial broadcast vs. periodic replay).
func (b *btcWallet) sendRawTransaction(tx *wire.MsgTx) error {
	if b.sendRawTxFn != nil {
		return b.sendRawTxFn(tx)
	}
	if b.rpcClient != nil {
		_, err := b.rpcClient.SendRawTransaction(tx, false)
		return err
	}
	_, err := b.chainClient.SendRawTransaction(tx, false)
	return err
}

// broadcastTransaction 广播交易
// lockedUTXOs: 已锁定的UTXO列表，广播失败时会自动释放
func (b *btcWallet) broadcastTransaction(tx *wire.MsgTx, btcTxHash string) error {
	if err := b.sendRawTransaction(tx); err != nil {
		log.Error("BroadcastTransaction failed", "txHash", btcTxHash, "err", err)
		return err
	}
	log.Debug("broadcastTransaction success", "txHash", btcTxHash)
	return nil
}

// broadcastSignedWithdrawTx broadcasts a signed withdrawal and, once the broadcast has
// succeeded, records its final bytes for periodic replay. A persistence failure is logged
// but does not fail the call: the broadcast already happened.
func (b *btcWallet) broadcastSignedWithdrawTx(tx *wire.MsgTx, btcTxHash string, burnTxHash []byte) error {
	if err := b.broadcastTransaction(tx, btcTxHash); err != nil {
		return err
	}
	if b.client != nil {
		b.client.persistSignedWithdrawTxBestEffort(burnTxHash, tx)
	}
	return nil
}

func buildBtcSpv(txHash, blockHash string, blockTime int64, blockHeight uint64, txs [][]byte, txIndex uint32) *ltypes.BtcSpv {
	return &ltypes.BtcSpv{
		TxHash:      txHash,
		Time:        blockTime,
		Height:      blockHeight,
		BlockHash:   blockHash,
		TxIndex:     txIndex,
		BranchProof: merkle.GetMerkleBranch(txs, txIndex),
	}
}

func buildTxHashesFromVerbose(txIDs []string, targetTxHash string) ([][]byte, uint32, error) {
	txs := make([][]byte, 0, len(txIDs))
	txIndex := uint32(0)
	found := false
	for idx, txID := range txIDs {
		hash, err := chainhash.NewHashFromStr(txID)
		if err != nil {
			return nil, 0, err
		}
		txs = append(txs, hash.CloneBytes())
		if txID == targetTxHash {
			txIndex = uint32(idx)
			found = true
		}
	}
	if !found {
		return nil, 0, fmt.Errorf("tx not found in block")
	}
	return txs, txIndex, nil
}

func buildTxHashesFromBlockTxs(blockTxs []*wire.MsgTx, targetTxHash chainhash.Hash) ([][]byte, uint32, error) {
	txs := make([][]byte, 0, len(blockTxs))
	txIndex := uint32(0)
	found := false
	for idx, tx := range blockTxs {
		hash := tx.TxHash()
		txs = append(txs, hash.CloneBytes())
		if hash == targetTxHash {
			txIndex = uint32(idx)
			found = true
		}
	}
	if !found {
		return nil, 0, fmt.Errorf("tx not found in block")
	}
	return txs, txIndex, nil
}

// buildTxExistenceProof 计算交易存在性证明（SPV）
// 输入: pendingTx（需要包含 tx 和 blockHash）
// 输出: ltypes.BtcSpv
//
// getblockverbose 路径带块级缓存：同一区块里的 N 笔充值只取一次块。缓存键是 blockHash，
// 块内容对给定 hash 不可变（重组后是同高度、不同 hash 的另一个块），因此无需失效逻辑。
// 缓存取块失败时退回原来的 neutrino 路径；缓存本身绝不会让证明失败或返回空证明。
func (b *btcWallet) buildTxExistenceProof(pending *btcPendingTx) (*ltypes.BtcSpv, error) {
	if pending == nil || pending.tx == nil {
		return nil, fmt.Errorf("pending tx data missing")
	}
	if pending.blockHeight <= 0 {
		return nil, fmt.Errorf("invalid pending block height")
	}
	txHashStr := pending.tx.TxHash().String()
	if fetch := b.blockVerboseEndpoint(); fetch != nil {
		meta, err := b.blockCache.getOrLoad(pending.blockHash, func(hash chainhash.Hash) (*btcBlockMeta, error) {
			block, err := fetch(&hash)
			if err != nil {
				return nil, err
			}
			if block == nil {
				return nil, fmt.Errorf("empty block result")
			}
			return &btcBlockMeta{
				txIDs:  block.Tx,
				time:   block.Time,
				height: block.Height,
			}, nil
		})
		if err != nil {
			log.Error("buildTxExistenceProof GetBlockVerbose failed, fallback neutrino",
				"txHash", txHashStr, "blockHash", pending.blockHash.String(), "err", err)
		} else {
			txs, txIndex, err := buildTxHashesFromVerbose(meta.txIDs, txHashStr)
			if err != nil {
				log.Error("buildTxExistenceProof buildTxHashesFromVerbose failed",
					"txHash", txHashStr, "blockHash", pending.blockHash.String(), "err", err)
				return nil, err
			}
			return buildBtcSpv(
				txHashStr, pending.blockHash.String(), meta.time, uint64(meta.height), txs, txIndex,
			), nil
		}
	}

	block, err := b.chainClient.GetBlock(&pending.blockHash)
	if err != nil {
		log.Error("buildTxExistenceProof GetBlock failed",
			"txHash", txHashStr, "blockHash", pending.blockHash.String(), "err", err)
		return nil, err
	}

	txs, txIndex, err := buildTxHashesFromBlockTxs(block.Transactions, pending.txHash)
	if err != nil {
		log.Error("buildTxExistenceProof buildTxHashesFromBlockTxs failed",
			"txHash", txHashStr, "blockHash", pending.blockHash.String(), "err", err)
		return nil, err
	}
	return buildBtcSpv(
		txHashStr, pending.blockHash.String(), block.Header.Timestamp.Unix(), uint64(pending.blockHeight), txs, txIndex,
	), nil
}

// GetBalance 获取余额
func (b *btcWallet) getBalance() (btcutil.Amount, error) {

	// 获取已确认余额
	balance, err := b.Wallet.CalculateBalance(b.requiredConfs)
	if err != nil {
		return 0, fmt.Errorf("calculate balance failed: %w", err)
	}

	return balance, nil
}

// GetDepositChannel 获取充值通知channel
func (b *btcWallet) GetDepositChannel() <-chan *btcPendingTx {
	return b.depositChan
}

// GetWithdrawChannel 获取提现通知channel
func (b *btcWallet) GetWithdrawChannel() <-chan *btcPendingTx {
	return b.withdrawChan
}
