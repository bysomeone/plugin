package rgb20

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// Config RGB20 适配器配置（由 neutrino 主包的 rgb20Config 映射而来）。
type Config struct {
	// SidecarAddr 侧车地址（unix socket 或 tcp host:port）。
	SidecarAddr string
	// TestListen **仅测试用**监听地址（127.0.0.1:port 或 unix 路径），且必须同时打开
	// TestSignPsbt；生产留空。它只提供 E2E 的 sign-psbt 端点，桥不对外提供任何 HTTP API。
	TestListen string
	// TransportEndpoint 写进充值 invoice 的公网标准 transport endpoint
	// （如 `rpcs://gw.example.com/0.2/json-rpc`）。空 = invoice 不携带 transports。
	TransportEndpoint string
	// TransportGateway 桥轮询 consignment 的内网 RGB HTTP JSON-RPC 网关地址
	// （如 `http://gw:8080/0.2/json-rpc`）。空 = 不启用网关轮询。
	//
	// 与 TransportEndpoint 相互独立：前者是付款方看到的公网字符串，后者是桥自己使用的
	// 内网地址；两者可以相同，也可以经不同的入口指向同一台网关。
	TransportGateway string
	// GatewayToken /bridge/* 接口的 bearer token（充值请求流与 invoice 回写）。
	// 与 TransportGateway 同时配置才会订阅充值请求；空 = 不订阅（旧的自定义入口已删除）。
	GatewayToken string
	// GatewayMetricsInterval 主动推送运行指标到网关的间隔。<=0 = 关闭；
	// 未配置时由 neutrino 主包填入默认值（30s）。
	GatewayMetricsInterval time.Duration
	// NodeID 本节点在网关指标视图里的稳定标识（chain title + hostname + rank）。
	// 空则由适配器回退到 ChainTitle。
	NodeID string
	// ChainTitle 本链 Title：充值请求的签名消息里包含它，桥在出票前用它验签
	// （权威校验仍在铸造时的链上执行）。必须与网关的 chain-title 一致。
	ChainTitle string
	// ChangeAddress 提现找零地址（TSS P2WPKH 地址）。
	ChangeAddress string
	// Contracts 注册的 RGB20 资产合约。
	//
	// 这里**没有**默认精度：精度不是配置，而是合约自己的声明（见 ContractPrecisions）。
	Contracts []Contract
	// MinConfirmations 充值铸造所需 BTC 确认数（默认与桥的 blockConfirmations 对齐）。
	MinConfirmations uint32
	// ConsignmentPublish 提现 consignment 的外部交付配置（空 url = 只本地保存）。
	ConsignmentPublish ConsignmentPublishConfig
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
	// TestSignPsbt 是否允许"无提现上下文的 PSBT 签名"（E2E 的 sign-psbt 测试端点用）。
	//
	// 该能力等于"用 TSS 组私钥签任意 PSBT"：签名节点无法对被签内容做任何提现核对，因此
	// **生产环境必须为 false**（默认 false），只有 E2E/regtest 部署显式打开。打开时协调者侧
	// 的 HTTP 端点与签名节点侧的通知处理都会放行，并在此路径上打 WARN 日志。
	TestSignPsbt bool
	// Safety 节点本地（不参与共识）的安全开关。
	Safety SafetyConfig
}

// SafetyConfig is the node-local kill switch surface. These values are not consensus state:
// they only decide whether *this* node takes on new work, and an operator can change them
// without a governance round. Money that has already left the bridge (broadcast withdrawals,
// on-chain confirms) is not affected — only new settlement work stops.
type SafetyConfig struct {
	// DisableDeposits stops this node from signing/submitting new mints. Deposits already in
	// flight stay in the local receive store and resume after the switch is cleared.
	DisableDeposits bool `json:"disableDeposits"`
	// DisableWithdrawals stops this node from starting new withdrawal settlements. Broadcast
	// withdrawals are still confirmed (the money has already moved).
	DisableWithdrawals bool `json:"disableWithdrawals"`
	// MaxPayoutMismatchBurns trips the automatic withdrawal breaker once this many distinct
	// burns were refused with a payout mismatch. 0 keeps the default (3); a negative value
	// disables the breaker.
	MaxPayoutMismatchBurns int `json:"maxPayoutMismatchBurns"`
}

