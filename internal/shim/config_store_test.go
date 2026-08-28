package shim

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoadConfigSeedsAndPersistsRuntimeConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(configPathEnv, configPath)
	t.Setenv("CC_AUTO_SHIM_LISTEN", "127.0.0.1:9876")
	t.Setenv("CC_ANYROUTER_SHIM_UPSTREAM", "https://a.example,https://b.example")
	t.Setenv("CC_CLIPROXY_SHIM_UPSTREAM", "http://127.0.0.1:9000")
	t.Setenv("CC_CLIPROXY_SHIM_CA", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.configPath != configPath {
		t.Fatalf("configPath = %q, want %q", cfg.configPath, configPath)
	}
	if cfg.runtime.ListenAddr != "127.0.0.1:9876" {
		t.Fatalf("listen_addr = %q", cfg.runtime.ListenAddr)
	}
	if got := cfg.runtime.AnyRouter.Entrances; len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("anyrouter entrances = %#v", got)
	}
	if cfg.runtime.CPA.Upstream != "http://127.0.0.1:9000" {
		t.Fatalf("cpa upstream = %q", cfg.runtime.CPA.Upstream)
	}
	if cfg.table == nil || len(cfg.table.routes) != 2 {
		t.Fatalf("routing table was not built: %#v", cfg.table)
	}

	persisted, err := readRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("seeded config was not persisted: %v", err)
	}
	if persisted.ListenAddr != cfg.runtime.ListenAddr {
		t.Fatalf("persisted listen_addr = %q, want %q", persisted.ListenAddr, cfg.runtime.ListenAddr)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat persisted config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file permissions = %o, want 600", perm)
	}
}

func TestLoadConfigExistingFileOverridesBootstrapEnv(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	fileConfig := &runtimeConfig{
		ListenAddr: "127.0.0.1:7777",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://from-file.example"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8318"},
	}
	if err := saveRuntimeConfig(configPath, fileConfig); err != nil {
		t.Fatalf("saveRuntimeConfig returned error: %v", err)
	}

	t.Setenv(configPathEnv, configPath)
	t.Setenv("CC_AUTO_SHIM_LISTEN", "127.0.0.1:9999")
	t.Setenv("CC_ANYROUTER_SHIM_UPSTREAM", "https://from-env.example")
	t.Setenv("CC_CLIPROXY_SHIM_UPSTREAM", "http://127.0.0.1:9998")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.runtime.ListenAddr != "127.0.0.1:7777" {
		t.Fatalf("listen_addr = %q, want file value", cfg.runtime.ListenAddr)
	}
	if got := cfg.runtime.AnyRouter.Entrances; len(got) != 1 || got[0] != "https://from-file.example" {
		t.Fatalf("anyrouter entrances = %#v, want file value", got)
	}
	if cfg.runtime.CPA.Upstream != "http://127.0.0.1:8318" {
		t.Fatalf("cpa upstream = %q, want file value", cfg.runtime.CPA.Upstream)
	}
}
func TestSaveAndApplyRuntimeConfigPersistsThenSwaps(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
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
	server := newProxyServer(appConfig{configPath: configPath, runtime: compiled.runtime, table: table})

	next := &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://second.example"}},
		CPA:        cpaRuntimeConfig{Upstream: "http://127.0.0.1:8317"},
	}
	restartRequired, err := server.saveAndApplyRuntimeConfig(next)
	if err != nil {
		t.Fatalf("saveAndApplyRuntimeConfig returned error: %v", err)
	}
	if restartRequired {
		t.Fatal("same listen_addr should not require restart")
	}

	// Disk must reflect the saved config.
	persisted, err := readRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("saved config could not be read back: %v", err)
	}
	if got := persisted.AnyRouter.Entrances; len(got) != 1 || got[0] != "https://second.example" {
		t.Fatalf("persisted entrances = %#v, want second.example", got)
	}
	// Live routing must reflect the same swap.
	if got := server.currentRoutingTable().routes[0].upstreams.url(0).String(); got != "https://second.example" {
		t.Fatalf("live routing upstream = %q, want second.example", got)
	}
}

