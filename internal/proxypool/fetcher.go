package proxypool

import (
	"context"
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
)

const maxAPIResponseSize = 1 << 20

var ErrRateLimited = errors.New("proxy API rate limited")

type Fetcher struct {
	APIURL   string
	Username string
	Password string
	Client   *http.Client
	Logger   *log.Logger
}

func NewFetcher(apiURL, username, password string, timeout time.Duration) *Fetcher {
	return &Fetcher{
		APIURL:   apiURL,
		Username: username,
		Password: password,
		Client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (f *Fetcher) Fetch(ctx context.Context) ([]*url.URL, error) {
	if strings.TrimSpace(f.APIURL) == "" {
		return nil, fmt.Errorf("proxy API URL is empty")
	}

	logURL := RedactedAPIURL(f.APIURL)
	startedAt := time.Now()
	f.logf("proxy API request method=GET url=%s", logURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.APIURL, nil)
	if err != nil {
		f.logf("proxy API response url=%s result=invalid_request duration=%s", logURL, time.Since(startedAt))
		return nil, fmt.Errorf("create proxy API request: %w", err)
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		f.logf("proxy API response url=%s result=transport_error duration=%s", logURL, time.Since(startedAt))
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return nil, fmt.Errorf("request proxy API: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		f.logResponse(logURL, resp, "read_error", startedAt, 0)
		return nil, fmt.Errorf("read proxy API response: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		f.logResponse(logURL, resp, "response_too_large", startedAt, len(body))
		return nil, fmt.Errorf("proxy API response exceeds %d bytes", maxAPIResponseSize)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusTooManyRequests {
			f.logResponse(logURL, resp, "rate_limited", startedAt, len(body))
			return nil, fmt.Errorf("%w: %s", ErrRateLimited, responseExcerpt(body))
		}
		f.logResponse(logURL, resp, "http_error", startedAt, len(body))
		return nil, fmt.Errorf("proxy API returned %s: %s", resp.Status, responseExcerpt(body))
	}
	if strings.Contains(strings.ToLower(string(body)), "too many requests") {
		f.logResponse(logURL, resp, "rate_limited", startedAt, len(body))
		return nil, fmt.Errorf("%w: %s", ErrRateLimited, responseExcerpt(body))
	}

	proxies, err := ParseProxyList(string(body), f.Username, f.Password)
	if err != nil {
		f.logResponse(logURL, resp, "invalid_response", startedAt, len(body))
		return nil, err
	}
	f.logResponse(logURL, resp, "ok", startedAt, len(body))
	return proxies, nil
}

func (f *Fetcher) logf(format string, values ...any) {
	if f.Logger != nil {
		f.Logger.Printf(format, values...)
	}
}

func (f *Fetcher) logResponse(logURL string, resp *http.Response, result string, startedAt time.Time, bodyBytes int) {
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if retryAfter == "" {
		retryAfter = "-"
	}
	f.logf(
		"proxy API response url=%s status=%d result=%s duration=%s bytes=%d retry_after=%s",
		logURL,
		resp.StatusCode,
		result,
		time.Since(startedAt).Round(time.Millisecond),
		bodyBytes,
		retryAfter,
	)
}

// RedactedAPIURL returns an API URL suitable for logs. Parameter names and
// non-secret configuration remain visible while credentials are replaced.
func RedactedAPIURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid>"
	}
	if parsed.User != nil {
		parsed.User = url.User("***")
	}
	query := parsed.Query()
	for key, values := range query {
		if !isSecretQueryParameter(key) {
			continue
		}
		for index := range values {
			values[index] = "***"
		}
		query[key] = values
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func isSecretQueryParameter(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
	for _, secret := range []string{"apikey", "key", "pwd", "password", "passwd", "token", "secret", "authorization", "auth", "user", "username", "account"} {
		if normalized == secret || strings.Contains(normalized, secret) {
			return true
		}
	}
	return false
}

// ParseProxyList parses the CRLF-separated ip:port format returned by the
// Domi API. Whitespace-separated values and explicit http:// URLs are also
// accepted to make the parser tolerant of API formatting changes.
func ParseProxyList(body, username, password string) ([]*url.URL, error) {
	items := strings.Fields(body)
	proxies := make([]*url.URL, 0, len(items))
	seen := make(map[string]struct{}, len(items))

	for _, item := range items {
		item = strings.TrimSpace(strings.Trim(item, "\",'"))
		if item == "" {
			continue
		}
		if !strings.Contains(item, "://") {
			item = "http://" + item
		}

		proxyURL, err := url.Parse(item)
		if err != nil || proxyURL.Scheme != "http" || proxyURL.Host == "" {
			continue
		}
		host, port, err := net.SplitHostPort(proxyURL.Host)
		if err != nil || host == "" {
			continue
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			continue
		}
		if proxyURL.Path != "" || proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
			continue
		}
		if username != "" {
			proxyURL.User = url.UserPassword(username, password)
		}

		key := proxyURL.Host
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		proxies = append(proxies, proxyURL)
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("proxy API returned no valid ip:port entries: %s", responseExcerpt([]byte(body)))
	}
	return proxies, nil
}

func responseExcerpt(body []byte) string {
	const maxExcerpt = 200
	text := strings.TrimSpace(string(body))
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > maxExcerpt {
		return text[:maxExcerpt] + "..."
	}
	return text
}
