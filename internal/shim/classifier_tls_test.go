package shim

// Classifier-target TLS three-state coverage.
//
// These tests exercise the generalized global-target TLS plumbing
// (buildTargetTLSConfig → newTargetHTTPClient, wired by buildRoutingTable) against
// a REAL TLS server (httptest.NewTLSServer) with a self-signed certificate. The
// plain-HTTP classifierTestServer cannot exercise TLS, and the buildTargetTLSConfig
// unit tests (transport_test.go) only assert the *tls.Config shape — they never
// prove the config actually governs a live handshake. Here the request flows
// compileRuntimeConfig → buildRoutingTable → ServeHTTP → newTargetHTTPClient, so the
// TLS config is the one thing under test:
//
//   - skip:   target_insecure_skip_verify=true → the self-signed target connects.
//   - CA:     target_ca_path points at the server's own cert PEM → it connects.
//   - verify: neither set (system roots) → the self-signed cert is untrusted → the
//             handshake fails → the shim FAILS CLOSED (502, never a synthesized
//             <block>no / 200 allow) and never charges the account pool.

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeServerCertPEM writes a TLS test server's own (self-signed) leaf certificate
// to a temp PEM file and returns the path. Loaded as target_ca_path it becomes the
// sole trusted root, so the client trusts exactly this server and nothing else —
// the real CA-pinning path through buildTargetTLSConfig.
func writeServerCertPEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	cert := server.Certificate()
	if cert == nil {
		t.Fatal("TLS test server has no certificate")
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "target-ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write cert pem: %v", err)
	}
	return path
}

// tlsTargetConfig builds a runtime config whose global classifier target is the
// given (self-signed) TLS URL, carrying an AnyRouter rotation account so the
// no-account invariant can be checked on the fail-closed path. target_type is
// generic (the TLS state, not the platform adaptation, is what these tests probe);
// a gpt classifier then proves the full pipeline by being reassembled on success.
func tlsTargetConfig(tlsURL, caPath string, insecure bool) *runtimeConfig {
	return &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter: anyRouterRuntimeConfig{
			Entrances: []string{"https://anyrouter.top"},
			Accounts:  []accountEntry{{Label: "acct-0", Key: "sk-rotation-key"}},
		},
		CPA: cpaRuntimeConfig{Upstream: "https://127.0.0.1:8317"},
		Classifier: classifierRuntimeConfig{
			TargetBaseURL:            tlsURL,
			TargetType:               "generic",
			TargetCAPath:             caPath,
			TargetInsecureSkipVerify: insecure,
		},
	}
}

// newRecordingTLSTarget starts a self-signed TLS server that records its hit count
// and replies with a GPT-style classifier body (so a connected request is visibly
// reassembled). hits is read only after the client round-trip completes.
func newRecordingTLSTarget(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// sendClassifierToTLSTarget drives a gpt classifier on /any through the full
// compile→build→ServeHTTP chain to the configured (TLS) global target and returns
// the client status + body.
func sendClassifierToTLSTarget(t *testing.T, cfg *runtimeConfig) (int, []byte) {
	t.Helper()
	server := newTestAdminServer(t, cfg, "")
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, out
}

func TestClassifierTargetTLSInsecureSkipVerifyConnects(t *testing.T) {
	target, hits := newRecordingTLSTarget(t)
	status, out := sendClassifierToTLSTarget(t, tlsTargetConfig(target.URL, "", true))

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (insecure skip-verify must connect to the self-signed target); body %s", status, out)
	}
	if *hits != 1 {
		t.Fatalf("TLS target hits = %d, want 1", *hits)
	}
	// Full pipeline ran: the GPT-style body was reassembled into the auto-mode shape.
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client response not JSON (should be reassembled): %v", err)
	}
	if m["stop_reason"] != "stop_sequence" {
		t.Fatalf("reassembled stop_reason = %v, want stop_sequence", m["stop_reason"])
	}
}

func TestClassifierTargetTLSCustomCAConnects(t *testing.T) {
	target, hits := newRecordingTLSTarget(t)
	caPath := writeServerCertPEM(t, target)
	status, out := sendClassifierToTLSTarget(t, tlsTargetConfig(target.URL, caPath, false))

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pinning the server's own cert as target_ca_path must verify); body %s", status, out)
	}
	if *hits != 1 {
		t.Fatalf("TLS target hits = %d, want 1", *hits)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client response not JSON (should be reassembled): %v", err)
	}
	if m["stop_reason"] != "stop_sequence" {
		t.Fatalf("reassembled stop_reason = %v, want stop_sequence", m["stop_reason"])
	}
}

// TestClassifierTargetTLSVerifyFailsClosed proves the safe default: with neither a
// CA nor skip-verify, the self-signed target's cert is untrusted by the system
// roots, the TLS handshake fails, and the shim FAILS CLOSED — the client gets a
// 502 (the propagated upstream failure), never a synthesized allow, and the target
// handler is never reached. It also asserts the failed request did not draw nor
// charge an AnyRouter account (the global-target gate holds even on a TLS error).
func TestClassifierTargetTLSVerifyFailsClosed(t *testing.T) {
	target, hits := newRecordingTLSTarget(t)
	cfg := tlsTargetConfig(target.URL, "", false) // no CA, no skip ⇒ verify against system roots

	server := newTestAdminServer(t, cfg, "")
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Fail closed: the propagated upstream failure, never a synthesized verdict.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (TLS verification failure must propagate, not be masked); body %s", resp.StatusCode, out)
	}
	if bytes.Contains(out, []byte("block")) {
		t.Fatalf("a TLS failure must never synthesize a <block> verdict, got %s", out)
	}
	// The handshake fails before any HTTP request reaches the target, so the handler
	// is never invoked — there is no upstream response to (mis)interpret as allow.
	if *hits != 0 {
		t.Fatalf("TLS target hits = %d, want 0 (handshake must fail before the request is served)", *hits)
	}

	// The global-target gate holds on the failure path too: no account was assigned
	// (no session pinned) nor charged (no failures / cooldown) — the third-party
	// host's TLS failure must not be attributed to an AnyRouter account.
	pool, ok := server.currentRoutingTable().routes[0].credentials.(*accountPool)
	if !ok {
		t.Fatalf("route[0] credentials = %T, want *accountPool", server.currentRoutingTable().routes[0].credentials)
	}
	pool.mu.Lock()
	sessions := len(pool.sessions)
	failures := pool.accounts[0].failures
	cooldown := pool.accounts[0].cooldownUntil
	pool.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("account sessions pinned = %d, want 0 (no account assigned for a global target)", sessions)
	}
	if failures != 0 || !cooldown.IsZero() {
		t.Fatalf("account charged failures = %d cooldown = %v, want none (TLS failure must not be charged to an account)", failures, cooldown)
	}
}
