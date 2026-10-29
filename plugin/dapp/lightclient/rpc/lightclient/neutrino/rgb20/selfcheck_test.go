package rgb20

import (
	"net/http"
	"testing"
)

func TestChainTitleMatches(t *testing.T) {
	tests := []struct {
		name       string
		gateway    string
		configured string
		want       bool
	}{
		{name: "identical", gateway: "user.p.rgbx.", configured: "user.p.rgbx.", want: true},
		{name: "leading/trailing whitespace differs", gateway: " user.p.rgbx. ", configured: "user.p.rgbx.", want: false},
		{name: "different chain", gateway: "user.p.other.", configured: "user.p.rgbx.", want: false},
		{name: "both empty", gateway: "", configured: "", want: true},
		{name: "gateway omits title", gateway: "", configured: "user.p.rgbx.", want: false},
		{name: "internal whitespace differs", gateway: "user.p. rgbx.", configured: "user.p.rgbx.", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chainTitleMatches(tc.gateway, tc.configured); got != tc.want {
				t.Fatalf("chainTitleMatches(%q, %q) = %t, want %t", tc.gateway, tc.configured, got, tc.want)
			}
		})
	}
}

func TestInterpretGatewayEventsStatus(t *testing.T) {
	tests := []struct {
		status       int
		wantSeverity selfCheckSeverity
		wantMessage  string
	}{
		{status: http.StatusOK, wantSeverity: selfCheckOK},
		{status: http.StatusNoContent, wantSeverity: selfCheckOK},
		{status: http.StatusUnauthorized, wantSeverity: selfCheckError, wantMessage: "rgb20 gateway token rejected"},
		{status: http.StatusForbidden, wantSeverity: selfCheckError, wantMessage: "rgb20 gateway token rejected"},
		{status: http.StatusNotFound, wantSeverity: selfCheckWarn, wantMessage: "rgb20 gateway events endpoint returned an unexpected status"},
		{status: http.StatusBadGateway, wantSeverity: selfCheckWarn, wantMessage: "rgb20 gateway events endpoint returned an unexpected status"},
		{status: http.StatusServiceUnavailable, wantSeverity: selfCheckWarn, wantMessage: "rgb20 gateway events endpoint returned an unexpected status"},
	}
	for _, tc := range tests {
		severity, message := interpretGatewayEventsStatus(tc.status)
		if severity != tc.wantSeverity {
			t.Fatalf("status %d: severity = %d, want %d", tc.status, severity, tc.wantSeverity)
		}
		if message != tc.wantMessage {
			t.Fatalf("status %d: message = %q, want %q", tc.status, message, tc.wantMessage)
		}
	}
}
