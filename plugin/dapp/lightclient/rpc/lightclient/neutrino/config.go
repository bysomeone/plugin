package neutrino

import (
	"os"
	"path/filepath"
	"time"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/neutrino"
)

// defaultBlockCacheSize is the size (in bytes) of blocks that will be
// keep in memory if no size is specified.
const defaultBlockCacheSize = 20 * 1024 * 1024 //20 MB

// defaultMainChainConfirmations 是主链（chain33）pending 的默认等待深度。
// 取 6 与拆分前的既有行为一致；主链出块远快于 BTC，这个值通常只要几秒。
const defaultMainChainConfirmations = 6

type config struct {

	// IsOfficialNode 是否为官方主节点。配置键显式写成 isOfficialNode（CONFIG.md 与 CI 的写法）；
	// 此前该字段没有 json tag，靠 encoding/json 对字段名的大小写不敏感匹配生效，
	// 于是历史配置文档里的 PascalCase 写法（IsOfficialNode）也能用。
	// 加上 tag 后两种写法仍然都能解析（json 的大小写不敏感匹配是兜底），只是契约在代码里显式了。
	IsOfficialNode bool `json:"isOfficialNode"`
	// AbandonOperators 手工放弃提现（AbandonWithdraw）提案的本地白名单（chain33 地址）。
	// 链上提案只是存证；节点只受理提案人在此列表内的提案。空 = 关闭手工放弃流程。
	// **各节点应配置同一份列表**，否则不同节点会对同一提案给出不同结论。
	AbandonOperators []string `json:"abandonOperators"`
	// MaxPeers is the maximum number of connections the client maintains.
	MaxPeer int `json:"maxPeer"`
	// BlockCacheSize indicates the size (in bytes) of blocks the block
	// cache will hold in memory at most. If a BlockCache is provided then
	// BlockCacheSize is ignored.
	BlockCacheSize uint64 `json:"blockCacheSize"`
	// NetName btc network name
	NetName string `json:"netName"`
	// AddPeers is a slice of hosts that should be connected to on startup,
	// and be maintained as persistent peers.
	AddPeers []string `json:"addPeers"`
	// ConnectPeers is a slice of hosts that should be connected to on
	// startup, and be established as persistent peers.
	//
	// NOTE: If specified, we'll *only* connect to this set of peers and
	// won't attempt to automatically seek outbound peers.
	ConnectPeers []string `json:"connectPeers"`

	// BtcBlockInterval 区块时间间隔，second
	BtcBlockInterval uint32 `json:"btcBlockInterval"`
	// BlockConfirmations BTC 确认数门限：钱包待确认、中继提交头链的保留深度（只提交到
	// best-N）、扫集默认确认数。它**不**决定本链 pending 的等待深度 —— 那是
	// MainChainConfirmations。BTC 一块约 10 分钟，本链一块数秒，两者必须分开配置。
	BlockConfirmations uint32 `json:"blockConfirmations"`
	// MainChainConfirmations 本链（chain33）pending 的等待深度：拉取 pending 时只处理
	// 主链高度 − N + 1 之前的记录，给本链重组留出回退窗口。<=0 用默认值
	// （defaultMainChainConfirmations）。
	MainChainConfirmations uint32 `json:"mainChainConfirmations"`
	// MaxUtxoRescanTime, utxo 检索最大时长，hour, 0为永不超时
	MaxUtxoRescanTime int64 `json:"maxUtxoRescanTime"`
	// BtcFullNodeRPC 可选，比特币全节点 RPC 配置，用于查询 block 构造SPV
	BtcRPC btcRPCConfig `json:"btcRPC"`
	// Tss tss config
	Tss tssConfig `json:"tss"`
	// Rgb20 RGB20 侧车桥配置（Phase 2b）。
	Rgb20 rgb20Config `json:"rgb20"`

	// MaxWatchedDepositScripts 用户充值脚本 watch 集上限：达到上限后**拒绝导入新登记**
	// （明确失败优于静默变慢/静默漏认充值）。<=0 用默认值（defaultMaxWatchedDepositScripts）。
	MaxWatchedDepositScripts int `json:"maxWatchedDepositScripts"`

	// UserDepositSweep 扫集配置（C4）：把散在用户 P2WSH 充值地址上的 BTC 闲时归集回主池。
	UserDepositSweep userDepositSweepConfig `json:"userDepositSweep"`
}

