package proxyserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"proxy_pools/internal/proxypool"
)

func TestHTTPRequestsRotateUpstreamProxies(t *testing.T) {
	t.Parallel()

	first := newRecordingHTTPProxy(t, "first")
	defer first.Close()
	second := newRecordingHTTPProxy(t, "second")
	defer second.Close()

	proxyServer, handler := newTestProxyServer(t, first.URL, second.URL)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	clientTransport := &http.Transport{Proxy: http.ProxyURL(parseURL(t, proxyServer.URL))}
	defer clientTransport.CloseIdleConnections()
	client := &http.Client{Transport: clientTransport, Timeout: 3 * time.Second}

	for index, want := range []string{"first", "second", "first"} {
		resp, err := client.Get(fmt.Sprintf("http://origin.invalid/request-%d", index))
		if err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read response %d: %v", index, readErr)
		}
		if got := string(body); got != want {
			t.Fatalf("response %d = %q, want %q", index, got, want)
		}
	}
}

func TestHTTPForwardLogIncludesURLAndProxy(t *testing.T) {
	t.Parallel()

	upstream := newRecordingHTTPProxy(t, "working")
	defer upstream.Close()
	upstreamURL := parseURL(t, upstream.URL)
	pool, err := proxypool.New([]*url.URL{upstreamURL})
	if err != nil {
		t.Fatalf("proxypool.New: %v", err)
	}
	var logs bytes.Buffer
	handler := New(pool, time.Second, log.New(&logs, "", 0))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	transport := &http.Transport{Proxy: http.ProxyURL(parseURL(t, proxyServer.URL))}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("http://origin.invalid/log?q=value")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	resp.Body.Close()

	want := "forward method=GET url=http://origin.invalid/log?q=value proxy=" + upstreamURL.Host + " attempt=1/1"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("forward log = %q, want it to contain %q", logs.String(), want)
	}
}

func TestRequestPreparerPopulatesEmptyPool(t *testing.T) {
	t.Parallel()

	upstream := newRecordingHTTPProxy(t, "loaded on demand")
	defer upstream.Close()
	upstreamURL := parseURL(t, upstream.URL)
	pool := proxypool.NewEmpty()
	handler := New(pool, time.Second, log.New(io.Discard, "", 0))
	prepared := make(chan struct{}, 1)
	handler.SetRequestPreparer(func(context.Context) error {
		prepared <- struct{}{}
		return pool.Replace([]*url.URL{upstreamURL})
	})
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	transport := &http.Transport{Proxy: http.ProxyURL(parseURL(t, proxyServer.URL))}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("http://origin.invalid/on-demand")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := string(body); got != "loaded on demand" {
		t.Fatalf("response = %q, want loaded on demand", got)
	}
	if got := len(prepared); got != 1 {
		t.Fatalf("prepare calls = %d, want 1", got)
	}
}

func TestHTTPGetRetriesNextUpstream(t *testing.T) {
	t.Parallel()

	working := newRecordingHTTPProxy(t, "working")
	defer working.Close()
	proxyServer, handler := newTestProxyServer(t, unavailableProxyURL(t), working.URL)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	transport := &http.Transport{Proxy: http.ProxyURL(parseURL(t, proxyServer.URL))}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("http://origin.invalid/retry")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := string(body); got != "working" {
		t.Fatalf("response = %q, want working", got)
	}
}

func TestHTTPGetRetriesDomiProxyError(t *testing.T) {
	t.Parallel()

	failing := newDomiErrorProxy(t, http.StatusRequestURITooLong)
	defer failing.Close()
	working := newRecordingHTTPProxy(t, "working")
	defer working.Close()
	proxyServer, handler := newTestProxyServer(t, failing.URL, working.URL)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	transport := &http.Transport{Proxy: http.ProxyURL(parseURL(t, proxyServer.URL))}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("http://origin.invalid/domi-error")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := string(body); got != "working" {
		t.Fatalf("response = %q, want working", got)
	}
}

