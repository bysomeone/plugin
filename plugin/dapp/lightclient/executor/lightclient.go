package executor

import (
	"errors"
	"sync"

	"github.com/33cn/chain33/common/db"
	log "github.com/33cn/chain33/common/log/log15"
	drivers "github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

/*
 * 执行器相关定义
 * 重载基类相关接口
 */

type config struct {
	CommitAddress        string `json:"commitAddress"`
	BtcNetName           string `json:"btcNetName"`
	AllowRegtestTimeWarp bool   `json:"allowRegtestTimeWarp"`
}

var (
	//日志
	elog        = log.New("module", "lightclient.exec")
	lightCfg    config
	cfgInitOnce sync.Once
)

var driverName = ltypes.LightclientX

// Init register dapp
func Init(_ string, cfg *types.Chain33Config, sub []byte) {
	initCfg(sub)
	drivers.Register(cfg, GetName(), newLightclient, cfg.GetDappFork(driverName, "Enable"))
	InitExecType()
}

func initCfg(sub []byte) {

	cfgInitOnce.Do(func() {
		types.MustDecode(sub, &lightCfg)
		if lightCfg.BtcNetName == "" {
			lightCfg.BtcNetName = "mainnet"
		}
		// sub 为空 = 本链没有 [exec.sub.lightclient] 段（例如平行链只跑 neutrino RPC、不提交
		// BTC 头），执行器不会用来写头链，因此不做必填校验。
		if len(sub) == 0 {
			return
		}
		if err := validateCommitAddress(lightCfg.CommitAddress); err != nil {
			panic(err)
		}
	})
}

// validateCommitAddress 校验 [exec.sub.lightclient].commitAddress 是否已配置。
//
// 过渡兜底：头链目前没有原生锚点（B5 只保证 bootstrap 起点与已知 checkpoint 一致，链一旦长起来
// 仍是"谁能写谁说了算"），commitAddress 就是唯一决定"谁能写头链"的开关，留空等于向全网开放
// BTC 头写入权（任何人可提交任意头，进而伪造充值），因此在启动期直接拒绝。
// 待 B4（头链锚点/治理）落地后可放宽为可选。
func validateCommitAddress(commitAddress string) error {
	if commitAddress != "" {
		return nil
	}
	return errors.New("lightclient: [exec.sub.lightclient].commitAddress must not be empty\n" +
		"  配置方法：在 chain33.toml 的 [exec.sub.lightclient] 段填写一个受控地址，例如\n" +
		"      [exec.sub.lightclient]\n" +
		"      btcNetName=\"mainnet\"\n" +
		"      commitAddress=\"1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4\"\n" +
		"  该地址的私钥由运维持有，是唯一有权提交 BTC 区块头的账户。\n" +
		"  为什么必须填：BTC 头链是充值的信任根，链上没有任何原生锚点，\n" +
		"  \"谁能写头链\"完全由这个地址决定；留空即任何人可提交任意头，可伪造充值。\n" +
		"  如需彻底停用该执行器：删除 [exec.sub.lightclient] 段并保持 [fork.sub.lightclient] Enable=-1。")
}

// InitExecType Init Exec Type
func InitExecType() {
	ety := types.LoadExecutorType(driverName)
	ety.InitFuncList(types.ListMethod(&lightclient{}))
}

type lightclient struct {
	drivers.DriverBase
}

func newLightclient() drivers.Driver {
	t := &lightclient{}
	t.SetChild(t)
	t.SetExecutorType(types.LoadExecutorType(driverName))
	return t
}

// GetName get driver name
func GetName() string {
	return newLightclient().GetName()
}

func (l *lightclient) GetDriverName() string {
	return driverName
}

// ExecutorOrder 设置localdb的EnableRead
func (l *lightclient) ExecutorOrder() int64 {
	return drivers.ExecLocalSameTime
}

func readDB(kdb db.KV, key []byte, result types.Message) error {

	val, err := kdb.Get(key)
	if err != nil {
		return err
	}
	return types.Decode(val, result)
}

func getBtcLastHeader(sdb db.KV) (*ltypes.BtcHeader, error) {

	header := &ltypes.BtcHeader{}
	err := readDB(sdb, btcLastHeaderKey(), header)
	if err == types.ErrNotFound {
		return &ltypes.BtcHeader{}, nil
	}
	return header, err
}

func getBtcHeader(ldb db.KV, height uint64) (*ltypes.BtcHeader, error) {

	header := &ltypes.BtcHeader{}
	err := readDB(ldb, btcHeaderKey(height), header)
	return header, err
}

func getBtcHeight(ldb db.KV, hash string) (*types.Int64, error) {

	height := &types.Int64{}
	err := readDB(ldb, btcHeaderHashHeightKey(hash), height)
	return height, err
}