// Contract 一个 RGB20 资产的合约注册项。
//
// 精度不在这里：它是合约声明的事实（ContractPrecisions 从侧车读），不是可以手填的配置。
// 提现手续费也不在这里：它读链上 rgbx 治理配置的 rgb20WithdrawFee（按 symbol），见
// Adapter.WithdrawFeeFor。
type Contract struct {
	Symbol        string `json:"symbol"`        // chain33 侧资产符号，如 RGB20_USDT（Registry 索引键）
	SidecarSymbol string `json:"sidecarSymbol"` // 侧车发行资产符号，如 USDT；空则回退 Symbol
	AssetID       string `json:"assetId"`       // rgb:...
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
	// SubmitDeposit 提交 rgbx Deposit 交易（RGB20 分支：验 thresholdSig 后铸造）。
	SubmitDeposit(dep *rtypes.DepositAsset) error
	// SubmitConfirm 提交 rgbx Confirm 交易（RGB20 提现确认销毁）。
	SubmitConfirm(confirm *rtypes.ConfirmTx) error
	// SignDepositMessage 执行 rgb20-deposit TSS 签名轮次，返回阈值签名（DER）。
	SignDepositMessage(payload *DepositSignPayload) ([]byte, error)
	// SignPsbt 通过 TSS 组对 RGB20 提现 PSBT 签名，并把提现上下文（chain33 提现哈希、金额、
	// 费率、同步高度门槛、收款 invoice、consignment）一并下发给 TSS 组，供签名节点独立核对
	// （E11：签名节点必须拿到上下文才可能执行 ValidateWithdrawPsbt；缺上下文的签名一律拒签）。
	SignPsbt(req *WithdrawSignRequest) ([]byte, error)
	// SignPsbtTestOnly 仅 E2E 的 sign-psbt 测试端点使用：**无提现上下文**，签名节点不做任何
	// 提现核对就直接参与 CGGMP 组签名。生产提现路径永远不走这里（见 Config.TestSignPsbt）。
	SignPsbtTestOnly(psbtBytes []byte) ([]byte, error)
	// BtcBestHeight 返回本节点 BTC 视图的 best height（中继自己的头链视图，与头链提交同源）。
	// 充值提交前的本地深度门控用它算"提交那一刻链上可见 tip 到哪"；取不到时返回错误（门控 fail-closed）。
	BtcBestHeight() (uint64, error)
	// RgbxMinBtcConfirmations 返回链上 rgbx 执行器生效的最小 BTC 确认数 N（B8）。
	// 中继不镜像这个值，直接向链上查询（N 的单一真相 = [exec.sub.rgbx].minBtcConfirmations）；
	// 查询失败时返回错误（门控 fail-closed）。
	RgbxMinBtcConfirmations() (uint64, error)
	// BroadcastTx 广播已签提现交易（由 neutrino 主包实现，走 btcwallet）。
	BroadcastTx(psbtSigned []byte, txid string) error
	// WithdrawState 返回该笔提现落盘的本地状态（空 = 从未处理到广播；见 neutrino 的
	// withdrawStateBucket）。RGB20 提现用它做与 BTC 侧同构的状态门：状态非空说明本笔已经走到过
	// 广播，此时若 sticky seal 记录为空，重新选 seal 构建就是 E9 的双付形态，必须停下。
	WithdrawState(chain33Hash []byte) []byte
	// TSSAddress 返回桥 TSS 的 P2WPKH 地址（regtest bcrt1…/testnet tb1…/mainnet bc1…）。
	// RGB20 提现的找零地址；config.changeAddress 留空时据此自动填充。
	TSSAddress() string
	// TSSPkScript 返回桥 TSS P2WPKH 输出的 pkScript（16 进制? 不，raw bytes）。
	// 签名节点交叉核对提现 PSBT 时，用它判断额外输入/找零输出是否受桥（TSS）控制。
	TSSPkScript() []byte
	// SignSweepPsbt 通过 TSS 组对一笔**扫集** PSBT 签名（C4）。
	//
	// 与 SignPsbt（提现）的差别在于可核对的东西：提现能拿链上 pending 当真值逐项比对，扫集没有
	// 任何链上上下文可对，所以签名节点改为核对"这笔交易有没有把桥的钱挪出桥"——每个输入都是
	// 桥控制的已登记用户充值脚本、每个输出都回主池、手续费有上界（见 Adapter.ValidateSweepPsbt）。
	// 判据全部来自 PSBT 与本节点自己的事实，不采信协调者的任何声称值。
	SignSweepPsbt(psbtBytes []byte) ([]byte, error)
	// BroadcastRawTx 广播一笔已定稿的原始交易（扫集用；提现走 BroadcastTx 是因为它还要登记
	// txid↔chain33 提现哈希的映射与 pending 确认跟踪，扫集两者都不需要）。
	BroadcastRawTx(rawTx []byte, txid string) error
	// IsUserDepositScript 判断某个输出脚本是否为**已登记**的用户 P2WSH 充值脚本
	// （桥发放过地址、且已纳入 watch 集），返回其 userID（= chain33 充值地址串）。
	//
	// 这类脚本同样受桥（TSS 群钥）控制 —— 脚本里的 `<tssPub>` 就是群公钥，花费需要 CGGMP 组签名
	// —— 因此签名节点交叉核对输入归属时必须接受它们；反过来，**未登记**的 P2WSH 一律不接受
	// （登记与否是签名节点自己的判断，不采信协调者下发的 PSBT 自称）。
	IsUserDepositScript(pkScript []byte) (string, bool)
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
	// ContractPrecisions 返回每个已注册合约**声明**的精度（chain33 侧符号为键），
	// 合约没声明精度 / 侧车不认识它时以 ErrContractPrecisionUnusable 拒绝。
	// 调用方（neutrino 主包）拿它与链上 crossChainInfo.precision 比对，并把值回填上链。
	ContractPrecisions(ctx context.Context) (map[string]uint32, error)
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
	// RecordEvent 投递一条审计事件到网关账本（未配置网关时为空操作）。
	RecordEvent(kind, key string, payload map[string]any)
	// WithdrawalsBlocked 返回本地禁止新提现结算的原因（空 = 允许）。
	WithdrawalsBlocked() string
	// DepositsDisabled 返回本地是否禁止新的充值铸造。
	DepositsDisabled() bool
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
	// withdrawFeeSource 返回某链上资产符号的提现手续费（该资产最小单位，0 = 未配置）。
	// 由 neutrino 主包注入（读链上 rgbx 治理配置的 rgb20WithdrawFee，按 symbol）；
	// nil 时一律返回 0 = 不收取、也不做金额精确比对（旧的宽松口径）。
	withdrawFeeSource func(symbol string) int64
	// depositSigs 已签充值的落盘产物（签名轮次的产出：提交失败/深度不够时只重发，见 deposit.go 与
	// depositsig.go）。
	depositSigs *DepositSignatureStore
	// depositNotes 充值重试路径的日志限流（30s 轮询会把同一个状态反复带回来，同一个 key 只报一次）。
	depositNotes *depositNoteOnce
	// events 网关审计账本的投递器（未配置网关时为 nil）。
	events *eventRecorder
	// breaker 提现自动熔断（见 withdrawBreaker）。
	breaker *withdrawBreaker
	// metrics 进程生命周期内的事件计数器（见 metrics.go）。
	metrics *bridgeMetricCounters
	// startedAt 适配器构造时间，用于快照的 uptime_seconds。
	startedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// publishClient consignment 上传复用的 HTTP 客户端（超时来自配置）。
	publishClient *http.Client

	// withdrawMu 提现串行（一次只处理一笔，避免并发花同一 seal）。
	withdrawMu sync.Mutex
	// stickySealMu 串行 sticky seal 记录的「读-比对-写」（两个调用方各自都在提现串行区内，
	// 这层锁只保证记录本身不会被交叉读改写）。**不可跨签名/构建调用持有**：它只保护记录。
	stickySealMu sync.Mutex
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
		metrics:      newBridgeMetricCounters(),
		startedAt:    time.Now(),
		// 上传是旁路：客户端只服务 consignment 交付，超时按配置（默认 10s）。
		publishClient: &http.Client{Timeout: cfg.ConsignmentPublish.timeout()},
	}
	// 已签充值的落盘产物：与 receive/seal 共用同一个 KVStore（不引入新存储引擎）。
	a.depositSigs = newDepositSignatureStore(store)
	// 审计事件投递：与充值轮询共用 gateway 配置；未配置网关时为空，记录全部变成空操作。
	a.events = newEventRecorder(cfg, store)
	a.breaker = newWithdrawBreaker(cfg.Safety.MaxPayoutMismatchBurns)
	return a, nil
}

