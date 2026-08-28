package shim

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Compile-time proof that static and rotating credentials satisfy the unified
// credentialProvider abstraction: AnyRouter multi-account rotation and the
// degenerate single-key CPA provider.
var (
	_ credentialProvider = (*accountPool)(nil)
	_ credentialProvider = staticKeyProvider{}
)

func TestSessionIDForRequest(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"from-body\",\"account_uuid\":\"x\"}"}}`)

	r := httptest.NewRequest(http.MethodPost, "/any/v1/messages", strings.NewReader(""))
	r.Header.Set(sessionHeader, "from-header")
	if got := sessionIDForRequest(r, body); got != "from-header" {
		t.Fatalf("session id = %q, want from-header (header wins)", got)
	}

	r2 := httptest.NewRequest(http.MethodPost, "/any/v1/messages", strings.NewReader(""))
	if got := sessionIDForRequest(r2, body); got != "from-body" {
		t.Fatalf("session id = %q, want from-body (metadata fallback)", got)
	}

	if got := sessionIDForRequest(r2, []byte(`{"messages":[]}`)); got != "" {
		t.Fatalf("session id = %q, want empty when absent", got)
	}
}

func TestApplyAccountAuthOverridesAndStrips(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer client-token")
	h.Set("X-Api-Key", "client-key")
	applyAccountAuth(h, "acct-key")
	if got := h.Get("Authorization"); got != "Bearer acct-key" {
		t.Fatalf("Authorization = %q, want Bearer acct-key", got)
	}
	if got := h.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key = %q, want stripped", got)
	}

	// Empty key forwards the client's auth unchanged.
	keep := http.Header{}
	keep.Set("Authorization", "Bearer client-token")
	applyAccountAuth(keep, "")
	if got := keep.Get("Authorization"); got != "Bearer client-token" {
		t.Fatalf("Authorization = %q, want client token preserved", got)
	}
}

// TestAnyRouterAccountAuthOverrideEndToEnd drives a request through the proxy
// server with a configured account pool and verifies the upstream sees the
// account key, not the client's forwarded credentials.
func TestStaticKeyProviderAssignAndReportNoop(t *testing.T) {
	p := staticKeyProvider{key: "shim-cpa-token"}
	for _, sid := range []string{"", "session-A", "session-B"} {
		key, idx, ok := p.assign(sid)
		if key != "shim-cpa-token" || idx != 0 || !ok {
			t.Fatalf("assign(%q) = (%q,%d,%v), want (shim-cpa-token,0,true)", sid, key, idx, ok)
		}
	}

	// reportResult must never panic nor change the served key, for any outcome —
	// including a 401 that hard-evicts a rotation account but must NOT touch a
	// single static key (the operator fixes a bad cpa.key; there is no peer).
	p.reportResult(0, http.StatusUnauthorized, false)
	p.reportResult(0, http.StatusTooManyRequests, false)
	p.reportResult(0, 0, true)
	if key, _, ok := p.assign("session-A"); key != "shim-cpa-token" || !ok {
		t.Fatalf("assign after reportResult = (%q,%v), want (shim-cpa-token,true) — static key never evicted", key, ok)
	}

	// An empty key is returned verbatim with ok=true (today's /cpa default with no
	// cpa.key: the client's own auth is forwarded by applyAccountAuth's no-op).
	if key, idx, ok := (staticKeyProvider{}).assign("session-A"); key != "" || idx != 0 || !ok {
		t.Fatalf("empty staticKeyProvider.assign = (%q,%d,%v), want (\"\",0,true)", key, idx, ok)
	}
}

// TestBuildRoutingTableWiresCredentialProviders proves buildRoutingTable configures
// one credentialProvider per route: AnyRouter rotates → *accountPool; CPA is
// single-key → staticKeyProvider seeded with cpa.key. This is the production wiring
// that makes the /cpa route yield Authorization: Bearer <cpa.key> through the
// unified main path.
func TestBuildRoutingTableWiresCredentialProviders(t *testing.T) {
	compiled, err := compileRuntimeConfig(&runtimeConfig{
		ListenAddr: defaultListenAddr,
		AnyRouter:  anyRouterRuntimeConfig{Accounts: []accountEntry{{Label: "a", Key: "k-any"}}},
		CPA:        cpaRuntimeConfig{Key: "shim-cpa-token"},
	})
	if err != nil {
		t.Fatalf("compileRuntimeConfig: %v", err)
	}
	table, err := buildRoutingTable(compiled)
	if err != nil {
		t.Fatalf("buildRoutingTable: %v", err)
	}

	var anyRoute, cpaRoute *proxyRoute
	for i := range table.routes {
		switch table.routes[i].prefix {
		case anyRouterPrefix:
			anyRoute = &table.routes[i]
		case cliproxyPrefix:
			cpaRoute = &table.routes[i]
		}
	}
	if anyRoute == nil || cpaRoute == nil {
		t.Fatalf("missing routes: any=%v cpa=%v", anyRoute, cpaRoute)
	}
	if _, ok := anyRoute.credentials.(*accountPool); !ok {
		t.Fatalf("anyrouter credentials = %T, want *accountPool", anyRoute.credentials)
	}
	sp, ok := cpaRoute.credentials.(staticKeyProvider)
	if !ok {
		t.Fatalf("cliproxy credentials = %T, want staticKeyProvider", cpaRoute.credentials)
	}
	if key, idx, ok := sp.assign("any-session"); key != "shim-cpa-token" || idx != 0 || !ok {
		t.Fatalf("cpa staticKeyProvider.assign = (%q,%d,%v), want (shim-cpa-token,0,true)", key, idx, ok)
	}
}

// recordingProvider is a credentialProvider test double that counts assign and
// reportResult calls so a test can assert the main path's charging decisions
// directly (independent of accountPool's internal health model).
type recordingProvider struct {
	key     string
	mu      sync.Mutex
	assigns int
	reports int
}

func (p *recordingProvider) assign(string) (string, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.assigns++
	return p.key, 0, true
}

func (p *recordingProvider) reportResult(int, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports++
}

func (p *recordingProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.assigns, p.reports
}

// TestCredentialNotChargedOnClientCancel proves that the unified credential path
// assigns a credential for the request, but a client cancel
// (not an entrance failure) must NOT call reportResult — the cancel path returns
// before the charge. Mirrors the failover client-cancel mechanism.
