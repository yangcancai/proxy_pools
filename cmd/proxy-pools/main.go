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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"proxy_pools/internal/clash"
	"proxy_pools/internal/mihomo"
	"proxy_pools/internal/proxypool"
	"proxy_pools/internal/proxyserver"
)

type config struct {
	subscriptionURL  string
	listen           string
	apiURL           string
	refreshInterval  time.Duration
	fetchTimeout     time.Duration
	dialTimeout      time.Duration
	shutdownTimeout  time.Duration
	username         string
	password         string
	clashConfig      string
	mihomoController string
	mihomoSecret     string
	portStart        int
	listenerPrefix   string
}

func main() {
	cfg := parseConfig()
	logger := log.New(os.Stdout, "proxy-pools ", log.LstdFlags|log.Lmsgprefix)
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.clashConfig != "" {
		if err := syncMihomo(context.Background(), cfg, logger); err != nil {
			logger.Fatalf("sync Mihomo listeners: %v", err)
		}
		return
	}
	if cfg.subscriptionURL != "" {
		if err := runManagedMihomo(rootCtx, cfg, logger); err != nil {
			logger.Fatalf("sync Mihomo listeners: %v", err)
		}
		return
	}

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
	flag.StringVar(&cfg.clashConfig, "clash-config", envOr("CLASH_CONFIG", ""), "Clash YAML to synchronize into Mihomo (management mode)")
	flag.StringVar(&cfg.mihomoController, "mihomo-controller", envOr("MIHOMO_CONTROLLER", "http://127.0.0.1:9090"), "Mihomo external controller URL")
	flag.StringVar(&cfg.mihomoSecret, "mihomo-secret", envOr("MIHOMO_SECRET", ""), "Mihomo external controller secret")
	flag.IntVar(&cfg.portStart, "port-start", envInt("MIHOMO_PORT_START", 19000), "first local port used in management mode")
	flag.StringVar(&cfg.listenerPrefix, "listener-prefix", envOr("MIHOMO_LISTENER_PREFIX", "proxy-pools"), "listener name prefix in management mode")
	flag.Parse()
	if flag.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: proxy-pools [subscription-url]")
		os.Exit(2)
	}
	if flag.NArg() == 1 {
		cfg.subscriptionURL = flag.Arg(0)
	}

	if cfg.subscriptionURL != "" || cfg.clashConfig != "" {
		if cfg.portStart < 1 || cfg.portStart > 65535 {
			fmt.Fprintln(os.Stderr, "-port-start must be between 1 and 65535")
			os.Exit(2)
		}
		return cfg
	}
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

func syncMihomo(ctx context.Context, cfg config, logger *log.Logger) error {
	proxies, err := clash.ParseFile(cfg.clashConfig)
	if err != nil {
		return err
	}
	return syncMihomoProxies(ctx, cfg, logger, proxies)
}

func syncMihomoProxies(ctx context.Context, cfg config, logger *log.Logger, proxies []clash.Proxy) error {
	client, err := mihomo.NewClient(cfg.mihomoController, cfg.mihomoSecret)
	if err != nil {
		return err
	}
	for index, proxy := range proxies {
		port := cfg.portStart + index
		if port > 65535 {
			return fmt.Errorf("too many proxies: port range exceeds 65535")
		}
		listener := mihomo.Listener{
			Name:   mihomo.ListenerName(cfg.listenerPrefix, proxy.Name),
			Type:   "mixed",
			Listen: "127.0.0.1",
			Port:   port,
			Proxy:  proxy.Name,
		}
		if err := client.PutListener(ctx, listener); err != nil {
			return err
		}
		logger.Printf("Mihomo listener ready name=%s port=%d proxy=%s", listener.Name, listener.Port, listener.Proxy)
	}
	return nil
}

func syncMihomoURL(ctx context.Context, cfg config, logger *log.Logger) error {
	proxies, err := clash.FetchURL(ctx, cfg.subscriptionURL, &http.Client{Timeout: cfg.fetchTimeout})
	if err != nil {
		return err
	}
	return syncMihomoProxies(ctx, cfg, logger, proxies)
}

func runManagedMihomo(ctx context.Context, cfg config, logger *log.Logger) error {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("find cache directory: %w", err)
	}
	cacheDir = filepath.Join(cacheDir, "proxy-pools")
	binary, err := mihomo.DownloadLatest(ctx, filepath.Join(cacheDir, "bin"), &http.Client{Timeout: 2 * time.Minute}, logger)
	if err != nil {
		return err
	}
	subscription, err := clash.DownloadURL(ctx, cfg.subscriptionURL, &http.Client{Timeout: cfg.fetchTimeout})
	if err != nil {
		return err
	}
	if _, err := clash.Parse(subscription); err != nil {
		return err
	}
	runtimeDir := filepath.Join(cacheDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return err
	}
	controller := cfg.mihomoController
	runtimeConfig, err := clash.PrepareRuntimeConfig(subscription, strings.TrimPrefix(strings.TrimPrefix(controller, "http://"), "https://"), cfg.mihomoSecret, cfg.listenerPrefix, cfg.portStart)
	if err != nil {
		return err
	}
	configPath := filepath.Join(runtimeDir, "config.yaml")
	if err := os.WriteFile(configPath, runtimeConfig, 0600); err != nil {
		return err
	}
	cmd, err := mihomo.Start(binary, runtimeDir, configPath, logger)
	if err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() }()
	client, err := mihomo.NewClient(controller, cfg.mihomoSecret)
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = client.WaitReady(readyCtx)
	cancel()
	if err != nil {
		return err
	}
	logger.Printf("Mihomo is running; press Ctrl-C to stop")
	<-ctx.Done()
	return nil
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

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be an integer: %v\n", key, err)
		os.Exit(2)
	}
	return parsed
}
