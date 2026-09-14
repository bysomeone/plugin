package rgb20

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖签名节点侧的「可支配额覆盖」核对（S1）与 A2 残留的语义修正：
//   - 可支配额 = 签名节点自己算出的「本笔实际动用的 seal 面额合计」
//     （锚定在本笔 PSBT 交易上的 opened seal 面额之和；RGB 守恒 ⇒ 等于被花 seal 面额）；
//   - 核对覆盖：可支配额 ≥ 提现额，且发给用户的资产 ≤ 链上 pending 提现额；
//   - 算不出可支配额一律 fail-closed 拒绝。

const coverageSealOutpoint = "1111111111111111111111111111111111111111111111111111111111111111:0"

// anchoredSeal 一个锚定在本笔提现交易上的 opened seal（vout → 面额）。
type anchoredSeal struct {
	vout   uint32
	amount int64
}

// psbtTxid 取未签 PSBT 的 txid —— 侧车以它作为状态转移的 witness id（seal 锚定在此 txid 上）。
func psbtTxid(t *testing.T, psbtBytes []byte) string {
	t.Helper()
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	require.NoError(t, err)
	return p.UnsignedTx.TxHash().String()
}

// anchoredConsignment 构造一份「锚定在本笔提现交易上」的侧车校验结果：
//   - amount 是侧车报的单点数字（ConsignmentValidation.amount）。真机上它是"历史中第一个
//     TSS 脚本 opened seal"的面额（充值收据口径），对提现只会随 bundle 顺序落在"找零 seal"
//     或"更早的充值收据"上——正是 A2 误拒的来源，本文件的用例按真机情形把它设成找零。
//   - seals 是本笔交易上打开的 seal（outpoint 锚定在本笔 PSBT 的 txid 上）。
func anchoredConsignment(t *testing.T, psbtBytes []byte, amount int64, closedSeals []string,
	seals ...anchoredSeal) *pb.ConsignmentValidation {
	t.Helper()
	anchor := psbtTxid(t, psbtBytes)
	v := &pb.ConsignmentValidation{
		Valid:        true,
		Amount:       amount,
		SyncedHeight: 200,
		ClosedSeals:  closedSeals,
	}
	for _, s := range seals {
		v.OpenedSeals = append(v.OpenedSeals, &pb.OpenedSeal{
			Outpoint: fmt.Sprintf("%s:%d", anchor, s.vout),
			Amount:   s.amount,
		})
	}
	return v
}

// buildWithdrawPSBT 构造未签提现 PSBT：输入 = sealOutpoint（prevout 脚本与面额由参数给出），
// 输出由参数给出。
func buildWithdrawPSBT(t *testing.T, sealOutpoint string, inputScript []byte, inputValue int64,
	outputs ...*wire.TxOut) []byte {
	t.Helper()
	op, err := wire.NewOutPointFromString(sealOutpoint)
	require.NoError(t, err)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(op, nil, nil))
	for _, out := range outputs {
		tx.AddTxOut(out)
	}
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: inputValue, PkScript: inputScript}
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	return buf.Bytes()
}

// buildAnchoredWithdrawPSBT 构造真机布局的提现 PSBT：vout0 = OP_RETURN（RGB 承诺，不可花）、
// vout1 = 收款 dust（非 TSS 脚本，离开桥控制）、vout2 = 找零回 TSS。
func buildAnchoredWithdrawPSBT(t *testing.T, sealOutpoint string) []byte {
	t.Helper()
	tss := (&fakeBridge{}).TSSPkScript()
	return buildWithdrawPSBT(t, sealOutpoint, tss, 5000,
		wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}),
		wire.NewTxOut(546, []byte{0x51}),
		wire.NewTxOut(4000, tss),
	)
}

