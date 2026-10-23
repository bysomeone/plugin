package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultListen    = "127.0.0.1:8080"
	defaultDataDir   = "rgb-consignment-data"
	defaultPath      = "/0.2/json-rpc"
	defaultMaxSize   = 16 << 20
	defaultTTL       = 7 * 24 * time.Hour
	defaultRateLimit = 60

	// maxConfiguredSize keeps the multipart body limit free of overflow.
	maxConfiguredSize = 1 << 40
)

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "rgb-consignment-gateway: %v\n", err)
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "rgb-consignment-gateway: ", log.LstdFlags)
	if err := run(cfg, logger); err != nil {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	cfg := config{
		listen:     envString(getenv, "RGB_GATEWAY_LISTEN", defaultListen),
		dataDir:    envString(getenv, "RGB_GATEWAY_DATA_DIR", defaultDataDir),
		path:       envString(getenv, "RGB_GATEWAY_PATH", defaultPath),
		maxSize:    defaultMaxSize,
		ttl:        defaultTTL,
		rateLimit:  defaultRateLimit,
		trustProxy: false,
	}
	var err error
	if cfg.maxSize, err = envInt64(getenv, "RGB_GATEWAY_MAX_SIZE", cfg.maxSize); err != nil {
		return config{}, err
	}
	if cfg.ttl, err = envDuration(getenv, "RGB_GATEWAY_TTL", cfg.ttl); err != nil {
		return config{}, err
	}
	if cfg.rateLimit, err = envInt(getenv, "RGB_GATEWAY_RATE_LIMIT", cfg.rateLimit); err != nil {
		return config{}, err
	}
	if cfg.trustProxy, err = envBool(getenv, "RGB_GATEWAY_TRUST_PROXY", cfg.trustProxy); err != nil {
		return config{}, err
	}

	fs := flag.NewFlagSet("rgb-consignment-gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.listen, "listen", cfg.listen, "TCP address to listen on")
	fs.StringVar(&cfg.dataDir, "data-dir", cfg.dataDir, "directory for consignments and index files")
	fs.StringVar(&cfg.path, "path", cfg.path, "HTTP path of the JSON-RPC endpoint")
	fs.Int64Var(&cfg.maxSize, "max-size", cfg.maxSize, "maximum consignment size in bytes")
	fs.DurationVar(&cfg.ttl, "ttl", cfg.ttl, "how long consignments are retained")
	fs.IntVar(&cfg.rateLimit, "rate-limit", cfg.rateLimit, "requests per client IP per minute (0 disables)")
	fs.BoolVar(&cfg.trustProxy, "trust-proxy", cfg.trustProxy, "use X-Forwarded-For/X-Real-IP from a trusted reverse proxy")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) validate() error {
	if strings.TrimSpace(c.listen) == "" {
		return errors.New("listen address must not be empty")
	}
	if strings.TrimSpace(c.dataDir) == "" {
		return errors.New("data directory must not be empty")
	}
	if !strings.HasPrefix(c.path, "/") {
		return fmt.Errorf("path %q must start with /", c.path)
	}
	if strings.ContainsAny(c.path, " \t\r\n{}") {
		return fmt.Errorf("path %q contains unsupported characters", c.path)
	}
	if c.maxSize <= 0 || c.maxSize > maxConfiguredSize {
		return fmt.Errorf("max-size must be between 1 and %d bytes", int64(maxConfiguredSize))
	}
	if c.ttl <= 0 {
		return errors.New("ttl must be positive")
	}
	if c.rateLimit < 0 {
		return errors.New("rate-limit must not be negative")
	}
	return nil
}

func run(cfg config, logger *log.Logger) error {
	g, err := newGateway(cfg, logger)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, err)
	}

	srv := &http.Server{
		Handler:           g.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go g.cleanupLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	logger.Printf("starting rgb-consignment-gateway %s (RGB HTTP JSON-RPC %s)", softwareVersion, protocolVersion)
	logger.Printf("config listen=%s path=%s data-dir=%s max-size=%d ttl=%s rate-limit=%d/min trust-proxy=%t",
		ln.Addr().String(), cfg.path, g.store.dir, cfg.maxSize, cfg.ttl, cfg.rateLimit, cfg.trustProxy)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger.Printf("shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// cleanupLoop removes expired consignments and stale rate-limit windows.
func (g *gateway) cleanupLoop(ctx context.Context) {
	interval := cleanupInterval(g.cfg.ttl)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := g.store.cleanup(); err != nil {
				g.logger.Printf("cleanup: %v", err)
			} else if n > 0 {
				g.logger.Printf("cleanup removed %d expired consignments", n)
			}
			g.limiter.sweep()
		}
	}
}

func cleanupInterval(ttl time.Duration) time.Duration {
	d := ttl / 4
	if d < time.Second {
		d = time.Second
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

func envString(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt64(getenv func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envBool(getenv func(string) string, key string, def bool) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
