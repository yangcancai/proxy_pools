package main

import (
	"bufio"
	"bytes"
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
	subscriptionURLs    []string
	listen              string
	apiURL              string
	refreshInterval     time.Duration
	fetchTimeout        time.Duration
	dialTimeout         time.Duration
	shutdownTimeout     time.Duration
	username            string
	password            string
	clashConfig         string
	mihomoController    string
	mihomoSecret        string
	portStart           int
	listenerPrefix      string
	subscriptionRefresh time.Duration
}

func main() {
	cfg := parseConfig()
	logger := log.New(os.Stdout, "proxy-pools ", log.LstdFlags|log.Lmsgprefix)
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	refreshSignal := make(chan os.Signal, 1)
	signal.Notify(refreshSignal, syscall.SIGHUP)
	defer signal.Stop(refreshSignal)
	if cfg.clashConfig != "" {
		if err := syncMihomo(context.Background(), cfg, logger); err != nil {
			logger.Fatalf("sync Mihomo listeners: %v", err)
		}
		return
	}
	if len(cfg.subscriptionURLs) > 0 {
		if err := runManagedMihomo(rootCtx, cfg, logger, refreshSignal); err != nil {
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
	flag.DurationVar(&cfg.subscriptionRefresh, "subscription-refresh", envDuration("MIHOMO_SUBSCRIPTION_REFRESH", time.Hour), "managed Clash subscription refresh interval")
	flag.Parse()
	if flag.NArg() > 0 {
		for index := 0; index < flag.NArg(); index++ {
			cfg.subscriptionURLs = append(cfg.subscriptionURLs, flag.Arg(index))
		}
	}
	if subscriptionFile := os.Getenv("PROXY_SUBSCRIPTIONS_FILE"); subscriptionFile != "" {
		data, err := os.ReadFile(subscriptionFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read PROXY_SUBSCRIPTIONS_FILE: %v\n", err)
			os.Exit(2)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				cfg.subscriptionURLs = append(cfg.subscriptionURLs, line)
			}
		}
	}

	if len(cfg.subscriptionURLs) > 0 && cfg.clashConfig != "" {
		fmt.Fprintln(os.Stderr, "subscription URLs and -clash-config cannot be used together")
		os.Exit(2)
	}

	if len(cfg.subscriptionURLs) > 0 || cfg.clashConfig != "" {
		if cfg.portStart < 1 || cfg.portStart > 65535 {
			fmt.Fprintln(os.Stderr, "-port-start must be between 1 and 65535")
			os.Exit(2)
		}
		if cfg.fetchTimeout <= 0 || cfg.shutdownTimeout <= 0 || cfg.subscriptionRefresh <= 0 {
			fmt.Fprintln(os.Stderr, "managed Mihomo duration options must be greater than zero")
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
	if len(cfg.subscriptionURLs) == 0 {
		return fmt.Errorf("subscription URL is empty")
	}
	proxies, err := clash.FetchURL(ctx, cfg.subscriptionURLs[0], &http.Client{Timeout: cfg.fetchTimeout})
	if err != nil {
		return err
	}
	return syncMihomoProxies(ctx, cfg, logger, proxies)
}

func runManagedMihomo(ctx context.Context, cfg config, logger *log.Logger, refreshSignal <-chan os.Signal) error {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("find cache directory: %w", err)
	}
	cacheDir = filepath.Join(cacheDir, "proxy-pools")
	binary, err := mihomo.DownloadLatest(ctx, filepath.Join(cacheDir, "bin"), &http.Client{Timeout: 2 * time.Minute}, logger)
	if err != nil {
		return err
	}
	subscription, proxies, err := fetchMergedSubscription(ctx, cfg.subscriptionURLs, cfg.fetchTimeout)
	if err != nil {
		return err
	}
	dataDir := envOr("PROXY_POOLS_DATA_DIR", filepath.Join(cacheDir, "data"))
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	portMapPath := filepath.Join(dataDir, "ports.tsv")
	portByName, err := loadListenerPorts(portMapPath)
	if err != nil {
		return err
	}
	runtimeDir := filepath.Join(cacheDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return err
	}
	portByName, err = allocateListenerPorts(proxies, cfg, portByName)
	if err != nil {
		return err
	}
	cmd, err := startManagedMihomo(ctx, cfg, logger, binary, runtimeDir, dataDir, subscription, proxies, portByName)
	if err != nil {
		return err
	}
	defer stopManagedMihomo(cmd)
	logger.Printf("Mihomo is running; subscription refresh interval=%s", cfg.subscriptionRefresh)

	ticker := time.NewTicker(cfg.subscriptionRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refreshSignal:
		case <-ticker.C:
		}

		updatedSubscription, updatedProxies, fetchErr := fetchMergedSubscription(ctx, cfg.subscriptionURLs, cfg.fetchTimeout)
		if fetchErr != nil {
			logger.Printf("subscription refresh failed; retaining current listeners: %v", fetchErr)
			continue
		}
		if bytes.Equal(updatedSubscription, subscription) {
			logger.Printf("subscription unchanged; retaining current listeners")
			continue
		}
		updatedPorts, allocateErr := allocateListenerPorts(updatedProxies, cfg, portByName)
		if allocateErr != nil {
			logger.Printf("subscription refresh cannot allocate listeners; retaining current listeners: %v", allocateErr)
			continue
		}
		stopManagedMihomo(cmd)
		cmd, err = startManagedMihomo(ctx, cfg, logger, binary, runtimeDir, dataDir, updatedSubscription, updatedProxies, updatedPorts)
		if err != nil {
			return err
		}
		subscription = updatedSubscription
		proxies = updatedProxies
		portByName = updatedPorts
		logger.Printf("subscription updated: %d nodes, listeners refreshed", len(proxies))
	}
}

func fetchMergedSubscription(ctx context.Context, urls []string, timeout time.Duration) ([]byte, []clash.Proxy, error) {
	documents := make([][]byte, 0, len(urls))
	for index, subscriptionURL := range urls {
		document, err := clash.DownloadURL(ctx, subscriptionURL, &http.Client{Timeout: timeout})
		if err != nil {
			return nil, nil, fmt.Errorf("fetch subscription %d: %w", index+1, err)
		}
		documents = append(documents, document)
	}
	return clash.MergeSubscriptions(documents...)
}

func startManagedMihomo(ctx context.Context, cfg config, logger *log.Logger, binary, runtimeDir, dataDir string, subscription []byte, proxies []clash.Proxy, portByName map[string]int) (*os.Process, error) {
	if err := writeListenerPorts(filepath.Join(dataDir, "ports.tsv"), portByName); err != nil {
		return nil, err
	}
	if err := writeListenerList(dataDir, cfg, proxies, portByName); err != nil {
		return nil, err
	}
	if err := writeListenerList(runtimeDir, cfg, proxies, portByName); err != nil {
		return nil, err
	}
	controller := cfg.mihomoController
	runtimeConfig, err := clash.PrepareRuntimeConfig(subscription, strings.TrimPrefix(strings.TrimPrefix(controller, "http://"), "https://"), cfg.mihomoSecret, cfg.listenerPrefix, cfg.portStart, portByName)
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(runtimeDir, "config.yaml")
	if err := os.WriteFile(configPath, runtimeConfig, 0600); err != nil {
		return nil, err
	}
	cmd, err := mihomo.Start(binary, runtimeDir, configPath, logger)
	if err != nil {
		return nil, err
	}
	client, err := mihomo.NewClient(controller, cfg.mihomoSecret)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = client.WaitReady(readyCtx)
	cancel()
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		return nil, err
	}
	return cmd.Process, nil
}

