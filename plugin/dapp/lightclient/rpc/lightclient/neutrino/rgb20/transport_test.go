package rgb20

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeTransportGateway is an in-memory gateway used to drive the poll loop.
type fakeTransportGateway struct {
	mu          sync.Mutex
	consignment []byte
	getErr      error
	ackErr      error
	gets        int
	acks        []bool
}

func (f *fakeTransportGateway) ConsignmentGet(_ context.Context, recipientID string) (*ConsignmentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.consignment) == 0 {
		return nil, ErrConsignmentNotFound
	}
	return &ConsignmentResult{Consignment: f.consignment, TxID: strings111}, nil
}

const strings111 = "1111111111111111111111111111111111111111111111111111111111111111"

func (f *fakeTransportGateway) AckPost(_ context.Context, _, _ string, ack bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return false, f.ackErr
	}
	f.acks = append(f.acks, ack)
	return len(f.acks) == 1, nil
}

func (f *fakeTransportGateway) counts() (int, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, append([]bool(nil), f.acks...)
}

// newTransportTestAdapter connects an adapter to the mock sidecar without starting the
// background pollers, so each test drives `pollTransportGatewayOnce` itself.
func newTransportTestAdapter(t *testing.T, mock *MockSidecar, cfg func(*Config)) (*Adapter, func()) {
	t.Helper()
	seedMockAsset(mock, mockSidecarAssetID, 6)
	sock, cleanup := StartTestSidecar(t, mock)
	conf := Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: mockSidecarAssetID, MinDeposit: 100, MinWithdraw: 100},
		},
	}
	if cfg != nil {
		cfg(&conf)
	}
	adapter, err := NewAdapter(conf, NewMemStore())
	require.NoError(t, err)
	require.NoError(t, adapter.Connect(context.Background()))
	return adapter, cleanup
}

func newPendingReceive(t *testing.T, adapter *Adapter) *ReceiveRecord {
	t.Helper()
	rec, err := adapter.DepositFlow(context.Background(), signedDepositRequest(t, &DepositRequest{
		RequestID:   "req-1",
		AssetSymbol: "RGB20_USDT",
		Amount:      1000,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	}))
	require.NoError(t, err)
	require.NotEmpty(t, rec.RecipientID, "sidecar must return the invoice's recipient_id")
	return rec
}

func TestCreateReceive_PassesTransportEndpointAndStoresRecipientID(t *testing.T) {
	mock := NewMockSidecar()
	var gotEndpoint string
	mock.CreateReceiveFn = func(_ context.Context, req *pb.CreateReceiveRequest) (*pb.ReceiveData, error) {
		gotEndpoint = req.TransportEndpoint
		return &pb.ReceiveData{
			Invoice:     "rgb:invoice",
			ReceiveId:   "rcv-1",
			RecipientId: "bcrt:beneficiary",
		}, nil
	}
	adapter, cleanup := newTransportTestAdapter(t, mock, func(c *Config) {
		c.TransportEndpoint = "rpcs://gw.example.com/0.2/json-rpc"
	})
	defer cleanup()

	rec, err := adapter.DepositFlow(context.Background(), signedDepositRequest(t, &DepositRequest{
		RequestID: "req-1", AssetSymbol: "RGB20_USDT", Amount: 1000,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	}))
	require.NoError(t, err)
	require.Equal(t, "rpcs://gw.example.com/0.2/json-rpc", gotEndpoint)
	require.Equal(t, "bcrt:beneficiary", rec.RecipientID)
	// The mapping is persisted, so a restart can resume polling.
	stored, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "bcrt:beneficiary", stored.RecipientID)
}

func TestPollTransport_SuccessAcksTrueAndIsIdempotent(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	rec := newPendingReceive(t, adapter)

	var provided []byte
	mock.OnProvide = func(_ context.Context, req *pb.ProvideConsignmentRequest) (*pb.TransferState, error) {
		provided = req.Consignment
		return &pb.TransferState{
			ReceiveId: rec.ReceiveID, Status: "settled", Amount: rec.Amount,
			AssetId: mockSidecarAssetID, Txid: strings111, Vout: 0,
		}, nil
	}
	gw := &fakeTransportGateway{consignment: []byte("consignment-bytes")}

	require.NoError(t, adapter.pollTransportGatewayOnce(context.Background(), gw))
	require.Equal(t, []byte("consignment-bytes"), provided, "the fetched bytes go to the existing ProvideConsignment path")
	gets, acks := gw.counts()
	require.Equal(t, 1, gets)
	require.Equal(t, []bool{true}, acks)
	stored, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "true", stored.GatewayAck)

	// A second round must not fetch, re-validate or re-ACK.
	require.NoError(t, adapter.pollTransportGatewayOnce(context.Background(), gw))
	gets, acks = gw.counts()
	require.Equal(t, 1, gets)
	require.Equal(t, []bool{true}, acks)
}

