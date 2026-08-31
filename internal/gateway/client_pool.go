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
	client    *http.Client
	transport *http.Transport
}

type clientKey struct {
	providerID string
	generation provider.ProviderGeneration
}

// ClientPool reuses connection pools only within one request-affecting
// provider generation. Reconciliation closes idle connections belonging to
// generations that are no longer present.
type ClientPool struct {
	mu      sync.Mutex
	clients map[clientKey]pooledClient
	closed  bool
}

func NewClientPool() *ClientPool {
	return &ClientPool{clients: make(map[clientKey]pooledClient)}
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
	p.clients[key] = pooledClient{client: client, transport: transport}
	return client, nil
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
	for key, entry := range p.clients {
		if _, ok := active[key]; ok {
			continue
		}
		entry.transport.CloseIdleConnections()
		delete(p.clients, key)
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
	for key, entry := range p.clients {
		entry.transport.CloseIdleConnections()
		delete(p.clients, key)
	}
	return nil
}
