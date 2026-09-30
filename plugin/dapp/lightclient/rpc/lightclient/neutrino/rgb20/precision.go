package rgb20

import (
	"context"
	"errors"
	"fmt"
	"math"
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
const PrecisionUnknown uint32 = math.MaxUint32

// ErrContractPrecisionMismatch 表示"配置声明的精度"与"合约自述的精度"对不上（或合约根本没声明）。
//
// 单独一个错误类型是为了让调用方（neutrino 的启动流程）能把它与"侧车暂时连不上/查询失败"这类
// 可重试错误区分开：前者是静态的配置/合约分歧，重试一万次也不会变，必须**拒绝启动**；后者等下一次
// 重试即可。策略留在调用方，机制留在这里。
var ErrContractPrecisionMismatch = errors.New("rgb20 contract precision mismatch")

// VerifyContractPrecisions 核对每个已注册合约：**侧车报告的合约精度必须存在、合法，且等于配置里
// 声明的 `contracts[].precision`**。
//
// 为什么必须在启动时做、且必须 fail-closed：金额换算（用户输入的十进制金额 → 链上最小单位）依赖
// 精度，而精度只能来自合约。配错一个数（或合约没声明）时，换算会整体差 10^(8-p) 倍，且**全程静默**
// —— 用户以为在提 1 USDT，实际烧掉的是 100 USDT 对应的最小单位。这与 #58 的合约身份校验同一取向：
// 宁可拒服务，不要静默算错钱。
//
// 判定（全部不通过即拒绝）：
//   - `contracts[].assetId` **为空**：跳过并 WARN。空 assetId 等于"身份还没配"，此时无从确定是哪个
//     合约，且充值路径早已对空 assetId fail-closed（见 verifyAssetContract）——启动期再拒一次只会
//     让"先起节点、后回填 assetId"的部署（E2E 就是这么做的）起不来。
//   - 侧车里查不到该 assetId 的合约：拒绝。配置声明了一个侧车不认识的合约，没有任何东西能证明精度。
//   - 精度是 PrecisionUnknown：拒绝。合约没有自述精度（schema 里没有 `spec` global），侧车照样采纳它
//     （拒绝一个描述性字段缺失的合约比报一个不精确的更糟），但**桥不替它猜**。
//   - 精度 > rtypes.MaxPrecision：拒绝。超出链上口径能表达的精度（含其它哨兵值），一律不认。
//   - 精度 != contracts[].precision：拒绝。两侧都在，且不一致 —— 这就是那个静默 100 倍误差。
//
// 所有不通过的项会**一次性全部返回**（不是碰到第一个就返回）：运维一次就能看到所有要改的配置，
// 而不是改一个重启一次。返回的错误都包了 ErrContractPrecisionMismatch，供调用方判类型。
func (a *Adapter) VerifyContractPrecisions(ctx context.Context) error {
	if len(a.cfg.Contracts) == 0 {
		return nil
	}
	sc := a.sidecar.Load()
	if sc == nil {
		return fmt.Errorf("rgb20 sidecar is not connected, cannot verify contract precisions")
	}
	resp, err := sc.ListAssets(ctx, &pb.ListAssetsRequest{})
	if err != nil {
		// 暂时性失败：交给调用方重试，不当成"配置不一致"。
		return fmt.Errorf("list sidecar assets to verify contract precisions: %w", err)
	}
	byAssetID := make(map[string]*pb.AssetInfo, len(resp.GetAssets()))
	for _, ai := range resp.GetAssets() {
		if id := strings.TrimSpace(ai.GetAssetId()); id != "" {
			byAssetID[id] = ai
		}
	}

	var problems []string
	for i := range a.cfg.Contracts {
		c := a.cfg.Contracts[i]
		wantID := strings.TrimSpace(c.AssetID)
		if wantID == "" {
			log.Warn("VerifyContractPrecisions: contract declares no assetId, so its precision cannot be "+
				"verified (deposits for it are already refused by the contract identity check)",
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
		case got != c.Precision:
			problems = append(problems, fmt.Sprintf(
				"symbol=%s assetId=%s: configured precision=%d but the contract declares precision=%d "+
					"(amounts would be off by 10^%d)",
				c.Symbol, wantID, c.Precision, got, int64(c.Precision)-int64(got)))
		}
	}
	if len(problems) > 0 {
		// 排序只为让报错稳定（配置顺序变了不影响 diff / 测试断言）。
		sort.Strings(problems)
		return fmt.Errorf("%w: %d contract(s) cannot be served: %s",
			ErrContractPrecisionMismatch, len(problems), strings.Join(problems, "; "))
	}
	return nil
}
