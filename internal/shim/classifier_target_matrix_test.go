package shim

// Classifier-target platform × model-family behavior coverage.
//
// This file covers two layers that config compilation's
// classifier_platform_test.go (config/compile) and the existing
// classifier_strategy_test.go (prefix-derived dispatch) do NOT:
//
//   - TestDispatchClassifierGlobalTargetTypeDrivesStrategy — the dispatch-level
//     resolution unit: a configured global target's resolved platform
//     (classifierTargetType) drives classifierProfileStrategy.{profile,platform}
//     for ALL THREE types (not just generic) and WINS over the matched prefix.
//
//   - TestServeHTTPGlobalTargetPlatformMatrix — the end-to-end behavior of global
//     classifier targets, driven through the REAL
//     compileRuntimeConfig → buildRoutingTable → ServeHTTP chain against a
//     recording fake upstream, asserting the per-platform request adaptation
//     (cloak / isolation / reassembly / strip-AE / pass-through) actually happens.
//     Every cell also proves the "global target never draws an account" invariant.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestDispatchClassifierGlobalTargetTypeDrivesStrategy closes a dispatch coverage
// gap: prove the global target's resolved platform drives the strategy
// for every type (anyrouter / cpa / generic), not only generic. We deliberately
// dispatch on the /any route (prefix → platformAnyRouter), so any resulting
// strategy platform other than anyrouter is itself proof that the configured
// global type overrode the prefix. It also pins the global-target invariants that
// must hold for EVERY type: globalTarget=true (so ServeHTTP skips the account
// pool), the target key filled, and the destination redirected to the target URL.
func TestDispatchClassifierGlobalTargetTypeDrivesStrategy(t *testing.T) {
	anyRoute := &proxyRoute{name: "anyrouter", prefix: anyRouterPrefix}
	models := map[string]classifierProfileKind{
		"claude-opus-4-8": anthropicClassifierProfile,
		"gpt-5.5":         openAIClassifierProfile,
	}
	for _, plat := range []classifierPlatform{platformAnyRouter, platformCPA, platformGeneric} {
		for model, wantProfile := range models {
			state := &proxyState{
				runtime: &runtimeConfig{Classifier: classifierRuntimeConfig{
					TargetBaseURL: "https://c.example",
					TargetKey:     "sk-target",
				}},
				table: &routingTable{
					classifierTarget:     newUpstreamPool([]*url.URL{mustURL(t, "https://c.example")}),
					classifierClient:     &http.Client{},
					classifierTargetType: plat,
				},
			}
			got := dispatchClassifier(state, anyRoute, resolvedTarget{routeName: anyRoute.name}, "/v1/messages", classifierBodyWithModel(model))
			ps, ok := got.profile.(classifierProfileStrategy)
			if !ok {
				t.Fatalf("platform=%s model=%s: profile type = %T, want classifierProfileStrategy", platformName(plat), model, got.profile)
			}
			if ps.profile != wantProfile {
				t.Errorf("platform=%s model=%s: profile = %v, want %v", platformName(plat), model, ps.profile, wantProfile)
			}
			// The crux: the strategy platform equals the configured global type, NOT
			// platformAnyRouter from the /any prefix the request arrived on.
			if ps.platform != plat {
				t.Errorf("platform=%s model=%s: strategy.platform = %s, want %s (global type must drive, overriding the /any prefix)",
					platformName(plat), model, platformName(ps.platform), platformName(plat))
			}
			// globalTarget gate must be set for every type — including anyrouter — so the
			// account pool is skipped (the gate is orthogonal to the platform).
			if !got.globalTarget {
				t.Errorf("platform=%s model=%s: globalTarget = false, want true (account pool must be skipped for any typed global target)", platformName(plat), model)
			}
			if got.key != "sk-target" {
				t.Errorf("platform=%s model=%s: key = %q, want sk-target", platformName(plat), model, got.key)
			}
			if got.baseURLs == nil || got.baseURLs.url(0).String() != "https://c.example" {
				t.Errorf("platform=%s model=%s: destination not redirected to the global target URL", platformName(plat), model)
			}
		}
	}
}

// recordedTarget captures what the recording fake global classifier target saw on
// its single request. It is read only AFTER the client response body is fully
// consumed: the HTTP round-trip establishes happens-before (the handler's writes
// precede the response bytes the client reads), so no lock is needed — the same
// pattern used by the other acceptance tests.
type recordedTarget struct {
	hits           int
	auth           string
	apiKey         string
	acceptEncoding string
	session        string
	body           []byte
}

