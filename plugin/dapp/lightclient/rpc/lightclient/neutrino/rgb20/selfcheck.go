package rgb20

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Startup consistency self-check against the consignment gateway.
//
// Misconfiguration here is silent in the money path: the gateway verifies the chain title in
// every deposit request, and /bridge/* refuses a wrong token, so the symptom is "users never get
// an invoice" rather than an error at startup. Two cheap, best-effort probes make that loud:
//
//	a) server.info chain_title must equal this adapter's ChainTitle;
//	b) GET /bridge/events?limit=1 must not answer 401/403.
//
// The checks never block startup. They run in their own goroutine bounded by one short overall
// deadline, and a failure never crashes the process (the bridge keeps serving withdrawals and
// confirmations); it only logs.

const (
	// gatewaySelfCheckTimeout bounds the whole self-check, including the single title retry.
	gatewaySelfCheckTimeout = 5 * time.Second
	// gatewaySelfCheckRetryDelay is the pause before re-reading a mismatched chain title once.
	gatewaySelfCheckRetryDelay = 2 * time.Second
)

// selfCheckSeverity is how a probe outcome should be reported.
type selfCheckSeverity int

const (
	selfCheckOK selfCheckSeverity = iota
	selfCheckWarn
	selfCheckError
)

// chainTitleMatches reports whether the gateway's title equals the locally configured one.
// The title is part of a signed byte string, so the comparison is exact: a whitespace difference
// would make the bridge reject valid requests just as surely as a different title.
func chainTitleMatches(gatewayTitle, configuredTitle string) bool {
	return gatewayTitle == configuredTitle
}

// interpretGatewayEventsStatus classifies the gateway's response to the token probe. A rejected
// token (401/403) is an operator error and must be loud; every other non-2xx answer is a warning
// (the gateway may be starting, behind a proxy, or partially degraded).
func interpretGatewayEventsStatus(status int) (selfCheckSeverity, string) {
	switch {
	case status >= 200 && status < 300:
		return selfCheckOK, ""
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return selfCheckError, "rgb20 gateway token rejected"
	default:
		return selfCheckWarn, "rgb20 gateway events endpoint returned an unexpected status"
	}
}

// runGatewaySelfCheck is started by Start when transportGateway and gatewayToken are configured.
func (a *Adapter) runGatewaySelfCheck() {
	defer a.wg.Done()

	base, err := gatewayBaseURL(a.cfg.TransportGateway)
	if err != nil {
		log.Warn("rgb20 gateway self-check skipped: invalid transportGateway", "err", err)
		return
	}
	client := &http.Client{Timeout: gatewaySelfCheckTimeout}
	tc, err := NewTransportClient(a.cfg.TransportGateway, client)
	if err != nil {
		log.Warn("rgb20 gateway self-check skipped: invalid transportGateway", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(a.ctx, gatewaySelfCheckTimeout)
	defer cancel()

	a.checkGatewayChainTitle(ctx, tc)
	a.checkGatewayToken(ctx, client, base+"/bridge/events?limit=1")
}

// checkGatewayChainTitle compares server.info's chain_title with the adapter configuration. On a
// mismatch it logs an ERROR and repeats the check at most once, so a gateway that is still
// starting up does not produce a permanent false alarm.
func (a *Adapter) checkGatewayChainTitle(ctx context.Context, tc *TransportClient) {
	info, err := tc.ServerInfo(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("rgb20 gateway self-check: server.info failed",
				"endpoint", a.cfg.TransportGateway, "err", err)
		}
		return
	}
	if chainTitleMatches(info.ChainTitle, a.cfg.ChainTitle) {
		log.Info("rgb20 gateway chain title verified", "chainTitle", a.cfg.ChainTitle)
		return
	}
	log.Error("rgb20 gateway chain title mismatch: deposit requests will be rejected",
		"gatewayChainTitle", info.ChainTitle, "configuredChainTitle", a.cfg.ChainTitle)

	select {
	case <-ctx.Done():
		return
	case <-time.After(gatewaySelfCheckRetryDelay):
	}
	info, err = tc.ServerInfo(ctx)
	if err != nil {
		return
	}
	if !chainTitleMatches(info.ChainTitle, a.cfg.ChainTitle) {
		log.Error("rgb20 gateway chain title mismatch persists: deposit requests will be rejected",
			"gatewayChainTitle", info.ChainTitle, "configuredChainTitle", a.cfg.ChainTitle)
	}
}

// checkGatewayToken probes the event ledger read path with the configured token.
func (a *Adapter) checkGatewayToken(ctx context.Context, client *http.Client, url string) {
	status, err := probeGatewayEvents(ctx, client, url, a.cfg.GatewayToken)
	if err != nil {
		// Stop cancels the adapter context; that is not a gateway failure.
		if a.ctx.Err() != nil {
			return
		}
		log.Warn("rgb20 gateway token self-check could not reach the gateway",
			"endpoint", a.cfg.TransportGateway, "err", err)
		return
	}
	severity, msg := interpretGatewayEventsStatus(status)
	switch severity {
	case selfCheckError:
		log.Error(msg, "status", status, "endpoint", a.cfg.TransportGateway)
	case selfCheckWarn:
		log.Warn(msg, "status", status, "endpoint", a.cfg.TransportGateway)
	default:
		log.Info("rgb20 gateway token verified")
	}
}

// probeGatewayEvents performs the GET probe and returns the HTTP status. The body is drained
// (bounded) so the connection can be reused; its content is not needed.
func probeGatewayEvents(ctx context.Context, client *http.Client, url, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return resp.StatusCode, nil
}