// Connect 建立侧车 gRPC 连接（unix socket 优先）。所有节点都需要（签名节点只读校验）。
//
// 连接成功后**立刻读一次合约精度**（见 ContractPrecisions）并校验其可用性，只有通过才把连接挂上：
// 于是 `IsConnected()`（提现/充值轮询等所有业务的入口）成立时，合约精度一定已经读过并可用 ——
// 否则会存在一个"业务已经在动钱、精度还没核对"的窗口。合约侧的**静态分歧**（合约没声明精度、
// 侧车不认识该合约）以 ErrContractPrecisionUnusable 返回，由调用方 fail-closed 拒绝启动；
// 其余错误（侧车暂时连不上/查询失败）返回普通 error，调用方按可重试处理。
//
// 注意这里只做**本地**判定（精度是否存在、是否在链上口径内）。与链上记录是否一致由
// neutrino 包在侧车连上之后单独做（见 ensureCrossChainAssetPrecisions）—— 那一步要用到主链，
// 不属于本适配器的职责。
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
	precisions, err := a.ContractPrecisions(ctx)
	if err != nil {
		// 核对没过就把连接摘掉：`IsConnected()` 的语义必须是"连上**且精度已核对**"，否则所有
		// 以它为入口的业务（提现/充值轮询）会在这段窗口里动钱。摘掉之后调用方要么重试
		// （暂时性失败），要么 fail-closed 拒绝启动（静态分歧，见 ErrContractPrecisionUnusable）。
		a.sidecar.CompareAndSwap(sc, nil)
		return err
	}
	log.Info("Connect rgb20 sidecar connected, contract precisions verified",
		"contracts", len(a.cfg.Contracts), "withDeclaredPrecision", len(precisions))
	return nil
}

