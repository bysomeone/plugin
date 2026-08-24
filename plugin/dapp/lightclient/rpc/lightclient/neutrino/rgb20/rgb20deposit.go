package rgb20

import "context"

// DepositFlow 充值入口的薄封装：创建 receive 并持久化 receive_id↔Chain33 请求映射。
// 供 HTTP 充值请求 API（http.go）与主链/桥请求使用。
func (a *Adapter) DepositFlow(ctx context.Context, req *DepositRequest) (*ReceiveRecord, error) {
	return a.CreateReceive(ctx, req)
}
