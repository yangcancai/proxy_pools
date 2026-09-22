package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"proxy_pools/internal/proxypool"
	"proxy_pools/internal/proxyserver"
)

type config struct {
	listen          string
	apiURL          string
	refreshInterval time.Duration
	fetchTimeout    time.Duration
	dialTimeout     time.Duration
	shutdownTimeout time.Duration
	username        string
	password        string
}

func main() {
	cfg := parseConfig()
	logger := log.New(os.Stdout, "proxy-pools ", log.LstdFlags|log.Lmsgprefix)
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fetcher := proxypool.NewFetcher(cfg.apiURL, cfg.username, cfg.password, cfg.fetchTimeout)
	fetcher.Logger = logger
	pool := proxypool.NewEmpty()
	refresher := newOnDemandRefresher(rootCtx, fetcher, pool, cfg.refreshInterval, logger)

	handler := proxyserver.New(pool, cfg.dialTimeout, logger)
	handler.SetRequestPreparer(refresher.Ensure)
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Printf("HTTP proxy listening on %s; upstream proxies will be loaded on first request", cfg.listen)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-rootCtx.Done():
		logger.Printf("shutdown requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP server failed: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("graceful shutdown failed: %v", err)
		_ = server.Close()
	}
	handler.CloseIdleConnections()
}

type proxyFetcher interface {
	Fetch(context.Context) ([]*url.URL, error)
}

// onDemandRefresher fetches proxies only while handling an incoming proxy
// request. Concurrent requests share one fetch, and no background timer calls
// the API while the service is idle.
type onDemandRefresher struct {
	ctx      context.Context
	fetcher  proxyFetcher
	pool     *proxypool.Pool
	logger   *log.Logger
	schedule *refreshSchedule
	now      func() time.Time

	mu          sync.Mutex
	inFlight    chan struct{}
	nextRefresh time.Time
	lastErr     error
}

func newOnDemandRefresher(ctx context.Context, fetcher proxyFetcher, pool *proxypool.Pool, interval time.Duration, logger *log.Logger) *onDemandRefresher {
	if logger == nil {
		logger = log.Default()
	}
	return &onDemandRefresher{
		ctx:      ctx,
		fetcher:  fetcher,
		pool:     pool,
		logger:   logger,
		schedule: newRefreshSchedule(interval),
		now:      time.Now,
	}
}

func (r *onDemandRefresher) Ensure(requestCtx context.Context) error {
	for {
		r.mu.Lock()
		if r.inFlight != nil {
			done := r.inFlight
			r.mu.Unlock()
			select {
			case <-requestCtx.Done():
				return requestCtx.Err()
			case <-done:
			}
			continue
		}

		if r.now().Before(r.nextRefresh) {
			poolLen := r.pool.Len()
			lastErr := r.lastErr
			r.mu.Unlock()
			if poolLen > 0 {
				return nil
			}
			if lastErr != nil {
				return lastErr
			}
			return proxypool.ErrEmptyPool
		}

		done := make(chan struct{})
		r.inFlight = done
		r.mu.Unlock()

		proxies, fetchErr := r.fetcher.Fetch(r.ctx)
		refreshErr := fetchErr
		if refreshErr == nil {
			refreshErr = r.pool.Replace(proxies)
		}

		r.mu.Lock()
		if errors.Is(refreshErr, proxypool.ErrRateLimited) {
			r.schedule.rateLimited()
		} else if refreshErr == nil {
			r.schedule.succeeded()
		}
		delay := r.schedule.delay()
		r.nextRefresh = r.now().Add(delay)
		r.lastErr = refreshErr
		poolLen := r.pool.Len()
		r.inFlight = nil
		close(done)
		r.mu.Unlock()

		if refreshErr == nil {
			r.logger.Printf("refreshed %d upstream proxies on demand; eligible to refresh again in %s", poolLen, delay)
			return nil
		}
		if errors.Is(refreshErr, proxypool.ErrRateLimited) {
			r.logger.Printf("proxy pool refresh rate limited; retaining %d existing proxies; next request may retry in %s: %v", poolLen, delay, refreshErr)
		} else {
			r.logger.Printf("proxy pool refresh failed; retaining %d existing proxies; next request may retry in %s: %v", poolLen, delay, refreshErr)
		}
		if poolLen > 0 {
			return nil
		}
		return refreshErr
	}
}

