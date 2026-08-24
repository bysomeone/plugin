package rgb20

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// MockSidecar 内存假侧车，供 Go 单测驱动 rgb20 适配器。
type MockSidecar struct {
	pb.UnimplementedRgbSidecarServer

	mu             sync.Mutex
	receiveCounter int
	transfers      map[string]*pb.TransferState // receive_id -> 状态
	seals          map[string]*pb.SealInfo
	// 可配置行为
	ValidateResp    []*pb.ConsignmentValidation // 依次返回；耗尽后返回 valid=false
	BuildResp       *pb.BuildWithdrawalResponse
	FinalizeResp    *pb.FinalizeWithdrawalResponse
	ParseResp       *pb.ParseBtcTxResponse
	CreateReceiveFn func(ctx context.Context, req *pb.CreateReceiveRequest) (*pb.ReceiveData, error)
	OnProvide       func(ctx context.Context, req *pb.ProvideConsignmentRequest) (*pb.TransferState, error)
	SidecarAddr     string // 提供给 BuildWithdrawal 的找零地址（TSS 地址）
}

// NewMockSidecar 构造假侧车。
func NewMockSidecar() *MockSidecar {
	return &MockSidecar{
		transfers: make(map[string]*pb.TransferState),
		seals:     make(map[string]*pb.SealInfo),
	}
}

// StartTestSidecar 在临时 unix socket 上启动假侧车 gRPC 服务，返回 socket 路径与清理函数。
// 注意：unix socket 路径长度受限（~104），需用短临时目录。
func StartTestSidecar(t testing.TB, mock *MockSidecar) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rgb")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	sock := filepath.Join(dir, "s.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterRgbSidecarServer(srv, mock)
	go func() { _ = srv.Serve(lis) }()
	cleanup := func() {
		srv.Stop()
		_ = lis.Close()
		_ = os.RemoveAll(dir)
	}
	return sock, cleanup
}

func (m *MockSidecar) CreateReceive(_ context.Context, req *pb.CreateReceiveRequest) (*pb.ReceiveData, error) {
	if m.CreateReceiveFn != nil {
		return m.CreateReceiveFn(context.Background(), req)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receiveCounter++
	rid := fmt.Sprintf("recv-%d", m.receiveCounter)
	m.transfers[rid] = &pb.TransferState{
		ReceiveId: rid,
		Status:    "waiting-counterparty",
		Amount:    req.Amount,
		AssetId:   fmt.Sprintf("rgb:asset-%s", req.AssetSymbol),
	}
	return &pb.ReceiveData{
		Invoice:   fmt.Sprintf("rgb:invoice-%d", m.receiveCounter),
		ReceiveId: rid,
	}, nil
}

func (m *MockSidecar) ProvideConsignment(ctx context.Context, req *pb.ProvideConsignmentRequest) (*pb.TransferState, error) {
	if m.OnProvide != nil {
		return m.OnProvide(ctx, req)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rid := req.ReceiveIdHint
	st, ok := m.transfers[rid]
	if !ok {
		return nil, fmt.Errorf("receive %s not found", rid)
	}
	st.Status = "settled"
	st.Txid = "0000000000000000000000000000000000000000000000000000000000000001"
	st.Vout = 0
	return st, nil
}

func (m *MockSidecar) ListTransfers(_ context.Context, req *pb.ListTransfersRequest) (*pb.ListTransfersResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &pb.ListTransfersResponse{}
	for _, st := range m.transfers {
		if req.StatusFilter == "" || st.Status == req.StatusFilter {
			out.Transfers = append(out.Transfers, proto.Clone(st).(*pb.TransferState))
		}
	}
	return out, nil
}

func (m *MockSidecar) ValidateConsignment(_ context.Context, _ *pb.ValidateConsignmentRequest) (*pb.ConsignmentValidation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ValidateResp) > 0 {
		resp := m.ValidateResp[0]
		m.ValidateResp = m.ValidateResp[1:]
		return resp, nil
	}
	return &pb.ConsignmentValidation{Valid: false, ErrorMessage: "no mock validation"}, nil
}

func (m *MockSidecar) Sync(_ context.Context, _ *pb.SyncRequest) (*pb.SyncResponse, error) {
	return &pb.SyncResponse{}, nil
}

func (m *MockSidecar) ListSeals(_ context.Context, _ *pb.ListSealsRequest) (*pb.ListSealsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &pb.ListSealsResponse{}
	for _, s := range m.seals {
		out.Seals = append(out.Seals, proto.Clone(s).(*pb.SealInfo))
	}
	return out, nil
}

func (m *MockSidecar) GetBalance(_ context.Context, _ *pb.GetBalanceRequest) (*pb.Balance, error) {
	return &pb.Balance{}, nil
}

func (m *MockSidecar) ListAssets(_ context.Context, _ *pb.ListAssetsRequest) (*pb.ListAssetsResponse, error) {
	return &pb.ListAssetsResponse{}, nil
}

func (m *MockSidecar) BuildWithdrawal(_ context.Context, _ *pb.BuildWithdrawalRequest) (*pb.BuildWithdrawalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.BuildResp != nil {
		return m.BuildResp, nil
	}
	return nil, fmt.Errorf("BuildWithdrawal not configured in mock")
}

func (m *MockSidecar) FinalizeWithdrawal(_ context.Context, _ *pb.FinalizeWithdrawalRequest) (*pb.FinalizeWithdrawalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FinalizeResp != nil {
		return m.FinalizeResp, nil
	}
	return nil, fmt.Errorf("FinalizeWithdrawal not configured in mock")
}

func (m *MockSidecar) ParseBtcTx(_ context.Context, _ *pb.ParseBtcTxRequest) (*pb.ParseBtcTxResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ParseResp != nil {
		return m.ParseResp, nil
	}
	return &pb.ParseBtcTxResponse{}, nil
}