func TestPollTransport_ValidationFailureAcksFalse(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	rec := newPendingReceive(t, adapter)

	mock.OnProvide = func(context.Context, *pb.ProvideConsignmentRequest) (*pb.TransferState, error) {
		return nil, status.Error(codes.Internal, "consignment invalid: no seal at the TSS script")
	}
	gw := &fakeTransportGateway{consignment: []byte("bad-consignment")}

	require.NoError(t, adapter.pollTransportGatewayOnce(context.Background(), gw))
	_, acks := gw.counts()
	require.Equal(t, []bool{false}, acks, "a rejected consignment must be NACKed")
	stored, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "false", stored.GatewayAck)

	require.NoError(t, adapter.pollTransportGatewayOnce(context.Background(), gw))
	_, acks = gw.counts()
	require.Equal(t, []bool{false}, acks, "repeated polling must not re-ACK")
}

func TestPollTransport_TransientSidecarErrorDoesNotAck(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	rec := newPendingReceive(t, adapter)

	mock.OnProvide = func(context.Context, *pb.ProvideConsignmentRequest) (*pb.TransferState, error) {
		return nil, status.Error(codes.Unavailable, "sidecar is restarting")
	}
	gw := &fakeTransportGateway{consignment: []byte("consignment-bytes")}

	err := adapter.pollTransportGatewayOnce(context.Background(), gw)
	require.Error(t, err)
	_, acks := gw.counts()
	require.Empty(t, acks, "cannot conclude invalidity when the sidecar is unreachable")
	stored, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Empty(t, stored.GatewayAck)
}

func TestPollTransport_GatewayNotFoundIsNotAnError(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	newPendingReceive(t, adapter)

	gw := &fakeTransportGateway{getErr: ErrConsignmentNotFound}
	require.NoError(t, adapter.pollTransportGatewayOnce(context.Background(), gw))
	_, acks := gw.counts()
	require.Empty(t, acks)
}

func TestPollTransport_GatewayHTTPErrorPropagates(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	newPendingReceive(t, adapter)

	gw := &fakeTransportGateway{getErr: &transportHTTPError{StatusCode: 503, Reason: "unavailable"}}
	err := adapter.pollTransportGatewayOnce(context.Background(), gw)
	require.Error(t, err)
	var httpErr *transportHTTPError
	require.ErrorAs(t, err, &httpErr)
	require.True(t, IsRetryableTransportError(httpErr))
	_, acks := gw.counts()
	require.Empty(t, acks)
}

func TestNextGatewayBackoff(t *testing.T) {
	require.Equal(t, 2*defaultGatewayPollInterval, nextGatewayBackoff(defaultGatewayPollInterval))
	require.Equal(t, gatewayMaxBackoff, nextGatewayBackoff(gatewayMaxBackoff))
	require.Equal(t, gatewayMaxBackoff, nextGatewayBackoff(gatewayMaxBackoff/2))
	require.Equal(t, defaultGatewayPollInterval, nextGatewayBackoff(0))
	// Never exceeds the cap.
	d := defaultGatewayPollInterval
	for i := 0; i < 20; i++ {
		d = nextGatewayBackoff(d)
		require.LessOrEqual(t, d, gatewayMaxBackoff)
	}
}

func TestStartWithoutTransportConfigDoesNotPanic(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, nil)
	defer cleanup()
	// No gateway and no endpoint configured: Start must simply not start the poller.
	require.NoError(t, adapter.Start(context.Background()))
	rec := newPendingReceive(t, adapter)
	require.NotEmpty(t, rec.RecipientID)
	adapter.Stop()
}

func TestStartWithGatewayConfigured(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newTransportTestAdapter(t, mock, func(c *Config) {
		c.TransportGateway = "http://127.0.0.1:1/0.2/json-rpc"
		c.TransportEndpoint = "rpcs://gw.example.com/0.2/json-rpc"
	})
	defer cleanup()
	require.NoError(t, adapter.Start(context.Background()))
	// Give the goroutine a moment to start, then stop: an unreachable gateway must not hang Stop.
	time.Sleep(10 * time.Millisecond)
	adapter.Stop()
}