// Test_WithdrawValidate_Coverage 覆盖核对（S1）：签名节点自己算出的可支配额（本笔动用的
// seal 面额合计）必须覆盖提现额，且发给用户的资产不得超过链上 pending 提现额。
func Test_WithdrawValidate_Coverage(t *testing.T) {
	const withdraw = int64(500000)
	cases := []struct {
		name     string
		leaving  int64 // 锚定 seal 中离开桥控制的部分（发给用户）
		change   int64 // 锚定 seal 中回流 TSS 的部分（找零）
		expected int64
		wantErr  string
	}{
		{
			name:    "covered: spent seal face covers the withdrawal",
			leaving: withdraw, change: 200000, expected: withdraw,
		},
		{
			name:    "insufficient: spent seal face below the withdrawal",
			leaving: 200000, change: 0, expected: withdraw,
			wantErr: "withdraw amount exceeds sealed balance",
		},
		{
			name:    "overpay: payout to the user exceeds the pending amount",
			leaving: 2 * withdraw, change: 100000, expected: withdraw,
			wantErr: "withdraw payout exceeds pending amount",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			psbtBytes := buildAnchoredWithdrawPSBT(t, coverageSealOutpoint)
			seals := []anchoredSeal{{vout: 1, amount: tc.leaving}}
			if tc.change > 0 {
				seals = append(seals, anchoredSeal{vout: 2, amount: tc.change})
			}
			mock := NewMockSidecar()
			mock.ValidateResp = []*pb.ConsignmentValidation{
				anchoredConsignment(t, psbtBytes, tc.change, []string{coverageSealOutpoint}, seals...),
			}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            psbtBytes,
				Consignment:     []byte("consignment"),
				ExpectedAmount:  tc.expected,
				MinSyncedHeight: 100,
				FeeRate:         1,
			})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// Test_WithdrawValidate_GenesisOnlySealCoverage 锁住 A2 修复（纯 genesis seal 的合法提现
// 不再被误拒）。
//
// 桥只持有 genesis seal（无任何已上链的历史锚）时，consignment 里锚定在本笔提现交易上的
// opened seal 只有两个：收款 seal（提现额）与找零 seal（持仓 − 提现额）。侧车报的单点数字
// （历史中第一个 TSS 脚本 opened seal）= 找零，提现额 > 持仓一半时它小于提现额。改动前 Go 桥
// 直接拿这个数字和提现额比 ⇒ 合法提现被 "withdraw amount exceeds sealed balance" 误拒；
// 现在按被花 seal 面额合计（= 收款 + 找零 = 持仓）判定 ⇒ 通过。
func Test_WithdrawValidate_GenesisOnlySealCoverage(t *testing.T) {
	const (
		supply   = int64(1000000) // genesis seal 面额（桥的全部持仓）
		withdraw = int64(800000)  // 提现额 > 持仓一半（A2 误拒区间）
	)
	change := supply - withdraw

	psbtBytes := buildAnchoredWithdrawPSBT(t, coverageSealOutpoint)
	v := anchoredConsignment(t, psbtBytes, change, /* 侧车单点数字 = 找零 */
		[]string{coverageSealOutpoint},
		anchoredSeal{vout: 1, amount: withdraw}, anchoredSeal{vout: 2, amount: change})

	// 对照：改动前的判据（consignment 单点数字 < 提现额 ⇒ 拒绝）会误拒这笔合法提现。
	require.Less(t, v.Amount, withdraw,
		"对照：侧车单点数字（找零 %d）小于提现额 %d，改动前会误拒", v.Amount, withdraw)

	mock := NewMockSidecar()
	mock.ValidateResp = []*pb.ConsignmentValidation{v}
	adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
	defer cleanup()

	require.NoError(t, adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
		Psbt:            psbtBytes,
		Consignment:     []byte("consignment"),
		ExpectedAmount:  withdraw,
		MinSyncedHeight: 100,
		FeeRate:         1,
	}), "纯 genesis seal 的合法提现必须通过（被花 seal 面额 %d ≥ 提现额 %d）", supply, withdraw)
}