// TestRuntimeConfigAccountsEmptySliceShapeConsistent pins the empty-accounts
// shape so the GET /admin/config view and the persisted on-disk file agree:
// both must serialize an empty (non-nil) account list as a JSON array `[]`,
// never `null`. The bug this guards: normalizeRuntimeConfig sets Accounts to a
// non-nil []accountEntry{}, but cloneRuntimeConfig used append([]T(nil), …),
// which collapses an empty source back to nil — so the live runtime served by
// GET marshalled `"accounts":null` while writeRuntimeConfigFile (which writes
// the normalized compiled.runtime directly) wrote `"accounts": []`.
func TestRuntimeConfigAccountsEmptySliceShapeConsistent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	// baseAdminConfig carries no accounts (nil slice), exercising the empty case.
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	// The live runtime stored in state (what GET serves) must keep the empty
	// account list as a non-nil slice, matching the normalized form on disk.
	if got := server.currentState().runtime.AnyRouter.Accounts; got == nil {
		t.Fatal("live runtime AnyRouter.Accounts is nil; clone collapsed the normalized empty slice (GET would emit null while disk emits [])")
	}

	getRaw := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/config status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	// GET view must show an array, not null.
	before := getRaw()
	if strings.Contains(before, `"accounts":null`) {
		t.Fatalf("GET /admin/config emitted \"accounts\":null, want []: %s", before)
	}
	if !strings.Contains(before, `"accounts":[]`) {
		t.Fatalf("GET /admin/config did not emit \"accounts\":[]: %s", before)
	}

	// Round-trip the live view back through POST so disk is written from it.
	rec := postAdminConfig(t, server, before)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST round-trip status = %d (%s)", rec.Code, rec.Body.String())
	}

	// The persisted file must also carry [], not null — same shape as GET.
	diskBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	disk := string(diskBytes)
	if strings.Contains(disk, `"accounts": null`) || strings.Contains(disk, `"accounts":null`) {
		t.Fatalf("persisted config emitted accounts null, want []: %s", disk)
	}
	if !strings.Contains(disk, `"accounts": []`) {
		t.Fatalf("persisted config did not emit \"accounts\": []: %s", disk)
	}

	// GET after the round-trip must remain consistent (still []).
	if after := getRaw(); !strings.Contains(after, `"accounts":[]`) || strings.Contains(after, `"accounts":null`) {
		t.Fatalf("GET after round-trip lost the [] shape: %s", after)
	}
}
func TestReadRuntimeConfigBackCompatMissingLogMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{
		"listen_addr": "127.0.0.1:8765",
		"enabled": true,
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	cfg, err := readRuntimeConfig(path)
	if err != nil {
		t.Fatalf("old config without log_max_bytes must decode, got: %v", err)
	}
	if cfg.LogMaxBytes != 0 {
		t.Fatalf("missing log_max_bytes should decode to 0, got %d", cfg.LogMaxBytes)
	}
	compiled, err := compileRuntimeConfig(cfg)
	if err != nil {
		t.Fatalf("compile after back-compat read: %v", err)
	}
	if compiled.runtime.LogMaxBytes != defaultMaxLogBytes {
		t.Fatalf("back-compat LogMaxBytes = %d, want default %d", compiled.runtime.LogMaxBytes, defaultMaxLogBytes)
	}
}

