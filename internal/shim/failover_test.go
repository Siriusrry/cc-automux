package shim

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const failoverClassifierBody = `{"max_tokens":64,"thinking":{"type":"disabled"},"stop_sequences":["</block>"],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[]}`

func testProxyServerWithUpstreams(t *testing.T, prefix string, strategy proxyStrategy, upstreamURLs ...string) *proxyServer {
	t.Helper()
	parsed := make([]*url.URL, 0, len(upstreamURLs))
	for _, raw := range upstreamURLs {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("failed to parse upstream URL %q: %v", raw, err)
		}
		parsed = append(parsed, u)
	}
	return testProxyServerFromRoutes([]proxyRoute{{
		name:      "test",
		prefix:    prefix,
		upstreams: newUpstreamPool(parsed),
		client:    &http.Client{},
		strategy:  strategy,
	}})
}

func TestAnyRouterFailoverOn503ServesRewrittenBodyFromSecondEntrance(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int64
	var secondaryHost atomic.Value

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		http.Error(w, `{"error":"primary down"}`, http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		if want, _ := secondaryHost.Load().(string); want != "" && r.Host != want {
			t.Errorf("secondary received Host %q, want %q", r.Host, want)
		}
		if r.URL.RequestURI() != "/v1/messages?beta=true" {
			t.Errorf("unexpected upstream path: %s", r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read upstream body: %v", err)
		}
		if !bytes.Contains(body, []byte(identityMarker)) {
			t.Errorf("failover attempt lost the classifier rewrite: %s", body)
		}
		if bytes.Contains(body, []byte(`"thinking"`)) {
			t.Errorf("failover attempt resent thinking.disabled: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()
	secondaryHost.Store(strings.TrimPrefix(secondary.URL, "http://"))

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, primary.URL, secondary.URL))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages?beta=true", "application/json", strings.NewReader(failoverClassifierBody))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 served by secondary", resp.StatusCode)
	}
	if primaryHits.Load() != 1 || secondaryHits.Load() != 1 {
		t.Fatalf("hits = primary %d secondary %d, want 1/1", primaryHits.Load(), secondaryHits.Load())
	}
}

func TestAnyRouterFailoverOn429(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"rate_limit_error"}}`, http.StatusTooManyRequests)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, primary.URL, secondary.URL))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 429 failover", resp.StatusCode)
	}
}

// TestFailoverIsNotBlockedByStalledErrorBody proves an entrance that sends a
// 503 header and then stalls its body does
// not block the switch to the next entrance.
func TestFailoverIsNotBlockedByStalledErrorBody(t *testing.T) {
	unblock := make(chan struct{})
	var aHits, bHits atomic.Int64

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"partial`))
		w.(http.Flusher).Flush()
		<-unblock // stall the rest of the error body until the test ends
	}))
	defer a.Close()
	defer close(unblock) // LIFO: releases the stalled handler before a.Close waits on it

	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("failover must not be blocked by a stalled 503 body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the second entrance", resp.StatusCode)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("hits = A %d B %d, want 1/1", aHits.Load(), bHits.Load())
	}
}

func TestAnyRouterFailoverOnConnectionRefused(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // port is now refused

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, deadURL, secondary.URL))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after connection-refused failover", resp.StatusCode)
	}
}

