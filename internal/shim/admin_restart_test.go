package shim

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestAdminConfigPostRestartRequiredTracksBoundAddr(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	// newTestAdminServer → newProxyServer records boundListenAddr = 127.0.0.1:8765.
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	post := func(listen string) bool {
		t.Helper()
		rec := postAdminConfig(t, server, `{
			"listen_addr": "`+listen+`",
			"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
			"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
			"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
		}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST listen_addr %q status = %d (%s)", listen, rec.Code, rec.Body.String())
		}
		var resp struct {
			RestartRequired bool `json:"restart_required"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode POST response: %v", err)
		}
		return resp.RestartRequired
	}

	// Change the listen address away from the bound one: restart required.
	if !post("127.0.0.1:8766") {
		t.Fatal("changing listen_addr away from the bound address should require restart")
	}
	// A subsequent unrelated save that leaves listen_addr at the new value must
	// STILL report restart required: the process is still bound to 127.0.0.1:8765,
	// regardless of the last-applied config value.
	if !post("127.0.0.1:8766") {
		t.Fatal("a later save while still bound to the original address should keep restart required")
	}
	// A save that returns listen_addr to the actually-bound address needs no
	// restart — the running listener already matches.
	if post("127.0.0.1:8765") {
		t.Fatal("returning listen_addr to the bound address should not require restart")
	}
}

