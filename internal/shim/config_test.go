package shim

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyRuntimeConfigSwapsRuntimeAndRoutingTable(t *testing.T) {
	initial := &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://first.example"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	}
	compiled, err := compileRuntimeConfig(initial)
	if err != nil {
		t.Fatalf("compileRuntimeConfig(initial) returned error: %v", err)
	}
	table, err := buildRoutingTable(compiled)
	if err != nil {
		t.Fatalf("buildRoutingTable(initial) returned error: %v", err)
	}
	server := newProxyServer(appConfig{runtime: compiled.runtime, table: table})

	next := &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://second.example"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	}
	restartRequired, err := server.applyRuntimeConfig(next)
	if err != nil {
		t.Fatalf("applyRuntimeConfig returned error: %v", err)
	}
	if restartRequired {
		t.Fatal("same listen_addr should not require restart")
	}
	next.AnyRouter.Entrances[0] = "https://mutated.example"
	if got := server.currentState().runtime.AnyRouter.Entrances[0]; got != "https://second.example" {
		t.Fatalf("stored runtime config = %q, want cloned second.example", got)
	}
	if got := server.currentRoutingTable().routes[0].upstreams.url(0).String(); got != "https://second.example" {
		t.Fatalf("routing table upstream = %q, want second.example", got)
	}

	restartCfg := &runtimeConfig{
		ListenAddr: "127.0.0.1:8766",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://second.example"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	}
	restartRequired, err = server.applyRuntimeConfig(restartCfg)
	if err != nil {
		t.Fatalf("applyRuntimeConfig(restartCfg) returned error: %v", err)
	}
	if !restartRequired {
		t.Fatal("changed listen_addr should require restart")
	}
}

func TestRuntimeConfigValidationKeepsListenAddrLoopbackOnly(t *testing.T) {
	err := validateRuntimeConfig(&runtimeConfig{
		ListenAddr: "0.0.0.0:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://anyrouter.top"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	})
	if err == nil {
		t.Fatal("non-loopback listen_addr should be rejected")
	}
}

func TestValidateListenAddrRejectsNonCanonicalPort(t *testing.T) {
	// strconv.Atoi accepts a leading '+' and leading zeros; both would persist a
	// non-canonical port literally and must be rejected.
	for _, addr := range []string{"127.0.0.1:+8080", "127.0.0.1:08081"} {
		if err := validateListenAddr(addr); err == nil {
			t.Fatalf("validateListenAddr(%q) = nil, want rejection", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8765", "127.0.0.1:1", "127.0.0.1:65535"} {
		if err := validateListenAddr(addr); err != nil {
			t.Fatalf("validateListenAddr(%q) = %v, want accepted", addr, err)
		}
	}
}
func TestSaveAndApplyRuntimeConfigRequiresConfigPath(t *testing.T) {
	server := &proxyServer{}
	server.state.Store(&proxyState{
		runtime: &runtimeConfig{ListenAddr: defaultListenAddr},
		table:   &routingTable{},
	})
	_, err := server.saveAndApplyRuntimeConfig(&runtimeConfig{
		ListenAddr: defaultListenAddr,
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://anyrouter.top"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	})
	if err == nil {
		t.Fatal("saveAndApplyRuntimeConfig with empty config path should fail")
	}
}

// logCapConfig returns an otherwise-valid runtimeConfig with the given log cap,
// isolating LogMaxBytes as the single variable under test (config validation).
func logCapConfig(n int64) *runtimeConfig {
	cfg := baseAdminConfig()
	cfg.LogMaxBytes = n
	return cfg
}

// TestRuntimeConfigLogMaxBytesNormalizeAndValidate covers the config validation config-layer
// contract for the log cap: normalize maps absent/0 to the default and leaves a
// positive value untouched, while a negative cap survives normalize so compile
// can reject it. The field stores bytes and uses 0 as the unset sentinel (not a
// *int64) because 0 is never a valid cap — see config.go.
func TestRuntimeConfigLogMaxBytesNormalizeAndValidate(t *testing.T) {
	// normalize: nil config → default cap (mirrors the Enabled/listen_addr defaults).
	if got := normalizeRuntimeConfig(nil).LogMaxBytes; got != defaultMaxLogBytes {
		t.Fatalf("normalize(nil) LogMaxBytes = %d, want default %d", got, defaultMaxLogBytes)
	}
	// normalize: an explicit 0 (the unset sentinel; also how an old config's absent
	// key decodes) → default. This is the back-compat path.
	if got := normalizeRuntimeConfig(&runtimeConfig{LogMaxBytes: 0}).LogMaxBytes; got != defaultMaxLogBytes {
		t.Fatalf("normalize(0) LogMaxBytes = %d, want default %d", got, defaultMaxLogBytes)
	}
	// normalize: a positive value is preserved verbatim (no clamping/defaulting).
	const custom int64 = 7 << 20
	if got := normalizeRuntimeConfig(&runtimeConfig{LogMaxBytes: custom}).LogMaxBytes; got != custom {
		t.Fatalf("normalize(%d) LogMaxBytes = %d, want preserved", custom, got)
	}
	// normalize does NOT rescue a negative value — it must reach validate intact,
	// otherwise the negative would silently default and never be rejected.
	if got := normalizeRuntimeConfig(&runtimeConfig{LogMaxBytes: -1}).LogMaxBytes; got != -1 {
		t.Fatalf("normalize(-1) LogMaxBytes = %d, want -1 left for validate", got)
	}

	// compile: a 0/absent cap defaults and compiles cleanly; the compiled runtime
	// (what GET serves and the file persists) carries the default.
	compiled, err := compileRuntimeConfig(logCapConfig(0))
	if err != nil {
		t.Fatalf("compile(0) error: %v", err)
	}
	if compiled.runtime.LogMaxBytes != defaultMaxLogBytes {
		t.Fatalf("compile(0) runtime LogMaxBytes = %d, want default %d", compiled.runtime.LogMaxBytes, defaultMaxLogBytes)
	}
	// compile roundtrip: a positive cap survives into the compiled runtime unchanged.
	compiled, err = compileRuntimeConfig(logCapConfig(custom))
	if err != nil {
		t.Fatalf("compile(%d) error: %v", custom, err)
	}
	if compiled.runtime.LogMaxBytes != custom {
		t.Fatalf("compile(%d) runtime LogMaxBytes = %d, want roundtrip", custom, compiled.runtime.LogMaxBytes)
	}

	// validate: a negative cap is rejected, and the error names the field.
	err = validateRuntimeConfig(logCapConfig(-1))
	if err == nil {
		t.Fatal("validate(-1) = nil, want rejection of a negative log cap")
	}
	if !strings.Contains(err.Error(), "log_max_bytes") {
		t.Fatalf("validate(-1) error = %q, want it to name log_max_bytes", err)
	}
}

// TestCloneRuntimeConfigCopiesLogMaxBytes pins that the (value-type) log cap is
// copied by cloneRuntimeConfig and not aliased — the struct copy already isolates
// it, so this guards against a future regression that would share storage.
func TestCloneRuntimeConfigCopiesLogMaxBytes(t *testing.T) {
	const custom int64 = 9 << 20
	orig := &runtimeConfig{LogMaxBytes: custom}
	clone := cloneRuntimeConfig(orig)
	if clone.LogMaxBytes != custom {
		t.Fatalf("clone LogMaxBytes = %d, want %d", clone.LogMaxBytes, custom)
	}
	orig.LogMaxBytes = 1
	if clone.LogMaxBytes != custom {
		t.Fatalf("clone LogMaxBytes changed to %d after mutating original; want %d", clone.LogMaxBytes, custom)
	}
}

// TestReadRuntimeConfigBackCompatMissingLogMaxBytes proves an older config file
// that predates the log_max_bytes field still decodes under DisallowUnknownFields
// (a *missing* key is fine — only *unknown* keys are rejected) and compiles to the
// default, so upgrading does not break or change behavior for existing installs.
func TestAdminConfigPostRejectsNegativeLogMaxBytes(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)
	rec := postAdminConfig(t, server, `{
		"listen_addr": "127.0.0.1:8765",
		"log_max_bytes": -1,
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST negative log_max_bytes status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestNormalizeFillsBlankAccountLabels covers the account-label behavior label autofill: an account
// with a key but no label gets the next free acct-N (continuing past the highest
// existing acct-N so it cannot collide), labels/keys are trimmed, an explicit
// non-acct label is preserved, and an account with no key is left unnamed (it
// carries no rotation weight). The autofill happens in normalizeRuntimeConfig, so
// it is what GET /admin/config serves and what the on-disk file persists.
func TestNormalizeFillsBlankAccountLabels(t *testing.T) {
	cfg := baseAdminConfig()
	cfg.AnyRouter.Accounts = []accountEntry{
		{Label: "  acct-2  ", Key: "  k2  "}, // trimmed to acct-2 / k2; sets max=2
		{Label: "", Key: "k-blank"},          // blank + key → next free past max → acct-3
		{Label: "", Key: ""},                 // blank + no key → left unnamed
		{Label: "custom", Key: "k-custom"},   // explicit non-acct label preserved
		{Label: "", Key: "k-blank2"},         // second blank + key → acct-4
	}
	got := normalizeRuntimeConfig(cfg).AnyRouter.Accounts
	want := []accountEntry{
		{Label: "acct-2", Key: "k2"},
		{Label: "acct-3", Key: "k-blank"},
		{Label: "", Key: ""},
		{Label: "custom", Key: "k-custom"},
		{Label: "acct-4", Key: "k-blank2"},
	}
	if len(got) != len(want) {
		t.Fatalf("normalized accounts len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("account[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCompileRejectsDuplicateAccountLabels proves the uniqueness guard: two
// accounts with the same non-empty label fail compile/validate (labels key the
// health snapshot + UI pills, so a duplicate would alias). The error names the
// field and the offending label. The control case — two blank-but-keyed accounts —
// must NOT collide, because normalizeRuntimeConfig gives each a distinct acct-N
// before the uniqueness check runs.
func TestCompileRejectsDuplicateAccountLabels(t *testing.T) {
	dup := baseAdminConfig()
	dup.AnyRouter.Accounts = []accountEntry{
		{Label: "acct-1", Key: "k1"},
		{Label: "acct-1", Key: "k2"},
	}
	err := validateRuntimeConfig(dup)
	if err == nil {
		t.Fatal("validate(duplicate labels) = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "anyrouter.accounts") || !strings.Contains(err.Error(), "acct-1") {
		t.Fatalf("error = %q, want it to name anyrouter.accounts and acct-1", err)
	}

	// Control: two blank labels with keys are auto-named acct-1 / acct-2 by
	// normalize, so they are distinct and compile cleanly.
	blank := baseAdminConfig()
	blank.AnyRouter.Accounts = []accountEntry{{Label: "", Key: "k1"}, {Label: "", Key: "k2"}}
	compiled, err := compileRuntimeConfig(blank)
	if err != nil {
		t.Fatalf("compile(two blank-keyed accounts) = %v, want clean", err)
	}
	got := compiled.runtime.AnyRouter.Accounts
	if len(got) != 2 || got[0].Label != "acct-1" || got[1].Label != "acct-2" {
		t.Fatalf("auto-named labels = %+v, want acct-1 then acct-2", got)
	}
}

// TestAdminConfigPostRejectsDuplicateAccountLabels proves the duplicate-label
// rejection surfaces as a 400 through the POST handler (the backend twin of the
// admin UI's inline guard), mirroring the negative-log-cap test above.
func TestAdminConfigPostRejectsDuplicateAccountLabels(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)
	rec := postAdminConfig(t, server, `{
		"listen_addr": "127.0.0.1:8765",
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": [
			{"label": "dup", "key": "k1"},
			{"label": "dup", "key": "k2"}
		]},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST duplicate account labels status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestParseAcctLabel pins the acct-N suffix parser that feeds the autofill counter.
// It matches the admin UI's /^acct-(\d+)$/: the "acct-" prefix followed by a
// non-empty run of ASCII digits (no sign, no trailing junk). A suffix too large
// for an int reports not-a-match so it simply does not advance the counter.
func TestParseAcctLabel(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"acct-1", 1, true},
		{"acct-42", 42, true},
		{"acct-0", 0, true},
		{"acct-007", 7, true}, // leading zeros parse via Atoi
		{"acct-", 0, false},
		{"acct-x", 0, false},
		{"acct-1a", 0, false},
		{"acct-1.5", 0, false},
		{"acct--1", 0, false}, // sign is not a digit
		{"acct-+1", 0, false},
		{"acct", 0, false},
		{"", 0, false},
		{"foo", 0, false},
		{"acct-99999999999999999999", 0, false}, // Atoi overflow → not-a-match
	}
	for _, c := range cases {
		n, ok := parseAcctLabel(c.in)
		if ok != c.ok || (ok && n != c.n) {
			t.Errorf("parseAcctLabel(%q) = (%d, %v), want (%d, %v)", c.in, n, ok, c.n, c.ok)
		}
	}
}

// TestSwapStateRestartRequiredTracksLogCap pins the persisted-config authority extension of
// restart_required: configureLogging runs only at startup (never on the hot-swap
// path), so a change to log_max_bytes needs a restart just like listen_addr,
// while leaving both unchanged does not. It mirrors
// TestApplyRuntimeConfigSwapsRuntimeAndRoutingTable but isolates the log-cap
// dimension and checks the listen_addr-OR-log_cap combination both ways. Like the
// listen_addr flag, the cap is measured against the bound value, not the
// last-applied config.
func TestSwapStateRestartRequiredTracksLogCap(t *testing.T) {
	const boundCap int64 = 5 << 20
	cfgFor := func(listen string, cap int64) *runtimeConfig {
		cfg := baseAdminConfig()
		cfg.ListenAddr = listen
		cfg.LogMaxBytes = cap
		return cfg
	}
	compiled, err := compileRuntimeConfig(cfgFor("127.0.0.1:8765", boundCap))
	if err != nil {
		t.Fatalf("compileRuntimeConfig(initial) returned error: %v", err)
	}
	table, err := buildRoutingTable(compiled)
	if err != nil {
		t.Fatalf("buildRoutingTable(initial) returned error: %v", err)
	}
	// newProxyServer records boundListenAddr=127.0.0.1:8765 and boundLogMaxBytes=boundCap.
	server := newProxyServer(appConfig{runtime: compiled.runtime, table: table})

	apply := func(listen string, cap int64) bool {
		t.Helper()
		restart, err := server.applyRuntimeConfig(cfgFor(listen, cap))
		if err != nil {
			t.Fatalf("applyRuntimeConfig(%q,%d) returned error: %v", listen, cap, err)
		}
		return restart
	}

	// Same listen, same cap: no restart.
	if apply("127.0.0.1:8765", boundCap) {
		t.Fatal("unchanged listen_addr and log cap should not require restart")
	}
	// Same listen, changed cap: restart required (the persisted-config authority addition).
	if !apply("127.0.0.1:8765", boundCap*2) {
		t.Fatal("changed log_max_bytes should require restart")
	}
	// Returning the cap to the bound value (listen still bound) needs no restart —
	// measured against boundLogMaxBytes, not the just-applied boundCap*2.
	if apply("127.0.0.1:8765", boundCap) {
		t.Fatal("returning log_max_bytes to the bound value should not require restart")
	}
	// Changed listen, unchanged cap: the listen dimension still stands alone.
	if !apply("127.0.0.1:8766", boundCap) {
		t.Fatal("changed listen_addr should still require restart")
	}
}

// TestLoadConfigLogCapFileWinsOverEnv pins persisted-config authority startup authority: once the config
// file exists its log_max_bytes is authoritative and CC_AUTO_SHIM_LOG_MAX_BYTES is
// ignored (demoted to a first-run seed, exactly like CC_AUTO_SHIM_LISTEN →
// listen_addr). Run applies cfg.runtime.LogMaxBytes to the writers and
// newProxyServer records it as boundLogMaxBytes, so the value loadConfig returns
// here is the cap the process actually binds.
