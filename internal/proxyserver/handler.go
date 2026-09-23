package proxyserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"proxy_pools/internal/proxypool"
)

type upstreamContextKey struct{}

type Handler struct {
	pool           *proxypool.Pool
	transport      *http.Transport
	dialer         net.Dialer
	logger         *log.Logger
	prepareRequest func(context.Context) error
	allowedIPs     []*net.IPNet
}

// SetRequestPreparer registers a callback that runs before an upstream proxy
// is selected. Configure it before the HTTP server starts serving requests.
func (h *Handler) SetRequestPreparer(prepare func(context.Context) error) {
	h.prepareRequest = prepare
}

func (h *Handler) SetAllowedIPs(value string) error {
	allowed, err := parseAllowedIPs(value)
	if err != nil {
		return err
	}
	h.allowedIPs = allowed
	return nil
}

func parseAllowedIPs(value string) ([]*net.IPNet, error) {
	var allowed []*net.IPNet
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("invalid allowlist IP %q", item)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			item += "/" + strconv.Itoa(bits)
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist network %q: %w", item, err)
		}
		allowed = append(allowed, network)
	}
	return allowed, nil
}

func New(pool *proxypool.Pool, dialTimeout time.Duration, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Default()
	}

	h := &Handler{
		pool: pool,
		dialer: net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		},
		logger: logger,
	}
	h.transport = &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, ok := req.Context().Value(upstreamContextKey{}).(*url.URL)
			if !ok || proxyURL == nil {
				return nil, proxypool.ErrEmptyPool
			}
			return proxyURL, nil
		},
		DialContext:           h.dialer.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: dialTimeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
	}
	return h
}

func (h *Handler) CloseIdleConnections() {
	h.transport.CloseIdleConnections()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !h.isAllowed(req) {
		http.Error(w, "client IP is not allowed", http.StatusForbidden)
		return
	}
	if h.prepareRequest != nil {
		if err := h.prepareRequest(req.Context()); err != nil {
			h.proxyError(w, req, "prepare upstream proxy pool", err)
			return
		}
	}
	if req.Method == http.MethodConnect {
		h.handleConnect(w, req)
		return
	}
	h.handleHTTP(w, req)
}

func (h *Handler) isAllowed(req *http.Request) bool {
	if len(h.allowedIPs) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range h.allowedIPs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *Handler) handleHTTP(w http.ResponseWriter, req *http.Request) {
	attempts := h.httpAttempts(req)
	var (
		proxyURL *url.URL
		resp     *http.Response
		err      error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		proxyURL, err = h.nextProxy()
		if err != nil {
			h.proxyError(w, req, "select upstream proxy", err)
			return
		}

		outReq := req.Clone(context.WithValue(req.Context(), upstreamContextKey{}, proxyURL))
		outReq.RequestURI = ""
		if outReq.URL.Scheme == "" {
			outReq.URL.Scheme = "http"
		}
		if outReq.URL.Host == "" {
			outReq.URL.Host = req.Host
		}
		removeHopByHopHeaders(outReq.Header)
		outReq.Header.Del("Proxy-Authorization")
		h.logForward(req.Method, outReq.URL.String(), proxyURL, attempt+1, attempts)

		resp, err = h.transport.RoundTrip(outReq)
		if err == nil && !isUpstreamProxyFailure(resp) {
			break
		}
		if err == nil {
			err = fmt.Errorf("upstream returned %s", resp.Status)
			resp.Body.Close()
		}
		if req.Context().Err() != nil {
			break
		}
	}
	if err != nil {
		h.proxyError(w, req, "request through "+proxyURL.Host, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		h.logger.Printf("%s %s via %s returned %s", req.Method, req.Host, proxyURL.Host, resp.Status)
	}

	removeHopByHopHeaders(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	for key := range resp.Trailer {
		w.Header().Add("Trailer", key)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && !isClosedConnectionError(err) {
		h.logger.Printf("proxy response copy failed via %s: %v", proxyURL.Host, err)
	}
	for key, values := range resp.Trailer {
		w.Header()[http.TrailerPrefix+key] = values
	}
}

func (h *Handler) handleConnect(w http.ResponseWriter, req *http.Request) {
	target, err := connectTarget(req.Host)
	if err != nil {
		h.proxyError(w, req, "validate CONNECT target", err)
		return
	}

	attempts := minInt(3, h.pool.Len())
	if attempts == 0 {
		attempts = 1
	}
	var (
		proxyURL       *url.URL
		upstreamConn   net.Conn
		upstreamReader *bufio.Reader
		upstreamResp   *http.Response
	)
	for attempt := 0; attempt < attempts; attempt++ {
		proxyURL, err = h.nextProxy()
		if err != nil {
			break
		}
		h.logForward(req.Method, "https://"+target, proxyURL, attempt+1, attempts)
		upstreamConn, upstreamReader, upstreamResp, err = h.openUpstreamTunnel(req.Context(), target, proxyURL, req)
		if err == nil {
			break
		}
		if req.Context().Err() != nil {
			break
		}
	}
	if err != nil {
		operation := "establish CONNECT tunnel"
		if proxyURL != nil {
			operation += " through " + proxyURL.Host
		}
		h.proxyError(w, req, operation, err)
		return
	}
	defer upstreamConn.Close()
	if upstreamResp.Body != nil {
		defer upstreamResp.Body.Close()
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		h.proxyError(w, req, "open tunnel", errors.New("HTTP server does not support connection hijacking"))
		return
	}
	clientConn, clientBuffer, err := hijacker.Hijack()
	if err != nil {
		h.logger.Printf("CONNECT %s via %s: hijack client: %v", target, proxyURL.Host, err)
		return
	}
	defer clientConn.Close()

	if _, err := clientBuffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := clientBuffer.Flush(); err != nil {
		return
	}

	tunnel(clientConn, clientBuffer.Reader, upstreamConn, upstreamReader)
}

func (h *Handler) openUpstreamTunnel(ctx context.Context, target string, proxyURL *url.URL, req *http.Request) (net.Conn, *bufio.Reader, *http.Response, error) {
	conn, err := h.dialer.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to upstream: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			conn.Close()
		}
	}()

	if h.dialer.Timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(h.dialer.Timeout)); err != nil {
			return nil, nil, nil, fmt.Errorf("set upstream handshake deadline: %w", err)
		}
	}
	if err := writeUpstreamConnect(conn, target, proxyURL); err != nil {
		return nil, nil, nil, fmt.Errorf("write upstream CONNECT: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read upstream CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, nil, nil, fmt.Errorf("upstream rejected CONNECT with %s", resp.Status)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, nil, nil, fmt.Errorf("clear upstream handshake deadline: %w", err)
	}

	closeOnError = false
	return conn, reader, resp, nil
}