// userDepositSweepConfig 扫集（C4）。只在**发放充值地址的那个节点**（官方节点）上开启：
// 充值 UTXO 的归属只有它（以及和它共用侧车的节点）看得见。
//
// 为什么必须有扫集：P2WSH 充值地址上线后，用户的充值不再进主池 —— 钱停在各自的 P2WSH 上，
// 而提现只能花主池的 BTC。没有扫集，"钱进得去、出不来"。
//
// 时序（规格 §6① 选项 A）：扫集是**纯 UTXO 整理**，不改变任何 RGB 账（入账在铸币那一刻已完成），
// 所以它不需要卡在铸币前后，只需"闲时按 UTXO 数阈值触发"——攒够 MinUtxos 笔再合并成一次，
// 手续费才划算。
type userDepositSweepConfig struct {
	// Enable 是否开启扫集（默认 false）。
	Enable bool `json:"enable"`
	// IntervalSeconds 触发检查间隔（秒）。<=0 用默认值（defaultSweepIntervalSeconds）。
	IntervalSeconds int `json:"intervalSeconds"`
	// MinUtxos 触发阈值：可扫的充值 UTXO 少于这个数就不扫（等它攒够）。<=0 用默认值。
	MinUtxos int `json:"minUtxos"`
	// FeeRate 扫集的费率下限（sat/vB）。<=0 用默认值。
	//
	// 每轮扫集前会向 btcd 估算动态费率（estimatesmartfee，6 个确认目标）：估算值低于本值时
	// 仍用本值，避免费率过低导致扫集长期不上链；估算失败 / 节点无估算（regtest 常态）时也回退到本值。
	FeeRate int `json:"feeRate"`
	// 费率上限不在这里：它读链上的 withdrawMaxFeeRateSatPerVbyte（rgbx 治理配置），
	// 见 bridgeconfig.go。上限是共识性参数，放在本地配置会让各节点对同一笔扫集给出不同结论。
	// MinConfirmations 只归集达到该确认数的充值 UTXO。<=0 用默认值（defaultSweepMinConfirmations）。
	MinConfirmations int `json:"minConfirmations"`
}

// rgb20Config RGB20 跨链桥（RGB20 USDT）配置。
type rgb20Config struct {
	// SidecarAddr RGB 侧车地址：unix socket 路径优先，否则 tcp host:port。
	SidecarAddr string `json:"sidecarAddr"`
	// TestListen 仅 E2E 使用的监听地址（127.0.0.1:port 或 unix 路径），必须与 testSignPsbt
	// 同时打开。生产留空：桥不提供任何 HTTP API，充值请求由桥主动从网关拉取。
	TestListen string `json:"testListen"`
	// TransportEndpoint 写进充值 invoice 的**公网**标准 transport endpoint，例如
	// `rpcs://gw.example.com/0.2/json-rpc`。空 = invoice 不携带 transports。
	// 这是付款方看到的地址，必须能从公网访问（TLS 通常由反向代理终止）。
	TransportEndpoint string `json:"transportEndpoint"`
	// TransportGateway 桥轮询 consignment 用的 RGB HTTP JSON-RPC 网关地址，
	// 例如 `http://gw:8080/0.2/json-rpc`。它通常与 transportEndpoint 指向同一台网关
	// （公网主机名 vs 内网地址），但两者是独立配置，可以不同。
	// 空 = 不启用网关轮询（充值交付只能走后门/手动，不再有自定义上传端点）。
	TransportGateway string `json:"transportGateway"`
	// GatewayToken /bridge/* 接口的 bearer token（充值请求 SSE 流 + invoice 回写）。
	// 与 transportGateway 同时配置才会订阅充值请求。
	GatewayToken string `json:"gatewayToken"`
	// GatewayMetricsInterval 桥主动推送运行指标到网关的间隔，Go duration 字符串
	// （如 "30s"）。空 = 默认 30s；"0s" 或负值 = 关闭。仅当 transportGateway 与
	// gatewayToken 同时配置时生效。
	GatewayMetricsInterval string `json:"gatewayMetricsInterval"`
	// Contracts RGB20 资产合约列表。
	Contracts []rgb20Contract `json:"contracts"`
	// ChangeAddress 提现找零地址（TSS P2WPKH 地址）；留空则由 TSS 地址自动填充。
	ChangeAddress string `json:"changeAddress"`
	// TestSignPsbt 是否允许"无提现上下文的 PSBT 签名"（E2E 的 sign-psbt 测试端点用）。
	// 该能力等价于"用 TSS 组私钥签任意 PSBT"（签名节点无从核对被签内容），**生产必须为 false**
	// （默认 false），只有 E2E/regtest 部署显式打开。
	TestSignPsbt bool `json:"testSignPsbt"`
	// ConsignmentPublish 提现 consignment 的外部交付配置。url 为空时只本地保存（WARN）。
	ConsignmentPublish consignmentPublishConfig `json:"consignmentPublish"`
	// Safety 节点本地（不参与共识）的安全开关：急停与自动熔断。链上治理管的是全局
	// 限额与暂停；这里管"本节点是否继续接新活"，无需治理即可生效。
	Safety rgb20SafetyConfig `json:"safety"`
}