// IsConnected 侧车连接是否已建立。
func (a *Adapter) IsConnected() bool {
	return a.sidecar.Load() != nil
}

// Start 启动充值轮询 + HTTP（仅官方节点调用）。非阻塞：
// 后台等待侧车连接成功后自动启动轮询与 HTTP（侧车可能晚于本节点启动）。
func (a *Adapter) Start(ctx context.Context) error {
	// Best-effort consistency check against the gateway (chain title and bridge token). It runs
	// in the background with its own short deadline and never blocks or fails startup.
	if a.cfg.TransportGateway != "" && a.cfg.GatewayToken != "" {
		a.wg.Add(1)
		go a.runGatewaySelfCheck()
	}
	// Runtime metrics (#85) start independently of the sidecar connection: "sidecar unreachable"
	// is one of the states operators need to see, so the pusher must not wait for Connect.
	if a.cfg.TransportGateway != "" && a.cfg.GatewayToken != "" && a.cfg.GatewayMetricsInterval > 0 {
		a.wg.Add(1)
		go a.runMetricsPusher(a.cfg.GatewayMetricsInterval)
	}
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
		if a.cfg.TransportGateway != "" {
			// 生产路径以网关为主；自定义上传端点（若同时配置）仍然保留。
			if a.cfg.TransportEndpoint == "" {
				log.Warn("rgb20 transportGateway is configured without transportEndpoint: invoices will not advertise a transport, so payers must learn the delivery address out of band",
					"gateway", a.cfg.TransportGateway)
			}
			a.wg.Add(1)
			go a.runTransportGatewayPolling()
		} else {
			if a.cfg.TransportEndpoint != "" {
				log.Warn("rgb20 transportEndpoint is configured but transportGateway is empty: invoices advertise a gateway this node will not poll; deposits must be delivered out of band",
					"endpoint", a.cfg.TransportEndpoint)
			}
			log.Info("rgb20 transportGateway not configured; using the custom consignment upload endpoint only")
		}
		if a.cfg.TransportGateway != "" && a.cfg.GatewayToken != "" && a.cfg.ChainTitle != "" {
			a.wg.Add(1)
			go a.runDepositRequestStream()
		} else if a.cfg.TransportGateway != "" {
			log.Warn("rgb20 deposit requests disabled: gatewayToken/chainTitle not configured",
				"gateway", a.cfg.TransportGateway)
		}
		if a.cfg.TestListen != "" {
			if !a.cfg.TestSignPsbt {
				// sign-psbt signs arbitrary PSBTs with the TSS group; never serve it implicitly.
				log.Error("rgb20 testListen ignored: testSignPsbt is disabled")
			} else {
				a.wg.Add(1)
				go a.serveTestHTTP(a.cfg.TestListen)
			}
		}
		log.Info("rgb20 adapter started", "sidecar", a.cfg.SidecarAddr)
	}()
	return nil
}