func (h *Handler) httpAttempts(req *http.Request) int {
	if (req.Method != http.MethodGet && req.Method != http.MethodHead) || (req.Body != nil && req.Body != http.NoBody) {
		return 1
	}
	attempts := minInt(3, h.pool.Len())
	if attempts < 1 {
		return 1
	}
	return attempts
}

func (h *Handler) nextProxy() (*url.URL, error) {
	return h.pool.Next()
}

func (h *Handler) logForward(method, requestURL string, proxyURL *url.URL, attempt, attempts int) {
	h.logger.Printf("forward method=%s url=%s proxy=%s attempt=%d/%d", method, requestURL, proxyURL.Host, attempt, attempts)
}

func minInt(first, second int) int {
	if first < second {
		return first
	}
	return second
}

func connectTarget(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "\r\n") {
		return "", errors.New("empty or invalid host")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host, nil
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return net.JoinHostPort(host, "443"), nil
}

func writeUpstreamConnect(conn net.Conn, target string, proxyURL *url.URL) error {
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credentials := proxyURL.User.Username() + ":" + password
		encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
		fmt.Fprintf(&request, "Proxy-Authorization: Basic %s\r\n", encoded)
	}
	request.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	_, err := io.WriteString(conn, request.String())
	return err
}

func tunnel(clientConn net.Conn, clientReader io.Reader, upstreamConn net.Conn, upstreamReader io.Reader) {
	done := make(chan struct{}, 2)
	go copyHalf(upstreamConn, clientReader, done)
	go copyHalf(clientConn, upstreamReader, done)
	<-done
	clientConn.Close()
	upstreamConn.Close()
	<-done
}

func copyHalf(dst net.Conn, src io.Reader, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closeWriter.CloseWrite()
	}
	done <- struct{}{}
}

func (h *Handler) proxyError(w http.ResponseWriter, req *http.Request, operation string, err error) {
	h.logger.Printf("%s %s: %s: %v", req.Method, req.Host, operation, err)
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, connectionHeader := range header.Values("Connection") {
		for _, key := range strings.Split(connectionHeader, ",") {
			header.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(key)
	}
}

func isClosedConnectionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "broken pipe") || strings.Contains(message, "connection reset")
}

func isUpstreamProxyFailure(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return strings.Contains(resp.Status, " PROXY ") && strings.HasSuffix(resp.Status, " RET")
}