// rgb20SafetyConfig is the node-local kill switch surface (not consensus state).
type rgb20SafetyConfig struct {
	// DisableDeposits stops signing/submitting new mints on this node.
	DisableDeposits bool `json:"disableDeposits"`
	// DisableWithdrawals stops starting new withdrawal settlements on this node.
	// Broadcast withdrawals are still confirmed.
	DisableWithdrawals bool `json:"disableWithdrawals"`
	// MaxPayoutMismatchBurns trips the automatic withdrawal breaker after this many distinct
	// burns were refused for a payout mismatch. 0 = default (3); negative disables it.
	MaxPayoutMismatchBurns int `json:"maxPayoutMismatchBurns"`
}

// consignmentPublishConfig consignment 外部交付：POST 原始字节到外部服务。
//
// 契约（与 CONFIG.md §4.4.6 一致）：
//   - body = 原始 consignment 字节；
//   - Content-Type: application/octet-stream；
//   - X-Consignment-Hash: consignment 的 sha256 hex；
//   - X-Withdraw-Hash: chain33 提现（burn）交易哈希 hex；
//   - token 非空时 Authorization: Bearer <token>；
//   - 2xx = 成功，其余按指数退避重试（上限 5 分钟），绝不阻塞提现。
type consignmentPublishConfig struct {
	URL            string `json:"url"`
	Token          string `json:"token"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// rgb20Contract RGB20 资产合约注册项。
//
// 注意这里**没有** precision：精度不是配置项，而是合约的声明（侧车 ListAssets 报出来的
// `AssetInfo.precision`）。桥在侧车连上后读它、写进链上的跨链资产记录（crossChainInfo.precision），
// 供前端 / CLI 读；配置再声明一份只会带来"两边不一致时按谁算"的静默分歧（旧版就是这样：
// 配置默认 6、合约发行 8，全程无人比对）。
type rgb20Contract struct {
	Symbol        string `json:"symbol"`        // chain33 侧资产符号，如 RGB20_USDT
	SidecarSymbol string `json:"sidecarSymbol"` // 侧车发行资产符号，如 USDT（空则回退 Symbol）
	AssetID       string `json:"assetId"`       // rgb:...
	MinDeposit    int64  `json:"minDeposit"`    // 最小充值（最小单位）
	MinWithdraw   int64  `json:"minWithdraw"`   // 最小提现（最小单位）
	// WithdrawFee 不在这里：每笔提现的固定手续费读链上 rgbx 治理配置的 rgb20WithdrawFee
	// （按 symbol，见 bridgeconfig.go）。放在本地配置会让各签名节点对同一笔提现给出不同结论。
}

type btcRPCConfig struct {
	// Host 例如: 127.0.0.1:8332 或 btc-node.example.com:8332
	Host string `json:"host"`
	// User/Pass 对应 bitcoin.conf 的 rpcuser/rpcpassword
	User string `json:"user"`
	Pass string `json:"pass"`
	// Mode: ws(默认) 或 http
	Mode string `json:"mode"`
	// DisableTLS 是否禁用 TLS
	DisableTLS bool `json:"disableTLS"`
	// CertFile TLS 证书文件（可选）
	CertFile string `json:"certFile"`
}

func (c *btcRPCConfig) toConnConfig() (*rpcclient.ConnConfig, error) {
	endpoint := "ws"
	httpPostMode := false
	if c.Mode == "http" {
		endpoint = "http"
		httpPostMode = true
	}
	conn := &rpcclient.ConnConfig{
		Host:         c.Host,
		Endpoint:     endpoint,
		User:         c.User,
		Pass:         c.Pass,
		DisableTLS:   c.DisableTLS,
		HTTPPostMode: httpPostMode,
	}
	if c.CertFile == "" {
		return conn, nil
	}
	certs, err := os.ReadFile(c.CertFile)
	if err != nil {
		return nil, err
	}
	conn.Certificates = certs
	return conn, nil
}

type tssConfig struct {
	// Peers peers name
	Peers []string `json:"peers"`
	// Threshold peer threshold
	//
	// CGGMP 下它同时是**签名者个数**：alice 的 cggmp.ProcessSign 要求 len(peers) == threshold，
	// 所以每轮签名只挑 threshold 个节点参与（挑法见 tss.go 的 selectSignerCombination），
	// 不再是 GG18 那种"连上的合法节点全上"。
	Threshold uint32 `json:"threshold"`
	// Rank 本节点的 **Birkhoff rank**（不是节点序号！），必须满足 rank + 1 < threshold
	// （即 rank <= threshold-2；等价于 alice 的 utils.EnsureRank）。
	//
	// 本项目 4 节点 + threshold=3 的正确取值是 {0,1,1,1}：官方节点 0，其余三个第三方节点都是 1。
	// **多个节点共用同一个 rank 是正常的**（alice 靠各自的 x 坐标区分），照抄"0,1,2,3"这种
	// 索引式分配会被 alice 直接拒（rank=2/3 不满足 rank+1<3）⇒ DKG 永远跑不起来。
	// 启动时会自检（tss.go validateCggmpRank），不合规直接拒绝启动而不是每分钟重试一次。
	Rank uint32 `json:"rank"`
	// AllowShareMismatch 逃生阀，默认 false（不写即关闭）：启动自检（见 client.go
	// checkTssShareAgainstChain）发现本地 share 与链上 CrossChainInfo 不一致时，是否仍允许启动。
	// 关闭时 fail-closed 拒绝启动（panic）—— 不一致的节点能"参与签名"但产出链上不认，是静默失灵。
	// 只在为了临时把节点拉起来做冷修时打开，修好必须改回 false。
	AllowShareMismatch bool `json:"allowShareMismatch"`
}

// mainChainConfirmations 返回本链 pending 的等待深度（未配置时用默认值）。
func (c config) mainChainConfirmations() uint32 {
	if c.MainChainConfirmations == 0 {
		return defaultMainChainConfirmations
	}
	return c.MainChainConfirmations
}

func (c config) getChainParams() chaincfg.Params {

	params := ltypes.GetBtcChainParams(c.NetName)
	return *params
}

func (n *neutrinoClient) initNeutrinoConfig(chainCfg *types.Chain33Config) error {

	if n.cfg.BlockCacheSize < 1024*1024 {
		n.cfg.BlockCacheSize = defaultBlockCacheSize
	}
	if n.cfg.MaxPeer < 1 {
		n.cfg.MaxPeer = 8
	}

	if n.cfg.BtcBlockInterval <= 0 {
		n.cfg.BtcBlockInterval = 600
	}
	if n.cfg.BlockConfirmations == 0 {
		n.cfg.BlockConfirmations = defaultRequiredConfs
	}
	// convert to second
	if n.cfg.MaxUtxoRescanTime > 0 {
		n.cfg.MaxUtxoRescanTime *= int64(time.Hour / time.Second)
	}
	dbPath := filepath.Join(chainCfg.GetModuleConfig().BlockChain.DbPath, "lightclient")
	_ = os.MkdirAll(dbPath, 0755)
	_, db, err := openWalletDB(dbPath, "neutrino.db")
	if err != nil {
		log.Error("initNeutrinoConfig open db error", "err", err)
		return err
	}

	neutrino.MaxPeers = n.cfg.MaxPeer
	neutrino.BanDuration = time.Hour * 48

	n.neutrinoCfg = neutrino.Config{
		DataDir:      dbPath,
		Database:     db,
		ChainParams:  n.cfg.getChainParams(),
		ConnectPeers: n.cfg.ConnectPeers,
		AddPeers:     n.cfg.AddPeers,
		//BlockCache:         lru.NewCache[wire.InvVect, *neutrino.CacheableBlock](clientCfg.BlockCacheSize),
		BlockCacheSize: n.cfg.BlockCacheSize,
	}

	logCfg := chainCfg.GetModuleConfig().Log
	dir := filepath.Dir(logCfg.LogFile)
	logfile, err := os.OpenFile(filepath.Join(dir, "rgbx.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Error("initNeutrinoConfig open log file error", "err", err)
		return err
	}
	logLevel := btclog.LevelInfo
	if logCfg.Loglevel == "debug" || logCfg.Loglevel == "dbug" {
		logLevel = btclog.LevelDebug
	}
	backend := btclog.NewBackend(logfile)
	logger := func(subsystemTag string) btclog.Logger {
		l := backend.Logger(subsystemTag)
		l.SetLevel(logLevel)
		return l
	}
	neutrino.UseLogger(logger("NEUT"))
	waddrmgr.UseLogger(logger("WADM"))
	wtxmgr.UseLogger(logger("WTXM"))
	chain.UseLogger(logger("CHNS"))
	wallet.UseLogger(logger("WLLT"))
	return nil

}

func openWalletDB(path, dbName string) (exist bool, db walletdb.DB, err error) {

	dbPath := filepath.Join(path, dbName)
	exist, err = fileExists(dbPath)
	if err != nil {
		return false, nil, err
	}
	if exist {
		db, err = walletdb.Open("bdb", dbPath, true, time.Second*10, false)
	} else {
		db, err = walletdb.Create("bdb", dbPath, false, time.Second*10, false)
	}
	return exist, db, err
}
