package executor

import (
	"bytes"
	"errors"
	"testing"

	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 本文件补 E14 去重守卫缺失的一条断言：**读失败必须 fail-closed**。
//
// checkConfirmNotUsed 的取键语义（见 checktx.go 的注释）与提现侧 S3（formatWithdrawUsedKey）对称：
//   - ErrNotFound ⇒ 没结算过，放行；
//   - **其余任何错误（含键存在）⇒ 一律拒绝** —— 不把"读不到"当成"没结算过"。
//
// 前一半（ErrNotFound 放行 / 键存在拒绝）由 confirm_dedup_test.go 覆盖；后一半（读错误 ⇒ 拒绝）
// 补测之前没有任何用例 —— 而它正是这条护栏最容易被"顺手简化"掉的地方：把实现写成
// `if err != nil { return nil }`（或把 `errors.Is(err, types.ErrNotFound)` 的取向写反）仍然能让
// 既有用例全绿，但共识结论会退化成"库读不出来 ⇒ 当作没结算过 ⇒ 放行二次结算（二次铸币）"。
// 本用例就是那条语义的反向验证：去掉它就变红。

// e14DedupReadErrKV 只让**某一把键**的读失败（其余键走真库）。
//
// 为什么要这么精确：把失败点放在别处（例如 payload 键）会被更早的错误码拦下，
// 那样本用例就证明不了"拒绝来自去重键的读失败"。
type e14DedupReadErrKV struct {
	db.KV
	failKey []byte
	err     error
}

func (d *e14DedupReadErrKV) Get(key []byte) ([]byte, error) {
	if bytes.Equal(key, d.failKey) {
		return nil, d.err
	}
	return d.KV.Get(key)
}

// Test_rgbx_confirmDedup_stateReadErrorFailsClosed E14 ③：去重键读失败（非 ErrNotFound）⇒ 拒绝。
func Test_rgbx_confirmDedup_stateReadErrorFailsClosed(t *testing.T) {
	const symbol = "e14readerr"

	// mint 与 transfer 两条分支都要 fail-closed（错误码不同、判定同源）。
	for _, tc := range []struct {
		name      string
		action    int32
		expectErr error
	}{
		{"mint", rtypes.TyMintAction, ErrMintAlreadyConfirmed},
		{"transfer", rtypes.TyTransferAction, ErrTransferAlreadyConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newE14ConfirmFixture(t, symbol+"-"+tc.name, tc.action)
			if tc.action == rtypes.TyMintAction {
				f.setPayload(t, &rtypes.MintAsset{Symbol: symbol, TotalAmount: 7})
			} else {
				addr, _ := util.Genaddress()
				f.setPayload(t, &rtypes.TransferAsset{
					Symbol: symbol, Amount: 3, FromUtxo: f.pendingUtxo, To: addr,
				})
				f.fundAsset(t, symbol, 10)
			}

			// 对照组（用例前提）：真库、去重键不存在（ErrNotFound）⇒ 放行。
			// 没有这一条，下面那条"被拒"就无法归因到"读错误的取向"上。
			require.NoError(t, f.r.checkConfirm(testCommitAddr, "txH0", f.confirm),
				"用例前提：去重键不存在时 checkConfirm 必须放行")

			// 关键：让去重键的读以**非 ErrNotFound** 的错误失败（模拟库损坏 / 句柄失效）。
			f.r.SetStateDB(&e14DedupReadErrKV{
				KV:      f.state,
				failKey: formatConfirmUsedKey(f.txHash),
				err:     errors.New("storage unavailable"),
			})
			require.Equal(t, tc.expectErr, f.r.checkConfirm(testCommitAddr, "txH1", f.confirm),
				"去重键读失败必须 fail-closed：读不到 ≠ 没结算过（当成放行就允许二次结算）")

			// 反向对照：失败点换成**别的键**（去重键照常读到 ErrNotFound）⇒ 必须走到更早/别的判据，
			// 而不是"换了 stateDB 就一律拒绝"。payload 键被读失败 ⇒ ErrConfirmPayloadNotExist。
			f.r.SetStateDB(&e14DedupReadErrKV{
				KV:      f.state,
				failKey: formatPayloadKey(f.txHash),
				err:     errors.New("storage unavailable"),
			})
			require.Equal(t, ErrConfirmPayloadNotExist, f.r.checkConfirm(testCommitAddr, "txH2", f.confirm),
				"失败点不在去重键上时，拒绝必须来自那处判据（否则上一条的错误码不是去重守卫给的）")
		})
	}
}
