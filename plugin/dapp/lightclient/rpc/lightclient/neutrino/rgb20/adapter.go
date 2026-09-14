package rgb20

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// Config RGB20 适配器配置（由 neutrino 主包的 rgb20Config 映射而来）。
type Config struct {
	// SidecarAddr 侧车地址（unix socket 或 tcp host:port）。
	SidecarAddr string
	// ConsignmentListen consignment 上传/充值请求 HTTP 监听地址。空则不开 HTTP。
	ConsignmentListen string
	// ChangeAddress 提现找零地址（TSS P2WPKH 地址）。
	ChangeAddress string
	// Contracts 注册的 RGB20 资产合约。
	Contracts []Contract
	// Precision 默认精度（若合约未指定，RGB20 USDT 为 6）。
	Precision uint32
	// MinConfirmations 充值铸造所需 BTC 确认数（默认与桥的 blockConfirmations 对齐）。
	MinConfirmations uint32
	// HeaderRelayConfirmations 中继提交头链时**保留的确认深度** B
	// （= [rpc.sub.light.neutrino].blockConfirmations，默认 6）。
	//
	// 头链提交只到 best - B（neutrino/bitcoin.go 的 btcConfirmedHeight），因此中继提交任何东西的
	// 那一刻，链上可见的头链 tip 至多到 best - B。充值提交前的本地深度门控
	// （deposit.go 的 checkSubmitDepth）用它把链上判据换算成本地判据：
	//
	//	链上：canonical tip >= H + N - 1   （H = 付款交易高度，N = 链上 minBtcConfirmations）
	//	本地：best         >= H + B + N - 1
	//
	// 0 = 未配置（等价于"头链不保留深度"，只影响门控的保守程度，不影响链上校验）。
	HeaderRelayConfirmations uint32
	// SignedDepositTTL 签名侧"已签集合"的保留期（TTL），单位 = BTC 区块数。
	// <= 0（默认 0）= 只增不删（永久保留）；正数 = 已签记录保留 N 个 BTC 块，到期后可被清理、
	// 同一 txid 允许再次签名。语义与重启行为见 CONFIG.md 4.4 与 signedset.go。
	SignedDepositTTL int64
}

// Contract 一个 RGB20 资产的合约注册项。
type Contract struct {
	Symbol        string `json:"symbol"`        // chain33 侧资产符号，如 RGB20_USDT（Registry 索引键）
	SidecarSymbol string `json:"sidecarSymbol"` // 侧车发行资产符号，如 USDT；空则回退 Symbol
	AssetID       string `json:"assetId"`       // rgb:...
	Precision     uint32 `json:"precision"`     // 资产小数位
	MinDeposit    int64  `json:"minDeposit"`    // 最小充值（最小单位）
	MinWithdraw   int64  `json:"minWithdraw"`   // 最小提现（最小单位）
}

// sidecarAssetSymbol 返回调用侧车 RPC 时使用的资产符号。
func (c *Contract) sidecarAssetSymbol() string {
	if c != nil && c.SidecarSymbol != "" {
		return c.SidecarSymbol
	}
	if c != nil {
		return c.Symbol
	}
	return ""
}

// Registry 合约注册表（按 symbol 索引）。
type Registry struct {
	mu        sync.RWMutex
	contracts map[string]*Contract
}

func newRegistry(contracts []Contract) *Registry {
	reg := &Registry{contracts: make(map[string]*Contract, len(contracts))}
	for i := range contracts {
		c := contracts[i]
		if c.Symbol != "" {
			reg.contracts[c.Symbol] = &c
		}
	}
	return reg
}

// Register 注册合约。
func (r *Registry) Register(c *Contract) {
	if c == nil || c.Symbol == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *c
	r.contracts[c.Symbol] = &cp
}

// Get 按 symbol 取合约。
func (r *Registry) Get(symbol string) (*Contract, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.contracts[symbol]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// IsRegistered 判断 symbol 是否已注册的 RGB20 合约。
func (r *Registry) IsRegistered(symbol string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.contracts[symbol]
	return ok
}

// Symbols 返回全部已注册的合约符号（chain33 侧符号）。
func (r *Registry) Symbols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.contracts))
	for symbol := range r.contracts {
		out = append(out, symbol)
	}
	return out
}

