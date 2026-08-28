package shim

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// cpaProxyServer builds a proxyServer with a single cliproxy route whose upstream
// is the given test server, and a runtime carrying the given cpa.key.
func cpaProxyServer(t *testing.T, upstream *httptest.Server, cpaKey string) *proxyServer {
	t.Helper()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	server := &proxyServer{}
	server.state.Store(&proxyState{
		runtime: &runtimeConfig{ListenAddr: defaultListenAddr, CPA: cpaRuntimeConfig{Key: cpaKey}},
		table: &routingTable{routes: []proxyRoute{{
			name:        "cliproxy",
			prefix:      cliproxyPrefix,
			upstreams:   newUpstreamPool([]*url.URL{u}),
			credentials: staticKeyProvider{key: cpaKey},
			client:      upstream.Client(),
			strategy:    cliproxyStrategy{},
		}}},
	})
	return server
}

// TestCPAKeyAuthOverrideEndToEnd proves that a configured cpa.key makes the shim
// own CLIProxyAPI's bearer token: the client's forwarded Authorization/X-Api-Key
// are stripped and replaced by Authorization: Bearer <cpa.key> — the same scheme
// Claude Code uses (ANTHROPIC_AUTH_TOKEN), per the upstream's auth model.
func TestCPAKeyAuthOverrideEndToEnd(t *testing.T) {
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(cpaProxyServer(t, upstream, "shim-cpa-token"))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if gotAuth != "Bearer shim-cpa-token" {
		t.Fatalf("upstream Authorization = %q, want Bearer shim-cpa-token", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
	}
}

// TestNormalAnyRouterTrafficIgnoresGlobalClassifierTarget proves the routing
// contract's transport half: even with a global classifier target configured,
// a NORMAL (non-classifier) /any request still routes by prefix to AnyRouter —
// never to the global target — and still draws its key from the AnyRouter
// account pool. The global target only captures detected classifier requests.
func TestNormalAnyRouterTrafficIgnoresGlobalClassifierTarget(t *testing.T) {
	var anyHits int
	var gotAuth, gotAPIKey string
	anyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anyHits++
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":"any"}`))
	}))
	defer anyUpstream.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("global classifier target must NOT be hit by normal (non-classifier) /any traffic")
	}))
	defer target.Close()

	anyURL, err := url.Parse(anyUpstream.URL)
	if err != nil {
		t.Fatalf("parse any upstream URL: %v", err)
	}
	server := classifierTestServer(t, classifierRuntimeConfig{
		TargetBaseURL: target.URL,
		TargetKey:     "sk-target",
	}, target, proxyRoute{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{anyURL}),
		credentials: newAccountPool([]accountEntry{{Label: "acct-1", Key: "sk-account"}}),
		client:      anyUpstream.Client(),
		strategy:    anyRouterStrategy{},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Non-classifier body: no security-monitor system prefix.
	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if anyHits != 1 {
		t.Fatalf("AnyRouter prefix upstream hits = %d, want 1 (normal traffic must hit the prefix upstream)", anyHits)
	}
	if string(out) != `{"ok":"any"}` {
		t.Fatalf("client body = %s, want the AnyRouter prefix upstream's body", out)
	}
	// Still drew the shim-owned account key, not the classifier target key nor the client's.
	if gotAuth != "Bearer sk-account" {
		t.Fatalf("upstream Authorization = %q, want Bearer sk-account (from the account pool)", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
	}
}

// TestCPAEmptyKeyForwardsClientAuth proves the default (no cpa.key) preserves
// today's behavior: the client's own credentials pass through unchanged.
func TestCPAEmptyKeyForwardsClientAuth(t *testing.T) {
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(cpaProxyServer(t, upstream, ""))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if gotAuth != "Bearer client-token" {
		t.Fatalf("upstream Authorization = %q, want the client's Bearer client-token", gotAuth)
	}
	if gotAPIKey != "client-key" {
		t.Fatalf("upstream X-Api-Key = %q, want the client's client-key", gotAPIKey)
	}
}

// TestClassifierGlobalTargetKeyAuthOverrideEndToEnd proves the integration wires
// the classifier global-target key through to the upstream as Authorization:
// Bearer (it was inert in the isolated classifier branch). A claude-model
// classifier on /any with a configured global target is routed to that target
// with the shim-owned target key, stripping the client's auth.
func TestClassifierGlobalTargetKeyAuthOverrideEndToEnd(t *testing.T) {
	var gotAuth, gotAPIKey string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"<block>no</block>"}]}`))
	}))
	defer target.Close()

	anyURL, err := url.Parse("https://anyrouter.invalid")
	if err != nil {
		t.Fatalf("parse any URL: %v", err)
	}
	server := classifierTestServer(t, classifierRuntimeConfig{
		TargetBaseURL: target.URL,
		TargetKey:     "shim-target-key",
	}, target, proxyRoute{
		name:      "anyrouter",
		prefix:    anyRouterPrefix,
		upstreams: newUpstreamPool([]*url.URL{anyURL}),
		client:    target.Client(),
		strategy:  anyRouterStrategy{},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if gotAuth != "Bearer shim-target-key" {
		t.Fatalf("global target Authorization = %q, want Bearer shim-target-key", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("global target X-Api-Key = %q, want stripped", gotAPIKey)
	}
}

// TestClassifierGlobalTargetEmptyKeyForwardsClientAuthNoAccount proves that when
// a global classifier target is configured with an EMPTY target_key, a classifier
// request on /any — whose route has an AnyRouter account pool — forwards the
// CLIENT's own auth to the global target and never draws from (nor charges) the
// account pool. Without the globalTarget gate the empty target_key would fall
// into the account-selection block, leaking an AnyRouter bearer token to the
// third-party host and mis-attributing its health to AnyRouter.
func TestClassifierGlobalTargetEmptyKeyForwardsClientAuthNoAccount(t *testing.T) {
	var gotAuth, gotAPIKey string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"<block>no</block>"}]}`))
	}))
	defer target.Close()

	anyURL, err := url.Parse("https://anyrouter.invalid")
	if err != nil {
		t.Fatalf("parse any URL: %v", err)
	}
	accounts := newAccountPool([]accountEntry{{Label: "acct-1", Key: "sk-account"}})
	server := classifierTestServer(t, classifierRuntimeConfig{
		TargetBaseURL: target.URL,
		TargetKey:     "", // empty ⇒ forward the client's own auth
	}, target, proxyRoute{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{anyURL}),
		credentials: accounts,
		client:      target.Client(),
		strategy:    anyRouterStrategy{},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The global target must see the client's own credentials, not an account key.
	if gotAuth != "Bearer client-token" {
		t.Fatalf("global target Authorization = %q, want the client's Bearer client-token", gotAuth)
	}
	if gotAPIKey != "client-key" {
		t.Fatalf("global target X-Api-Key = %q, want the client's client-key", gotAPIKey)
	}
	// No account was assigned (no session pinned) nor evicted (no failures charged).
	accounts.mu.Lock()
	gotSessions := len(accounts.sessions)
	gotFailures := accounts.accounts[0].failures
	gotCooldown := accounts.accounts[0].cooldownUntil
	accounts.mu.Unlock()
	if gotSessions != 0 {
		t.Fatalf("account sessions pinned = %d, want 0 (no account assigned)", gotSessions)
	}
	if gotFailures != 0 || !gotCooldown.IsZero() {
		t.Fatalf("account charged failures = %d cooldown = %v, want none (no account evicted)", gotFailures, gotCooldown)
	}
}
