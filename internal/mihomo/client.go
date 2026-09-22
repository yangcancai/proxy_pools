package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

type Listener struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Listen string `json:"listen"`
	Port   int    `json:"port"`
	Proxy  string `json:"proxy"`
}

func NewClient(baseURL, secret string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Mihomo controller URL %q", baseURL)
	}
	return &Client{BaseURL: baseURL, Secret: secret, HTTP: http.DefaultClient}, nil
}

// PutListener creates or updates a Mihomo mixed listener. A mixed listener
// accepts HTTP(S) proxy requests and SOCKS5 on the same local port.
func (c *Client) PutListener(ctx context.Context, listener Listener) error {
	body, err := json.Marshal(listener)
	if err != nil {
		return err
	}
	endpoint := c.BaseURL + "/listeners/" + url.PathEscape(listener.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Mihomo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call Mihomo controller: %w", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Mihomo listener %q returned %s: %s", listener.Name, resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *Client) WaitReady(ctx context.Context) error {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/version", nil)
		if err != nil {
			return err
		}
		if c.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+c.Secret)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Mihomo: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func ListenerName(prefix, proxyName string) string {
	return strings.TrimSuffix(prefix, "-") + "-" + proxyName
}

func FormatPort(port int) string { return strconv.Itoa(port) }