// Stop 关闭适配器。
func (a *Adapter) Stop() {
	a.cancel()
	a.wg.Wait()
	if a.events != nil {
		a.events.stop()
	}
	if sc := a.sidecar.Load(); sc != nil {
		_ = sc.Close()
	}
}

// RecordEvent queues one audit event for the gateway ledger. A nil recorder (no gateway
// configured) makes it a no-op. It never blocks the caller's money path.
func (a *Adapter) RecordEvent(kind, key string, payload map[string]any) {
	if a == nil {
		return
	}
	// Counters are updated here, at the single place every audit event passes through, so no
	// money-path call site has to know about metrics.
	a.metrics.inc(kind)
	a.events.record(kind, key, payload)
}

// WithdrawalsBlocked reports why this node must not start a new withdrawal settlement
// (node-local kill switch or the automatic breaker). An empty reason means "allowed".
func (a *Adapter) WithdrawalsBlocked() string {
	if a == nil {
		return ""
	}
	if a.cfg.Safety.DisableWithdrawals {
		return "withdrawals are disabled by the local safety configuration"
	}
	return a.breaker.tripReason()
}

// DepositsDisabled reports whether the node-local kill switch rejects new mints.
func (a *Adapter) DepositsDisabled() bool {
	return a != nil && a.cfg.Safety.DisableDeposits
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

// SetWithdrawFeeSource 注入提现手续费的取值来源（由 neutrino 主包接上链上 BridgeConfig）。
// fn 返回该资产最小单位的手续费；0 表示未配置（不收取，也不做金额精确比对）。
func (a *Adapter) SetWithdrawFeeSource(fn func(symbol string) int64) { a.withdrawFeeSource = fn }

// WithdrawFeeFor 返回某个链上资产符号的提现手续费（该资产的最小单位）。
//
// 值来自注入的链上配置读取器（rgbx 治理的 rgb20WithdrawFee，按 symbol）。未注入、该符号
// 没有配置、治理未设置（0）都返回 0 = 不收取、也不做金额比对（保持旧的宽松口径）。
func (a *Adapter) WithdrawFeeFor(symbol string) int64 {
	if a.withdrawFeeSource == nil {
		return 0
	}
	return a.withdrawFeeSource(symbol)
}

// ExpectedWithdrawPayout 算出本笔提现**应当发到用户手里**的精确资产金额，即
// `链上 pending 金额 − 该资产的手续费`（见 WithdrawFeeFor）。
//
//	configured=false：该资产没有配置手续费（0 / 未注册 / 非正数）⇒ **不做**金额精确比对，
//	  调用方保持"只判上界（发给用户的资产 ≤ pending）"的原有行为。0 = 跳过检查这一口径与
//	  userDepositSweep.feeRate 一致：节点配置缺失（或链上治理查询暂时失败）不该把合法提现全拒。
//	err != nil：链上 pending 金额连手续费都不够（应发金额 ≤ 0）⇒ 这笔提现必须被拒绝，
//	  调用方原样上抛。
//
// 协调者与签名节点用**同一个**函数取值（各自读链上同一份治理配置），口径天然一致。
func (a *Adapter) ExpectedWithdrawPayout(symbol string, pendingAmount int64) (int64, bool, error) {
	fee := a.WithdrawFeeFor(symbol)
	if fee <= 0 {
		return 0, false, nil
	}
	if pendingAmount <= fee {
		return 0, false, fmt.Errorf(
			"withdraw amount %d does not cover the configured withdrawal fee %d for %s",
			pendingAmount, fee, symbol)
	}
	return pendingAmount - fee, true, nil
}

func (a *Adapter) Bridge() Chain33Bridge     { return a.bridge }
func (a *Adapter) SetBridge(b Chain33Bridge) { a.bridge = b }

// TestSignEnabled 是否允许无上下文的 PSBT 签名（E2E 的 sign-psbt 测试端点）。默认关闭。
func (a *Adapter) TestSignEnabled() bool { return a.cfg.TestSignPsbt }

// FormatOutpoint 将 txid 与 vout 格式化为 "txid:vout"。
func FormatOutpoint(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}
