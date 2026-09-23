package clash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxSubscriptionSize = 10 << 20

// Proxy is the part of a Clash proxy definition needed to address a node.
// The complete definition is intentionally not interpreted here: Mihomo is
// responsible for parsing and running the node itself.
type Proxy struct {
	Name   string `yaml:"name"`
	Server string `yaml:"server"`
}

type Export struct {
	ExportedAt string        `json:"exported_at"`
	Proxies    []ExportProxy `json:"proxies"`
	Accounts   []any         `json:"accounts"`
}

type ExportProxy struct {
	ProxyKey       string `json:"proxy_key"`
	Name           string `json:"name"`
	Protocol       string `json:"protocol"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	Status         string `json:"status"`
	FallbackMode   string `json:"fallback_mode"`
	ExpiryWarnDays int    `json:"expiry_warn_days"`
}

// ExportJSON creates an export of the locally generated SOCKS5 listeners.
// The source node fields are used for the display name, while the endpoint
// fields point at the managed local listener that clients can actually use.
func ExportJSON(data []byte, now time.Time, listenHost string, portByName map[string]int, authByName map[string]Auth) ([]byte, error) {
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse Clash YAML for export: %w", err)
	}
	items, ok := config["proxies"].([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("Clash YAML has no proxies to export")
	}
	export := Export{ExportedAt: now.UTC().Format(time.RFC3339), Proxies: make([]ExportProxy, 0, len(items)), Accounts: []any{}}
	for _, item := range items {
		proxy, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := proxy["name"].(string)
		protocol, _ := proxy["type"].(string)
		host, _ := proxy["server"].(string)
		port := yamlInt(proxy["port"])
		username, _ := proxy["username"].(string)
		password, _ := proxy["password"].(string)
		if listenerPort, ok := portByName[name]; ok {
			protocol = "socks5"
			host = listenHost
			port = listenerPort
			if auth, ok := authByName[name]; ok {
				username = auth.Username
				password = auth.Password
			}
		}
		item := ExportProxy{
			ProxyKey:       fmt.Sprintf("%s|%s|%d|%s|%s", protocol, host, port, username, password),
			Name:           name,
			Protocol:       protocol,
			Host:           host,
			Port:           port,
			Username:       username,
			Password:       password,
			Status:         "active",
			FallbackMode:   "none",
			ExpiryWarnDays: 7,
		}
		export.Proxies = append(export.Proxies, item)
	}
	result, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write proxy export: %w", err)
	}
	return append(result, '\n'), nil
}

func yamlInt(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case uint64:
		return int(value)
	case float64:
		return int(value)
	case string:
		var result int
		_, _ = fmt.Sscanf(value, "%d", &result)
		return result
	default:
		return 0
	}
}

// PrepareRuntimeConfig makes a subscription usable by the managed Mihomo
// process without modifying the downloaded subscription in place.
func PrepareRuntimeConfig(data []byte, controller, secret, listenerPrefix string, portStart int, portByName map[string]int, authByName map[string]Auth) ([]byte, error) {
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse Clash YAML: %w", err)
	}
	if config == nil {
		return nil, fmt.Errorf("Clash YAML is empty")
	}
	config["external-controller"] = controller
	listenAddress := strings.TrimSpace(os.Getenv("MIHOMO_LISTEN"))
	if listenAddress == "" {
		listenAddress = "127.0.0.1"
	}
	if listenAddress != "127.0.0.1" && listenAddress != "::1" {
		config["allow-lan"] = true
	}
	if allowIPs := configuredAllowIPs(); len(allowIPs) > 0 {
		config["lan-allowed-ips"] = allowIPs
	}
	if secret != "" {
		config["secret"] = secret
	}
	proxies, err := Parse(data)
	if err != nil {
		return nil, err
	}
	listeners := make([]map[string]any, 0, len(proxies))
	for index, proxy := range proxies {
		port := portStart + index
		if mappedPort, ok := portByName[proxy.Name]; ok {
			port = mappedPort
		}
		if port > 65535 {
			return nil, fmt.Errorf("too many proxies: port range exceeds 65535")
		}
		listener := map[string]any{
			"name":   strings.TrimSuffix(listenerPrefix, "-") + "-" + proxy.Name,
			"type":   "mixed",
			"listen": listenAddress,
			"port":   port,
			"proxy":  proxy.Name,
		}
		if auth, ok := authByName[proxy.Name]; ok {
			listener["users"] = []map[string]string{{"username": auth.Username, "password": auth.Password}}
		}
		listeners = append(listeners, listener)
	}
	config["listeners"] = listeners
	result, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("write runtime Clash YAML: %w", err)
	}
	return result, nil
}

func configuredAllowIPs() []string {
	values := strings.Split(os.Getenv("PROXY_ALLOW_IPS"), ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

type Auth struct {
	Username string
	Password string
}

// Config is a Clash configuration containing the inline proxies section.
type Config struct {
	Proxies []Proxy `yaml:"proxies"`
}

func ParseFile(path string) ([]Proxy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Clash YAML: %w", err)
	}
	return Parse(data)
}

func FetchURL(ctx context.Context, rawURL string, client *http.Client) ([]Proxy, error) {
	body, err := DownloadURL(ctx, rawURL, client)
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

func DownloadURL(ctx context.Context, rawURL string, client *http.Client) ([]byte, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("Clash subscription URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create subscription request: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Clash subscription: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Clash subscription returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionSize+1))
	if err != nil {
		return nil, fmt.Errorf("read Clash subscription: %w", err)
	}
	if len(body) > maxSubscriptionSize {
		return nil, fmt.Errorf("Clash subscription exceeds %d bytes", maxSubscriptionSize)
	}
	return body, nil
}

func Parse(data []byte) ([]Proxy, error) {
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse Clash YAML: %w", err)
	}
	if len(config.Proxies) == 0 {
		return nil, fmt.Errorf("Clash YAML has no inline proxies")
	}

	result := make([]Proxy, 0, len(config.Proxies))
	seen := make(map[string]struct{}, len(config.Proxies))
	for index, proxy := range config.Proxies {
		proxy.Name = strings.TrimSpace(proxy.Name)
		if proxy.Name == "" {
			return nil, fmt.Errorf("Clash proxy at index %d has no name", index)
		}
		if _, exists := seen[proxy.Name]; exists {
			return nil, fmt.Errorf("duplicate Clash proxy name %q", proxy.Name)
		}
		seen[proxy.Name] = struct{}{}
		result = append(result, proxy)
	}
	return result, nil
}

// MergeSubscriptions combines Clash subscriptions, keeping the first proxy
// with a given name so the resulting configuration remains valid for Mihomo.
func MergeSubscriptions(documents ...[]byte) ([]byte, []Proxy, error) {
	if len(documents) == 0 {
		return nil, nil, fmt.Errorf("no Clash subscriptions")
	}
	var mergedConfig map[string]any
	merged := make([]any, 0)
	seen := make(map[string]struct{})
	for index, document := range documents {
		var config map[string]any
		if err := yaml.Unmarshal(document, &config); err != nil {
			return nil, nil, fmt.Errorf("parse Clash subscription %d: %w", index+1, err)
		}
		if index == 0 {
			mergedConfig = config
		}
		items, ok := config["proxies"].([]any)
		if !ok {
			return nil, nil, fmt.Errorf("Clash subscription %d has no inline proxies", index+1)
		}
		for _, item := range items {
			proxy, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := proxy["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			merged = append(merged, proxy)
		}
	}
	if len(merged) == 0 {
		return nil, nil, fmt.Errorf("merged Clash subscriptions have no proxies")
	}
	mergedConfig["proxies"] = merged
	result, err := yaml.Marshal(mergedConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("write merged Clash YAML: %w", err)
	}
	proxies, err := Parse(result)
	if err != nil {
		return nil, nil, err
	}
	return result, proxies, nil
}
