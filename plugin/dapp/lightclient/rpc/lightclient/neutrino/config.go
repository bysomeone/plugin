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

type config struct {

	// IsOfficialNode 是否为官方主节点。配置键显式写成 isOfficialNode（CONFIG.md 与 CI 的写法）；
	// 此前该字段没有 json tag，靠 encoding/json 对字段名的大小写不敏感匹配生效，
	// 于是 TECHNICAL.md / RGB_USDT_INTEGRATION.md 里的 PascalCase 写法（IsOfficialNode）也能用。
	// 加上 tag 后两种写法仍然都能解析（json 的大小写不敏感匹配是兜底），只是契约在代码里显式了。
	IsOfficialNode bool `json:"isOfficialNode"`
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
	// BlockConfirmations 区块确认数
	BlockConfirmations uint32 `json:"blockConfirmations"`
	// MaxUtxoRescanTime, utxo 检索最大时长，hour, 0为永不超时
	MaxUtxoRescanTime int64 `json:"maxUtxoRescanTime"`
	// BtcFullNodeRPC 可选，比特币全节点 RPC 配置，用于查询 block 构造SPV
	BtcRPC btcRPCConfig `json:"btcRPC"`
	// Tss tss config
	Tss tssConfig `json:"tss"`
	// Rgb20 RGB20 侧车桥配置（Phase 2b）。
	Rgb20 rgb20Config `json:"rgb20"`

	// DepositAddressListen 用户 BTC 充值地址发放 HTTP 监听地址（空 = 不开启）。
	//
	// 为什么需要一个"发放"入口：P2WSH 充值地址 = f(chain33 地址, TSS 群公钥) 是纯函数，谁都能自己算；
	// 但**钱包必须先 watch 那个脚本才能看见充值**（analyzeTransaction 靠 program 反解归属），
	// 所以"要地址"这件事必须真的到达桥、触发按需 import —— 这就是本入口的唯一职责。
	// 地址本身不是秘密（离线可枚举，见 C0 §7 风险 1），注册别人地址也不会让别人丢钱
	// （脚本绑定的是那个人的 userID），唯一可被滥用的是 watch 集增长，由
	// maxWatchedDepositScripts 兜住。
	DepositAddressListen string `json:"depositAddressListen"`
	// MaxWatchedDepositScripts 用户充值脚本 watch 集上限：达到上限后**拒绝发放新地址**
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
	// FeeRate 扫集的费率（sat/vB）。<=0 用默认值。
	//
	// 它同时是签名节点核对扫集手续费区间的费率（见 tssService.sweepSignFeeRate）：扫集没有链上
	// pending 可作真值，费率只能取本地配置。因此各节点的这个值应当一致，否则签名节点可能拒签
	// 一笔合法扫集（费率上界按各节点自己的值算）。
	FeeRate int `json:"feeRate"`
	// MinConfirmations 只归集达到该确认数的充值 UTXO。<=0 用默认值（defaultSweepMinConfirmations）。
	MinConfirmations int `json:"minConfirmations"`
}

// rgb20Config RGB20 跨链桥（RGB20 USDT）配置。
type rgb20Config struct {
	// SidecarAddr RGB 侧车地址：unix socket 路径优先，否则 tcp host:port。
	SidecarAddr string `json:"sidecarAddr"`
	// ConsignmentListen consignment 上传/充值请求 HTTP 监听地址。空则不开 HTTP。
	ConsignmentListen string `json:"consignmentListen"`
	// Contracts RGB20 资产合约列表。
	Contracts []rgb20Contract `json:"contracts"`
	// Precision 默认精度（RGB20 USDT 为 6）。
	Precision uint32 `json:"precision"`
	// ChangeAddress 提现找零地址（TSS P2WPKH 地址）；留空则由 TSS 地址自动填充。
	ChangeAddress string `json:"changeAddress"`
	// TestSignPsbt 是否允许"无提现上下文的 PSBT 签名"（E2E 的 sign-psbt 测试端点用）。
	// 该能力等价于"用 TSS 组私钥签任意 PSBT"（签名节点无从核对被签内容），**生产必须为 false**
	// （默认 false），只有 E2E/regtest 部署显式打开。
	TestSignPsbt bool `json:"testSignPsbt"`
}

// rgb20Contract RGB20 资产合约注册项。
type rgb20Contract struct {
	Symbol        string `json:"symbol"`        // chain33 侧资产符号，如 RGB20_USDT
	SidecarSymbol string `json:"sidecarSymbol"` // 侧车发行资产符号，如 USDT（空则回退 Symbol）
	AssetID       string `json:"assetId"`       // rgb:...
	Precision     uint32 `json:"precision"`     // 小数位
	MinDeposit    int64  `json:"minDeposit"`    // 最小充值（最小单位）
	MinWithdraw   int64  `json:"minWithdraw"`   // 最小提现（最小单位）
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
	Threshold uint32 `json:"threshold"`
	// Rank peer rank
	Rank uint32 `json:"rank"`
	// AllowShareMismatch 逃生阀，默认 false（不写即关闭）：启动自检（见 client.go
	// checkTssShareAgainstChain）发现本地 share 与链上 CrossChainInfo 不一致时，是否仍允许启动。
	// 关闭时 fail-closed 拒绝启动（panic）—— 不一致的节点能"参与签名"但产出链上不认，是静默失灵。
	// 只在为了临时把节点拉起来做冷修时打开，修好必须改回 false。
	AllowShareMismatch bool `json:"allowShareMismatch"`
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