func TestHTTPSConnectRotatesUpstreamProxies(t *testing.T) {
	t.Parallel()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "secure")
	}))
	defer origin.Close()

	first := newConnectProxy(t)
	defer first.Close()
	second := newConnectProxy(t)
	defer second.Close()

	proxyServer, handler := newTestProxyServer(t, first.URL, second.URL)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	for index := 0; index < 2; index++ {
		transport := &http.Transport{
			Proxy:           http.ProxyURL(parseURL(t, proxyServer.URL)),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Test server certificate.
		}
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("HTTPS request %d: %v", index, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		transport.CloseIdleConnections()
		if readErr != nil {
			t.Fatalf("read HTTPS response %d: %v", index, readErr)
		}
		if got := string(body); got != "secure" {
			t.Fatalf("HTTPS response %d = %q, want secure", index, got)
		}
	}

	if got := first.connects(); got != 1 {
		t.Fatalf("first upstream CONNECT count = %d, want 1", got)
	}
	if got := second.connects(); got != 1 {
		t.Fatalf("second upstream CONNECT count = %d, want 1", got)
	}
}

func TestHTTPSConnectRetriesNextUpstream(t *testing.T) {
	t.Parallel()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "secure retry")
	}))
	defer origin.Close()
	working := newConnectProxy(t)
	defer working.Close()
	proxyServer, handler := newTestProxyServer(t, unavailableProxyURL(t), working.URL)
	defer proxyServer.Close()
	defer handler.CloseIdleConnections()

	transport := &http.Transport{
		Proxy:           http.ProxyURL(parseURL(t, proxyServer.URL)),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Test server certificate.
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("HTTPS request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read HTTPS response: %v", err)
	}
	if got := string(body); got != "secure retry" {
		t.Fatalf("HTTPS response = %q, want secure retry", got)
	}
	if got := working.connects(); got != 1 {
		t.Fatalf("working upstream CONNECT count = %d, want 1", got)
	}
}

func newTestProxyServer(t *testing.T, upstreams ...string) (*httptest.Server, *Handler) {
	t.Helper()
	proxyURLs := make([]*url.URL, 0, len(upstreams))
	for _, upstream := range upstreams {
		proxyURLs = append(proxyURLs, parseURL(t, upstream))
	}
	pool, err := proxypool.New(proxyURLs)
	if err != nil {
		t.Fatalf("proxypool.New: %v", err)
	}
	handler := New(pool, time.Second, log.New(io.Discard, "", 0))
	return httptest.NewServer(handler), handler
}

func newRecordingHTTPProxy(t *testing.T, name string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Scheme != "http" || req.URL.Host != "origin.invalid" {
			t.Errorf("upstream %s received URL %q", name, req.URL.String())
		}
		fmt.Fprint(w, name)
	}))
}

func newDomiErrorProxy(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijacking unsupported")
			return
		}
		conn, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buffer, "HTTP/1.1 %d PROXY %d RET\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", statusCode, statusCode)
		if err := buffer.Flush(); err != nil {
			t.Errorf("flush proxy error response: %v", err)
		}
	}))
}

type connectProxy struct {
	*httptest.Server
	mu           sync.Mutex
	connectCount int
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	proxy := &connectProxy{}
	proxy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		proxy.mu.Lock()
		proxy.connectCount++
		proxy.mu.Unlock()

		upstream, err := net.DialTimeout("tcp", req.Host, time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		client, buffer, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			client.Close()
			upstream.Close()
			return
		}
		if err := buffer.Flush(); err != nil {
			client.Close()
			upstream.Close()
			return
		}
		go testTunnel(client, buffer.Reader, upstream, bufio.NewReader(upstream))
	}))
	return proxy
}

func (p *connectProxy) connects() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connectCount
}

func testTunnel(client net.Conn, clientReader io.Reader, upstream net.Conn, upstreamReader io.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstreamReader)
		done <- struct{}{}
	}()
	<-done
	client.Close()
	upstream.Close()
	<-done
}

func parseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", value, err)
	}
	return parsed
}

func unavailableProxyURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve unavailable address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved address: %v", err)
	}
	return "http://" + address
}

func TestConnectTarget(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"example.com:8443": "example.com:8443",
		"example.com":      "example.com:443",
		"[::1]":            "[::1]:443",
	} {
		got, err := connectTarget(input)
		if err != nil {
			t.Fatalf("connectTarget(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("connectTarget(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := connectTarget(strings.Repeat(" ", 2)); err == nil {
		t.Fatal("connectTarget accepted an empty target")
	}
}
