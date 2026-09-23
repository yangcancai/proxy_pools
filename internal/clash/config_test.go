package clash

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	proxies, err := Parse([]byte("proxies:\n  - name: hk\n    type: ss\n  - name: '日本 / 01'\n    type: vmess\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 || proxies[1].Name != "日本 / 01" {
		t.Fatalf("proxies = %#v", proxies)
	}
}

func TestParseRejectsDuplicateNames(t *testing.T) {
	_, err := Parse([]byte("proxies:\n  - name: same\n  - name: same\n"))
	if err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestMergeSubscriptionsDeduplicatesProxyNames(t *testing.T) {
	merged, proxies, err := MergeSubscriptions(
		[]byte("proxies:\n  - name: one\n    type: ss\n  - name: two\n    type: ss\n"),
		[]byte("proxies:\n  - name: two\n    type: vmess\n  - name: three\n    type: vmess\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 3 || !strings.Contains(string(merged), "name: three") {
		t.Fatalf("merged proxies = %#v, config = %s", proxies, merged)
	}
}

func TestPrepareRuntimeConfigAddsListeners(t *testing.T) {
	data := []byte("proxies:\n  - name: one\n    type: ss\n")
	runtimeConfig, err := PrepareRuntimeConfig(data, "127.0.0.1:9090", "secret", "proxy-pools", 19000, map[string]int{"one": 20000})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(runtimeConfig); !containsAll(got, "listeners:", "port: 20000", "proxy: one", "external-controller: 127.0.0.1:9090") {
		t.Fatalf("runtime config = %s", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
