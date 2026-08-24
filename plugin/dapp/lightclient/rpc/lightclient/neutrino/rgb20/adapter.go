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
	// GetMainchainHeight 返回主链最新高度。
	GetMainchainHeight() int64
	// BuildSpvProof 构造付款 BTC 交易的存在性证明（SPV，对 lightclient 头）。
	BuildSpvProof(txid string) (*SpvProof, error)
	// VerifyDepositSpv 签名节点独立验证充值 SPV 证明（对 lightclient 头）。
	VerifyDepositSpv(proof *rtypes.BtcTxProof) error
	// SubmitDeposit 提交 rgbx Deposit 交易（RGB20 分支：验 threshold_sig 后铸造）。
	SubmitDeposit(dep *rtypes.DepositAsset) error
	// SubmitConfirm 提交 rgbx Confirm 交易（RGB20 提现确认销毁）。
	SubmitConfirm(confirm *rtypes.ConfirmTx) error
	// SignDepositMessage 执行 rgb20-deposit TSS 签名轮次，返回阈值签名（DER）。
	SignDepositMessage(payload *DepositSignPayload) ([]byte, error)
	// SignPsbt 通过 TSS 对 PSBT 签名，返回已签 PSBT 字节。
	SignPsbt(psbtBytes []byte) ([]byte, error)
	// GetBtcTipHeight 返回本地 lightclient 已知的 BTC 链尖高度。
	GetBtcTipHeight() int64
	// BroadcastTx 广播已签提现交易（由 neutrino 主包实现，走 btcwallet）。
	BroadcastTx(psbtSigned []byte, txid string) error
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
		cfg:      cfg,
		store:    store,
		receives: newReceiveStore(store),
		seals:    newSealIndex(store),
		reg:      newRegistry(cfg.Contracts),
		ctx:      ctx,
		cancel:   cancel,
	}
	return a, nil
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
