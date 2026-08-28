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
	"testing"
)

func TestAdminConfigGetReflectsLiveConfig(t *testing.T) {
	cfg := baseAdminConfig()
	cfg.AnyRouter.Entrances = []string{"https://live-1.example", "https://live-2.example"}
	cfg.Classifier.ModelOverride = "claude-test"
	server := newTestAdminServer(t, cfg, "")

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config status = %d, want 200", rec.Code)
	}
	var got runtimeConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /admin/config: %v", err)
	}
	if got.ListenAddr != "127.0.0.1:8765" {
		t.Fatalf("listen_addr = %q", got.ListenAddr)
	}
	if len(got.AnyRouter.Entrances) != 2 || got.AnyRouter.Entrances[0] != "https://live-1.example" {
		t.Fatalf("entrances = %#v", got.AnyRouter.Entrances)
	}
	if got.Classifier.ModelOverride != "claude-test" {
		t.Fatalf("model_override = %q", got.Classifier.ModelOverride)
	}
}
func TestAdminConfigPostAppliesAndReportsRestart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	// Unchanged listen_addr: applies without requiring a restart.
	rec := postAdminConfig(t, server, `{
		"listen_addr": "127.0.0.1:8765",
		"anyrouter": {"entrances": ["https://changed.example"], "accounts": [{"label": "acct-1", "key": "sk-x"}]},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if resp.RestartRequired {
		t.Fatal("same listen_addr should not require restart")
	}
	if got := server.currentState().runtime.AnyRouter.Entrances; len(got) != 1 || got[0] != "https://changed.example" {
		t.Fatalf("live entrances = %#v, want changed.example", got)
	}
	if got := server.currentRoutingTable().routes[0].upstreams.url(0).String(); got != "https://changed.example" {
		t.Fatalf("live routing upstream = %q, want changed.example", got)
	}

	// Changed port (listen_addr): applies but reports restart required.
	rec = postAdminConfig(t, server, `{
		"listen_addr": "127.0.0.1:8766",
		"anyrouter": {"entrances": ["https://changed.example"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST (port change) status = %d (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if !resp.RestartRequired {
		t.Fatal("changed listen_addr should require restart")
	}
	if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8766" {
		t.Fatalf("live listen_addr = %q, want 127.0.0.1:8766", got)
	}

	// Disk reflects the last applied config.
	persisted, err := readRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if persisted.ListenAddr != "127.0.0.1:8766" {
		t.Fatalf("persisted listen_addr = %q", persisted.ListenAddr)
	}
}

// TestAdminConfigPostTogglesClassifierTarget proves POST /admin/config can set
// and clear the global classifier target (classifier.target_base_url), that the
// live routing table's classifierTarget is built/torn-down to match, and that
// detected classifier requests reroute to the target when set and fall back to
// the prefix upstream when cleared. (Prior admin tests covered only entrances /
// listen_addr changes.)
func TestAdminConfigPostTogglesClassifierTarget(t *testing.T) {
	var anyHits, targetHits int
	anyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anyHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"any"}`))
	}))
	defer anyUpstream.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"target"}`))
	}))
	defer target.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := baseAdminConfig()
	// Point the AnyRouter prefix at a reachable test server so the fallback path
	// (no global target) can actually be served.
	cfg.AnyRouter.Entrances = []string{anyUpstream.URL}
	server := newTestAdminServer(t, cfg, configPath)

	postCfg := func(classifierTarget string) {
		t.Helper()
		c := *cfg
		c.Classifier = classifierRuntimeConfig{TargetBaseURL: classifierTarget}
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		rec := postAdminConfig(t, server, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST (target=%q) status = %d (%s)", classifierTarget, rec.Code, rec.Body.String())
		}
	}

	// sendClassifier drives a detected claude-family classifier request through
	// the full ServeHTTP path and returns the client-visible body.
	sendClassifier := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		req.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("classifier request status = %d (%s)", rec.Code, rec.Body.String())
		}
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}

	// Initially no global target: live table has none, classifier falls back to prefix.
	if server.currentRoutingTable().classifierTarget != nil {
		t.Fatal("classifierTarget should be nil before any target is configured")
	}
	if got := sendClassifier(); got != `{"ok":"any"}` {
		t.Fatalf("fallback classifier body = %s, want the AnyRouter prefix upstream's body", got)
	}
	if anyHits != 1 || targetHits != 0 {
		t.Fatalf("after fallback: anyHits=%d targetHits=%d, want 1/0", anyHits, targetHits)
	}

	// Set the global target: live table builds it; classifier reroutes there.
	postCfg(target.URL)
	ct := server.currentRoutingTable().classifierTarget
	if ct == nil {
		t.Fatal("classifierTarget not built after POST set target_base_url")
	}
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}
	if got := ct.url(0).Host; got != targetURL.Host {
		t.Fatalf("classifierTarget host = %q, want %q", got, targetURL.Host)
	}
	if got := sendClassifier(); got != `{"ok":"target"}` {
		t.Fatalf("rerouted classifier body = %s, want the global target's body", got)
	}
	if anyHits != 1 || targetHits != 1 {
		t.Fatalf("after reroute: anyHits=%d targetHits=%d, want 1/1 (prefix upstream untouched)", anyHits, targetHits)
	}

	// Clear the global target: live table tears it down; classifier falls back again.
	postCfg("")
	if server.currentRoutingTable().classifierTarget != nil {
		t.Fatal("classifierTarget not cleared after POST empty target_base_url")
	}
	if got := sendClassifier(); got != `{"ok":"any"}` {
		t.Fatalf("post-clear classifier body = %s, want the AnyRouter prefix upstream's body", got)
	}
	if anyHits != 2 || targetHits != 1 {
		t.Fatalf("after clear: anyHits=%d targetHits=%d, want 2/1", anyHits, targetHits)
	}
}

