package shim

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestMain neutralizes the self re-exec for the ENTIRE test binary. A
// restart-required POST /admin/config calls scheduleSelfRestart, whose default
// restartProcess would syscall.Exec the test process itself (replacing it
// mid-suite). Stubbing restartProcess with a no-op and zeroing restartDelay (which
// also makes the trigger run inline, so no goroutine outlives a test) keeps every
// test that posts a restart-required change safe. A test that needs to OBSERVE the
// restart overrides restartProcess locally (see TestAdminConfigPostAutoRestart).
func TestMain(m *testing.M) {
	restartProcess = func() error { return nil }
	restartDelay = 0
	os.Exit(m.Run())
}

func newTestAdminServer(t *testing.T, cfg *runtimeConfig, configPath string) *proxyServer {
	t.Helper()
	compiled, err := compileRuntimeConfig(cfg)
	if err != nil {
		t.Fatalf("compileRuntimeConfig returned error: %v", err)
	}
	table, err := buildRoutingTable(compiled)
	if err != nil {
		t.Fatalf("buildRoutingTable returned error: %v", err)
	}
	return newProxyServer(appConfig{configPath: configPath, runtime: compiled.runtime, table: table})
}

func baseAdminConfig() *runtimeConfig {
	return &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://anyrouter.top"}},
		CPA:        cpaRuntimeConfig{Upstream: "https://127.0.0.1:8317"},
	}
}
func postAdminConfig(t *testing.T, server *proxyServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/config", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(rec, req)
	return rec
}
