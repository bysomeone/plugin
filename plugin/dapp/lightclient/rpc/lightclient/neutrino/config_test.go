package neutrino

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var lightConfig = `

[rpc.sub.light]
clients=["neutrino"]
commitAddr="14KEKbYtKKQm4wMthSK9J4La4nAiidGozt"

[rpc.sub.light.neutrino]
netName="regtest"
addPeers=["127.0.0.1:8333"]
btcBlockInterval=10
blockConfirmations=0
maxUtxoRescanTime=24

`

func Test_config(t *testing.T) {

	cfg := types.NewChain33Config(types.MergeCfg(types.GetDefaultCfgstring(), lightConfig))
	light := &lightclient.Config{}
	types.MustDecode(cfg.GetSubConfig().RPC["light"], light)
	require.Equal(t, 1, len(light.EnableClients))
	require.Equal(t, "neutrino", light.EnableClients[0])

	sub, err := json.Marshal(light.Neutrino)
	require.NoError(t, err)
	subCfg := &config{}
	types.MustDecode(sub, subCfg)
	require.Equal(t, subCfg.NetName, "regtest")

	// test init neutrino config
	n := &neutrinoClient{}
	n.cfg = *subCfg
	q := queue.New("test")
	q.SetConfig(cfg)
	require.NoError(t, err)
	util.ResetDatadir(cfg.GetModuleConfig(), "$TEMP/")
	err = n.initNeutrinoConfig(cfg)
	if err != nil {
		t.Skipf("initNeutrinoConfig needs walletdb/bdb (environment): %v", err)
	}

	require.True(t, n.cfg.BlockCacheSize == defaultBlockCacheSize)
	require.True(t, n.cfg.MaxPeer == 8)
	require.True(t, n.cfg.BtcBlockInterval == 10)
	require.True(t, n.cfg.BlockConfirmations == 0)
	require.True(t, n.cfg.MaxUtxoRescanTime == int64(24*time.Hour/time.Second))
	dir := cfg.GetModuleConfig().BlockChain.DbPath + "/lightclient"
	require.Equal(t, dir, n.neutrinoCfg.DataDir)
	require.Equal(t, n.cfg.NetName, n.neutrinoCfg.ChainParams.Name)
}

// TestBtcRPCConfig tests the btcRPCConfig.toConnConfig method
func TestBtcRPCConfig(t *testing.T) {
	tests := []struct {
		name     string
		config   btcRPCConfig
		wantErr  bool
		wantMode string
	}{
		{
			name: "ws mode (default)",
			config: btcRPCConfig{
				Host:       "127.0.0.1:8332",
				User:       "testuser",
				Pass:       "testpass",
				Mode:       "",
				DisableTLS: true,
			},
			wantErr:  false,
			wantMode: "ws",
		},
		{
			name: "http mode",
			config: btcRPCConfig{
				Host:       "127.0.0.1:8332",
				User:       "testuser",
				Pass:       "testpass",
				Mode:       "http",
				DisableTLS: true,
			},
			wantErr:  false,
			wantMode: "http",
		},
		{
			name: "with TLS cert",
			config: btcRPCConfig{
				Host:       "127.0.0.1:8332",
				User:       "testuser",
				Pass:       "testpass",
				Mode:       "ws",
				DisableTLS: false,
				CertFile:   "/tmp/nonexistent_cert.pem",
			},
			wantErr: true, // file doesn't exist
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.config.toConnConfig()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, conn)
			assert.Equal(t, tt.config.Host, conn.Host)
			assert.Equal(t, tt.config.User, conn.User)
			assert.Equal(t, tt.config.Pass, conn.Pass)
			assert.Equal(t, tt.wantMode, conn.Endpoint)
			if tt.wantMode == "http" {
				assert.True(t, conn.HTTPPostMode)
			}
		})
	}
}

// TestConfigGetChainParams tests the config.getChainParams method
func TestConfigGetChainParams(t *testing.T) {
	tests := []struct {
		netName  string
		wantName string
	}{
		{"mainnet", "mainnet"},
		{"testnet", "testnet3"},
		{"testnet4", "testnet4"},
		{"testnet3", "testnet3"},
		{"regtest", "regtest"},
		{"simnet", "simnet"},
		{"signet", "signet"},
		{"unknown", "mainnet"}, // default to mainnet
	}

	for _, tt := range tests {
		cfg := config{NetName: tt.netName}
		params := cfg.getChainParams()
		assert.Equal(t, tt.wantName, params.Name)
	}
}