// TestAdminConfigPostRoundTripsLogMaxBytes proves the admin log-cap round-trip round-trip closes the
// cross-stage [major] that landed with persisted-config authority: a desk Save must carry log_max_bytes so
// a non-default cap is preserved, instead of the field being omitted (decoded to 0,
// then reset to the 100 MiB default by normalizeRuntimeConfig). It drives the real
// POST handler with bodies that include the cap (the post-admin log-cap round-trip desk shape) and one
// that omits it (the pre-admin log-cap round-trip desk shape, kept here to pin exactly the regression),
// and asserts the embedded admin.html actually wires the field through
// collect()/render().
func TestAdminConfigPostRoundTripsLogMaxBytes(t *testing.T) {
	const (
		boundCap   = int64(7 << 20) // 7 MiB: a non-default cap (the default is 100 MiB)
		changedCap = int64(9 << 20) // 9 MiB
	)

	configPath := filepath.Join(t.TempDir(), "config.json")
	// newProxyServer records boundLogMaxBytes = 7 MiB, so restart_required is measured
	// against 7 MiB for the life of this server.
	server := newTestAdminServer(t, logCapConfig(boundCap), configPath)

	post := func(body string) bool {
		t.Helper()
		rec := postAdminConfig(t, server, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST status = %d (%s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			RestartRequired bool `json:"restart_required"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode POST response: %v", err)
		}
		return resp.RestartRequired
	}
	getCap := func() int64 {
		t.Helper()
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/config status = %d", rec.Code)
		}
		var got runtimeConfig
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode /admin/config: %v", err)
		}
		return got.LogMaxBytes
	}
	diskCap := func() int64 {
		t.Helper()
		persisted, err := readRuntimeConfig(configPath)
		if err != nil {
			t.Fatalf("read persisted config: %v", err)
		}
		return persisted.LogMaxBytes
	}
	// bodyWithCap builds a valid POST body, splicing in an optional log_max_bytes
	// member. An empty cap reproduces the pre-admin log-cap round-trip desk body that omits the field.
	bodyWithCap := func(cap string) string {
		return `{
			"listen_addr": "127.0.0.1:8765",` + cap + `
			"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
			"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
			"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
		}`
	}

	// (1) A POST carrying the unchanged bound cap preserves it and needs no restart —
	// the post-admin log-cap round-trip desk behavior on a plain Save of some other field.
	if restart := post(bodyWithCap(`"log_max_bytes": 7340032,`)); restart {
		t.Fatal("carrying the unchanged bound cap must not require restart")
	}
	if got := getCap(); got != boundCap {
		t.Fatalf("after carrying cap: GET log_max_bytes = %d, want preserved %d", got, boundCap)
	}
	if got := diskCap(); got != boundCap {
		t.Fatalf("after carrying cap: disk log_max_bytes = %d, want %d", got, boundCap)
	}

	// (2) A POST carrying a CHANGED cap persists it (GET + disk) and reports restart
	// required (9 MiB differs from the bound 7 MiB).
	if restart := post(bodyWithCap(`"log_max_bytes": 9437184,`)); !restart {
		t.Fatal("changing the cap away from the bound value should require restart")
	}
	if got := getCap(); got != changedCap {
		t.Fatalf("after changing cap: GET log_max_bytes = %d, want %d", got, changedCap)
	}
	if got := diskCap(); got != changedCap {
		t.Fatalf("after changing cap: disk log_max_bytes = %d, want %d", got, changedCap)
	}

	// (3) The major this closes: a POST that OMITS log_max_bytes (the pre-admin log-cap round-trip desk
	// body) decodes the field to 0, which normalizeRuntimeConfig maps to the 100 MiB
	// default — silently resetting the cap and spuriously reporting restart_required.
	// This is exactly why collect() must send the field.
	if restart := post(bodyWithCap(``)); !restart {
		t.Fatal("omitting log_max_bytes resets the cap away from the bound value; should require restart")
	}
	if got := getCap(); got != defaultMaxLogBytes {
		t.Fatalf("omitting log_max_bytes should reset to the default (the major admin log-cap round-trip closes); GET = %d, want %d", got, defaultMaxLogBytes)
	}

	// (4) The desk must actually carry the field. The page exposes the input and
	// restart hint, while admin.js reads it and adds log_max_bytes to the POST body.
	// Checking both assets guards the wiring from silent removal after asset splits.
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	htmlBody := rec.Body.String()
	for _, marker := range []string{"logMaxMb", "Max log size", "log size"} {
		if !strings.Contains(htmlBody, marker) {
			t.Fatalf("admin.html missing %q — the desk no longer round-trips the log cap", marker)
		}
	}

	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/assets/admin.js", nil))
	jsBody := rec.Body.String()
	for _, marker := range []string{"log_max_bytes:", "logMaxMb"} {
		if !strings.Contains(jsBody, marker) {
			t.Fatalf("admin.js missing %q — the desk no longer round-trips the log cap", marker)
		}
	}
}

func TestAdminConfigPostInvalidRejectedWithoutApplying(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	rec := postAdminConfig(t, server, `{
		"listen_addr": "0.0.0.0:8765",
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("invalid POST status = %d, want 4xx", rec.Code)
	}
	if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8765" {
		t.Fatalf("live listen_addr = %q, want unchanged 127.0.0.1:8765", got)
	}
	if _, err := readRuntimeConfig(configPath); err == nil {
		t.Fatal("invalid POST must not write the config file")
	}
}

func TestAdminConfigPostUnknownFieldRejected(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), filepath.Join(t.TempDir(), "config.json"))
	rec := postAdminConfig(t, server, `{"listen_addr": "127.0.0.1:8765", "bogus": true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field POST status = %d, want 400", rec.Code)
	}
}

func TestAdminConfigPostRejectsBadPortWithoutApplying(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:", "127.0.0.1:abc", "127.0.0.1:70000", "127.0.0.1:0", "127.0.0.1:+8080", "127.0.0.1:08081"} {
		configPath := filepath.Join(t.TempDir(), "config.json")
		server := newTestAdminServer(t, baseAdminConfig(), configPath)
		rec := postAdminConfig(t, server, `{
			"listen_addr": "`+listen+`",
			"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
			"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
			"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
		}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST listen_addr %q status = %d, want 400", listen, rec.Code)
		}
		if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8765" {
			t.Fatalf("POST listen_addr %q left live = %q, want unchanged", listen, got)
		}
		if _, err := readRuntimeConfig(configPath); err == nil {
			t.Fatalf("POST listen_addr %q must not write the config file", listen)
		}
	}
}
func TestAdminConfigRejectsUnsupportedMethod(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/admin/config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /admin/config status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet+", "+http.MethodPost {
		t.Fatalf("PUT /admin/config Allow = %q, want %q", allow, http.MethodGet+", "+http.MethodPost)
	}
}

func TestAdminConfigPostOversizedBodyRejectedWithoutApplying(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	// A body larger than maxAdminConfigBytes must trip MaxBytesReader and be
	// rejected before anything is applied or persisted.
	body := `{"listen_addr":"127.0.0.1:` + strings.Repeat("9", int(maxAdminConfigBytes)+16) + `"}`
	rec := postAdminConfig(t, server, body)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("oversized POST status = %d, want 4xx", rec.Code)
	}
	if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8765" {
		t.Fatalf("live listen_addr = %q, want unchanged", got)
	}
	if _, err := readRuntimeConfig(configPath); err == nil {
		t.Fatal("oversized POST must not write the config file")
	}
}

func TestAdminConfigPostTrailingJSONRejectedWithoutApplying(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	rec := postAdminConfig(t, server, `{
		"listen_addr": "127.0.0.1:8799",
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}{"junk":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing-JSON POST status = %d, want 400", rec.Code)
	}
	if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8765" {
		t.Fatalf("live listen_addr = %q, want unchanged", got)
	}
	if _, err := readRuntimeConfig(configPath); err == nil {
		t.Fatal("trailing-JSON POST must not write the config file")
	}
}