// Chain33Bridge 是 rgb20 适配器回连 neutrino 主包所需的链上/签名接口。
// 由 neutrino 主包实现，注入适配器（避免 rgb20 -> neutrino 的导入环）。
type Chain33Bridge interface {
	// GetMainChainHeight 返回主链最新高度。
	GetMainChainHeight() int64
	// BuildSpvProof 构造付款 BTC 交易的存在性证明（SPV，对 lightclient 头）。
	BuildSpvProof(txid string) (*SpvProof, error)
	// VerifyDepositSpv 签名节点独立验证充值 SPV 证明（对 lightclient 头）。
	// 实现方必须同时拒绝非规范编码的 TxData（尾部多余字节，A3）：txid 口径的 SPV 本身
	// 无法区分"同一笔交易的另一份编码"，见 neutrino/rgb20deposit.go。
	VerifyDepositSpv(proof *rtypes.BtcTxProof) error
	// SubmitDeposit 提交 rgbx Deposit 交易（RGB20 分支：验 threshold_sig 后铸造）。
	SubmitDeposit(dep *rtypes.DepositAsset) error
	// SubmitConfirm 提交 rgbx Confirm 交易（RGB20 提现确认销毁）。
	SubmitConfirm(confirm *rtypes.ConfirmTx) error
	// SignDepositMessage 执行 rgb20-deposit TSS 签名轮次，返回阈值签名（DER）。
	SignDepositMessage(payload *DepositSignPayload) ([]byte, error)
	// SignPsbt 通过 TSS 对 PSBT 签名，返回已签 PSBT 字节。
	SignPsbt(psbtBytes []byte) ([]byte, error)
	// BtcTipHeight 返回链上 lightclient 头链的 canonical tip 高度（BTC 高度）。
	// 已签集合的 TTL 判定用它当"当前高度"：所有节点（含没有本地 neutrino 头库的 validator 节点）
	// 都查主链，结果一致、单调，且不受本机时钟漂移影响；链上还没有任何头时返回 0。
	BtcTipHeight() (uint64, error)
	// BtcBestHeight 返回本节点 BTC 视图的 best height（中继自己的头链视图，与头链提交同源）。
	// 充值提交前的本地深度门控用它算"提交那一刻链上可见 tip 到哪"；取不到时返回错误（门控 fail-closed）。
	BtcBestHeight() (uint64, error)
	// RgbxMinBtcConfirmations 返回链上 rgbx 执行器生效的最小 BTC 确认数 N（B8）。
	// 中继不镜像这个值，直接向链上查询（N 的单一真相 = [exec.sub.rgbx].minBtcConfirmations）；
	// 查询失败时返回错误（门控 fail-closed）。
	RgbxMinBtcConfirmations() (uint64, error)
	// BroadcastTx 广播已签提现交易（由 neutrino 主包实现，走 btcwallet）。
	BroadcastTx(psbtSigned []byte, txid string) error
	// TSSAddress 返回桥 TSS 的 P2WPKH 地址（regtest bcrt1…/testnet tb1…/mainnet bc1…）。
	// RGB20 提现的找零地址；config.changeAddress 留空时据此自动填充。
	TSSAddress() string
	// TSSPkScript 返回桥 TSS P2WPKH 输出的 pkScript（16 进制? 不，raw bytes）。
	// 签名节点交叉核对提现 PSBT 时，用它判断额外输入/找零输出是否受桥（TSS）控制。
	TSSPkScript() []byte
}

// RGB20Adapter 是 neutrino 主包使用的适配器接口。
type RGB20Adapter interface {
	// Connect 建立侧车 gRPC 连接（所有节点：签名节点用只读校验）。
	Connect(ctx context.Context) error
	// Start 启动充值轮询 + HTTP（官方节点）。
	Start(ctx context.Context) error
	Stop()
	// IsConnected 侧车连接是否已建立。
	IsConnected() bool
	// Sidecar 返回底层侧车客户端（用于签名节点只读校验）。
	Sidecar() *Sidecar
	// IsKnownRgbTxid 判断 txid 是否为已知 RGB 交易（充值收款 / 提现交易），
	// btcwallet.analyzeTransaction 分类排除用。
	IsKnownRgbTxid(txid string) bool
	// IsSealOutpoint 判断 outpoint 是否为已登记 RGB seal（含 pending-mint），
	// btcwallet.listUnspent 排除用。
	IsSealOutpoint(outpoint string) bool
	ReceiveStore() *ReceiveStore
	Seals() *SealIndex
	Registry() *Registry
	// Bridge 返回注入的链上桥接实现（可为空，测试用 mock）。
	Bridge() Chain33Bridge
	SetBridge(b Chain33Bridge)
	// ValidateDepositConsignment 签名节点对 rgb20-deposit 消息做独立校验。
	ValidateDepositConsignment(payload *DepositSignPayload) error
	// CheckDepositSigned 签名侧去重（A3 后半）：payload 的付款交易已签过则返回错误。
	CheckDepositSigned(payload *DepositSignPayload) error
	// MarkDepositSigned 登记 payload 的付款交易为已签（幂等，须在签名成功之后调用）。
	MarkDepositSigned(payload *DepositSignPayload) error
	// PruneSignedDeposits 按当前 TTL 清理过期的已签记录（启动/改配置重启后立即执行一次）。
	PruneSignedDeposits() error
	// ValidateWithdrawPsbt 签名节点对 rgb20 提现 PSBT+consignment 做交叉核对（BL-4/HR-3）。
	ValidateWithdrawPsbt(req *ValidateWithdrawRequest) error
	// BuildDepositSignMessage 构造 rgb20-deposit 签名消息（主节点侧）。
	BuildDepositSignMessage(rec *ReceiveRecord, consignment []byte, blockHeight uint64, blockHash string, txIndex uint32) (*DepositSignPayload, error)
}

