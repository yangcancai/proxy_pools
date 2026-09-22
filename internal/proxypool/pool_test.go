package proxypool

import (
	"net/url"
	"testing"
)

func TestPoolRoundRobinAndReplace(t *testing.T) {
	t.Parallel()

	first := mustURL(t, "http://127.0.0.1:8001")
	second := mustURL(t, "http://127.0.0.1:8002")
	pool, err := New([]*url.URL{first, second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []string{first.Host, second.Host, first.Host}
	for index, wantHost := range want {
		got, err := pool.Next()
		if err != nil {
			t.Fatalf("Next %d: %v", index, err)
		}
		if got.Host != wantHost {
			t.Fatalf("Next %d host = %q, want %q", index, got.Host, wantHost)
		}
	}

	replacement := mustURL(t, "http://127.0.0.1:9001")
	if err := pool.Replace([]*url.URL{replacement}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := pool.Next()
	if err != nil {
		t.Fatalf("Next after Replace: %v", err)
	}
	if got.Host != replacement.Host {
		t.Fatalf("Next after Replace host = %q, want %q", got.Host, replacement.Host)
	}

	if err := pool.Replace(nil); err != ErrEmptyPool {
		t.Fatalf("empty Replace error = %v, want %v", err, ErrEmptyPool)
	}
	if pool.Len() != 1 {
		t.Fatalf("empty Replace changed pool length to %d", pool.Len())
	}
}

func TestNextWithCycle(t *testing.T) {
	t.Parallel()

	pool, err := New([]*url.URL{
		mustURL(t, "http://127.0.0.1:8001"),
		mustURL(t, "http://127.0.0.1:8002"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, cycleComplete, err := pool.NextWithCycle()
	if err != nil {
		t.Fatalf("first NextWithCycle: %v", err)
	}
	if cycleComplete {
		t.Fatal("first selection unexpectedly completed the cycle")
	}
	_, cycleComplete, err = pool.NextWithCycle()
	if err != nil {
		t.Fatalf("second NextWithCycle: %v", err)
	}
	if !cycleComplete {
		t.Fatal("second selection did not complete the cycle")
	}
}

func TestNewEmptyCanBePopulatedLater(t *testing.T) {
	t.Parallel()

	pool := NewEmpty()
	if pool.Len() != 0 {
		t.Fatalf("new empty pool length = %d, want 0", pool.Len())
	}
	if _, err := pool.Next(); err != ErrEmptyPool {
		t.Fatalf("Next error = %v, want %v", err, ErrEmptyPool)
	}

	proxyURL := mustURL(t, "http://127.0.0.1:8001")
	if err := pool.Replace([]*url.URL{proxyURL}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if pool.Len() != 1 {
		t.Fatalf("populated pool length = %d, want 1", pool.Len())
	}
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", value, err)
	}
	return parsed
}