func stopManagedMihomo(process *os.Process) {
	if process == nil {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
	_, _ = process.Wait()
}

type managedListener struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Proxy  string `json:"proxy"`
	SOCKS5 string `json:"socks5"`
}

func loadListenerPorts(path string) (map[string]int, error) {
	ports := make(map[string]int)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return ports, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 || fields[0] == "NAME" {
			continue
		}
		port, err := strconv.Atoi(fields[1])
		if err == nil && port >= 1 && port <= 65535 {
			ports[fields[0]] = port
		}
	}
	return ports, scanner.Err()
}

func allocateListenerPorts(proxies []clash.Proxy, cfg config, existing map[string]int) (map[string]int, error) {
	ports := make(map[string]int, len(proxies))
	used := make(map[int]struct{}, len(proxies))
	nextPort := cfg.portStart
	for _, proxy := range proxies {
		if port, ok := existing[proxy.Name]; ok {
			if _, duplicate := used[port]; !duplicate {
				ports[proxy.Name] = port
				used[port] = struct{}{}
				continue
			}
		}
		for {
			if nextPort > 65535 {
				return nil, fmt.Errorf("too many proxies: port range exceeds 65535")
			}
			if _, occupied := used[nextPort]; !occupied {
				ports[proxy.Name] = nextPort
				used[nextPort] = struct{}{}
				nextPort++
				break
			}
			nextPort++
		}
	}
	return ports, nil
}

func writeListenerList(runtimeDir string, cfg config, proxies []clash.Proxy, portByName map[string]int) error {
	listeners := make([]managedListener, 0, len(proxies))
	for _, proxy := range proxies {
		port := portByName[proxy.Name]
		if port > 65535 {
			return fmt.Errorf("too many proxies: port range exceeds 65535")
		}
		listeners = append(listeners, managedListener{
			Name:   mihomo.ListenerName(cfg.listenerPrefix, proxy.Name),
			Port:   port,
			Proxy:  proxy.Name,
			SOCKS5: fmt.Sprintf("socks5://127.0.0.1:%d", port),
		})
	}
	var data strings.Builder
	data.WriteString("NAME\tPORT\tPROXY\tSOCKS5\n")
	for _, listener := range listeners {
		fmt.Fprintf(&data, "%s\t%d\t%s\t%s\n", listener.Name, listener.Port, listener.Proxy, listener.SOCKS5)
	}
	return os.WriteFile(filepath.Join(runtimeDir, "listeners.tsv"), []byte(data.String()), 0600)
}

func writeListenerPorts(path string, portByName map[string]int) error {
	var data strings.Builder
	data.WriteString("NAME\tPORT\n")
	for name, port := range portByName {
		fmt.Fprintf(&data, "%s\t%d\n", name, port)
	}
	return os.WriteFile(path, []byte(data.String()), 0600)
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