// 编译期断言：*Adapter 满足 RGB20Adapter 接口。
var _ RGB20Adapter = (*Adapter)(nil)

// Adapter RGB20 适配器实现。
type Adapter struct {
	sidecar  atomic.Pointer[Sidecar]
	store    KVStore
	receives *ReceiveStore
	seals    *SealIndex
	reg      *Registry
	cfg      Config
	bridge   Chain33Bridge
	// signed 已签集合（签名侧去重，A3 后半）。
	signed *SignedDepositSet
	// depositSigs 已签充值的落盘产物（签名轮次的产出，提交失败/深度不够时只重发，见 deposit.go 与
	// depositsig.go）。
	depositSigs *DepositSignatureStore
	// depositNotes 充值重试路径的日志限流（30s 轮询会把同一个状态反复带回来，同一个 key 只报一次）。
	depositNotes *depositNoteOnce

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// withdrawMu 提现串行（一次只处理一笔，避免并发花同一 seal）。
	withdrawMu sync.Mutex
}

// NewAdapter 构造适配器。
func NewAdapter(cfg Config, store KVStore) (*Adapter, error) {
	if cfg.SidecarAddr == "" {
		return nil, fmt.Errorf("rgb20 sidecar addr empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adapter{
		cfg:          cfg,
		store:        store,
		receives:     newReceiveStore(store),
		seals:        newSealIndex(store),
		reg:          newRegistry(cfg.Contracts),
		ctx:          ctx,
		cancel:       cancel,
		depositNotes: newDepositNoteOnce(),
	}
	// 已签集合：TTL 判定的"当前高度"经 btcTipHeight 走桥接查链上 tip（SetBridge 之后才可用；
	// TTL <= 0 时根本不会调用它）。
	a.signed = newSignedDepositSet(store, cfg.SignedDepositTTL, a.btcTipHeight)
	// 已签充值的落盘产物：与 receive/seal/已签集合共用同一个 KVStore（不引入新存储引擎）。
	a.depositSigs = newDepositSignatureStore(store)
	return a, nil
}

// btcTipHeight 取链上 BTC canonical tip 高度（已签集合 TTL 判定用，见 signedset.go）。
func (a *Adapter) btcTipHeight() (uint64, error) {
	if a.bridge == nil {
		return 0, fmt.Errorf("chain33 bridge not set")
	}
	return a.bridge.BtcTipHeight()
}

// Connect 建立侧车 gRPC 连接（unix socket 优先）。所有节点都需要（签名节点只读校验）。
func (a *Adapter) Connect(ctx context.Context) error {
	if a.sidecar.Load() != nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sc, err := NewSidecar(ctx, SidecarConfig{Addr: a.cfg.SidecarAddr})
	if err != nil {
		return err
	}
	a.sidecar.Store(sc)
	return nil
}

// IsConnected 侧车连接是否已建立。
func (a *Adapter) IsConnected() bool {
	return a.sidecar.Load() != nil
}

// Start 启动充值轮询 + HTTP（仅官方节点调用）。非阻塞：
// 后台等待侧车连接成功后自动启动轮询与 HTTP（侧车可能晚于本节点启动）。
func (a *Adapter) Start(ctx context.Context) error {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// 等待侧车连接（Connect 由所有节点的后台 goroutine 建立，这里兜底重试）。
		for {
			if a.sidecar.Load() != nil {
				break
			}
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(3 * time.Second):
				if err := a.Connect(ctx); err != nil {
					log.Debug("Start rgb20 connect retry", "err", err)
				}
			}
		}
		a.wg.Add(1)
		go a.pollTransfers()
		if a.cfg.ConsignmentListen != "" {
			a.wg.Add(1)
			go a.serveHTTP(a.cfg.ConsignmentListen)
		}
		log.Info("rgb20 adapter started", "sidecar", a.cfg.SidecarAddr)
	}()
	return nil
}

// Stop 关闭适配器。
func (a *Adapter) Stop() {
	a.cancel()
	a.wg.Wait()
	if sc := a.sidecar.Load(); sc != nil {
		_ = sc.Close()
	}
}

func (a *Adapter) Sidecar() *Sidecar { return a.sidecar.Load() }

func (a *Adapter) IsKnownRgbTxid(txid string) bool {
	if a.receives == nil || txid == "" {
		return false
	}
	return a.receives.IsKnownRgbTxid(txid)
}

func (a *Adapter) IsSealOutpoint(outpoint string) bool {
	if a.seals == nil || outpoint == "" {
		return false
	}
	return a.seals.IsSealOutpoint(outpoint)
}

func (a *Adapter) ReceiveStore() *ReceiveStore { return a.receives }
func (a *Adapter) Seals() *SealIndex           { return a.seals }
func (a *Adapter) Registry() *Registry         { return a.reg }

func (a *Adapter) Bridge() Chain33Bridge     { return a.bridge }
func (a *Adapter) SetBridge(b Chain33Bridge) { a.bridge = b }

// FormatOutpoint 将 txid 与 vout 格式化为 "txid:vout"。
func FormatOutpoint(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}
