package neutrino

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * CommitDKG 的提交与**核对**（C2）。
 *
 * 为什么不能像以前那样"提交成功即认为完成"：
 *   - checkCommitDKG 对**所有** symbol（含 BTC/XBTC）都强制 33 字节压缩 pubkey，
 *     并校验 hash160(pubkey) == pkScript[2:]；
 *   - P2WSH 充值地址 = f(userID, tssPub)，桥发的地址与执行器重建的脚本必须用**同一把**群公钥。
 *
 * 于是"本节点提交过"完全不代表"链上那把钥是我的"：另一个 guardian 先提交、或本地 neutrino.db
 * 与链上不是同一代（数据目录被清后重跑 DKG），都会让两者静默错开 —— 桥照发地址、用户照打 BTC，
 * 而执行器按链上那把钥重建脚本，永远认不出这笔充值。旧实现用错误文案判成功
 * （submitMainChainTxUntilSuccess 里按 "duplicate" 子串），恰好会把这种错开也当成成功。
 *
 * 所以这里的成功判据是**链上状态**：链上 CrossChainInfo 存在、且 pubkey 逐字节等于本地公钥。
 * 提交只是"让链上达到该状态"的手段，失败/被拒/还没出块都只是"状态未达成"，会被周期性报出来，
 * 而不是静默地无限重试（旧行为在 E2E 里表现为"桥起不来"而不是"报错退出"）。
 */

const (
	// commitDKGVerifyInterval 两次核对之间的间隔。
	commitDKGVerifyInterval = 3 * time.Second
	// commitDKGStallErrorAfter 状态未达成持续这么久之后，日志升级为 ERROR。
	// 之前用 Info：等待出块、guardian 还没凑齐都是正常形态，不该一上来就报错。
	commitDKGStallErrorAfter = 30 * time.Second
	// commitDKGStallRepeatLogs 同一个原因每核对这么多次重报一条（首次与原因变化必报），避免刷屏。
	commitDKGStallRepeatLogs = 20
)

// buildCommitDKGPayload 组装本节点要提交的 CommitDKG。
//
// 返回 nil 表示本地 DKG 结果还不完整（公钥或地址缺失），调用方跳过提交：宁可明说
// "没提交"，也不要提交一份链上必拒（无 pubkey）或地址与公钥不配套（hash160 对不上）的载荷。
func (t *tssService) buildCommitDKGPayload(symbol string) *rtypes.CommitDKG {
	if t.tssPublicKey == nil || t.tssAddress == nil {
		log.Error("buildCommitDKGPayload local dkg result incomplete, skip commitDKG",
			"symbol", symbol, "hasPubkey", t.tssPublicKey != nil, "hasAddress", t.tssAddress != nil)
		return nil
	}
	payload := &rtypes.CommitDKG{
		AssetSymbol: symbol,
		DkgAddress:  t.tssAddress.EncodeAddress(),
		PkScript:    t.pkScript,
		// 33 字节压缩格式（btcec.ParsePubKey 后 SerializeCompressed）：非压缩编码会让同一把钥
		// 派生出另一个地址，checkCommitDKG 直接以 ErrInvalidDkgAddress 拒绝。
		Pubkey: t.tssPublicKey.SerializeCompressed(),
	}
	// 本地自检：链上的两条硬判据（33 字节压缩 + hash160(pubkey)==pkScript[2:]）在这里先过一遍，
	// 把"提交上去被拒"变成"提交前就报清楚"。
	if _, err := rtypes.ParseDepositTssPubKey(payload.GetPubkey()); err != nil {
		log.Error("buildCommitDKGPayload invalid tss pubkey", "symbol", symbol, "err", err)
		return nil
	}
	return payload
}

// commitDKGToChain 提交 CommitDKG 并阻塞到**链上出现同一把群公钥**为止（fail-closed，不设放弃）。
//
// 状态未达成时按原因分类报告（首次/原因变化/每 commitDKGStallRepeatLogs 次各报一条），
// 持续超过 commitDKGStallErrorAfter 升级为 ERROR —— 需要运维介入的形态（链上是另一把钥、
// guardian 未凑齐、本节点不是合法提交者）必须能一眼看见，且不该靠"多试几次"自愈。
func (t *tssService) commitDKGToChain(payload *rtypes.CommitDKG) error {
	return t.commitDKGToChainWith(t.client.ctx, payload, t.client.queryCrossChainInfoBounded, t.submitDKGToMainChain)
}

// submitDKGToMainChain 生产路径的提交依赖（单测用替身注入，见 commitDKGToChainWith）。
func (t *tssService) submitDKGToMainChain(exec, action string, payload *rtypes.CommitDKG) (string, error) {
	return t.client.submitMainChainTx(exec, action, payload)
}

