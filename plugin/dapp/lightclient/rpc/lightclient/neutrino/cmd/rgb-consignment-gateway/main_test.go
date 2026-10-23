package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, func(string) string { return "" })
	require.NoError(t, err)
	assert.Equal(t, defaultListen, cfg.listen)
	assert.Equal(t, defaultDataDir, cfg.dataDir)
	assert.Equal(t, defaultPath, cfg.path)
	assert.EqualValues(t, defaultMaxSize, cfg.maxSize)
	assert.Equal(t, defaultTTL, cfg.ttl)
	assert.Equal(t, defaultRateLimit, cfg.rateLimit)
	assert.False(t, cfg.trustProxy)
}

func TestParseConfigEnvironment(t *testing.T) {
	env := map[string]string{
		"RGB_GATEWAY_LISTEN":      ":9000",
		"RGB_GATEWAY_DATA_DIR":    "/tmp/rgb",
		"RGB_GATEWAY_PATH":        "/rpc",
		"RGB_GATEWAY_MAX_SIZE":    "2048",
		"RGB_GATEWAY_TTL":         "30m",
		"RGB_GATEWAY_RATE_LIMIT":  "5",
		"RGB_GATEWAY_TRUST_PROXY": "true",
	}
	cfg, err := parseConfig(nil, func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, ":9000", cfg.listen)
	assert.Equal(t, "/tmp/rgb", cfg.dataDir)
	assert.Equal(t, "/rpc", cfg.path)
	assert.EqualValues(t, 2048, cfg.maxSize)
	assert.Equal(t, 30*time.Minute, cfg.ttl)
	assert.Equal(t, 5, cfg.rateLimit)
	assert.True(t, cfg.trustProxy)
}

func TestParseConfigFlagsOverrideEnvironment(t *testing.T) {
	env := map[string]string{"RGB_GATEWAY_MAX_SIZE": "1024", "RGB_GATEWAY_RATE_LIMIT": "9"}
	cfg, err := parseConfig([]string{"--max-size", "4096", "--rate-limit", "0"}, func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.EqualValues(t, 4096, cfg.maxSize)
	assert.Equal(t, 0, cfg.rateLimit)
}

func TestParseConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"bad size env", map[string]string{"RGB_GATEWAY_MAX_SIZE": "lots"}, nil},
		{"bad ttl env", map[string]string{"RGB_GATEWAY_TTL": "soon"}, nil},
		{"bad rate env", map[string]string{"RGB_GATEWAY_RATE_LIMIT": "many"}, nil},
		{"bad bool env", map[string]string{"RGB_GATEWAY_TRUST_PROXY": "maybe"}, nil},
		{"relative path", nil, []string{"--path", "rpc"}},
		{"zero size", nil, []string{"--max-size", "0"}},
		{"zero ttl", nil, []string{"--ttl", "0s"}},
		{"negative rate", nil, []string{"--rate-limit", "-1"}},
		{"empty listen", nil, []string{"--listen", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig(tc.args, func(k string) string { return tc.env[k] })
			require.Error(t, err)
		})
	}
}