// TestFailoverIsStickyUntilCurrentEntranceFails covers the lifecycle: failover
// to B, stay on B even after A recovers, fall back to A only when B fails,
// then stay on A.
func TestFailoverIsStickyUntilCurrentEntranceFails(t *testing.T) {
	var aFail, bFail atomic.Bool
	var aHits, bHits atomic.Int64

	serve := func(fail *atomic.Bool, hits *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if fail.Load() {
				http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}

	a := httptest.NewServer(serve(&aFail, &aHits))
	defer a.Close()
	b := httptest.NewServer(serve(&bFail, &bHits))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	post := func() int {
		t.Helper()
		resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Phase 1: A is down; the request fails over to B, which becomes current.
	aFail.Store(true)
	if got := post(); got != http.StatusOK {
		t.Fatalf("phase 1 status = %d, want 200", got)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("phase 1 hits = A %d B %d, want 1/1", aHits.Load(), bHits.Load())
	}

	// Phase 2: A recovers, but B keeps working — no switchback, A gets no traffic.
	aFail.Store(false)
	for i := 0; i < 3; i++ {
		if got := post(); got != http.StatusOK {
			t.Fatalf("phase 2 status = %d, want 200", got)
		}
	}
	if aHits.Load() != 1 || bHits.Load() != 4 {
		t.Fatalf("phase 2 hits = A %d B %d, want sticky 1/4", aHits.Load(), bHits.Load())
	}

	// Phase 3: B fails; the request falls back to A, which becomes current.
	bFail.Store(true)
	if got := post(); got != http.StatusOK {
		t.Fatalf("phase 3 status = %d, want 200", got)
	}
	if aHits.Load() != 2 || bHits.Load() != 5 {
		t.Fatalf("phase 3 hits = A %d B %d, want 2/5", aHits.Load(), bHits.Load())
	}

	// Phase 4: A is current; B (still failing) gets no traffic.
	if got := post(); got != http.StatusOK {
		t.Fatalf("phase 4 status = %d, want 200", got)
	}
	if aHits.Load() != 3 || bHits.Load() != 5 {
		t.Fatalf("phase 4 hits = A %d B %d, want 3/5", aHits.Load(), bHits.Load())
	}
}

func TestAllEntrancesFailReturnsLastRealResponse(t *testing.T) {
	var aHits, bHits atomic.Int64

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		http.Error(w, `{"error":"a-down"}`, http.StatusServiceUnavailable)
	}))
	defer a.Close()

	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "13")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"b-rate-limited"}`))
	}))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the last entrance's real 429", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("b-rate-limited")) {
		t.Fatalf("body = %s, want the last entrance's real error body", body)
	}
	if resp.Header.Get("Retry-After") != "13" {
		t.Fatalf("Retry-After = %q, want upstream header preserved", resp.Header.Get("Retry-After"))
	}

	// Both entrances failed, so the current pointer must not move: the next
	// request tries A first again.
	resp2, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("second proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if aHits.Load() != 2 || bHits.Load() != 2 {
		t.Fatalf("hits = A %d B %d, want 2/2 (pointer unchanged while all fail)", aHits.Load(), bHits.Load())
	}
}

func TestNon429ClientErrorPassesThroughWithoutFailover(t *testing.T) {
	var bHits atomic.Int64

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	}))
	defer a.Close()

	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("invalid_request")) {
		t.Fatalf("body = %s, want upstream 400 body preserved", body)
	}
	if bHits.Load() != 0 {
		t.Fatalf("secondary hits = %d, want 0 (4xx must not fail over)", bHits.Load())
	}
}

func TestMidStreamUpstreamFailureSwitchesNextRequest(t *testing.T) {
	var aHits, bHits atomic.Int64

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-stream
	}))
	defer a.Close()

	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	// First request: headers already went out, so the shim cannot retry; the
	// client sees the broken stream.
	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed before body read: %v", err)
	}
	_, _ = io.ReadAll(resp.Body) // read error or truncated body — both acceptable
	resp.Body.Close()
	if aHits.Load() != 1 || bHits.Load() != 0 {
		t.Fatalf("hits after broken stream = A %d B %d, want 1/0 (no in-request retry)", aHits.Load(), bHits.Load())
	}

	// Second request (the client's retry): must go straight to B.
	resp2, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 from the other entrance", resp2.StatusCode)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("hits after retry = A %d B %d, want 1/1 (pointer moved to B)", aHits.Load(), bHits.Load())
	}
}

func TestClientCancelDoesNotFailOverOrMovePointer(t *testing.T) {
	var aHits, bHits atomic.Int64

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aHits.Add(1) == 1 {
			// Drain the body so the server's background read can detect the
			// canceled shim connection and fire this request's context.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done() // hang until the canceled client tears the request down
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer a.Close()

	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer b.Close()

	proxy := httptest.NewServer(testProxyServerWithUpstreams(t, anyRouterPrefix, anyRouterStrategy{}, a.URL, b.URL))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("canceled request should fail on the client side")
	}

	// The cancellation must not have been treated as an entrance failure.
	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("follow-up request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200 from A", resp.StatusCode)
	}
	if aHits.Load() != 2 || bHits.Load() != 0 {
		t.Fatalf("hits = A %d B %d, want 2/0 (cancel must not switch entrances)", aHits.Load(), bHits.Load())
	}
}

// TestCliproxySingleUpstreamErrorsPassThrough guards the red line: the /cpa
// route has exactly one upstream, so the failover loop must degenerate to the
// previous behavior — errors pass through, transport failures return 502.
func TestCliproxySingleUpstreamErrorsPassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"cliproxy down"}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, cliproxyPrefix, upstream, cliproxyStrategy{}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/cpa/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 passed through unchanged", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("cliproxy down")) {
		t.Fatalf("body = %s, want upstream error body preserved", body)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	proxy2 := httptest.NewServer(testProxyServerWithUpstreams(t, cliproxyPrefix, cliproxyStrategy{}, deadURL))
	defer proxy2.Close()

	resp2, err := http.Post(proxy2.URL+"/cpa/v1/messages", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for single-upstream transport failure", resp2.StatusCode)
	}
}

func TestParseUpstreamEntries(t *testing.T) {
	urls, err := parseUpstreamEntries([]string{" https://anyrouter.top ", " https://a-ocnfniawgw.cn-shanghai.fcapp.run ", ""})
	if err != nil {
		t.Fatalf("parseUpstreamEntries returned error: %v", err)
	}
	if len(urls) != 2 || urls[0].Host != "anyrouter.top" || urls[1].Host != "a-ocnfniawgw.cn-shanghai.fcapp.run" {
		t.Fatalf("unexpected list: %v", urls)
	}

	single, err := parseUpstreamEntries([]string{"https://anyrouter.top"})
	if err != nil || len(single) != 1 {
		t.Fatalf("single URL must stay valid, got %v / %v", single, err)
	}

	for _, bad := range [][]string{
		{},
		{" ", ""},
		{"ftp://example.com"},
		{"https://anyrouter.top", "https://anyrouter.top"},
		{"https://anyrouter.top", "not a url://"},
	} {
		if _, err := parseUpstreamEntries(bad); err == nil {
			t.Fatalf("parseUpstreamEntries(%q) should fail", bad)
		}
	}
}

func TestLoadConfigDefaultsAnyRouterUpstreams(t *testing.T) {
	t.Setenv(configPathEnv, filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("CC_AUTO_SHIM_LISTEN", "")
	t.Setenv("CC_ANYROUTER_SHIM_UPSTREAM", "")
	t.Setenv("CC_CLIPROXY_SHIM_UPSTREAM", "")
	t.Setenv("CC_CLIPROXY_SHIM_CA", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if len(cfg.runtime.AnyRouter.Entrances) != 2 {
		t.Fatalf("default upstream count = %d, want 2", len(cfg.runtime.AnyRouter.Entrances))
	}
	if cfg.runtime.AnyRouter.Entrances[0] != "https://anyrouter.top" {
		t.Fatalf("first default upstream = %s, want https://anyrouter.top", cfg.runtime.AnyRouter.Entrances[0])
	}
	if cfg.runtime.AnyRouter.Entrances[1] != "https://a-ocnfniawgw.cn-shanghai.fcapp.run" {
		t.Fatalf("second default upstream = %s, want the fcapp.run entrance", cfg.runtime.AnyRouter.Entrances[1])
	}
}