// TestRuntimeConfigLogMaxBytesGetDiskRoundTrip pins that the live GET
// /admin/config view and the persisted on-disk file agree on log_max_bytes —
// no "0-vs-default" or omitted-vs-present drift. The normalized runtime always
// carries a positive cap, so both the (compact) GET body and the (indented) disk
// file must show it. Mirrors TestRuntimeConfigAccountsEmptySliceShapeConsistent.
func TestRuntimeConfigLogMaxBytesGetDiskRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		cap     int64
		wantStr string
	}{
		// An absent/0 cap (the back-compat shape) must surface as the default in
		// BOTH views, never as 0 in one and the default in the other.
		{name: "absent_defaults", cap: 0, wantStr: strconv.FormatInt(defaultMaxLogBytes, 10)},
		// A custom positive cap must survive the round-trip identically.
		{name: "custom_preserved", cap: 7 << 20, wantStr: strconv.FormatInt(7<<20, 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			server := newTestAdminServer(t, logCapConfig(tc.cap), configPath)

			getRaw := func() string {
				t.Helper()
				rec := httptest.NewRecorder()
				server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET /admin/config status = %d, want 200", rec.Code)
				}
				return rec.Body.String()
			}

			// GET (compact JSON) must show the resolved cap.
			wantGet := `"log_max_bytes":` + tc.wantStr
			before := getRaw()
			if !strings.Contains(before, wantGet) {
				t.Fatalf("GET /admin/config missing %q: %s", wantGet, before)
			}

			// Round-trip the live view back through POST so disk is written from it.
			if rec := postAdminConfig(t, server, before); rec.Code != http.StatusOK {
				t.Fatalf("POST round-trip status = %d (%s)", rec.Code, rec.Body.String())
			}

			// Disk (indented JSON) must carry the same cap — same value as GET.
			diskBytes, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read persisted config: %v", err)
			}
			wantDisk := `"log_max_bytes": ` + tc.wantStr
			if disk := string(diskBytes); !strings.Contains(disk, wantDisk) {
				t.Fatalf("persisted config missing %q: %s", wantDisk, disk)
			}

			// GET after the round-trip must remain consistent.
			if after := getRaw(); !strings.Contains(after, wantGet) {
				t.Fatalf("GET after round-trip lost %q: %s", wantGet, after)
			}
		})
	}
}

// TestAdminConfigPostRejectsNegativeLogMaxBytes proves the negative-cap rejection
// surfaces as the 400 the config validation DoD calls for, through the existing POST handler.
func TestLoadConfigLogCapFileWinsOverEnv(t *testing.T) {
	const fileCap int64 = 3 << 20
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(configPathEnv, configPath)
	t.Setenv("CC_AUTO_SHIM_LISTEN", "")
	// Persist a config file carrying fileCap, then point the env at a DIFFERENT cap.
	if err := saveRuntimeConfig(configPath, logCapConfig(fileCap)); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	t.Setenv("CC_AUTO_SHIM_LOG_MAX_BYTES", strconv.FormatInt(fileCap*4, 10))

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.runtime.LogMaxBytes != fileCap {
		t.Fatalf("LogMaxBytes = %d, want file value %d (env must not override an existing file)", cfg.runtime.LogMaxBytes, fileCap)
	}
}

// TestSeedRuntimeConfigFromEnvSeedsLogCap pins the first-run seed direction of the
// persisted-config authority demotion: with NO config file, CC_AUTO_SHIM_LOG_MAX_BYTES seeds the freshly
// created config's log_max_bytes (mirroring CC_AUTO_SHIM_LISTEN → listen_addr) and
// is persisted. Combined with TestLoadConfigLogCapFileWinsOverEnv (the env is
// ignored once the file exists), this is the full "env is a first-run seed only"
// contract.
func TestSeedRuntimeConfigFromEnvSeedsLogCap(t *testing.T) {
	const envCap int64 = 11 << 20
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(configPathEnv, configPath)
	t.Setenv("CC_AUTO_SHIM_LISTEN", "")
	t.Setenv("CC_AUTO_SHIM_LOG_MAX_BYTES", strconv.FormatInt(envCap, 10))

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.runtime.LogMaxBytes != envCap {
		t.Fatalf("seeded LogMaxBytes = %d, want env value %d", cfg.runtime.LogMaxBytes, envCap)
	}
	// The seeded value must also be persisted (the file is now authoritative).
	persisted, err := readRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if persisted.LogMaxBytes != envCap {
		t.Fatalf("persisted LogMaxBytes = %d, want env value %d", persisted.LogMaxBytes, envCap)
	}
}