// TestAdminConfigPostAutoRestart proves the auto-restart-on-Save trigger: a POST
// that changes a restart-only binding (listen_addr or log_max_bytes) invokes the
// re-exec hook, the response carries restarting=true plus the just-applied
// listen_addr, and a POST that changes NEITHER leaves the hook untouched and
// reports restarting=false. restartProcess is stubbed to record into a channel
// (the package TestMain already blocks a real re-exec); the response is fully
// decoded before the hook is observed, which is the ordering guarantee — the
// handler writes + flushes the 200 first, then schedules the exec.
func TestAdminConfigPostAutoRestart(t *testing.T) {
	restarted := make(chan struct{}, 8)
	origRestart, origDelay := restartProcess, restartDelay
	t.Cleanup(func() { restartProcess, restartDelay = origRestart, origDelay })
	// restartDelay 0 keeps scheduleSelfRestart inline (deterministic, no goroutine);
	// the stub records the trigger instead of re-exec'ing the test binary.
	restartProcess = func() error { restarted <- struct{}{}; return nil }
	restartDelay = 0

	configPath := filepath.Join(t.TempDir(), "config.json")
	// newProxyServer binds boundListenAddr=127.0.0.1:8765 and boundLogMaxBytes=default.
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	type postResp struct {
		RestartRequired bool   `json:"restart_required"`
		Restarting      bool   `json:"restarting"`
		ListenAddr      string `json:"listen_addr"`
	}
	post := func(body string) postResp {
		t.Helper()
		rec := postAdminConfig(t, server, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST status = %d (%s)", rec.Code, rec.Body.String())
		}
		var r postResp
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode POST response: %v", err)
		}
		return r
	}
	hookFired := func() bool {
		select {
		case <-restarted:
			return true
		case <-time.After(time.Second):
			return false
		}
	}
	assertNoHook := func() {
		t.Helper()
		select {
		case <-restarted:
			t.Fatal("restart hook fired for a POST that changed neither listen_addr nor log_max_bytes")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// body builds a valid POST with the given listen port + log cap (bytes).
	body := func(port string, logBytes int64) string {
		return `{
			"listen_addr": "127.0.0.1:` + port + `",
			"log_max_bytes": ` + strconv.FormatInt(logBytes, 10) + `,
			"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
			"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
			"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
		}`
	}

	// (1) Neither binding changes (bound port + bound default cap) -> no restart,
	// restarting=false, listen_addr echoes the unchanged address, hook NOT invoked.
	if r := post(body("8765", defaultMaxLogBytes)); r.RestartRequired || r.Restarting {
		t.Fatalf("unchanged bindings: restart_required=%v restarting=%v, want both false", r.RestartRequired, r.Restarting)
	} else if r.ListenAddr != "127.0.0.1:8765" {
		t.Fatalf("listen_addr echo = %q, want 127.0.0.1:8765", r.ListenAddr)
	}
	assertNoHook()

	// (2) Change listen_addr -> restart required, restarting=true, listen_addr echoes
	// the NEW address, hook invoked.
	if r := post(body("8766", defaultMaxLogBytes)); !r.RestartRequired || !r.Restarting {
		t.Fatalf("listen change: restart_required=%v restarting=%v, want both true", r.RestartRequired, r.Restarting)
	} else if r.ListenAddr != "127.0.0.1:8766" {
		t.Fatalf("listen_addr echo = %q, want 127.0.0.1:8766", r.ListenAddr)
	}
	if !hookFired() {
		t.Fatal("changing listen_addr did not invoke the restart hook")
	}

	// (3) Change only log_max_bytes (listen back to the bound 8765 -> no listen
	// change) -> restart required via the log cap, hook invoked.
	if r := post(body("8765", 9<<20)); !r.RestartRequired || !r.Restarting {
		t.Fatalf("log cap change: restart_required=%v restarting=%v, want both true", r.RestartRequired, r.Restarting)
	}
	if !hookFired() {
		t.Fatal("changing log_max_bytes did not invoke the restart hook")
	}
}

// TestAdminConfigPostBindProbe proves the pre-persist bind probe in
// saveAndApplyRuntimeConfig closes the auto-restart footgun: a Save that would move
// the listener onto an OCCUPIED/unbindable port is rejected with 400 — nothing is
// persisted, swapped, or restarted — so the shim can never re-exec itself onto a
// dead port (which would kill the service on both the old and new port and
// crash-loop under launchd). A Save onto a genuinely FREE port still succeeds with
// restarting=true (the happy path is intact). The probe runs only for an actual
// change versus the bound address.
func TestAdminConfigPostBindProbe(t *testing.T) {
	restarted := make(chan struct{}, 4)
	origRestart, origDelay := restartProcess, restartDelay
	t.Cleanup(func() { restartProcess, restartDelay = origRestart, origDelay })
	// restartDelay 0 keeps scheduleSelfRestart inline; the stub records the trigger
	// instead of re-exec'ing the test binary (TestMain already blocks a real exec).
	restartProcess = func() error { restarted <- struct{}{}; return nil }
	restartDelay = 0

	hookFired := func() bool {
		select {
		case <-restarted:
			return true
		case <-time.After(time.Second):
			return false
		}
	}
	assertNoHook := func() {
		t.Helper()
		select {
		case <-restarted:
			t.Fatal("restart hook fired for a Save that was rejected before any swap")
		case <-time.After(100 * time.Millisecond):
		}
	}
	bodyFor := func(addr string) string {
		return `{
			"listen_addr": "` + addr + `",
			"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
			"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
			"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
		}`
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	// newProxyServer records boundListenAddr = 127.0.0.1:8765 for the life of this server.
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	// ---- (A) Occupied port -> 400, nothing persisted, restart hook NOT fired. ----
	// Hold a real listener open on an ephemeral 127.0.0.1 port for the whole test so
	// the bind probe must fail when the Save tries to move the listener onto it.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	occupiedAddr := occupied.Addr().String()
	// The probe is a no-op when the new addr equals the bound addr; the OS handing us
	// 8765 would make phase (A) vacuous, so skip that astronomically unlikely case.
	if occupiedAddr == "127.0.0.1:8765" {
		t.Skipf("occupied port collided with the bound test port %q", occupiedAddr)
	}

	rec := postAdminConfig(t, server, bodyFor(occupiedAddr))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST onto an occupied port status = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	// Live config unchanged (no swap)...
	if got := server.currentState().runtime.ListenAddr; got != "127.0.0.1:8765" {
		t.Fatalf("live listen_addr = %q after a rejected Save, want the unchanged 127.0.0.1:8765", got)
	}
	// ...GET /admin/config still reports the old addr...
	getRec := httptest.NewRecorder()
	server.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	var got runtimeConfig
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET /admin/config: %v", err)
	}
	if got.ListenAddr != "127.0.0.1:8765" {
		t.Fatalf("GET listen_addr = %q after a rejected Save, want 127.0.0.1:8765", got.ListenAddr)
	}
	// ...and nothing was written to disk (the probe precedes the persist).
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config file must not exist after a rejected Save; stat err = %v", statErr)
	}
	assertNoHook()

	// ---- (B) Free port -> 200 + restarting:true + hook fires (happy path intact). ----
	// Reserve an ephemeral port, then release it so the bind probe can bind it.
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	freeAddr := reserve.Addr().String()
	reserve.Close()
	if freeAddr == "127.0.0.1:8765" {
		t.Skipf("free port collided with the bound test port %q", freeAddr)
	}

	rec = postAdminConfig(t, server, bodyFor(freeAddr))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST onto a free port status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		RestartRequired bool   `json:"restart_required"`
		Restarting      bool   `json:"restarting"`
		ListenAddr      string `json:"listen_addr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if !resp.RestartRequired || !resp.Restarting {
		t.Fatalf("free-port Save: restart_required=%v restarting=%v, want both true", resp.RestartRequired, resp.Restarting)
	}
	if resp.ListenAddr != freeAddr {
		t.Fatalf("free-port Save listen_addr echo = %q, want %q", resp.ListenAddr, freeAddr)
	}
	if got := server.currentState().runtime.ListenAddr; got != freeAddr {
		t.Fatalf("live listen_addr = %q after a free-port Save, want %q", got, freeAddr)
	}
	if !hookFired() {
		t.Fatal("a free-port listen change did not invoke the restart hook")
	}
}
