package shim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// passthroughServer builds a proxyServer whose global switch is OFF
// (runtimeConfig.enabled == false), so every request takes the servePassthrough
// path. It mirrors testProxyServerFromRoutes but pins Enabled=false.
func passthroughServer(routes []proxyRoute) *proxyServer {
	disabled := false
	server := &proxyServer{}
	server.state.Store(&proxyState{
		runtime: &runtimeConfig{ListenAddr: defaultListenAddr, Enabled: &disabled},
		table:   &routingTable{routes: routes},
	})
	return server
}

func TestRuntimeConfigIsEnabledAndDefaults(t *testing.T) {
	yes, no := true, false
	if !(&runtimeConfig{}).isEnabled() {
		t.Fatal("nil Enabled must report enabled (back-compat default)")
	}
	if !(&runtimeConfig{Enabled: &yes}).isEnabled() {
		t.Fatal("Enabled=&true must report enabled")
	}
	if (&runtimeConfig{Enabled: &no}).isEnabled() {
		t.Fatal("Enabled=&false must report disabled")
	}

	// normalize fills a nil switch with true (and persists it, since the on-disk
	// config is written from the normalized config).
	norm := normalizeRuntimeConfig(nil)
	if norm.Enabled == nil || !*norm.Enabled {
		t.Fatalf("normalize(nil) Enabled = %v, want non-nil true", norm.Enabled)
	}
	// An explicit false survives normalization unchanged.
	norm = normalizeRuntimeConfig(&runtimeConfig{Enabled: &no})
	if norm.Enabled == nil || *norm.Enabled {
		t.Fatalf("normalize(false) Enabled = %v, want non-nil false", norm.Enabled)
	}
}

func TestCloneRuntimeConfigDeepCopiesEnabled(t *testing.T) {
	yes := true
	orig := &runtimeConfig{Enabled: &yes}
	clone := cloneRuntimeConfig(orig)
	if clone.Enabled == orig.Enabled {
		t.Fatal("clone must not alias the original Enabled pointer (hot-swap would share mutable storage)")
	}
	// Mutating the original's pointee must not bleed into the clone.
	*orig.Enabled = false
	if !clone.isEnabled() {
		t.Fatal("clone changed after mutating the original's *Enabled — aliasing bug")
	}
	// A nil switch clones to nil (still reports enabled by default).
	if cloneRuntimeConfig(&runtimeConfig{}).Enabled != nil {
		t.Fatal("clone of nil Enabled must stay nil")
	}
}