// commitDKGToChainWith 是 commitDKGToChain 的实现，两个外部依赖（查链上状态、提交）显式传入，
// 便于单测覆盖三种判据（链上无记录 ⇒ 提交、链上另一把钥 ⇒ 不提交、链上同一把钥 ⇒ 不重复提交）。
//
// 返回值只有一种情况非 nil：**链上已有该 symbol 且是另一把群公钥**（不可自愈，调用方据此拒绝启动）。
// "链上还没出现 / 查询暂时不可用"都是等待，不是错误——它们会自愈（出块、grpc 恢复），
// 所以这个循环不设放弃、也不上抛。
func (t *tssService) commitDKGToChainWith(ctx context.Context, payload *rtypes.CommitDKG,
	query func(symbol string) (*rtypes.CrossChainInfo, error),
	submit func(exec, action string, payload *rtypes.CommitDKG) (string, error)) error {

	if payload == nil {
		return nil
	}
	symbol := payload.GetAssetSymbol()
	localPub := payload.GetPubkey()
	start := time.Now()

	// submitted 表示"本节点已成功投递过一次"：链上还没出现时不再重复投递（重复只会换来
	// ErrDuplicateDKGCommit，或者更糟——把同一份载荷反复灌进 mempool），等出块即可。
	var submitted bool
	var reported string
	var lastReportAt int

	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			log.Warn("commitDKGToChain client shutting down, stop waiting for the on-chain state",
				"symbol", symbol)
			return nil
		default:
		}
		info, err := query(symbol)
		// 防御：执行器对**不存在**的 symbol 返回空记录 + nil error（既有契约，见 client.go 的
		// crossChainInfoAbsentOnChain）。判据落在记录内容上：空记录一律按"链上还没有"处理，
		// 否则它会落进下面的"另一把钥"分支——那条分支不可自愈、也永远不会去提交（死锁）。
		if err == nil && crossChainInfoAbsentOnChain(info) {
			err = fmt.Errorf("%w: symbol=%s (the executor answers an empty record for an absent symbol)",
				errCrossChainInfoNotOnChain, symbol)
		}
		switch {
		case err == nil && bytes.Equal(info.GetPubkey(), localPub):
			log.Info("commitDKG confirmed on chain", "symbol", symbol, "attempt", attempt,
				"elapsed", time.Since(start).String(), "tssAddress", info.GetTssAddress())
			return nil

		case err == nil:
			// 链上已有该 symbol，但群公钥不是本地这把：**不可自愈**（同一 symbol 只能提交一次，
			// 换钥被 ErrDuplicateDKGCommit 挡住），桥发的充值地址执行器永远不认。
			//
			// 以前这里只是"报 stall 然后无限等"：日志刷得再多，节点也仍然活着、仍然参与签名，
			// 于是静态失灵被拖成"能跑但产出无人认"。现在直接上抛 —— 调用方 fail-closed
			// 拒绝启动（见 ensureDKGOnChainWith），把不可逆的后果挡在启动阶段。
			log.Error("commitDKG on-chain cross chain info carries a different tss pubkey",
				"symbol", symbol, "localPubkey", hexOrEmpty(localPub), "chainPubkey", hexOrEmpty(info.GetPubkey()),
				"chainTssAddress", info.GetTssAddress())
			return fmt.Errorf("%w: symbol=%s localPubkey=%s chainPubkey=%s",
				errChainGroupKeyMismatch, symbol, hexOrEmpty(localPub), hexOrEmpty(info.GetPubkey()))

		case errors.Is(err, errCrossChainInfoNotOnChain):
			if submitted {
				reportCommitDKGStall(&reported, &lastReportAt, attempt, start,
					"commitDKG submitted but the on-chain cross chain info is still missing",
					"symbol", symbol)
				break
			}
			hash, submitErr := submit(rtypes.RgbxX, rtypes.NameCommitDKGAction, payload)
			if submitErr != nil {
				reportCommitDKGStall(&reported, &lastReportAt, attempt, start,
					"submit commitDKG failed", "symbol", symbol, "err", submitErr)
				break
			}
			submitted = true
			reported, lastReportAt = "", 0
			log.Info("commitDKG submitted to main chain", "symbol", symbol, "txHash", hash,
				"dkgAddress", payload.GetDkgAddress())

		default:
			// 查询本身不可用（主链 grpc hang / rgbx 插件未起）：没有判据，不算失败也不算成功。
			reportCommitDKGStall(&reported, &lastReportAt, attempt, start,
				"cannot read the on-chain cross chain info", "symbol", symbol, "err", err)
		}
		time.Sleep(commitDKGVerifyInterval)
	}
}

// reportCommitDKGStall 报告"commitDKG 状态未达成"。
//
// 同一原因不重复刷屏（首次与原因变化必报，之后每 commitDKGStallRepeatLogs 次一条）；
// 持续超过 commitDKGStallErrorAfter 的等待升级为 ERROR，之前只记 Info（等出块是正常的）。
func reportCommitDKGStall(reported *string, lastReportAt *int, attempt int, start time.Time,
	reason string, kv ...interface{}) {

	if *reported == reason && attempt-*lastReportAt < commitDKGStallRepeatLogs {
		log.Debug("commitDKG not confirmed on chain, still waiting", append([]interface{}{
			"attempt", attempt, "reason", reason}, kv...)...)
		return
	}
	*reported, *lastReportAt = reason, attempt
	args := append([]interface{}{
		"attempt", attempt, "reason", reason, "elapsed", time.Since(start).String()}, kv...)
	if time.Since(start) >= commitDKGStallErrorAfter {
		log.Error("commitDKG not confirmed on chain, blocking on-chain deposit address derivation "+
			"(the bridge must not hand out deposit addresses derived from a key the chain does not have)", args...)
		return
	}
	log.Info("commitDKG not confirmed on chain yet", args...)
}

// hexOrEmpty 只用于日志：把（可能缺失的）公钥打成可读的十六进制，缺失时给出显式标记而不是空串。
func hexOrEmpty(b []byte) string {
	if len(b) == 0 {
		return "<empty>"
	}
	return hex.EncodeToString(b)
}
