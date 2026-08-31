package gateway

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
)

type pooledClient struct {
	client     *http.Client
	transport  *http.Transport
	refs       int
	retired    bool
	idleClosed bool
}

// requestClientLease keeps a transport generation alive for one upstream
// attempt. Reconciliation may retire the generation while the request is in
// flight; the transport is then closed only after the last lease is released.
type requestClientLease struct {
	pool  *ClientPool
	entry *pooledClient
	once  sync.Once
}

func (l *requestClientLease) Client() *http.Client {
	if l == nil || l.entry == nil {
		return nil
	}
	return l.entry.client
}

func (l *requestClientLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.pool != nil && l.entry != nil {
			l.pool.release(l.entry)
		}
	})
}

type clientKey struct {
	providerID string
	generation provider.ProviderGeneration
}

// ClientPool reuses connection pools only within one request-affecting
// provider generation. Reconciliation closes idle connections belonging to
// generations that are no longer present.
type ClientPool struct {
	mu         sync.Mutex
	clients    map[clientKey]*pooledClient
	active     map[clientKey]struct{}
	reconciled bool
	closed     bool
}

func NewClientPool() *ClientPool {
	return &ClientPool{
		clients: make(map[clientKey]*pooledClient),
		active:  make(map[clientKey]struct{}),
	}
}

func (p *ClientPool) Client(item *provider.CompiledProvider) (*http.Client, error) {
	if item == nil {
		return nil, errors.New("provider is nil")
	}
	return p.ClientForTarget(&item.CompiledTarget)
}

// ClientForTarget returns a pooled HTTP client for any compiled target,
// including a pool-external fixed classifier target. Pool identity remains the
// target ID plus generation, so changing endpoint/auth/TLS never reuses an old
// transport.
func (p *ClientPool) ClientForTarget(item *provider.CompiledTarget) (*http.Client, error) {
	if p == nil {
		return nil, errors.New("provider client pool is nil")
	}
	if item == nil {
		return nil, errors.New("target is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("provider client pool is closed")
	}
	key := clientKey{providerID: item.ID, generation: item.Generation}
	if existing, ok := p.clients[key]; ok {
		return existing.client, nil
	}
	if p.reconciled {
		if _, active := p.active[key]; !active {
			return nil, errors.New("target generation is not active")
		}
	}
	entry := newPooledClient(item)
	p.clients[key] = entry
	return entry.client, nil
}

func newPooledClient(item *provider.CompiledTarget) *pooledClient {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       item.TLSClone(),
		DisableCompression:    true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &pooledClient{client: client, transport: transport}
}

// Acquire returns a request-scoped lease for a pool Provider generation.
func (p *ClientPool) Acquire(item *provider.CompiledProvider) (*requestClientLease, error) {
	if item == nil {
		return nil, errors.New("provider is nil")
	}
	return p.AcquireTarget(&item.CompiledTarget)
}

// AcquireTarget returns a request-scoped lease for any compiled target. Once
// reconciliation has published an active target set, a stale generation gets
// an ephemeral retired entry that is never inserted back into the current
// pool. The request may finish normally, and Release closes its idle transport.
func (p *ClientPool) AcquireTarget(item *provider.CompiledTarget) (*requestClientLease, error) {
	if p == nil {
		return nil, errors.New("provider client pool is nil")
	}
	if item == nil {
		return nil, errors.New("target is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("provider client pool is closed")
	}
	key := clientKey{providerID: item.ID, generation: item.Generation}
	if existing, ok := p.clients[key]; ok {
		existing.refs++
		return &requestClientLease{pool: p, entry: existing}, nil
	}
	entry := newPooledClient(item)
	entry.refs = 1
	if p.reconciled {
		if _, active := p.active[key]; !active {
			entry.retired = true
			return &requestClientLease{pool: p, entry: entry}, nil
		}
	}
	p.clients[key] = entry
	return &requestClientLease{pool: p, entry: entry}, nil
}

func (p *ClientPool) Reconcile(providers []*provider.CompiledProvider) {
	p.ReconcileTargets(providers, nil)
}

// ReconcileTargets keeps both pool-provider clients and the optional fixed
// classifier target client set aligned with the published runtime snapshot.
// Fixed targets are intentionally not represented as CompiledProvider values,
// but their target ID/generation still own a transport that must be retired on
// hot update or mode disable.
func (p *ClientPool) ReconcileTargets(providers []*provider.CompiledProvider, fixed *provider.CompiledFixedTarget) {
	if p == nil {
		return
	}
	active := make(map[clientKey]struct{}, len(providers)+1)
	for _, item := range providers {
		if item != nil {
			active[clientKey{providerID: item.ID, generation: item.Generation}] = struct{}{}
		}
	}
	if fixed != nil {
		active[clientKey{providerID: fixed.ID, generation: fixed.Generation}] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.active = active
	p.reconciled = true
	for key, entry := range p.clients {
		if _, ok := active[key]; ok {
			continue
		}
		delete(p.clients, key)
		p.retireLocked(entry)
	}
}

func (p *ClientPool) release(entry *pooledClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.refs > 0 {
		entry.refs--
	}
	if entry.retired && entry.refs == 0 {
		p.closeIdleLocked(entry)
	}
}

func (p *ClientPool) retireLocked(entry *pooledClient) {
	if entry == nil {
		return
	}
	entry.retired = true
	if entry.refs == 0 {
		p.closeIdleLocked(entry)
	}
}

func (p *ClientPool) closeIdleLocked(entry *pooledClient) {
	if entry == nil || entry.idleClosed {
		return
	}
	entry.idleClosed = true
	if entry.transport != nil {
		entry.transport.CloseIdleConnections()
	}
}

func (p *ClientPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.active = nil
	for key, entry := range p.clients {
		delete(p.clients, key)
		p.retireLocked(entry)
	}
	return nil
}
