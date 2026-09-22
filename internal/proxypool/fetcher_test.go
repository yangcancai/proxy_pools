package proxypool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseProxyList(t *testing.T) {
	t.Parallel()

	proxies, err := ParseProxyList("1.2.3.4:8000\r\nhttp://5.6.7.8:9000\ninvalid\n1.2.3.4:8000\r\n", "user", "pass")
	if err != nil {
		t.Fatalf("ParseProxyList: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("proxy count = %d, want 2", len(proxies))
	}
	if got := proxies[0].Host; got != "1.2.3.4:8000" {
		t.Fatalf("first proxy host = %q", got)
	}
	if got := proxies[1].Host; got != "5.6.7.8:9000" {
		t.Fatalf("second proxy host = %q", got)
	}
	password, ok := proxies[0].User.Password()
	if proxies[0].User.Username() != "user" || !ok || password != "pass" {
		t.Fatalf("proxy credentials were not applied")
	}
}

func TestParseProxyListRejectsAPIError(t *testing.T) {
	t.Parallel()

	if _, err := ParseProxyList(`{"code": 1001, "msg": "invalid key"}`, "", ""); err == nil {
		t.Fatal("ParseProxyList accepted an API error response")
	}
}

func TestFetcher(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", req.Method)
		}
		fmt.Fprint(w, "10.0.0.1:8100\r\n10.0.0.2:8200\r\n")
	}))
	defer api.Close()

	fetcher := NewFetcher(api.URL, "", "", time.Second)
	proxies, err := fetcher.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("proxy count = %d, want 2", len(proxies))
	}
}

func TestFetcherErrorDoesNotExposeAPIURL(t *testing.T) {
	t.Parallel()

	fetcher := &Fetcher{
		APIURL: "http://api.example.test/list?apikey=top-secret",
		Client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unavailable")
			}),
		},
	}
	_, err := fetcher.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want error")
	}
	if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "api.example.test") {
		t.Fatalf("Fetch error exposed API URL: %v", err)
	}
}

func TestFetcherRecognizesDomiRateLimit(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code": 1, "msg": "error!Too Many Requests", "success": false}`)
	}))
	defer api.Close()

	fetcher := NewFetcher(api.URL, "", "", time.Second)
	_, err := fetcher.Fetch(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Fetch error = %v, want ErrRateLimited", err)
	}
}

func TestFetcherLogsRedactedRequestAndRateLimitResponse(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		fmt.Fprint(w, `{"code": 1, "msg": "error!Too Many Requests", "success": false}`)
	}))
	defer api.Close()

	var logs bytes.Buffer
	fetcher := NewFetcher(api.URL+"/list?apikey=top-secret&pwd=hidden&getnum=2", "", "", time.Second)
	fetcher.Logger = log.New(&logs, "", 0)
	_, err := fetcher.Fetch(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Fetch error = %v, want ErrRateLimited", err)
	}
	logText := logs.String()
	for _, want := range []string{
		"proxy API request method=GET url=",
		"apikey=%2A%2A%2A",
		"pwd=%2A%2A%2A",
		"getnum=2",
		"status=200",
		"result=rate_limited",
		"retry_after=60",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("logs = %q, want substring %q", logText, want)
		}
	}
	if strings.Contains(logText, "top-secret") || strings.Contains(logText, "hidden") {
		t.Fatalf("logs exposed API credentials: %q", logText)
	}
}

func TestRedactedAPIURLHidesCredentials(t *testing.T) {
	t.Parallel()

	got := RedactedAPIURL("http://user:pass@api.example.test/list?API_KEY=secret-key&getnum=10&password=secret-password")
	for _, secret := range []string{"user:pass", "secret-key", "secret-password"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RedactedAPIURL result %q contains secret %q", got, secret)
		}
	}
	if !strings.Contains(got, "getnum=10") {
		t.Fatalf("RedactedAPIURL result %q omitted non-secret configuration", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