// serveClassifierViaGlobalTarget runs a classifier of the given model on the given
// prefix through the REAL compileRuntimeConfig → buildRoutingTable → ServeHTTP
// chain (newTestAdminServer compiles + builds the routing table, so the global
// target client is the real newTargetHTTPClient with DisableCompression). mkCfg
// receives the live recording-target URL so a cell can pin target_type and, for the
// auto local-CPA cell, set cpa.upstream to the same URL.
//
// The config built by mkCfg MUST carry an AnyRouter account pool with the key
// "sk-rotation-key" and an EMPTY classifier target_key: the request always sends
// the client's own creds, so the helper can assert — for every cell — that a typed
// global target never draws an account (the client's auth flows through unchanged
// rather than being replaced by the rotation key). The inbound request also always
// carries Accept-Encoding: gzip so gpt cells can assert the openai-profile strip.
func serveClassifierViaGlobalTarget(t *testing.T, mkCfg func(targetURL string) *runtimeConfig, prefix, model string, upstreamResp []byte) (*recordedTarget, []byte) {
	t.Helper()
	rec := &recordedTarget{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits++
		rec.auth = r.Header.Get("Authorization")
		rec.apiKey = r.Header.Get("X-Api-Key")
		rec.acceptEncoding = r.Header.Get("Accept-Encoding")
		rec.session = r.Header.Get(sessionHeader)
		rec.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamResp)
	}))
	defer target.Close()

	server := newTestAdminServer(t, mkCfg(target.URL), "")
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+prefix+"/v1/messages", bytes.NewReader(classifierBodyWithModel(model)))
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
	clientOut, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; client body %s", resp.StatusCode, clientOut)
	}
	if rec.hits != 1 {
		t.Fatalf("global target hits = %d, want 1 (classifier must reach the target exactly once)", rec.hits)
	}
	// Invariant for every cell: a global target never draws a route account; the
	// route has an account pool ("sk-rotation-key") but a global-target classifier
	// must never draw it. With an empty target_key the client's own creds flow to the
	// target unchanged — so seeing the client token (not the rotation key) proves the
	// account block was skipped.
	if rec.auth != "Bearer client-token" {
		t.Fatalf("global target Authorization = %q, want the client's Bearer client-token (a typed global target must not draw the account key sk-rotation-key)", rec.auth)
	}
	if rec.apiKey != "client-key" {
		t.Fatalf("global target X-Api-Key = %q, want the client's client-key forwarded (empty target_key ⇒ applyAccountAuth no-op)", rec.apiKey)
	}
	return rec, clientOut
}

// matrixCfg builds the common config skeleton for the global-target matrix: a
// valid AnyRouter route carrying a rotation account, an empty target_key, and the
// per-cell target_base_url / target_type / cpa.upstream.
func matrixCfg(targetURL, targetType, cpaUpstream string) *runtimeConfig {
	return &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter: anyRouterRuntimeConfig{
			Entrances: []string{"https://anyrouter.top"},
			Accounts:  []accountEntry{{Label: "acct-0", Key: "sk-rotation-key"}},
		},
		CPA: cpaRuntimeConfig{Upstream: cpaUpstream},
		Classifier: classifierRuntimeConfig{
			TargetBaseURL: targetURL,
			TargetType:    targetType,
			TargetKey:     "",
		},
	}
}

const defaultMatrixCPAUpstream = "https://127.0.0.1:8317"

// streamedClaudeBody is the plain anthropic verdict body a claude-family global
// target returns; the anthropic profile must stream it back to the client verbatim.
var streamedClaudeBody = []byte(`{"type":"message","content":[{"type":"text","text":"<block>yes</block>"}]}`)

