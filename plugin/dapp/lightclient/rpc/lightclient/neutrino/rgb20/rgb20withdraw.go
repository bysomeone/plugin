package rgb20

import "context"

// WithdrawFlow 提现入口的薄封装：invoice → BuildWithdrawal → TSS 签 → Finalize → txid↔pending。
// 供 neutrino 主包 withdrawalProcessor 的 RGB 分支调用（mutex 串行在 Withdraw 内部保证）。
func (a *Adapter) WithdrawFlow(ctx context.Context, req *WithdrawRequest) (*WithdrawResult, error) {
	return a.Withdraw(ctx, req)
}
