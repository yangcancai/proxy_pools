package proxypool

import (
	"errors"
	"net/url"
	"sync"
)

var ErrEmptyPool = errors.New("proxy pool is empty")

// Pool is a concurrency-safe, round-robin collection of upstream proxies.
type Pool struct {
	mu      sync.Mutex
	proxies []*url.URL
	next    uint64
}

func New(proxies []*url.URL) (*Pool, error) {
	p := &Pool{}
	if err := p.Replace(proxies); err != nil {
		return nil, err
	}
	return p, nil
}

// NewEmpty returns an empty pool that can be populated later with Replace.
// It is useful when upstream proxies should only be fetched on first use.
func NewEmpty() *Pool {
	return &Pool{}
}

// Next returns a copy of the next proxy URL in the pool.
func (p *Pool) Next() (*url.URL, error) {
	proxyURL, _, err := p.NextWithCycle()
	return proxyURL, err
}

// NextWithCycle returns the next proxy and reports whether this selection
// completed one full pass through the current pool.
func (p *Pool) NextWithCycle() (*url.URL, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 {
		return nil, false, ErrEmptyPool
	}

	proxyURL := p.proxies[p.next%uint64(len(p.proxies))]
	p.next++
	cycleComplete := p.next%uint64(len(p.proxies)) == 0
	copyURL := *proxyURL
	return &copyURL, cycleComplete, nil
}

// Replace atomically replaces the current proxy list and restarts rotation.
// An empty list is rejected so a failed refresh cannot erase a working pool.
func (p *Pool) Replace(proxies []*url.URL) error {
	if len(proxies) == 0 {
		return ErrEmptyPool
	}

	copied := make([]*url.URL, 0, len(proxies))
	for _, proxyURL := range proxies {
		if proxyURL == nil {
			continue
		}
		copyURL := *proxyURL
		copied = append(copied, &copyURL)
	}
	if len(copied) == 0 {
		return ErrEmptyPool
	}

	p.mu.Lock()
	p.proxies = copied
	p.next = 0
	p.mu.Unlock()
	return nil
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.proxies)
}
