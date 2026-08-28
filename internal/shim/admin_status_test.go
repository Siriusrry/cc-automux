package shim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statusConfig builds a config whose AnyRouter route has two named entrances and
// two rotation accounts, so GET /admin/status has a populated entrance list and
// account snapshot to report.
func statusConfig() *runtimeConfig {
	cfg := baseAdminConfig()
	cfg.AnyRouter.Entrances = []string{"https://entrance-1.example", "https://entrance-2.example"}
	cfg.AnyRouter.Accounts = []accountEntry{{Label: "acct-1", Key: "sk-1"}, {Label: "acct-2", Key: "sk-2"}}
	return cfg
}

func getAdminStatus(t *testing.T, server *proxyServer) (*httptest.ResponseRecorder, adminStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/status status = %d (%s)", rec.Code, rec.Body.String())
	}
	var got adminStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /admin/status: %v", err)
	}
	return rec, got
}

// TestAdminStatusReturnsRuntimeSnapshot asserts the status code, content type,
// and the full /admin/status JSON contract (top-level keys + nested entrance and
// account keys with the right Go types), so a future rename of any field breaks
// this test rather than silently breaking the dynamic admin UI UI wiring.
func TestAdminStatusReturnsRuntimeSnapshot(t *testing.T) {
	server := newTestAdminServer(t, statusConfig(), "")

	rec, got := getAdminStatus(t, server)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	// Top-level wire keys present (the names dynamic admin UI reads).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw status: %v", err)
	}
	for _, k := range []string{"product", "version", "uptime_seconds", "start_time", "rewrites", "entrances", "accounts"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("status JSON missing top-level key %q", k)
		}
	}

	// Nested wire keys present on the first entrance and account.
	var nested struct {
		Entrances []map[string]json.RawMessage `json:"entrances"`
		Accounts  []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nested); err != nil {
		t.Fatalf("decode nested status: %v", err)
	}
	if len(nested.Entrances) != 2 {
		t.Fatalf("entrances len = %d, want 2", len(nested.Entrances))
	}
	for _, k := range []string{"host", "url", "active"} {
		if _, ok := nested.Entrances[0][k]; !ok {
			t.Fatalf("entrance JSON missing key %q", k)
		}
	}
	if len(nested.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2", len(nested.Accounts))
	}
	for _, k := range []string{"label", "fail_rate", "samples", "sessions", "deprioritized", "cooldown"} {
		if _, ok := nested.Accounts[0][k]; !ok {
			t.Fatalf("account JSON missing key %q", k)
		}
	}

	// Typed values.
	if got.Product != "CC AutoMux" || got.Version != "v1.0.0-dev" {
		t.Fatalf("product/version = %q/%q, want CC AutoMux/v1.0.0-dev", got.Product, got.Version)
	}
	if got.UptimeSeconds < 0 {
		t.Fatalf("uptime_seconds = %d, want >= 0", got.UptimeSeconds)
	}
	if _, err := time.Parse(time.RFC3339, got.StartTime); err != nil {
		t.Fatalf("start_time %q not RFC3339: %v", got.StartTime, err)
	}
	if got.Rewrites != 0 {
		t.Fatalf("rewrites = %d on a fresh server, want 0", got.Rewrites)
	}
	if got.Entrances[0].Host != "entrance-1.example" || got.Entrances[0].URL != "https://entrance-1.example" {
		t.Fatalf("entrances[0] = %+v, want host/url for entrance-1", got.Entrances[0])
	}
	if !got.Entrances[0].Active || got.Entrances[1].Active {
		t.Fatalf("entrance active flags = [%v %v], want first active", got.Entrances[0].Active, got.Entrances[1].Active)
	}
	if got.Accounts[0].Label != "acct-1" {
		t.Fatalf("accounts[0].Label = %q, want acct-1", got.Accounts[0].Label)
	}
	// A fresh account is neutral: no traffic, no health signal.
	if a := got.Accounts[0]; a.Deprioritized || a.Cooldown || a.Samples != 0 || a.Sessions != 0 || a.FailRate != 0 {
		t.Fatalf("accounts[0] not neutral on a fresh server: %+v", a)
	}
}

