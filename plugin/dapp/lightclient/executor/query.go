package executor

import (
	"encoding/json"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rgbxtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

func (l *lightclient) Query_GetBtcLastHeader(req *types.ReqNil) (types.Message, error) {

	header, err := getBtcLastHeader(l.GetStateDB())
	return header, err
}

// Query_GetBtcHeader 按高度返回 canonical 链上的 BTC 头。
//
// 读的是 localdb（节点私有），而读方是 rgbx 的共识判定 → 必须先做两道交叉校验（见 btc_index_guard.go）：
//  1. localdb 在共识 tip 高度上的头要与 statedb 的 btc-lastheader 一致（丢库/落后/停在别的链上直接拒）；
//  2. 请求高度落在 statedb 的 canonical 窗口里时，头必须与窗口节点的 hash 一致（hash 已承诺 merkleRoot，
//     所以这一步等价于校验 merkleRoot）；窗口里没有这条则维持原行为。
func (l *lightclient) Query_GetBtcHeader(req *ltypes.ReqGetBtcHeader) (types.Message, error) {
	allow := lightCfg.AllowBtcIndexMismatch
	if err := checkBtcLocalIndexTip(l.GetStateDB(), l.GetLocalDB(), allow); err != nil {
		return nil, err
	}
	header, err := getBtcHeader(l.GetLocalDB(), req.GetHeight())
	if err != nil {
		return nil, err
	}
	state, err := getBtcChainState(l.GetStateDB())
	if err != nil {
		elog.Error("Query_GetBtcHeader getBtcChainState", "height", req.GetHeight(), "err", err)
		return nil, err
	}
	if err := checkBtcHeaderCanonical(state, header, allow); err != nil {
		return nil, err
	}
	return header, nil
}

// Query_GetBtcHeaderByHash 按 hash 返回 canonical 链上的 BTC 头（先经 localdb 的 hash → height 索引）。
// 交叉校验与 Query_GetBtcHeader 相同：索引越旧/错位时，取到的头与窗口节点对不上就会被拒，
// 而不是返回一个"查得到但不是它"的头。
func (l *lightclient) Query_GetBtcHeaderByHash(req *types.ReqString) (types.Message, error) {
	allow := lightCfg.AllowBtcIndexMismatch
	if err := checkBtcLocalIndexTip(l.GetStateDB(), l.GetLocalDB(), allow); err != nil {
		return nil, err
	}
	height, err := getBtcHeight(l.GetLocalDB(), req.GetData())
	if err != nil {
		elog.Error("Query_GetBtcHeaderByHash", "hash", req.GetData(), "err", err)
		return nil, err
	}
	header, err := getBtcHeader(l.GetLocalDB(), uint64(height.GetData()))
	if err != nil {
		return nil, err
	}
	state, err := getBtcChainState(l.GetStateDB())
	if err != nil {
		elog.Error("Query_GetBtcHeaderByHash getBtcChainState", "hash", req.GetData(), "err", err)
		return nil, err
	}
	if err := checkBtcHeaderCanonical(state, header, allow); err != nil {
		return nil, err
	}
	return header, nil
}

func (l *lightclient) Query_GetBtcNetName(req *types.ReqNil) (types.Message, error) {
	return &types.ReplyString{Data: lightCfg.BtcNetName}, nil
}

// Query_GetBtcCheckpoint 返回本网络最高的有效锚点（= bootstrap 的信任根），复用 ltypes.BtcHeader，
// 不新增 proto。
//
// 用途（L2）：中继在链上头链还是空（tip==0）时用它断言"本地推出的起点 == 本查询给出的锚点高度 + 1"。
// 那个起点不是配置项，而是中继自己从同一份 chaincfg 推出的（neutrino 的 btcHeaderStartHeight）；
// 起点与锚点不配套的话，bootstrap 的每个批都会被执行器以 ErrBtcHeaderNoAnchor 拒收（见
// checkBootstrapAnchor），而中继自己不会退（同一个批重发多少次都一样）—— 中继启动期就把它变成一条
// 显式的 ERROR，而不是让人从运行期的报错里反推。两边对不上时**没有键可改**：唯一成因是两边 btcd 的
// checkpoint 表不同源（两边 build 的 btcd 版本不同，或本执行器启用了 extraBtcCheckpoints），修法是
// 同步升级两边的 btcd / 去掉额外锚点。
//
// 本网络没有锚点（regtest/testnet4/signet/simnet）时返回高度 0 的空头：调用方据此跳过断言，
// 不做硬依赖。
func (l *lightclient) Query_GetBtcCheckpoint(req *types.ReqNil) (types.Message, error) {
	params := ltypes.GetBtcChainParams(lightCfg.BtcNetName)
	height, hash, ok := highestCheckpoint(btcCheckpointTable[params.Net])
	if !ok {
		return &ltypes.BtcHeader{}, nil
	}
	return &ltypes.BtcHeader{Height: height, Hash: hash}, nil
}

// rgbxMinBtcConfirmationsDefault 链上最小确认数（B8）的默认值：与 rgbx 执行器
// （plugin/dapp/rgbx/executor/rgbx.go 的 defaultMinBtcConfirmations）保持一致。
const rgbxMinBtcConfirmationsDefault = int64(6)

// Query_GetRgbxMinBtcConfirmations 返回本链 rgbx 执行器**实际生效**的最小 BTC 确认数 N（B8）。
//
// 存在理由（N 的单一真相）：中继要在提交充值证明之前判断"提交那一刻链上看得见的确认深度够不够"，
// 就必须知道 N。N 属于 rgbx 执行器（`[exec.sub.rgbx].minBtcConfirmations`，默认 6），中继既不想、
// 也不该自己再配一份必须人肉对齐的值 —— 于是链上把它暴露成一次只读查询，中继读它就够了：
//   - 配置只有一份：`[exec.sub.rgbx].minBtcConfirmations`；
//   - 取值口径只有一份：这里按 json 解码该段，规则与 rgbx 执行器 initCfg 完全一致
//     （正数用它；未配置 / 非正数一律取默认值 6，不允许用 0 表达"不校验深度"）。
//
// 复用 types.Int64，不新增 proto。链上没有 rgbx 执行器（缺 `[exec.sub.rgbx]` 段）时返回默认值 6：
// 那种链上本来也提交不了 rgbx Deposit（执行器不存在），给默认值不影响任何行为。
//
// 中继侧的用法与失败取向见 neutrino 的 RgbxMinBtcConfirmations：查询不到就不提交（fail-closed）。
func (l *lightclient) Query_GetRgbxMinBtcConfirmations(_ *types.ReqNil) (types.Message, error) {
	var cfg *types.Chain33Config
	if api := l.GetAPI(); api != nil {
		cfg = api.GetConfig()
	}
	return &types.Int64{Data: rgbxMinBtcConfirmations(cfg)}, nil
}

// rgbxMinBtcConfirmations 从链配置里取 rgbx 执行器生效的最小确认数（无配置/取不到时给默认值）。
func rgbxMinBtcConfirmations(cfg *types.Chain33Config) int64 {
	if cfg == nil || cfg.GetSubConfig() == nil {
		return rgbxMinBtcConfirmationsDefault
	}
	// 段名 = 执行器名（chain33 pluginmgr 用 `cfg.GetSubConfig().Exec[execName]` 取子配置，见
	// pluginmgr/base.go 的 InitExec），rgbx 的执行器名就是 rgbxtypes.RgbxX。
	return rgbxMinBtcConfirmationsFromSub(cfg.GetSubConfig().Exec[rgbxtypes.RgbxX])
}

// rgbxMinBtcConfirmationsFromSub 解析 `[exec.sub.rgbx]` 段（子配置以 json 存放）。
// 口径与 rgbx 执行器 initCfg 一致，方向都是 fail-closed：解不出来 / 配了非正数都取默认值 6，
// 绝不解释成"不做深度校验"。
func rgbxMinBtcConfirmationsFromSub(sub []byte) int64 {
	if len(sub) == 0 {
		return rgbxMinBtcConfirmationsDefault
	}
	c := struct {
		MinBtcConfirmations int64 `json:"minBtcConfirmations"`
	}{}
	if err := json.Unmarshal(sub, &c); err != nil {
		// 同一段字节在启动期由 rgbx 执行器用 types.MustDecode(=json.Unmarshal) 解过，
		// 解不出来说明 rgbx 那边已经 panic 起不来，这里只是不让查询把它变成 panic。
		elog.Error("rgbxMinBtcConfirmationsFromSub decode [exec.sub.rgbx]", "err", err)
		return rgbxMinBtcConfirmationsDefault
	}
	if c.MinBtcConfirmations <= 0 {
		return rgbxMinBtcConfirmationsDefault
	}
	return c.MinBtcConfirmations
}