// TestPassthroughStrategyMethods locks the no-op contract of the strategy used in
// passthrough mode: identity body rewrite, header copy that preserves the client's
// credentials / Accept-Encoding and injects nothing, and a plain streamed response.
func TestPassthroughStrategyMethods(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"disabled"}}`)
	out, ctx, err := (passthroughStrategy{}).rewriteRequest(
		httptest.NewRequest(http.MethodPost, "/any/v1/messages", nil), "/v1/messages", raw)
	if err != nil {
		t.Fatalf("rewriteRequest returned error: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("rewriteRequest must forward the raw body unchanged, got %s", out)
	}
	if ctx.rewritten || ctx.classifier || ctx.stopSequences != nil || ctx.sessionID != "" {
		t.Fatalf("rewriteRequest must return an empty requestContext, got %+v", ctx)
	}

	src := http.Header{}
	src.Set("Authorization", "Bearer client-token")
	src.Set("X-Api-Key", "client-key")
	src.Set("Accept-Encoding", "gzip")
	src.Set("Connection", "keep-alive") // hop-by-hop: must be dropped by copyRequestHeaders
	dst := http.Header{}
	(passthroughStrategy{}).applyRequestHeaders(dst, src, requestContext{})
	if dst.Get("Authorization") != "Bearer client-token" {
		t.Fatalf("passthrough must forward the client Authorization, got %q", dst.Get("Authorization"))
	}
	if dst.Get("X-Api-Key") != "client-key" {
		t.Fatalf("passthrough must forward the client X-Api-Key, got %q", dst.Get("X-Api-Key"))
	}
	if dst.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("passthrough must not strip Accept-Encoding, got %q", dst.Get("Accept-Encoding"))
	}
	if dst.Get(sessionHeader) != "" {
		t.Fatalf("passthrough must not inject a session header, got %q", dst.Get(sessionHeader))
	}
	if dst.Get("Connection") != "" {
		t.Fatalf("copyRequestHeaders should drop hop-by-hop Connection, got %q", dst.Get("Connection"))
	}

	resp := &http.Response{
		StatusCode: http.StatusTeapot,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Test": []string{"v"}},
		Body:       io.NopCloser(strings.NewReader(`{"streamed":true}`)),
	}
	rec := httptest.NewRecorder()
	if err := (passthroughStrategy{}).writeResponse(rec, httptest.NewRequest(http.MethodPost, "/x", nil), resp, requestContext{}); err != nil {
		t.Fatalf("writeResponse returned error: %v", err)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("writeResponse status = %d, want streamed 418", rec.Code)
	}
	if rec.Body.String() != `{"streamed":true}` {
		t.Fatalf("writeResponse body = %s, want streamed verbatim", rec.Body.String())
	}
	if rec.Header().Get("X-Test") != "v" {
		t.Fatalf("writeResponse must copy upstream headers, X-Test = %q", rec.Header().Get("X-Test"))
	}
}

// TestPassthroughForwardsVerbatim is the disabled-mode core: with the switch off, all six
// request classes (normal/classifier × /any claude/gpt × /cpa) are forwarded
// byte-for-byte with NO cloak, NO thinking.disabled→adaptive, NO CPA session
// isolation, NO response reassembly, and NO Accept-Encoding strip; the client's
// own credentials reach the upstream unchanged (not replaced by a shim/account
// key), the AnyRouter account pool is never drawn (0 sessions), and the response
// is streamed back verbatim.
func TestPassthroughForwardsVerbatim(t *testing.T) {
	gptResp := cliproxyGPTStyleResponse("<block>no</block>")
	okResp := []byte(`{"ok":"upstream"}`)
	cases := []struct {
		name         string
		prefix       string
		strategy     proxyStrategy
		withAccounts bool
		body         []byte
		upstreamResp []byte
	}{
		// Non-classifier /any with thinking.disabled: enabled would promote it to
		// adaptive; passthrough must leave it disabled (byte-identical body).
		{"any-normal-thinking-disabled", anyRouterPrefix, anyRouterStrategy{}, true,
			[]byte(`{"model":"claude-opus-4-8","thinking":{"type":"disabled"},"messages":[]}`), okResp},
		// claude classifier /any: enabled would cloak (identity marker + delete thinking).
		{"any-classifier-claude", anyRouterPrefix, anyRouterStrategy{}, true,
			classifierBodyWithModel("claude-opus-4-8"), okResp},
		// gpt classifier /any: enabled would reassemble + strip Accept-Encoding.
		{"any-classifier-gpt", anyRouterPrefix, anyRouterStrategy{}, true,
			classifierBodyWithModel("gpt-5.5"), gptResp},
		{"cpa-normal", cliproxyPrefix, cliproxyStrategy{}, false,
			[]byte(`{"model":"gpt-5.5","messages":[]}`), okResp},
		// gpt classifier /cpa: enabled would isolate the session + reassemble.
		{"cpa-classifier-gpt", cliproxyPrefix, cliproxyStrategy{}, false,
			classifierBodyWithModel("gpt-5.5"), gptResp},
		{"cpa-classifier-claude", cliproxyPrefix, cliproxyStrategy{}, false,
			classifierBodyWithModel("claude-opus-4-8"), okResp},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			var gotAuth, gotAPIKey, gotAE, gotSession string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				gotAuth = r.Header.Get("Authorization")
				gotAPIKey = r.Header.Get("X-Api-Key")
				gotAE = r.Header.Get("Accept-Encoding")
				gotSession = r.Header.Get(sessionHeader)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(tc.upstreamResp)
			}))
			defer upstream.Close()

			route := proxyRoute{
				name:      "test",
				prefix:    tc.prefix,
				upstreams: newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
				// DisableCompression mirrors the real upstream clients so a (regressed)
				// Accept-Encoding strip would surface instead of being re-added.
				client:   &http.Client{Transport: &http.Transport{DisableCompression: true}},
				strategy: tc.strategy,
			}
			var pool *accountPool
			if tc.withAccounts {
				pool = newAccountPool([]accountEntry{{Label: "acct-1", Key: "sk-account"}})
				route.credentials = pool
			}
			proxy := httptest.NewServer(passthroughServer([]proxyRoute{route}))
			defer proxy.Close()

			req, err := http.NewRequest(http.MethodPost, proxy.URL+tc.prefix+"/v1/messages", bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer client-token")
			req.Header.Set("X-Api-Key", "client-key")
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy request failed: %v", err)
			}
			clientBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if !bytes.Equal(gotBody, tc.body) {
				t.Fatalf("upstream body changed (no rewrite allowed in passthrough):\n want %s\n got  %s", tc.body, gotBody)
			}
			if gotAuth != "Bearer client-token" {
				t.Fatalf("upstream Authorization = %q, want the client's Bearer client-token (not replaced)", gotAuth)
			}
			if gotAPIKey != "client-key" {
				t.Fatalf("upstream X-Api-Key = %q, want the client's client-key (not stripped)", gotAPIKey)
			}
			if gotAE != "gzip" {
				t.Fatalf("upstream Accept-Encoding = %q, want gzip preserved (not stripped)", gotAE)
			}
			if gotSession != "" {
				t.Fatalf("upstream session header = %q, want none (no isolation in passthrough)", gotSession)
			}
			if !bytes.Equal(clientBody, tc.upstreamResp) {
				t.Fatalf("client body changed (response must be streamed verbatim, not reassembled):\n want %s\n got  %s", tc.upstreamResp, clientBody)
			}
			if pool != nil {
				pool.mu.Lock()
				n := len(pool.sessions)
				pool.mu.Unlock()
				if n != 0 {
					t.Fatalf("account pool was drawn in passthrough: %d sessions, want 0", n)
				}
			}
		})
	}
}

// TestPassthroughKeepsAnyRouterFailover proves the /any double-entrance sticky
// failover survives passthrough mode: a 503 from the first entrance still fails
// over to the second, and the body that reaches it is the raw client body (a
// claude classifier is NOT cloaked).
func TestPassthroughKeepsAnyRouterFailover(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int64
	var gotBody []byte
	body := classifierBodyWithModel("claude-opus-4-8")

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		http.Error(w, `{"error":"primary down"}`, http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()

	route := proxyRoute{
		name:      "anyrouter",
		prefix:    anyRouterPrefix,
		upstreams: newUpstreamPool([]*url.URL{mustURL(t, primary.URL), mustURL(t, secondary.URL)}),
		client:    &http.Client{},
		strategy:  anyRouterStrategy{},
	}
	proxy := httptest.NewServer(passthroughServer([]proxyRoute{route}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 served by the second entrance", resp.StatusCode)
	}
	if primaryHits.Load() != 1 || secondaryHits.Load() != 1 {
		t.Fatalf("hits = primary %d secondary %d, want 1/1 (failover preserved)", primaryHits.Load(), secondaryHits.Load())
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("failover entrance received a rewritten body in passthrough:\n want %s\n got  %s", body, gotBody)
	}
	if bytes.Contains(gotBody, []byte(identityMarker)) {
		t.Fatalf("passthrough must not cloak the claude classifier on failover, got %s", gotBody)
	}
}

// TestAdminConfigPostTogglesEnable proves POST /admin/config can flip the global
// switch with no restart, and that the live behavior follows: enabled=false makes
// a claude classifier pass through uncloaked, enabled=true restores the cloak.
func TestAdminConfigPostTogglesEnable(t *testing.T) {
	var lastBody []byte
	anyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"any"}`))
	}))
	defer anyUpstream.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := baseAdminConfig()
	cfg.AnyRouter.Entrances = []string{anyUpstream.URL}
	server := newTestAdminServer(t, cfg, configPath)

	postEnabled := func(enabled bool) {
		t.Helper()
		c := *cfg
		c.Enabled = &enabled
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		rec := postAdminConfig(t, server, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST enabled=%v status = %d (%s)", enabled, rec.Code, rec.Body.String())
		}
		var resp struct {
			RestartRequired bool `json:"restart_required"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode POST response: %v", err)
		}
		if resp.RestartRequired {
			t.Fatalf("toggling enabled=%v must not require a restart", enabled)
		}
	}

	sendClaudeClassifier := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		req.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("classifier request status = %d (%s)", rec.Code, rec.Body.String())
		}
	}

	// Switch OFF → passthrough: the claude classifier reaches the upstream raw.
	postEnabled(false)
	sendClaudeClassifier()
	if bytes.Contains(lastBody, []byte(identityMarker)) {
		t.Fatalf("passthrough must not cloak the claude classifier, got %s", lastBody)
	}
	var off map[string]any
	if err := json.Unmarshal(lastBody, &off); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if th, _ := off["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("passthrough must keep thinking.disabled, got %v", off["thinking"])
	}

	// Switch ON → full functionality restored: the cloak is applied again.
	postEnabled(true)
	sendClaudeClassifier()
	if !bytes.Contains(lastBody, []byte(identityMarker)) {
		t.Fatalf("re-enabled shim must cloak the claude classifier on /any, got %s", lastBody)
	}
	var on map[string]any
	if err := json.Unmarshal(lastBody, &on); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if _, ok := on["thinking"]; ok {
		t.Fatalf("re-enabled shim must delete thinking.disabled, got %s", lastBody)
	}
}