// TestAdminStatusEmptySlicesAreJSONArrays guards the null/[] wire contract (a
// recurring bug class in this project): the status snapshot's slice fields must
// serialize as [] when empty, never null, so the dynamic admin UI UI always receives arrays
// it can iterate. Covers the empty-accounts case via the normal config path, and
// the degenerate no-AnyRouter-route snapshot for both slices, which would
// regress to null if the explicit initializers in statusSnapshot were removed.
func TestAdminStatusEmptySlicesAreJSONArrays(t *testing.T) {
	// Empty-accounts case via the normal config path (baseAdminConfig has one
	// entrance and zero rotation accounts).
	server := newTestAdminServer(t, baseAdminConfig(), "")
	rec, _ := getAdminStatus(t, server)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got := string(raw["accounts"]); got != "[]" {
		t.Fatalf("accounts JSON = %s, want [] with no accounts configured (must not be null)", got)
	}

	// Degenerate snapshot with no AnyRouter route at all: both entrances and
	// accounts must still serialize as [] (the explicit initializers), never null.
	emptyTableServer := newProxyServer(appConfig{table: &routingTable{}})
	payload, err := json.Marshal(emptyTableServer.statusSnapshot())
	if err != nil {
		t.Fatalf("marshal empty-table status: %v", err)
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode empty-table status: %v", err)
	}
	if got := string(raw["entrances"]); got != "[]" {
		t.Fatalf("entrances JSON = %s, want [] with no AnyRouter entrances (must not be null)", got)
	}
	if got := string(raw["accounts"]); got != "[]" {
		t.Fatalf("accounts JSON = %s, want [] with no AnyRouter route (must not be null)", got)
	}
}

// TestAdminStatusRewriteCounterIncrements proves the rewrites counter is scoped
// to CLASSIFIER rewrites only. A non-classifier /any request carrying
// thinking.disabled IS rewritten (promoted to adaptive, ctx.rewritten=true) but
// is not a classifier fix (ctx.classifier=false), so any number of them must
// leave the counter at 0; a claude-family classifier (cloak rewrite,
// ctx.classifier=true) then takes it 0 → 1. This pins the classifier-only
// semantics against a regression back to counting every rewrite.
func TestAdminStatusRewriteCounterIncrements(t *testing.T) {
	var lastUpstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUpstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := baseAdminConfig()
	cfg.AnyRouter.Entrances = []string{upstream.URL}
	server := newTestAdminServer(t, cfg, "")

	send := func(body []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/any/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request status = %d (%s)", rec.Code, rec.Body.String())
		}
	}

	if _, got := getAdminStatus(t, server); got.Rewrites != 0 {
		t.Fatalf("rewrites before any request = %d, want 0", got.Rewrites)
	}

	// Several non-classifier /any requests with thinking.disabled: each is rewritten
	// by the thinking.disabled→adaptive promotion, but none is a classifier fix, so
	// the counter must stay 0.
	nonClassifierThinking := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 3; i++ {
		send(nonClassifierThinking)
	}
	// Confirm these requests really were rewritten (promoted), so the assertion
	// below proves the counter excludes a genuine rewrite — not merely that an
	// untouched passthrough does not count.
	if !bytes.Contains(lastUpstreamBody, []byte(`"adaptive"`)) || bytes.Contains(lastUpstreamBody, []byte(`"disabled"`)) {
		t.Fatalf("non-classifier request was not promoted to thinking.adaptive upstream: %s", lastUpstreamBody)
	}
	if _, got := getAdminStatus(t, server); got.Rewrites != 0 {
		t.Fatalf("rewrites after thinking.disabled→adaptive promotions = %d, want 0 (counter is classifier-only)", got.Rewrites)
	}

	// A claude-family classifier on /any is cloak-rewritten (ctx.classifier=true): counts.
	send(classifierBodyWithModel("claude-opus-4-8"))
	if _, got := getAdminStatus(t, server); got.Rewrites != 1 {
		t.Fatalf("rewrites after one classifier rewrite = %d, want 1", got.Rewrites)
	}
}

// TestAdminStatusReflectsActiveEntrance proves the active-entrance flag tracks
// the live upstream pool pointer: moving it (as failover does) flips which
// entrance /admin/status marks active.
func TestAdminStatusReflectsActiveEntrance(t *testing.T) {
	server := newTestAdminServer(t, statusConfig(), "")

	if _, got := getAdminStatus(t, server); !got.Entrances[0].Active || got.Entrances[1].Active {
		t.Fatalf("initial active flags = [%v %v], want first active", got.Entrances[0].Active, got.Entrances[1].Active)
	}

	// Simulate failover by promoting the live pool's second entrance to current.
	if !server.currentRoutingTable().routes[0].upstreams.promote(1) {
		t.Fatal("promote(1) reported no change")
	}

	if _, got := getAdminStatus(t, server); got.Entrances[0].Active || !got.Entrances[1].Active {
		t.Fatalf("post-failover active flags = [%v %v], want second active", got.Entrances[0].Active, got.Entrances[1].Active)
	}
}

func TestAdminStatusRejectsNonGet(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/status status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("POST /admin/status Allow = %q, want %q", allow, http.MethodGet)
	}
}
