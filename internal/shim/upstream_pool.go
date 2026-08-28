package shim

import (
	"net/url"
	"sync"
)

// upstreamPool tracks which entrance a route is currently using when several
// interchangeable upstream URLs front the same backend (the AnyRouter
// entrances). Selection is sticky: the current entrance serves all traffic
// until a request fails on it, and whichever entrance then succeeds becomes
// the new current one. There is no time-based switchback or probing; real
// requests are the only signal.
type upstreamPool struct {
	mu      sync.Mutex
	current int
	entries []*url.URL
}

func newUpstreamPool(entries []*url.URL) *upstreamPool {
	return &upstreamPool{entries: entries}
}

func (p *upstreamPool) url(i int) *url.URL { return p.entries[i] }

// attemptOrder returns the entrance indices one request should try: the
// current entrance first, then the remaining entrances in configured order.
func (p *upstreamPool) attemptOrder() []int {
	p.mu.Lock()
	current := p.current
	p.mu.Unlock()

	order := make([]int, 0, len(p.entries))
	order = append(order, current)
	for i := range p.entries {
		if i != current {
			order = append(order, i)
		}
	}
	return order
}

// promote makes entrance i the current one and reports whether that changed
// anything.
func (p *upstreamPool) promote(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == i {
		return false
	}
	p.current = i
	return true
}

// advanceFrom moves the current pointer to the entrance after i, but only if
// i is still current — a concurrent promote from a successful request wins
// over this stale failure signal. It is used when a response died mid-stream:
// that request cannot be retried, but the client's own retry should not land
// on the same broken entrance.
func (p *upstreamPool) advanceFrom(i int) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) < 2 || p.current != i {
		return p.current, false
	}
	p.current = (i + 1) % len(p.entries)
	return p.current, true
}

// entranceStatus is one entrance in a GET /admin/status snapshot: its host and
// full URL, and whether it is the entrance currently serving traffic (the rest
// are standby). Field names are part of the /admin/status contract.
type entranceStatus struct {
	Host   string `json:"host"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// snapshot returns each entrance with its host/URL and whether it is the current
// (active) one, taken under the lock for a consistent view of the pointer
// alongside the entry list. The conversions are pure string ops, so the lock is
// never held across I/O.
func (p *upstreamPool) snapshot() []entranceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]entranceStatus, len(p.entries))
	for i, u := range p.entries {
		out[i] = entranceStatus{Host: u.Host, URL: u.String(), Active: i == p.current}
	}
	return out
}
