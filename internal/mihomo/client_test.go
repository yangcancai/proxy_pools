package mihomo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPutListener(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/listeners/pool-hk one" {
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
		var got Listener
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Type != "mixed" || got.Port != 18081 || got.Proxy != "hk one" {
			t.Fatalf("listener = %#v", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PutListener(context.Background(), Listener{Name: "pool-hk one", Type: "mixed", Port: 18081, Proxy: "hk one"}); err != nil {
		t.Fatal(err)
	}
}
