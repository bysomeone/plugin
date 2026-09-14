package executor

import (
	"strings"
	"sync"

	log "github.com/33cn/chain33/common/log/log15"
	drivers "github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	rgbxtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 执行器相关定义
 * 重载基类相关接口
 */

var (
	//日志
	elog        = log.New("module", "rgbx.executor")
	cfgInitOnce sync.Once
)

var driverName = rgbxtypes.RgbxX

const (
	// defaultMinBtcConfirmations 链上最小确认数默认值（见 config.MinBtcConfirmations）。
	defaultMinBtcConfirmations = int64(6)
)

type config struct {
	CommitAddress          string `json:"commitAddress"`
	CrossChainAssetPrefix  string `json:"crossChainAssetPrefix"`
	GuardianParachainTitle string `json:"guardianParachainTitle"`
	// MinBtcConfirmations 链上最小确认数（B8）：充值证明 / 提现确认证明所在 BTC 区块，必须在
	// lightclient 的 canonical 头链里被确认至少 N 个块（tip.Height >= proof.BlockHeight + N - 1），
	// 否则拒绝（ErrInsufficientBtcConfirmations）。<= 0 取默认值 6。
	//
	// 与中继 [rpc.sub.light.neutrino].blockConfirmations 的关系（按代码事实写，容易搞反）：
	//   - 中继**通知**充值/提现时要求交易在自己视图里有 blockConfirmations 个确认
	//     （btcwallet.go updateTransactionConfirmations: confirmations >= requiredConfs）；
	//   - 但中继**上链的头链只到 best - blockConfirmations**
	//     （bitcoin.go submitBitcoinHeaders: confirmedHeight = best.Height - BlockConfirmations）。
	// 两者相减 ⇒ 中继提交那一刻，链上可见的深度是 0（该交易所在高度的头往往还没上链：SPV 头校验本身
	// 还得再等一个 BTC 块），链上深度要达到 N 需要在中继提交之后再出 N 个 BTC 块。
	// 即：B8 的额外时延 ≈ N 个 BTC 块（主网 ~10N 分钟），与 blockConfirmations 取值无关 ——
	// N = 1 才与"中继等 blockConfirmations 个确认"的现状持平，N > 1 是刻意加的额外等待。
	// （取值是安全/时延的取舍：N 越大，"头链看得见的深度"越深，重组导致的 mint-on-orphan
	//  概率越低，但充值到账越慢；上线前按链上风险容忍度定，改这里即可，无需改代码。）
	MinBtcConfirmations int64 `json:"minBtcConfirmations"`
}

var rgbxCfg = config{}

func initCfg(sub []byte) {

	cfgInitOnce.Do(func() {
		types.MustDecode(sub, &rgbxCfg)
		prefix := strings.TrimSpace(rgbxCfg.CrossChainAssetPrefix)
		if prefix == "" {
			prefix = "X"
		}
		rgbxCfg.CrossChainAssetPrefix = formatSymbol(prefix)
		rgbxCfg.GuardianParachainTitle = strings.TrimSpace(rgbxCfg.GuardianParachainTitle)
		if rgbxCfg.GuardianParachainTitle == "" {
			rgbxCfg.GuardianParachainTitle = defaultGuardianParachainTitle
		}
		// B8：未配置 / 配了非正数一律取默认值。方向是 fail-closed：写 0 想表达"不要链上深度要求"
		// 时不会意外放宽，只会在下面报"确认数不足"（N=0 语义上等于不校验深度，这里不给这个口子）。
		if rgbxCfg.MinBtcConfirmations <= 0 {
			rgbxCfg.MinBtcConfirmations = defaultMinBtcConfirmations
		}
	})
}

// Init register dapp
func Init(_ string, cfg *types.Chain33Config, sub []byte) {
	initCfg(sub)
	drivers.Register(cfg, GetName(), newRgbx, cfg.GetDappFork(driverName, "Enable"))
	InitExecType()
}

// InitExecType Init Exec Type
func InitExecType() {
	ety := types.LoadExecutorType(driverName)
	ety.InitFuncList(types.ListMethod(&rgbx{}))
}

type rgbx struct {
	drivers.DriverBase
}

func newRgbx() drivers.Driver {
	t := &rgbx{}
	t.SetChild(t)
	t.SetExecutorType(types.LoadExecutorType(driverName))
	return t
}

// GetName get driver name
func GetName() string {
	return newRgbx().GetName()
}

func (r *rgbx) GetDriverName() string {
	return driverName
}

func (r *rgbx) ExecutorOrder() int64 {
	return drivers.ExecLocalSameTime
}