// Test_WithdrawValidate_CoverageFailClosed 算不出可支配额时必须拒绝（S1 fail-closed）：
// consignment 与待签交易对不上、seal 指向交易外的 vout、seal 锚在不可花输出上、面额为负。
func Test_WithdrawValidate_CoverageFailClosed(t *testing.T) {
	const withdraw = int64(500000)
	psbtBytes := buildAnchoredWithdrawPSBT(t, coverageSealOutpoint)
	anchor := psbtTxid(t, psbtBytes)
	foreignTxid := strings.Repeat("cc", 32)

	cases := []struct {
		name    string
		opened  []*pb.OpenedSeal
		wantErr string
	}{
		{
			name:    "consignment does not commit to this transaction",
			opened:  []*pb.OpenedSeal{{Outpoint: foreignTxid + ":1", Amount: withdraw}},
			wantErr: "opens no seal at this transaction",
		},
		{
			name:    "seal points outside the transaction",
			opened:  []*pb.OpenedSeal{{Outpoint: anchor + ":9", Amount: withdraw}},
			wantErr: "points outside the transaction",
		},
		{
			name:    "seal anchored at an unspendable output",
			opened:  []*pb.OpenedSeal{{Outpoint: anchor + ":0", Amount: withdraw}},
			wantErr: "unspendable output",
		},
		{
			name:    "negative seal amount",
			opened:  []*pb.OpenedSeal{{Outpoint: anchor + ":1", Amount: -1}},
			wantErr: "invalid seal amount",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockSidecar()
			mock.ValidateResp = []*pb.ConsignmentValidation{{
				Valid:        true,
				Amount:       withdraw,
				SyncedHeight: 200,
				ClosedSeals:  []string{coverageSealOutpoint},
				OpenedSeals:  tc.opened,
			}}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            psbtBytes,
				Consignment:     []byte("consignment"),
				ExpectedAmount:  withdraw,
				MinSyncedHeight: 100,
				FeeRate:         1,
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Test_WithdrawValidate_LedgerFaceCrossCheck 交叉核对（S1）：签名节点自己 seal 账本里被花
// seal 的面额合计不得超过 consignment 体现的转移总量——两边视图分叉即拒绝；一致（或账本视图
// 少算，如 genesis seal 未经 Go 侧登记）时放行。
func Test_WithdrawValidate_LedgerFaceCrossCheck(t *testing.T) {
	cases := []struct {
		name        string
		ledgerFace  int64 // 签名节点 seal 账本（侧车 ListSeals）里该 seal 的面额
		ledgerKnown bool  // 账本里是否登记了该 outpoint
		wantErr     string
	}{
		{
			name:        "ledger face matches the consignment transition",
			ledgerFace:  700000,
			ledgerKnown: true,
		},
		{
			name:        "ledger face below the consignment (view incomplete: genesis seal unregistered)",
			ledgerKnown: false,
		},
		{
			name:        "ledger face exceeds the consignment transition (views diverged)",
			ledgerFace:  900000,
			ledgerKnown: true,
			wantErr:     "seal face mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			psbtBytes := buildAnchoredWithdrawPSBT(t, coverageSealOutpoint)
			mock := NewMockSidecar()
			// 锚定 seal：收款 500000（离开桥控制）+ 找零 200000（回流 TSS）⇒ 可支配额 700000。
			mock.ValidateResp = []*pb.ConsignmentValidation{
				anchoredConsignment(t, psbtBytes, 200000, []string{coverageSealOutpoint},
					anchoredSeal{vout: 1, amount: 500000}, anchoredSeal{vout: 2, amount: 200000}),
			}
			if tc.ledgerKnown {
				mock.seals[coverageSealOutpoint] = &pb.SealInfo{
					Outpoint: coverageSealOutpoint,
					Amount:   tc.ledgerFace,
					Status:   SealStatusMinted,
				}
			}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            psbtBytes,
				Consignment:     []byte("consignment"),
				ExpectedAmount:  500000,
				MinSyncedHeight: 100,
				FeeRate:         1,
			})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// Test_WithdrawValidate_ExistingGuardsStillHold 新的覆盖核对不削弱既有护栏：
// 输入必须受 TSS 控制、离开桥控制的输出只能有一个且限 dust。
func Test_WithdrawValidate_ExistingGuardsStillHold(t *testing.T) {
	t.Run("input not controlled by the tss script", func(t *testing.T) {
		tss := (&fakeBridge{}).TSSPkScript()
		psbtBytes := buildWithdrawPSBT(t, coverageSealOutpoint, []byte{0x51}, 5000,
			wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}),
			wire.NewTxOut(546, []byte{0x51}),
			wire.NewTxOut(4000, tss),
		)
		mock := NewMockSidecar()
		mock.ValidateResp = []*pb.ConsignmentValidation{
			anchoredConsignment(t, psbtBytes, 500000, nil, anchoredSeal{vout: 1, amount: 500000}),
		}
		adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
		defer cleanup()

		err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
			Psbt: psbtBytes, Consignment: []byte("consignment"), ExpectedAmount: 500000,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not a TSS-controlled utxo")
	})

	t.Run("more than one non-tss output", func(t *testing.T) {
		tss := (&fakeBridge{}).TSSPkScript()
		psbtBytes := buildWithdrawPSBT(t, coverageSealOutpoint, tss, 5000,
			wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}),
			wire.NewTxOut(546, []byte{0x51}),
			wire.NewTxOut(546, []byte{0x52}),
			wire.NewTxOut(4000, tss),
		)
		mock := NewMockSidecar()
		mock.ValidateResp = []*pb.ConsignmentValidation{
			anchoredConsignment(t, psbtBytes, 500000, nil, anchoredSeal{vout: 1, amount: 500000}),
		}
		adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
		defer cleanup()

		err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
			Psbt: psbtBytes, Consignment: []byte("consignment"), ExpectedAmount: 500000,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "more than one non-TSS output")
	})

	t.Run("non-tss output above the dust cap", func(t *testing.T) {
		tss := (&fakeBridge{}).TSSPkScript()
		psbtBytes := buildWithdrawPSBT(t, coverageSealOutpoint, tss, 500000,
			wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}),
			wire.NewTxOut(rgb20RecipientDustCap+1, []byte{0x51}),
			wire.NewTxOut(4000, tss),
		)
		mock := NewMockSidecar()
		mock.ValidateResp = []*pb.ConsignmentValidation{
			anchoredConsignment(t, psbtBytes, 500000, nil, anchoredSeal{vout: 1, amount: 500000}),
		}
		adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
		defer cleanup()

		err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
			Psbt: psbtBytes, Consignment: []byte("consignment"), ExpectedAmount: 500000,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds dust cap")
	})
}
