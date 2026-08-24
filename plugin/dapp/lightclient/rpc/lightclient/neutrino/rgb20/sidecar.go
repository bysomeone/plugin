package rgb20

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// SidecarConfig 侧车连接配置。
type SidecarConfig struct {
	// Addr 侧车地址：unix socket 路径（/path/to.sock 或 unix:///path/to.sock）优先，
	// 否则按 TCP host:port 处理。
	Addr string
	// Timeout 单次 RPC 超时（默认 10s）。
	Timeout time.Duration
}

// Sidecar 是 RGB 侧车的 gRPC 客户端封装，向侧车发起全部 RGB 共识操作。
// 侧车为 watch-only 单脚本（wpkh(TSS 压缩公钥)），不持私钥；所有签名都由 TSS 组完成。
type Sidecar struct {
	client  pb.RgbSidecarClient
	conn    *grpc.ClientConn
	timeout time.Duration
}

// NewSidecar 建立到侧车的 gRPC 连接。unix socket 优先。
// 使用阻塞拨号 + 短超时，侧车不可达时快速失败（供调用方降级/跳过）。
func NewSidecar(ctx context.Context, cfg SidecarConfig) (*Sidecar, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("sidecar addr empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	dialTimeout := cfg.Timeout
	if dialTimeout > 5*time.Second {
		dialTimeout = 5 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	}
	target := cfg.Addr
	if strings.HasPrefix(cfg.Addr, "/") {
		// 裸路径 -> unix socket
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.Addr)
		}))
		target = "passthrough:///rgb-sidecar"
	} else if strings.HasPrefix(cfg.Addr, "unix://") {
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", strings.TrimPrefix(cfg.Addr, "unix://"))
		}))
		target = "passthrough:///rgb-sidecar"
	}
	conn, err := grpc.DialContext(dialCtx, target, opts...)
	if err != nil {
		return nil, fmt.Errorf("sidecar dial %q: %w", cfg.Addr, err)
	}
	return &Sidecar{
		client:  pb.NewRgbSidecarClient(conn),
		conn:    conn,
		timeout: cfg.Timeout,
	}, nil
}

// Close 关闭 gRPC 连接。
func (s *Sidecar) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Sidecar) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, s.timeout)
}

// CreateReceive 创建充值 receive（witness_receive），返回 invoice 与 receive_id。
func (s *Sidecar) CreateReceive(ctx context.Context, req *pb.CreateReceiveRequest) (*pb.ReceiveData, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.CreateReceive(cctx, req)
}

// ProvideConsignment 交付 consignment，触发结算。
func (s *Sidecar) ProvideConsignment(ctx context.Context, req *pb.ProvideConsignmentRequest) (*pb.TransferState, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ProvideConsignment(cctx, req)
}

// ListTransfers 轮询转账状态。
func (s *Sidecar) ListTransfers(ctx context.Context, req *pb.ListTransfersRequest) (*pb.ListTransfersResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ListTransfers(cctx, req)
}

// ValidateConsignment 只读校验 consignment（签名节点使用），返回 closed/open seals 与 synced_height。
func (s *Sidecar) ValidateConsignment(ctx context.Context, req *pb.ValidateConsignmentRequest) (*pb.ConsignmentValidation, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ValidateConsignment(cctx, req)
}

// Sync 触发侧车同步。
func (s *Sidecar) Sync(ctx context.Context, req *pb.SyncRequest) (*pb.SyncResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.Sync(cctx, req)
}

// ListSeals 列出侧车已知 seal。
func (s *Sidecar) ListSeals(ctx context.Context, req *pb.ListSealsRequest) (*pb.ListSealsResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ListSeals(cctx, req)
}

// GetBalance 查询侧车余额。
func (s *Sidecar) GetBalance(ctx context.Context, req *pb.GetBalanceRequest) (*pb.Balance, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.GetBalance(cctx, req)
}

// ListAssets 列出侧车支持的资产。
func (s *Sidecar) ListAssets(ctx context.Context, req *pb.ListAssetsRequest) (*pb.ListAssetsResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ListAssets(cctx, req)
}

// BuildWithdrawal 构造提现（send_begin 阶段），返回未签 PSBT + consignment。
func (s *Sidecar) BuildWithdrawal(ctx context.Context, req *pb.BuildWithdrawalRequest) (*pb.BuildWithdrawalResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.BuildWithdrawal(cctx, req)
}

// FinalizeWithdrawal 提交已签 PSBT（send_end 阶段），返回 txid 与打开的 seal。
func (s *Sidecar) FinalizeWithdrawal(ctx context.Context, req *pb.FinalizeWithdrawalRequest) (*pb.FinalizeWithdrawalResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.FinalizeWithdrawal(cctx, req)
}

// ParseBtcTx 解析 BTC 交易是否携带 RGB 承诺。
func (s *Sidecar) ParseBtcTx(ctx context.Context, req *pb.ParseBtcTxRequest) (*pb.ParseBtcTxResponse, error) {
	cctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.client.ParseBtcTx(cctx, req)
}