// Test_openWalletDB verifies that openWalletDB correctly creates a new database
// when it doesn't exist and opens an existing one.
func Test_openWalletDB(t *testing.T) {
	tmpDir := t.TempDir()
	dbName := "test_neutrino.db"

	// Test 1: Create new database (doesn't exist yet)
	exist, db, err := openWalletDB(tmpDir, dbName)
	if err != nil {
		t.Skipf("walletdb.Create requires bdb driver (environment): %v", err)
	}
	require.NoError(t, err)
	assert.False(t, exist, "database should not exist before creation")
	assert.NotNil(t, db, "database handle should not be nil after creation")

	// Verify database file was created
	dbPath := filepath.Join(tmpDir, dbName)
	_, statErr := os.Stat(dbPath)
	assert.NoError(t, statErr, "database file should exist on disk")

	// Close the database before reopening
	db.Close()

	// Test 2: Open existing database
	exist2, db2, err2 := openWalletDB(tmpDir, dbName)
	require.NoError(t, err2)
	assert.True(t, exist2, "database should exist when reopening")
	assert.NotNil(t, db2, "database handle should not be nil when reopening")
	db2.Close()
}

// Test_btcHeaderStartHeight 头链 bootstrap 起点由 btcd 内置锚点本地推出（= 最高锚点 + 1），
// 不是配置项：锚点是编译期常量，中继与执行器共用 ltypes.GetBtcChainParams，各自都能算出同一个值。
func Test_btcHeaderStartHeight(t *testing.T) {
	// 有内置锚点的网络（值来自 btcd chaincfg，与执行器 btcCheckpointTable 同源 —— 升级 btcd 会让
	// 这两行变红，那是显式事件：锚点集合同时影响中继与执行器，两边必须同一个 build）。
	require.Equal(t, uint64(810001), btcHeaderStartHeight("mainnet"))
	require.Equal(t, uint64(2344475), btcHeaderStartHeight("testnet3"))
	// 没有内置锚点的网络：只能从创世之后第一个块起（chaincfg 对它们没有 checkpoint）。
	for _, netName := range []string{"regtest", "testnet4", "signet", "simnet"} {
		require.Equal(t, uint64(1), btcHeaderStartHeight(netName), netName)
	}
}

// Test_legacyBtcHeaderStartHeightKeyIsIgnored 老 TOML 里留着的 btcHeaderStartHeight 键**无害**：
// 解析路径是 TOML → lightclient.Config.Neutrino → json → config（见 client.go 的 Init），
// 未知键被 json.Unmarshal 忽略 —— 解析照常成功，且它不再是 config 的字段，因而影响不到任何行为
// （起点只由内置锚点推出）。运维升级时不必先清理配置文件。
func Test_legacyBtcHeaderStartHeightKeyIsIgnored(t *testing.T) {
	legacy := `
[rpc.sub.light]
clients=["neutrino"]

[rpc.sub.light.neutrino]
netName="mainnet"
btcHeaderStartHeight=800000
maxUtxoRescanTime=24
`
	// 1) TOML 解析（未知键不报错）。
	cfg := types.NewChain33Config(types.MergeCfg(types.GetDefaultCfgstring(), legacy))
	light := &lightclient.Config{}
	types.MustDecode(cfg.GetSubConfig().RPC["light"], light)

	// 2) 子配置解码（与运行时同一条路径）：老键被忽略，其余字段照常生效。
	sub, err := json.Marshal(light.Neutrino)
	require.NoError(t, err)
	require.Contains(t, string(sub), "btcHeaderStartHeight", "老键确实出现在待解码的 json 里（否则本用例没测到东西）")
	subCfg := &config{}
	types.MustDecode(sub, subCfg)
	require.Equal(t, "mainnet", subCfg.NetName)
	require.Equal(t, int64(24), subCfg.MaxUtxoRescanTime)

	// 3) 配置结构里已经没有该字段：老键无法再被解析成任何行为（重新加回字段会让这条变红）。
	back, err := json.Marshal(subCfg)
	require.NoError(t, err)
	require.NotContains(t, string(back), "btcHeaderStartHeight")

	// 4) 起点只由内置锚点推出，与老配置里写的 800000 无关。
	require.Equal(t, uint64(810001), btcHeaderStartHeight(subCfg.NetName))
}
