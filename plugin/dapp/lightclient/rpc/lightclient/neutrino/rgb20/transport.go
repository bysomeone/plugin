package rgb20

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Consignment delivery through the standard RGB HTTP JSON-RPC transport.
//
// After `CreateReceive`, the bridge registers the invoice's `recipient_id` on the local receive
// record and polls the gateway for a consignment. The fetching/validation/ACK loop is strictly
// additive: the custom upload endpoint (http.go) stays available for E2E and out-of-band
// delivery, while the gateway is the production path.

const (
	// defaultGatewayPollInterval is how often a healthy gateway is polled.
	defaultGatewayPollInterval = 5 * time.Second
	// gatewayMaxBackoff caps the retry delay when the gateway is unavailable.
	gatewayMaxBackoff = time.Minute
)

// transportGateway is the subset of TransportClient the poll loop needs (an interface so tests
// can drive the loop without HTTP).
type transportGateway interface {
	ConsignmentGet(ctx context.Context, recipientID string) (*ConsignmentResult, error)
	AckPost(ctx context.Context, recipientID string, ack bool) (bool, error)
}

// runTransportGatewayPolling polls the gateway until the adapter stops. Failures back off
// exponentially (capped) and never block the rest of the bridge.
func (a *Adapter) runTransportGatewayPolling() {
	defer a.wg.Done()
	client, err := NewTransportClient(a.cfg.TransportGateway, nil)
	if err != nil {
		// A malformed endpoint is a configuration error; retrying cannot fix it. Log loudly and
		// leave the rest of the adapter running (the custom upload endpoint still works).
		log.Error("rgb20 transport gateway disabled: invalid transportGateway", "endpoint", a.cfg.TransportGateway, "err", err)
		return
	}
	log.Info("rgb20 transport gateway polling started", "endpoint", a.cfg.TransportGateway)

	delay := defaultGatewayPollInterval
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := a.pollTransportGatewayOnce(a.ctx, client); err != nil {
			next := nextGatewayBackoff(delay)
			log.Warn("rgb20 transport gateway poll failed; will retry",
				"endpoint", a.cfg.TransportGateway, "retryIn", next, "err", err)
			delay = next
			continue
		}
		delay = defaultGatewayPollInterval
	}
}

// nextGatewayBackoff doubles the retry delay up to gatewayMaxBackoff. A first failure moves from
// the normal interval to twice it, so a flapping gateway is not hammered.
func nextGatewayBackoff(current time.Duration) time.Duration {
	if current < defaultGatewayPollInterval {
		return defaultGatewayPollInterval
	}
	next := current * 2
	if next < current || next > gatewayMaxBackoff {
		return gatewayMaxBackoff
	}
	return next
}

// pollTransportGatewayOnce advances every created receive that has a recipient_id and has not
// been ACKed yet. It stops the round on the first gateway error so the caller can back off.
func (a *Adapter) pollTransportGatewayOnce(ctx context.Context, gw transportGateway) error {
	recs, err := a.receives.ListByStatus(ReceiveStatusCreated)
	if err != nil {
		return fmt.Errorf("list created receives: %w", err)
	}
	for _, rec := range recs {
		if rec.RecipientID == "" || rec.GatewayAck != "" {
			// Legacy receive (no sidecar recipient_id) or already ACKed: nothing to poll.
			continue
		}
		if err := a.pollTransportReceive(ctx, gw, rec); err != nil {
			return fmt.Errorf("receive %s: %w", rec.ReceiveID, err)
		}
	}
	return nil
}

// pollTransportReceive fetches, validates and ACKs one receive.
//
// Funds correctness: ACK true only after the sidecar accepted the consignment. A sidecar
// rejection is ACKed false. Transport/sidecar-unavailable errors are *not* ACKed at all — the
// bridge cannot conclude the consignment is invalid, so it retries later instead of telling the
// payer "rejected".
func (a *Adapter) pollTransportReceive(ctx context.Context, gw transportGateway, rec *ReceiveRecord) error {
	res, err := gw.ConsignmentGet(ctx, rec.RecipientID)
	if errors.Is(err, ErrConsignmentNotFound) {
		return nil // not delivered yet
	}
	if err != nil {
		return err
	}

	st, err := a.ProvideConsignment(ctx, res.Consignment, rec.ReceiveID)
	if err != nil {
		if !isConsignmentRejection(err) {
			return fmt.Errorf("provide consignment (transient, will retry): %w", err)
		}
		log.Error("rgb20 transport: sidecar rejected the consignment, recording ACK false",
			"receiveId", rec.ReceiveID, "txid", res.TxID, "err", err)
		return a.recordTransportAck(ctx, gw, rec, false)
	}
	// The sidecar is the authority on the settled outpoint. The gateway's txid is advisory; a
	// mismatch is worth surfacing but must not turn a successfully validated transfer into a NACK.
	if res.TxID != "" && st != nil && st.Txid != "" && !strings.EqualFold(res.TxID, st.Txid) {
		log.Warn("rgb20 transport: gateway txid differs from the settled outpoint",
			"receiveId", rec.ReceiveID, "gatewayTxid", res.TxID, "settledTxid", st.Txid)
	}
	return a.recordTransportAck(ctx, gw, rec, true)
}

// recordTransportAck posts the ACK and persists it. A repeated ACK with the same value is
// idempotent at the gateway (it answers false); once persisted, the receive is never polled again.
func (a *Adapter) recordTransportAck(ctx context.Context, gw transportGateway, rec *ReceiveRecord, ack bool) error {
	created, err := gw.AckPost(ctx, rec.RecipientID, ack)
	if err != nil {
		return fmt.Errorf("ack.post(%t): %w", ack, err)
	}
	// Re-read: `ProvideConsignment` persists the consignment bytes on the same record, so writing
	// the stale copy from the poll snapshot would drop them (and the later mint path needs them).
	stored, err := a.receives.Get(rec.ReceiveID)
	if err != nil {
		return fmt.Errorf("reload receive for gateway ack: %w", err)
	}
	stored.GatewayAck = strconv.FormatBool(ack)
	if err := a.receives.Put(stored); err != nil {
		return fmt.Errorf("persist gateway ack: %w", err)
	}
	log.Info("rgb20 transport: consignment ACK recorded",
		"receiveId", stored.ReceiveID, "ack", ack, "created", created)
	return nil
}

// isConsignmentRejection distinguishes "the sidecar validated and refused this consignment"
// from "the sidecar could not be reached / is shutting down". Only the former is a NACK.
func isConsignmentRejection(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		// Local failures (state store, encoding) are retryable, not a validation verdict.
		return false
	}
	switch st.Code() {
	case codes.OK, codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		return false
	default:
		return true
	}
}
