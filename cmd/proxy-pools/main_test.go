package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"proxy_pools/internal/clash"
	"proxy_pools/internal/proxypool"
)

func TestWriteListenerList(t *testing.T) {
	directory := t.TempDir()
	cfg := config{portStart: 19000, listenerPrefix: "proxy-pools"}
	if err := writeListenerList(directory, cfg, []clash.Proxy{{Name: "node-one"}}, map[string]int{"node-one": 19000}, map[string]clash.Auth{"node-one": {Username: "user", Password: "pass"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(directory + "/listeners.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proxy-pools-node-one", "19000", "user", "pass", "socks5://user:pass@127.0.0.1:19000"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("listener list %q does not contain %q", data, want)
		}
	}
}

func TestOnDemandRefresherOnlyFetchesWhenRequestedAndDue(t *testing.T) {
	proxyURL := mustProxyURL(t, "http://127.0.0.1:15001")
	fetcher := &countingFetcher{results: []fetchResult{
		{proxies: []*url.URL{proxyURL}},
		{proxies: []*url.URL{proxyURL}},
	}}
	pool := proxypool.NewEmpty()
	refresher := newOnDemandRefresher(context.Background(), fetcher, pool, time.Second, discardLogger())
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	refresher.now = func() time.Time { return now }

	if got := fetcher.callCount(); got != 0 {
		t.Fatalf("fetches before a proxy request = %d, want 0", got)
	}
	if err := refresher.Ensure(context.Background()); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("fetches after first request = %d, want 1", got)
	}

	now = now.Add(19 * time.Second)
	if err := refresher.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure before refresh interval: %v", err)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("fetches before minimum interval = %d, want 1", got)
	}

	now = now.Add(time.Second)
	if err := refresher.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure when refresh is due: %v", err)
	}
	if got := fetcher.callCount(); got != 2 {
		t.Fatalf("fetches when refresh is due = %d, want 2", got)
	}
}

func TestOnDemandRefresherRateLimitWaitsForLaterRequest(t *testing.T) {
	rateLimitErr := errors.Join(proxypool.ErrRateLimited, errors.New("Too Many Requests"))
	proxyURL := mustProxyURL(t, "http://127.0.0.1:15001")
	fetcher := &countingFetcher{results: []fetchResult{
		{err: rateLimitErr},
		{proxies: []*url.URL{proxyURL}},
	}}
	refresher := newOnDemandRefresher(context.Background(), fetcher, proxypool.NewEmpty(), 20*time.Second, discardLogger())
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	refresher.now = func() time.Time { return now }

	if err := refresher.Ensure(context.Background()); !errors.Is(err, proxypool.ErrRateLimited) {
		t.Fatalf("first Ensure error = %v, want rate limited", err)
	}
	now = now.Add(39 * time.Second)
	if err := refresher.Ensure(context.Background()); !errors.Is(err, proxypool.ErrRateLimited) {
		t.Fatalf("Ensure during backoff error = %v, want retained rate-limit error", err)
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("fetches during backoff = %d, want 1", got)
	}

	now = now.Add(time.Second)
	if err := refresher.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after backoff: %v", err)
	}
	if got := fetcher.callCount(); got != 2 {
		t.Fatalf("fetches after backoff = %d, want 2", got)
	}
}

func TestOnDemandRefresherSharesConcurrentFetch(t *testing.T) {
	proxyURL := mustProxyURL(t, "http://127.0.0.1:15001")
	fetcher := &blockingFetcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
		proxies: []*url.URL{proxyURL},
	}
	refresher := newOnDemandRefresher(context.Background(), fetcher, proxypool.NewEmpty(), 20*time.Second, discardLogger())

	const requestCount = 20
	errorsFound := make(chan error, requestCount)
	var requests sync.WaitGroup
	requests.Add(requestCount)
	for index := 0; index < requestCount; index++ {
		go func() {
			defer requests.Done()
			errorsFound <- refresher.Ensure(context.Background())
		}()
	}
	<-fetcher.started
	close(fetcher.release)
	requests.Wait()
	close(errorsFound)

	for err := range errorsFound {
		if err != nil {
			t.Errorf("concurrent Ensure: %v", err)
		}
	}
	if got := fetcher.callCount(); got != 1 {
		t.Fatalf("concurrent fetch count = %d, want 1", got)
	}
}

func TestRefreshScheduleBackoffAndRecovery(t *testing.T) {
	schedule := newRefreshSchedule(time.Second)
	if got := schedule.delay(); got != 20*time.Second {
		t.Fatalf("minimum delay = %s, want 20s", got)
	}

	for _, want := range []time.Duration{40 * time.Second, 80 * time.Second, 2 * time.Minute, 2 * time.Minute} {
		schedule.rateLimited()
		if got := schedule.delay(); got != want {
			t.Fatalf("rate-limited delay = %s, want %s", got, want)
		}
	}

	for _, want := range []time.Duration{time.Minute, 30 * time.Second, 20 * time.Second} {
		for count := 0; count < 3; count++ {
			schedule.succeeded()
		}
		if got := schedule.delay(); got != want {
			t.Fatalf("recovered delay = %s, want %s", got, want)
		}
	}
}

func TestRefreshScheduleNeverDropsBelowConfiguredBase(t *testing.T) {
	schedule := newRefreshSchedule(3 * time.Minute)
	schedule.rateLimited()
	if got := schedule.delay(); got < 3*time.Minute {
		t.Fatalf("rate-limited delay = %s, below configured base", got)
	}
}

type fetchResult struct {
	proxies []*url.URL
	err     error
}

type countingFetcher struct {
	mu      sync.Mutex
	calls   int
	results []fetchResult
}

func (f *countingFetcher) Fetch(context.Context) ([]*url.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	index := f.calls - 1
	if index >= len(f.results) {
		index = len(f.results) - 1
	}
	return f.results[index].proxies, f.results[index].err
}

func (f *countingFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type blockingFetcher struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	proxies []*url.URL
}

func (f *blockingFetcher) Fetch(context.Context) ([]*url.URL, error) {
	f.mu.Lock()
	f.calls++
	if f.calls == 1 {
		close(f.started)
	}
	f.mu.Unlock()
	<-f.release
	return f.proxies, nil
}

func (f *blockingFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func mustProxyURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", value, err)
	}
	return parsed
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
