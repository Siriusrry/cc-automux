package shim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestAnyRouterAccountRotationMultiSessionEndToEnd(t *testing.T) {
	type hit struct {
		session string
		auth    string
		apiKey  string
	}
	var mu sync.Mutex
	var hits []hit
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, hit{
			session: r.Header.Get(sessionHeader),
			auth:    r.Header.Get("Authorization"),
			apiKey:  r.Header.Get("X-Api-Key"),
		})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	const accounts = 3
	pool := make([]accountEntry, 0, accounts)
	for i := 0; i < accounts; i++ {
		pool = append(pool, accountEntry{Label: "acct-" + strconv.Itoa(i), Key: "key-" + strconv.Itoa(i)})
	}
	server := testProxyServerFromRoutes([]proxyRoute{{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{upstreamURL}),
		credentials: newAccountPool(pool),
		client:      upstream.Client(),
		strategy:    anyRouterStrategy{},
	}})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// A non-classifier body: no security-monitor system prefix, so it is never
	// detected as a classifier and routes by prefix to AnyRouter unchanged.
	doRequest := func(session string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(sessionHeader, session)
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request for %s failed: %v", session, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Six distinct new sessions, one request each. With a round-robin cursor
	// starting at 0 over three healthy accounts, they distribute key-0,1,2,0,1,2.
	for i := 0; i < 6; i++ {
		doRequest("session-" + strconv.Itoa(i))
	}
	// Repeat one earlier session: it must stick to the account it was first
	// pinned to (session-0 → key-0).
	doRequest("session-0")

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 7 {
		t.Fatalf("upstream saw %d requests, want 7", len(hits))
	}

	sessionKey := map[string]string{}
	keyCounts := map[string]int{}
	for _, h := range hits {
		// (3) Per-request auth override: client credentials always replaced.
		if h.apiKey != "" {
			t.Fatalf("session %s: upstream X-Api-Key = %q, want stripped", h.session, h.apiKey)
		}
		if !strings.HasPrefix(h.auth, "Bearer key-") {
			t.Fatalf("session %s: upstream Authorization = %q, want a shim account key (Bearer key-*)", h.session, h.auth)
		}
		if prev, ok := sessionKey[h.session]; ok {
			// (2) Sticky: a repeated session keeps the same account key.
			if prev != h.auth {
				t.Fatalf("session %s not sticky: saw %q then %q", h.session, prev, h.auth)
			}
		} else {
			sessionKey[h.session] = h.auth
		}
	}
	// (1) Round-robin spread: the six distinct sessions covered all 3 accounts.
	for s, k := range sessionKey {
		keyCounts[k]++
		_ = s
	}
	if len(keyCounts) != accounts {
		t.Fatalf("distinct sessions spread over %d accounts, want all %d: %v", len(keyCounts), accounts, keyCounts)
	}
	for k, c := range keyCounts {
		if c != 2 {
			t.Fatalf("account %s pinned %d sessions, want balanced 2: %v", k, c, keyCounts)
		}
	}
	// Sticky was actually exercised (session-0 appeared twice).
	if sessionKey["session-0"] != "Bearer key-0" {
		t.Fatalf("session-0 pinned to %q, want Bearer key-0", sessionKey["session-0"])
	}
}
func TestAnyRouterAccountAuthOverrideEndToEnd(t *testing.T) {
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	server := testProxyServerFromRoutes([]proxyRoute{{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{upstreamURL}),
		credentials: newAccountPool([]accountEntry{{Label: "acct-1", Key: "shim-account-key"}}),
		client:      upstream.Client(),
		strategy:    anyRouterStrategy{},
	}})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if gotAuth != "Bearer shim-account-key" {
		t.Fatalf("upstream Authorization = %q, want Bearer shim-account-key", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
	}
}

// TestAccountPoolSnapshotReportsRelativeHealth proves accountPool.snapshot surfaces the
// relative-health model, not the retired absolute cooling:
// a persistently 429'd account is soft-deprioritized (with a positive EWMA rate
// and its sticky session still counted) but NOT in cooldown; only a 401/403 sets
// cooldown.
