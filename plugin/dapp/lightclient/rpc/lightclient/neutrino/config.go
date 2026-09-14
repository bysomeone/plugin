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
	// BtcHeaderStartHeight 首次启动提交btc header的起始高度
	BtcHeaderStartHeight uint64 `json:"btcHeaderStartHeight"`
	// MaxUtxoRescanTime, utxo 检索最大时长，hour, 0为永不超时
	MaxUtxoRescanTime int64 `json:"maxUtxoRescanTime"`
	// BtcFullNodeRPC 可选，比特币全节点 RPC 配置，用于查询 block 构造SPV
	BtcRPC btcRPCConfig `json:"btcRPC"`
	// Tss tss config
	Tss tssConfig `json:"tss"`
	// Rgb20 RGB20 侧车桥配置（Phase 2b）。
	Rgb20 rgb20Config `json:"rgb20"`
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
	// SignedDepositTTL 签名侧"已签集合"的保留期（TTL），单位 = **BTC 区块数**。
	// 语义：签名节点在签 thresholdSig 成功后把付款交易 txid 记进本地已签集合，之后的同一 txid
	// 直接拒绝签名；链上 tip 高度 - 记录高度 >= 本值即视为过期，过期记录被清理、同一 txid 可再签。
	// 0 / -1（或任何 <= 0）= 只增不删（永久保留，默认值）。
	// 改成正数后**重启立即生效**：启动时按新 TTL 清理一次旧记录（含只增不删期间攒下的）。
	// 详见 CONFIG.md 4.4 与 rgb20/signedset.go。
	SignedDepositTTL int64 `json:"signedDepositTTL"`
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
	if n.cfg.BtcHeaderStartHeight == 0 {
		n.cfg.BtcHeaderStartHeight = 1
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
