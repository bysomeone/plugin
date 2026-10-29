package main

import (
	"net/http"
	"os"
)

// Health endpoint.
//
// A container/load-balancer probe needs one unauthenticated answer about the pieces that can
// fail independently of the process staying up: the deposit API being configured, the event
// ledger being open, and the data directory still accepting writes. It deliberately exposes no
// paths, tokens or counts.

// handleHealthz reports readiness (200) or a degraded state (503).
func (g *gateway) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writable := dataDirWritable(g.store.dir)
	status, code := "ok", http.StatusOK
	if !writable {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":            status,
		"deposit_api":       g.depositEnabled(),
		"events":            g.events != nil,
		"metrics":           g.metrics.receivedAny(),
		"data_dir_writable": writable,
	})
}

// dataDirWritable creates and removes a probe file in dir. It is a cheap, side-effect-free check
// (a read-only mount or a full disk fails it), and the probe file is always removed.
func dataDirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".healthz-*")
	if err != nil {
		return false
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return false
	}
	return os.Remove(name) == nil
}
