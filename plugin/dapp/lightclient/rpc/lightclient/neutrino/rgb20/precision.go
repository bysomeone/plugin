package rgb20

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// PrecisionUnknown 是侧车报告"这个合约没有声明精度"的哨兵值。
//
// 为什么需要哨兵：`AssetInfo.precision` 是 proto3 的 `uint32`，**没有 presence** —— 不写与写 0
// 在线上是同一串字节。而 0 是合法精度（不可分割资产），所以"没声明"不能用 0 表示，只能用精度空间
// 之外的值。真实精度不可能到这个值（RGB 的 Precision 最大是 Atto=18，本仓 rgbx 的
// `rtypes.MaxPrecision` 是 8），因此不会与任何真精度混淆。
//
// **这是两侧共享的约定**：Rust 侧 `rgb_sidecar::service::PRECISION_UNKNOWN` 必须取同一个值
// （`u32::MAX`），`proto/rgb_sidecar.proto` 的 `AssetInfo.precision` 也写明了。改一个就必须同时
// 改另一个；`TestPrecisionUnknownCannotCollideWithARealPrecision` 把"哨兵在精度空间之外"这条
// 钉住，若哪天 RGB 的精度上限越过它，测试会失败，而不是某个合约静静被当成"未知"。
//
// 同一个值也是**链上**"精度尚未回填"的哨兵（rtypes.PrecisionUnknown，由本常量指向它）：
// 桥把侧车读到的真实精度写进链上记录之前，链上记录就是这个值。
const PrecisionUnknown uint32 = rtypes.PrecisionUnknown

// ErrContractPrecisionUnusable 表示侧车报告的合约精度**不可用于换算金额**：合约没声明精度、
// 声明了一个超出链上口径的值（含别的哨兵），或侧车根本不认识这个已注册的 assetId。
//
// 单独一个错误类型是为了让调用方（neutrino 的启动流程）能把它与"侧车暂时连不上 / 查询失败"这类
// 可重试错误区分开：前者是静态分歧（换来换去也不会变），必须**拒绝启动**；后者等下一次重试即可。
// 策略留在调用方，机制留在这里。
var ErrContractPrecisionUnusable = errors.New("rgb20 contract precision unusable")

// ContractPrecisions 读取每个已注册合约的**合约声明精度**（侧车 ListAssets），并逐个校验它可用。
//
// 返回 symbol → 精度（chain33 侧资产符号为键，与链上 crossChainInfo 的键同一口径）。
//
// 为什么放在启动路径上、且必须 fail-closed：金额换算（用户输入的十进制金额 → 最小单位）依赖精度，
// 而精度只能来自合约。配错（或合约没声明）时换算会整体差 10^(8-p) 倍，且**全程静默** ——
// 用户以为在提 1 USDT，实际烧掉的是 100 USDT 对应的最小单位。这与 #58 的合约身份校验同一取向：
// 宁可拒服务，不要静默算错钱。
//
// 判定（任何一条不通过即拒绝，错误包 ErrContractPrecisionUnusable）：
//   - `contracts[].assetId` **为空**：跳过并 WARN。空 assetId 等于"身份还没配"，此时无从确定是哪个
//     合约，且充值路径早已对空 assetId fail-closed（见 verifyAssetContract）——启动期再拒一次只会
//     让"先起节点、后回填 assetId"的部署（E2E 就是这么做的）起不来。
//   - 侧车里查不到该 assetId 的合约：拒绝。注册了一个侧车不认识的合约，没有任何东西能证明精度。
//   - 精度是 PrecisionUnknown：拒绝。合约没有自述精度（schema 里没有 `spec` global），侧车照样采纳它
//     （拒绝一个描述性字段缺失的合约比报一个不精确的更糟），但**桥不替它猜**。
//   - 精度 > rtypes.MaxPrecision：拒绝。超出链上口径能表达的精度（含其它哨兵值），一律不认。
//
// 所有不通过的项会**一次性全部返回**（不是碰到第一个就返回）：运维一次就能看到所有要改的配置，
// 而不是改一个重启一次。这是"合约精度是否可用"的**本地**判定；把它与"链上记录的精度是否相等"
// （neutrino 包里的 ensureCrossChainAssetPrecisions）叠加起来，才是完整的启动自检。
func (a *Adapter) ContractPrecisions(ctx context.Context) (map[string]uint32, error) {
	if len(a.cfg.Contracts) == 0 {
		return nil, nil
	}
	sc := a.sidecar.Load()
	if sc == nil {
		return nil, fmt.Errorf("rgb20 sidecar is not connected, cannot read contract precisions")
	}
	resp, err := sc.ListAssets(ctx, &pb.ListAssetsRequest{})
	if err != nil {
		// 暂时性失败：交给调用方重试，不当成"合约不可用"。
		return nil, fmt.Errorf("list sidecar assets to read contract precisions: %w", err)
	}
	byAssetID := make(map[string]*pb.AssetInfo, len(resp.GetAssets()))
	for _, ai := range resp.GetAssets() {
		if id := strings.TrimSpace(ai.GetAssetId()); id != "" {
			byAssetID[id] = ai
		}
	}

	precisions := make(map[string]uint32, len(a.cfg.Contracts))
	var problems []string
	for i := range a.cfg.Contracts {
		c := a.cfg.Contracts[i]
		wantID := strings.TrimSpace(c.AssetID)
		if wantID == "" {
			log.Warn("ContractPrecisions: contract declares no assetId, so its precision cannot be "+
				"verified or recorded on chain (deposits for it are already refused by the contract "+
				"identity check)",
				"symbol", c.Symbol)
			continue
		}
		ai, ok := byAssetID[wantID]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"symbol=%s assetId=%s: the sidecar knows no such contract, so nothing can vouch for its "+
					"precision (is the contract adopted by the sidecar? see RGB_SIDECAR_CONTRACTS)",
				c.Symbol, wantID))
			continue
		}
		got := ai.GetPrecision()
		switch {
		case got == PrecisionUnknown:
			problems = append(problems, fmt.Sprintf(
				"symbol=%s assetId=%s: the contract does not declare its precision (its schema has no "+
					"`spec` global), so there is no authoritative scale to convert amounts with; this "+
					"bridge will not guess one",
				c.Symbol, wantID))
		case got > rtypes.MaxPrecision:
			problems = append(problems, fmt.Sprintf(
				"symbol=%s assetId=%s: the contract reports precision=%d, above this chain's maximum of %d",
				c.Symbol, wantID, got, rtypes.MaxPrecision))
		default:
			precisions[c.Symbol] = got
		}
	}
	if len(problems) > 0 {
		// 排序只为让报错稳定（配置顺序变了不影响 diff / 测试断言）。
		sort.Strings(problems)
		return nil, fmt.Errorf("%w: %d contract(s) cannot be served: %s",
			ErrContractPrecisionUnusable, len(problems), strings.Join(problems, "; "))
	}
	return precisions, nil
}