// assertCloaked checks the AnyRouter strict-path cloak was applied to the body the
// target received: the identity marker is prepended as system[0] (block count grew
// from the client's 1 to 2) and thinking.disabled was deleted.
func assertCloaked(t *testing.T, body []byte) {
	t.Helper()
	if !bytes.Contains(body, []byte(identityMarker)) {
		t.Fatalf("expected the identity-marker cloak, got %s", body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("target body not JSON: %v", err)
	}
	sys, _ := m["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("system blocks = %d, want 2 (identity marker prepended), body %s", len(sys), body)
	}
	if marker, _ := sys[0].(map[string]any); marker["text"] != identityMarker {
		t.Fatalf("system[0] = %v, want the identity marker", sys[0])
	}
	if _, ok := m["thinking"]; ok {
		t.Fatalf("thinking must be deleted by the AnyRouter cloak, got %v", m["thinking"])
	}
}

// assertClaudeUnchanged checks a claude classifier reached a NON-AnyRouter target
// untouched: no identity marker, thinking.disabled preserved, exactly the one
// system block the client sent, and no CPA session header (the anthropic profile
// never isolates — Claude-to-CPA no-isolation rule).
func assertClaudeUnchanged(t *testing.T, rec *recordedTarget) {
	t.Helper()
	if bytes.Contains(rec.body, []byte(identityMarker)) || bytes.Contains(rec.body, []byte(correctionText)) {
		t.Fatalf("claude classifier to a non-AnyRouter target must not be cloaked, got %s", rec.body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.body, &m); err != nil {
		t.Fatalf("target body not JSON: %v", err)
	}
	if th, _ := m["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("claude classifier to a non-AnyRouter target must keep thinking.disabled, got %v", m["thinking"])
	}
	if sys, _ := m["system"].([]any); len(sys) != 1 {
		t.Fatalf("system blocks = %d, want 1 (unchanged), body %s", len(sys), rec.body)
	}
	if rec.session != "" {
		t.Fatalf("anthropic profile must not set a CPA session header, got %q", rec.session)
	}
}

// assertStreamedVerbatim checks the anthropic profile streamed the upstream body
// back byte-for-byte with no reassembly fields injected.
func assertStreamedVerbatim(t *testing.T, clientOut, want []byte) {
	t.Helper()
	if !bytes.Equal(clientOut, want) {
		t.Fatalf("anthropic profile must stream verbatim:\n want %s\n got  %s", want, clientOut)
	}
	if bytes.Contains(clientOut, []byte("stop_reason")) || bytes.Contains(clientOut, []byte("stop_sequence")) {
		t.Fatalf("anthropic profile must not inject reassembly fields, got %s", clientOut)
	}
}

// assertGPTReassembled checks the openai profile reassembled the GPT-style body
// into the auto-mode shape: truncated at </block> with a synthesized stop_sequence.
func assertGPTReassembled(t *testing.T, clientOut []byte) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(clientOut, &out); err != nil {
		t.Fatalf("client response not JSON (should be reassembled): %v", err)
	}
	if out["stop_reason"] != "stop_sequence" {
		t.Fatalf("reassembled stop_reason = %v, want stop_sequence", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "<block>no" {
		t.Fatalf("reassembled content = %v, want a single text block \"<block>no\"", content)
	}
}

// assertSessionIsolated checks CPA session isolation on the gpt body the target
// received: the session header is a fresh id (not orig-session), it matches the
// body's metadata.user_id.session_id, and device_id is preserved.
func assertSessionIsolated(t *testing.T, rec *recordedTarget) {
	t.Helper()
	if rec.session == "" || rec.session == "orig-session" {
		t.Fatalf("CPA session header not isolated, got %q", rec.session)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.body, &m); err != nil {
		t.Fatalf("target body not JSON: %v", err)
	}
	uidRaw, _ := m["metadata"].(map[string]any)["user_id"].(string)
	var uid map[string]any
	if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
		t.Fatalf("user_id not JSON: %v", err)
	}
	if uid["session_id"] == "orig-session" {
		t.Fatal("body session_id was not isolated")
	}
	if uid["session_id"] != rec.session {
		t.Fatalf("header session %q != body session %v (isolation must be consistent)", rec.session, uid["session_id"])
	}
	if uid["device_id"] != "dev-1" {
		t.Fatalf("device_id must be preserved, got %v", uid["device_id"])
	}
}

// assertSessionNotIsolated checks a gpt classifier on a NON-CPA target kept its
// session: no session header, and the body still carries orig-session.
func assertSessionNotIsolated(t *testing.T, rec *recordedTarget) {
	t.Helper()
	if rec.session != "" {
		t.Fatalf("gpt classifier on a non-CPA target must not get a session header, got %q", rec.session)
	}
	if !bytes.Contains(rec.body, []byte("orig-session")) {
		t.Fatalf("session must not be isolated on a non-CPA target, body %s", rec.body)
	}
}

// TestServeHTTPGlobalTargetPlatformMatrix drives global-target combinations
// end-to-end. Every request is sent on /any, so the prefix alone
// would imply platformAnyRouter; any non-anyrouter adaptation observed is itself
// proof that the configured target_type (or auto-detected platform) selected
// the adaptation, not the prefix.
func TestServeHTTPGlobalTargetPlatformMatrix(t *testing.T) {
	gptResp := cliproxyGPTStyleResponse("<block>no</block>")

	// AnyRouter + Claude: cloak, delete thinking, and stream.
	t.Run("anyrouter_claude_cloak_stream", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "anyrouter", defaultMatrixCPAUpstream)
		}, anyRouterPrefix, "claude-opus-4-8", streamedClaudeBody)
		assertCloaked(t, rec.body)
		assertStreamedVerbatim(t, clientOut, streamedClaudeBody)
	})

	// AnyRouter + GPT: no cloak, strip Accept-Encoding, no isolation,
	// reassembled (the openai profile never cloaks and isolation needs platform==cpa).
	t.Run("anyrouter_gpt_no_cloak_strip_ae_reassemble", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "anyrouter", defaultMatrixCPAUpstream)
		}, anyRouterPrefix, "gpt-5.5", gptResp)
		if bytes.Contains(rec.body, []byte(identityMarker)) {
			t.Fatalf("openai profile must never cloak, even on an anyrouter-typed target, got %s", rec.body)
		}
		if rec.acceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, target saw %q", rec.acceptEncoding)
		}
		assertSessionNotIsolated(t, rec)
		assertGPTReassembled(t, clientOut)
	})

	// Auto-detected local CPA + Claude: no
	// isolation (Claude-to-CPA no-isolation rule) and no cloak (platform != anyrouter), forwarded unchanged and
	// streamed. Proves the auto resolver classifies a local-CPA target as cpa.
	t.Run("cpa_local_auto_claude_no_isolation_unchanged_stream", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			// target_type "" = auto; cpa.upstream == target URL ⇒ auto resolves cpa.
			return matrixCfg(targetURL, "", targetURL)
		}, anyRouterPrefix, "claude-opus-4-8", streamedClaudeBody)
		assertClaudeUnchanged(t, rec)
		assertStreamedVerbatim(t, clientOut, streamedClaudeBody)
	})

	// Auto-detected local CPA + GPT: session isolation + strip AE +
	// reassembled.
	t.Run("cpa_local_auto_gpt_isolation_strip_ae_reassemble", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "", targetURL)
		}, anyRouterPrefix, "gpt-5.5", gptResp)
		assertSessionIsolated(t, rec)
		if rec.acceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, target saw %q", rec.acceptEncoding)
		}
		assertGPTReassembled(t, clientOut)
	})

	// Generic + Claude: no adaptation, streamed verbatim.
	t.Run("generic_claude_no_adaptation_stream", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "generic", defaultMatrixCPAUpstream)
		}, anyRouterPrefix, "claude-opus-4-8", streamedClaudeBody)
		assertClaudeUnchanged(t, rec)
		assertStreamedVerbatim(t, clientOut, streamedClaudeBody)
	})

	// Generic + GPT: strip AE + reassembled, no isolation.
	t.Run("generic_gpt_strip_ae_reassemble_no_isolation", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "generic", defaultMatrixCPAUpstream)
		}, anyRouterPrefix, "gpt-5.5", gptResp)
		if bytes.Contains(rec.body, []byte(identityMarker)) {
			t.Fatalf("generic target must not cloak a gpt classifier, got %s", rec.body)
		}
		if rec.acceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, target saw %q", rec.acceptEncoding)
		}
		assertSessionNotIsolated(t, rec)
		assertGPTReassembled(t, clientOut)
	})

	// Public CPA (explicit type=cpa, target URL != the configured cpa.upstream,
	// so auto could not detect it) + Claude: no isolation, no cloak,
	// streamed. Proves explicit target_type:"cpa" forces cpa adaptation on a target
	// the auto resolver would call generic.
	t.Run("public_cpa_explicit_claude_no_isolation_unchanged_stream", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "cpa", defaultMatrixCPAUpstream) // cpa.upstream != target
		}, anyRouterPrefix, "claude-opus-4-8", streamedClaudeBody)
		assertClaudeUnchanged(t, rec)
		assertStreamedVerbatim(t, clientOut, streamedClaudeBody)
	})

	// Public CPA (explicit type=cpa, URL != configured CPA) + GPT:
	// isolation + strip AE + reassembled.
	t.Run("public_cpa_explicit_gpt_isolation_strip_ae_reassemble", func(t *testing.T) {
		rec, clientOut := serveClassifierViaGlobalTarget(t, func(targetURL string) *runtimeConfig {
			return matrixCfg(targetURL, "cpa", defaultMatrixCPAUpstream) // cpa.upstream != target
		}, anyRouterPrefix, "gpt-5.5", gptResp)
		assertSessionIsolated(t, rec)
		if rec.acceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, target saw %q", rec.acceptEncoding)
		}
		assertGPTReassembled(t, clientOut)
	})
}