type refreshSchedule struct {
	base               time.Duration
	current            time.Duration
	successesAtBackoff int
}

func newRefreshSchedule(configured time.Duration) *refreshSchedule {
	const minimumInterval = 20 * time.Second
	if configured < minimumInterval {
		configured = minimumInterval
	}
	return &refreshSchedule{base: configured, current: configured}
}

func (s *refreshSchedule) delay() time.Duration {
	return s.current
}

func (s *refreshSchedule) rateLimited() {
	const (
		minimumBackoff = 40 * time.Second
		defaultMaximum = 2 * time.Minute
	)
	maximumBackoff := defaultMaximum
	if maximumBackoff < s.base {
		maximumBackoff = s.base
	}
	next := s.current * 2
	if next < minimumBackoff {
		next = minimumBackoff
	}
	if next > maximumBackoff {
		next = maximumBackoff
	}
	s.current = next
	s.successesAtBackoff = 0
}

func (s *refreshSchedule) succeeded() {
	if s.current <= s.base {
		return
	}
	s.successesAtBackoff++
	if s.successesAtBackoff < 3 {
		return
	}
	s.current /= 2
	if s.current < s.base {
		s.current = s.base
	}
	s.successesAtBackoff = 0
}

func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.listen, "listen", envOr("PROXY_LISTEN", "127.0.0.1:18080"), "HTTP proxy listen address")
	flag.StringVar(&cfg.apiURL, "api", "", "Domi proxy API URL (or PROXY_API_URL)")
	flag.DurationVar(&cfg.refreshInterval, "refresh", envDuration("PROXY_REFRESH_INTERVAL", 20*time.Second), "proxy pool refresh interval")
	flag.DurationVar(&cfg.fetchTimeout, "fetch-timeout", envDuration("PROXY_FETCH_TIMEOUT", 10*time.Second), "proxy API request timeout")
	flag.DurationVar(&cfg.dialTimeout, "dial-timeout", envDuration("PROXY_DIAL_TIMEOUT", 5*time.Second), "upstream connect and response-header timeout")
	flag.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", envDuration("PROXY_SHUTDOWN_TIMEOUT", 10*time.Second), "graceful shutdown timeout")
	flag.StringVar(&cfg.username, "upstream-username", "", "optional upstream proxy username (or PROXY_UPSTREAM_USERNAME)")
	flag.StringVar(&cfg.password, "upstream-password", "", "optional upstream proxy password (or PROXY_UPSTREAM_PASSWORD)")
	flag.Parse()

	if cfg.apiURL == "" {
		cfg.apiURL = os.Getenv("PROXY_API_URL")
	}
	if cfg.username == "" {
		cfg.username = os.Getenv("PROXY_UPSTREAM_USERNAME")
	}
	if cfg.password == "" {
		cfg.password = os.Getenv("PROXY_UPSTREAM_PASSWORD")
	}
	if cfg.apiURL == "" {
		fmt.Fprintln(os.Stderr, "PROXY_API_URL or -api is required")
		flag.Usage()
		os.Exit(2)
	}
	if cfg.refreshInterval <= 0 || cfg.fetchTimeout <= 0 || cfg.dialTimeout <= 0 || cfg.shutdownTimeout <= 0 {
		fmt.Fprintln(os.Stderr, "all duration options must be greater than zero")
		os.Exit(2)
	}
	return cfg
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be a Go duration such as 30s or 5m: %v\n", key, err)
		os.Exit(2)
	}
	return duration
}
