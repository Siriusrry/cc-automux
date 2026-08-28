package shim

// TestGatewayContract consolidates the gateway's eleven primary request classes
// into one reproducible fake-upstream regression. Each behavior is a named
// subtest driven end-to-end
// through ServeHTTP against recording fake upstreams (no live network), reusing
// the existing test builders (testProxyServerFromRoutes / classifierTestServer /
// cpaProxyServer / newTestAdminServer / newAccountPool / staticKeyProvider).
//
// The grouping covers six prefix-routed request classes, three global-target
// cases, an admin hot reload, and a classifier model override. The independent
// classifier target matrix test covers every platform × model-family cell.
// A Claude classifier sent to a non-AnyRouter destination is forwarded unchanged;
// the identity cloak runs only when the resolved platform is AnyRouter.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestGatewayContract(t *testing.T) {
	// 普通 /any: account round-robin across new sessions, sticky on repeat,
	// thinking.disabled→adaptive promotion, client x-api-key stripped + Authorization
	// replaced by the shim account key, and NO identity cloak (system block count
	// unchanged) because a non-classifier request is never rewritten as a classifier.
	t.Run("normal_any_rotation_sticky_thinking_no_cloak", func(t *testing.T) {
		type hit struct {
			session string
			auth    string
			apiKey  string
			body    []byte
		}
		var mu sync.Mutex
		var hits []hit
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			hits = append(hits, hit{
				session: r.Header.Get(sessionHeader),
				auth:    r.Header.Get("Authorization"),
				apiKey:  r.Header.Get("X-Api-Key"),
				body:    body,
			})
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		const accounts = 3
		pool := make([]accountEntry, 0, accounts)
		for i := 0; i < accounts; i++ {
			pool = append(pool, accountEntry{Label: "acct-" + strconv.Itoa(i), Key: "key-" + strconv.Itoa(i)})
		}
		server := testProxyServerFromRoutes([]proxyRoute{{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
			credentials: newAccountPool(pool),
			client:      upstream.Client(),
			strategy:    anyRouterStrategy{},
		}})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		// Non-classifier body: system[0] is neither the security-monitor marker (so it
		// is never detected as a classifier) nor the identity marker (so the no-cloak
		// assertion below is not masked by a coincidental identityMarker substring). It
		// carries thinking.disabled so the /any promotion is exercised.
		normalBody := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"disabled"},` +
			`"system":[{"type":"text","text":"You are a helpful coding assistant."}],"messages":[]}`)
		doRequest := func(session string) {
			t.Helper()
			req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(normalBody))
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

		for i := 0; i < 6; i++ {
			doRequest("session-" + strconv.Itoa(i))
		}
		doRequest("session-0") // repeat → must stick to its first account

		mu.Lock()
		defer mu.Unlock()
		if len(hits) != 7 {
			t.Fatalf("upstream saw %d requests, want 7", len(hits))
		}
		sessionKey := map[string]string{}
		for _, h := range hits {
			if h.apiKey != "" {
				t.Fatalf("session %s: upstream X-Api-Key = %q, want stripped", h.session, h.apiKey)
			}
			if !strings.HasPrefix(h.auth, "Bearer key-") {
				t.Fatalf("session %s: upstream Authorization = %q, want a shim account key (Bearer key-*)", h.session, h.auth)
			}
			// thinking.disabled→adaptive promotion on every normal /any request.
			if !bytes.Contains(h.body, []byte(`"adaptive"`)) || bytes.Contains(h.body, []byte(`"disabled"`)) {
				t.Fatalf("session %s: upstream body not promoted to thinking.adaptive: %s", h.session, h.body)
			}
			// No identity cloak: a non-classifier request never gets the marker, and the
			// system block count is unchanged (1, exactly what the client sent).
			if bytes.Contains(h.body, []byte(identityMarker)) {
				t.Fatalf("session %s: normal request must not be cloaked: %s", h.session, h.body)
			}
			var m map[string]any
			if err := json.Unmarshal(h.body, &m); err != nil {
				t.Fatalf("session %s: upstream body not JSON: %v", h.session, err)
			}
			if sys, _ := m["system"].([]any); len(sys) != 1 {
				t.Fatalf("session %s: system blocks = %d, want 1 (no cloak prepend)", h.session, len(sys))
			}
			if prev, ok := sessionKey[h.session]; ok {
				if prev != h.auth {
					t.Fatalf("session %s not sticky: saw %q then %q", h.session, prev, h.auth)
				}
			} else {
				sessionKey[h.session] = h.auth
			}
		}
		keyCounts := map[string]int{}
		for _, k := range sessionKey {
			keyCounts[k]++
		}
		if len(keyCounts) != accounts {
			t.Fatalf("distinct sessions spread over %d accounts, want all %d: %v", len(keyCounts), accounts, keyCounts)
		}
		for k, c := range keyCounts {
			if c != 2 {
				t.Fatalf("account %s pinned %d sessions, want balanced 2: %v", k, c, keyCounts)
			}
		}
		if sessionKey["session-0"] != "Bearer key-0" {
			t.Fatalf("session-0 pinned to %q, want Bearer key-0", sessionKey["session-0"])
		}
	})

	// 分类器 /any claude: the anthropic profile applies the AnyRouter identity
	// cloak (a prepended identity-marker system block + a correction line before the
	// original security prompt) AND deletes thinking.disabled, the account pool also
	// covers this classifier request (shim account key, client creds stripped), and
	// the response is STREAMED back verbatim (no GPT reassembly).
	t.Run("classifier_any_claude_cloak_delete_thinking_rotation_stream", func(t *testing.T) {
		var gotAuth, gotAPIKey string
		var gotBody []byte
		streamedBody := []byte(`{"type":"message","content":[{"type":"text","text":"<block>yes</block>"}]}`)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(streamedBody)
		}))
		defer upstream.Close()

		server := testProxyServerFromRoutes([]proxyRoute{{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
			credentials: newAccountPool([]accountEntry{{Label: "acct-0", Key: "key-0"}}),
			client:      upstream.Client(),
			strategy:    anyRouterStrategy{},
		}})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Cloak: identity marker prepended as system[0], the client's original security
		// prompt corrected as system[1], and the system block count grew from 1 to 2.
		// (correctionText carries real newlines that JSON escapes, so the correction is
		// checked on the decoded string, not the raw body bytes.)
		if !bytes.Contains(gotBody, []byte(identityMarker)) {
			t.Fatalf("claude classifier on /any must be cloaked with the identity marker, got %s", gotBody)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		sys, _ := m["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("system blocks = %d, want 2 (identity marker prepended to the client's 1)", len(sys))
		}
		if marker, _ := sys[0].(map[string]any); marker["text"] != identityMarker {
			t.Fatalf("system[0] = %v, want the identity marker block", sys[0])
		}
		if corrected, _ := sys[1].(map[string]any); !strings.HasPrefix(corrected["text"].(string), correctionText) {
			t.Fatalf("system[1] must begin with the correction text, got %v", sys[1])
		}
		// thinking.disabled deleted (the AnyRouter strict-path quirk).
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking must be deleted for the AnyRouter claude classifier, got %v", m["thinking"])
		}
		// Account rotation covers the classifier request: shim key in, client creds out.
		if gotAuth != "Bearer key-0" {
			t.Fatalf("upstream Authorization = %q, want Bearer key-0 (account pool covers classifier /any)", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
		// Streamed verbatim: the anthropic profile must not reassemble (no stop_reason
		// injection), so the client sees the upstream body byte-for-byte.
		if !bytes.Equal(clientOut, streamedBody) {
			t.Fatalf("anthropic profile must stream the upstream body verbatim:\n want %s\n got  %s", streamedBody, clientOut)
		}
		if bytes.Contains(clientOut, []byte("stop_reason")) || bytes.Contains(clientOut, []byte("stop_sequence")) {
			t.Fatalf("anthropic profile must not inject reassembly fields, got %s", clientOut)
		}
	})

	// 分类器 /any gpt: the openai profile strips Accept-Encoding (so the
	// reassembler gets uncompressed bytes), does NOT cloak (no identity marker) and
	// does NOT isolate the session (no CPA session header, body session_id untouched)
	// because the destination is AnyRouter not CPA, the account pool still covers it,
	// and the GPT-style response is reassembled (here truncated at </block> →
	// "<block>no", stop_reason stop_sequence).
	t.Run("classifier_any_gpt_reassemble_strip_ae_no_cloak_no_isolation_rotation", func(t *testing.T) {
		var gotAuth, gotAPIKey, gotAcceptEncoding, gotSession string
		var gotBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotAcceptEncoding = r.Header.Get("Accept-Encoding")
			gotSession = r.Header.Get(sessionHeader)
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
		}))
		defer upstream.Close()

		server := testProxyServerFromRoutes([]proxyRoute{{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
			credentials: newAccountPool([]accountEntry{{Label: "acct-0", Key: "key-0"}}),
			// DisableCompression mirrors the real AnyRouter client so a Del'd
			// Accept-Encoding is not re-added by the transport (faithful strip check).
			client:   &http.Client{Transport: &http.Transport{DisableCompression: true}},
			strategy: anyRouterStrategy{},
		}})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		req.Header.Set("Accept-Encoding", "gzip") // something to strip
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if gotAcceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, upstream saw %q", gotAcceptEncoding)
		}
		if gotSession != "" {
			t.Fatalf("GPT classifier on AnyRouter must NOT get a CPA session header, got %q", gotSession)
		}
		if bytes.Contains(gotBody, []byte(identityMarker)) {
			t.Fatalf("openai profile must not add the anthropic identity marker: %s", gotBody)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		// No isolation: the body's session_id is left as the client sent it.
		uidRaw, _ := m["metadata"].(map[string]any)["user_id"].(string)
		if !strings.Contains(uidRaw, "orig-session") {
			t.Fatalf("session must not be rewritten on a non-CPA destination, got %s", uidRaw)
		}
		// Account rotation covers the classifier request.
		if gotAuth != "Bearer key-0" {
			t.Fatalf("upstream Authorization = %q, want Bearer key-0 (account pool covers classifier /any)", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
		// Reassembled: GPT-style body collapsed to a single text block, truncated at
		// the stop sequence, with the synthesized stop_reason.
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
	})

	// 普通 /cpa: verbatim passthrough with the cpa.key shim-owned auth
	// override, and (unlike /any) thinking.disabled is NOT promoted — the body
	// reaches CPA byte-for-byte. CPA is single-key, so there is no rotation.
	t.Run("normal_cpa_passthrough_cpakey_thinking_preserved", func(t *testing.T) {
		var gotAuth, gotAPIKey string
		var gotBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		proxy := httptest.NewServer(cpaProxyServer(t, upstream, "shim-cpa-token"))
		defer proxy.Close()

		// thinking.disabled on a non-classifier body: on /cpa it must be preserved
		// (the promotion is an /any-only fix), so the body passes through unchanged.
		normalBody := []byte(`{"model":"gpt-5.5","thinking":{"type":"disabled"},` +
			`"system":[{"type":"text","text":"You are a helpful coding assistant."}],"messages":[]}`)
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages", bytes.NewReader(normalBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if gotAuth != "Bearer shim-cpa-token" {
			t.Fatalf("upstream Authorization = %q, want Bearer shim-cpa-token", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
		if !bytes.Equal(gotBody, normalBody) {
			t.Fatalf("cpa passthrough body changed:\n want %s\n got  %s", normalBody, gotBody)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		if th, _ := m["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
			t.Fatalf("thinking must stay disabled on /cpa (no promotion), got %v", m["thinking"])
		}
	})

	// 分类器 /cpa gpt: the openai profile isolates the CPA session (a fresh
	// UUID written to BOTH the session header and the body's metadata.user_id
	// session_id, consistently, preserving device_id), applies the cpa.key auth
	// override, strips Accept-Encoding, and reassembles the GPT-style response.
	t.Run("classifier_cpa_gpt_isolation_cpakey_strip_ae_reassemble", func(t *testing.T) {
		var gotAuth, gotAPIKey, gotAcceptEncoding, gotSession string
		var gotBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotAcceptEncoding = r.Header.Get("Accept-Encoding")
			gotSession = r.Header.Get(sessionHeader)
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
		}))
		defer upstream.Close()

		server := testProxyServerFromRoutes([]proxyRoute{{
			name:        "cliproxy",
			prefix:      cliproxyPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
			credentials: staticKeyProvider{key: "shim-cpa-token"},
			// DisableCompression mirrors the real CPA client (faithful strip check).
			client:   &http.Client{Transport: &http.Transport{DisableCompression: true}},
			strategy: cliproxyStrategy{},
		}})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
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

		// Session isolation: the header was rewritten to a fresh id (not orig-session).
		if gotSession == "" || gotSession == "orig-session" {
			t.Fatalf("CPA session header not isolated, got %q", gotSession)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		uidRaw, _ := m["metadata"].(map[string]any)["user_id"].(string)
		var uid map[string]any
		if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
			t.Fatalf("user_id not JSON: %v", err)
		}
		if uid["session_id"] == "orig-session" {
			t.Fatal("body session_id was not isolated")
		}
		if uid["session_id"] != gotSession {
			t.Fatalf("header session %q != body session %v (must be consistent)", gotSession, uid["session_id"])
		}
		if uid["device_id"] != "dev-1" {
			t.Fatalf("device_id must be preserved, got %v", uid["device_id"])
		}
		// cpa.key auth override + Accept-Encoding strip.
		if gotAuth != "Bearer shim-cpa-token" {
			t.Fatalf("upstream Authorization = %q, want Bearer shim-cpa-token", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
		if gotAcceptEncoding != "" {
			t.Fatalf("openai profile must strip Accept-Encoding, upstream saw %q", gotAcceptEncoding)
		}
		// Reassembled response.
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
	})

	// A Claude classifier on /cpa selects the anthropic profile, while the
	// AnyRouter cloak/delete-thinking behavior remains gated by the
	// destination platform, which is platformCPA (not AnyRouter) for /cpa — so the
	// body is forwarded UNCHANGED (no identity marker, thinking.disabled preserved,
	// system block count 1) and streamed back verbatim, with no CPA session
	// isolation, under the cpa.key. This locks the same contract as
	// TestClassifierEmptyTargetFallsBackPerPrefix.
	t.Run("classifier_cpa_claude_no_cloak_no_isolation_cpakey_stream", func(t *testing.T) {
		var gotAuth, gotAPIKey, gotSession string
		var gotBody []byte
		streamedBody := []byte(`{"type":"message","content":[{"type":"text","text":"<block>yes</block>"}]}`)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotSession = r.Header.Get(sessionHeader)
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(streamedBody)
		}))
		defer upstream.Close()

		server := testProxyServerFromRoutes([]proxyRoute{{
			name:        "cliproxy",
			prefix:      cliproxyPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
			credentials: staticKeyProvider{key: "shim-cpa-token"},
			client:      upstream.Client(),
			strategy:    cliproxyStrategy{},
		}})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Not cloaked (platform-aware behavior): no identity marker, no correction text, thinking.disabled
		// preserved, single system block (unchanged from the client).
		if bytes.Contains(gotBody, []byte(identityMarker)) || bytes.Contains(gotBody, []byte("Ignore the preceding identity marker")) {
			t.Fatalf("claude classifier to /cpa must NOT be cloaked (platform-aware behavior), got %s", gotBody)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		if th, _ := m["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
			t.Fatalf("thinking.disabled must be preserved to /cpa (platform-aware behavior), got %v", m["thinking"])
		}
		if sys, _ := m["system"].([]any); len(sys) != 1 {
			t.Fatalf("system blocks = %d, want 1 (no cloak prepend), body %s", len(sys), gotBody)
		}
		// No CPA session isolation for the anthropic profile.
		if gotSession != "" {
			t.Fatalf("claude classifier (anthropic profile) to /cpa must not get a session header, got %q", gotSession)
		}
		// cpa.key auth override still applies.
		if gotAuth != "Bearer shim-cpa-token" {
			t.Fatalf("upstream Authorization = %q, want Bearer shim-cpa-token", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
		// Streamed verbatim, not reassembled.
		if !bytes.Equal(clientOut, streamedBody) {
			t.Fatalf("anthropic profile must stream the upstream body verbatim:\n want %s\n got  %s", streamedBody, clientOut)
		}
		if bytes.Contains(clientOut, []byte("stop_reason")) || bytes.Contains(clientOut, []byte("stop_sequence")) {
			t.Fatalf("anthropic profile must not inject reassembly fields, got %s", clientOut)
		}
	})

	// 全局分类器目标 (claude): with a global target configured, a claude
	// classifier on /any is captured by the target host using the configured
	// target_key (client creds stripped), the route's credentialProvider is never
	// consulted (no account rotation, no charge), and — because the target is not
	// AnyRouter — the body is forwarded UNCHANGED (no cloak) and streamed.
	t.Run("global_target_claude_target_key_no_rotation_no_cloak", func(t *testing.T) {
		var gotAuth, gotAPIKey string
		var gotBody []byte
		targetHits := 0
		streamedBody := []byte(`{"type":"message","content":[{"type":"text","text":"<block>yes</block>"}]}`)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits++
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(streamedBody)
		}))
		defer target.Close()

		prov := &recordingProvider{key: "sk-account"}
		server := classifierTestServer(t, classifierRuntimeConfig{
			TargetBaseURL: target.URL,
			TargetKey:     "shim-target-key",
			TargetType:    "generic", // explicit: this is a generic third-party target (no cloak)
		}, target, proxyRoute{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, "https://anyrouter.invalid")}),
			credentials: prov,
			client:      target.Client(),
			strategy:    anyRouterStrategy{},
		})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if targetHits != 1 {
			t.Fatalf("global target hits = %d, want 1", targetHits)
		}
		if gotAuth != "Bearer shim-target-key" {
			t.Fatalf("global target Authorization = %q, want Bearer shim-target-key", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("global target X-Api-Key = %q, want stripped", gotAPIKey)
		}
		// No account rotation: the route's credentialProvider is never consulted.
		if assigns, reports := prov.counts(); assigns != 0 || reports != 0 {
			t.Fatalf("global target consulted the account pool: assigns=%d reports=%d, want 0/0", assigns, reports)
		}
		// claude → non-AnyRouter target is forwarded UNCHANGED.
		if bytes.Contains(gotBody, []byte(identityMarker)) {
			t.Fatalf("claude classifier to a non-AnyRouter target must not be cloaked, got %s", gotBody)
		}
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("target body not JSON: %v", err)
		}
		if th, _ := m["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
			t.Fatalf("claude classifier to a global target must keep thinking.disabled, got %v", m["thinking"])
		}
		if sys, _ := m["system"].([]any); len(sys) != 1 {
			t.Fatalf("system blocks = %d, want 1 (unchanged), body %s", len(sys), gotBody)
		}
		// Streamed verbatim, not reassembled.
		if !bytes.Equal(clientOut, streamedBody) {
			t.Fatalf("claude global-target response must be streamed verbatim:\n want %s\n got  %s", streamedBody, clientOut)
		}
	})

	// 全局分类器目标 (gpt): a gpt classifier on /any is also captured by the
	// target with the target_key (no rotation), keeps its openai profile (no
	// isolation since the target is not CPA, session untouched), and the GPT-style
	// response is reassembled.
	t.Run("global_target_gpt_target_key_no_rotation_reassemble", func(t *testing.T) {
		var gotAuth, gotAPIKey, gotSession string
		var gotBody []byte
		targetHits := 0
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits++
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotSession = r.Header.Get(sessionHeader)
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
		}))
		defer target.Close()

		prov := &recordingProvider{key: "sk-account"}
		server := classifierTestServer(t, classifierRuntimeConfig{
			TargetBaseURL: target.URL,
			TargetKey:     "shim-target-key",
			TargetType:    "generic", // explicit: generic third-party target (no isolation since not CPA)
		}, target, proxyRoute{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, "https://anyrouter.invalid")}),
			credentials: prov,
			client:      target.Client(),
			strategy:    anyRouterStrategy{},
		})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if targetHits != 1 {
			t.Fatalf("global target hits = %d, want 1", targetHits)
		}
		if gotAuth != "Bearer shim-target-key" {
			t.Fatalf("global target Authorization = %q, want Bearer shim-target-key", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("global target X-Api-Key = %q, want stripped", gotAPIKey)
		}
		if assigns, reports := prov.counts(); assigns != 0 || reports != 0 {
			t.Fatalf("global target consulted the account pool: assigns=%d reports=%d, want 0/0", assigns, reports)
		}
		// No isolation (target is not CPA): no session header, body session untouched.
		if gotSession != "" {
			t.Fatalf("gpt classifier to a non-CPA global target must not get a session header, got %q", gotSession)
		}
		if !bytes.Contains(gotBody, []byte("orig-session")) {
			t.Fatalf("session must not be isolated on a non-CPA target, body %s", gotBody)
		}
		// Reassembled.
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
	})

	// 全局目标:普通 /any 不受影响. A global classifier target captures only
	// detected CLASSIFIER requests; a NORMAL (non-classifier) /any request still
	// routes by prefix to the AnyRouter upstream (never the target) and still draws
	// the shim account key from the rotation pool.
	t.Run("global_target_normal_any_unaffected_prefix_rotation", func(t *testing.T) {
		var anyHits int
		var gotAuth, gotAPIKey string
		anyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			anyHits++
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(`{"ok":"any"}`))
		}))
		defer anyUpstream.Close()
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("global classifier target must NOT be hit by normal (non-classifier) /any traffic")
		}))
		defer target.Close()

		server := classifierTestServer(t, classifierRuntimeConfig{
			TargetBaseURL: target.URL,
			TargetKey:     "sk-target",
		}, target, proxyRoute{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool([]*url.URL{mustURL(t, anyUpstream.URL)}),
			credentials: newAccountPool([]accountEntry{{Label: "acct-1", Key: "sk-account"}}),
			client:      anyUpstream.Client(),
			strategy:    anyRouterStrategy{},
		})
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		// Non-classifier body (no security-monitor system prefix).
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Api-Key", "client-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if anyHits != 1 {
			t.Fatalf("AnyRouter prefix upstream hits = %d, want 1 (normal traffic must hit the prefix)", anyHits)
		}
		if string(out) != `{"ok":"any"}` {
			t.Fatalf("client body = %s, want the AnyRouter prefix upstream's body", out)
		}
		// Drew the shim account key (rotation), not the target key nor the client's.
		if gotAuth != "Bearer sk-account" {
			t.Fatalf("upstream Authorization = %q, want Bearer sk-account (from the account pool)", gotAuth)
		}
		if gotAPIKey != "" {
			t.Fatalf("upstream X-Api-Key = %q, want stripped", gotAPIKey)
		}
	})

	// /admin 热更新: POST /admin/config hot-applies without a restart (listen
	// unchanged → restart_required:false) and SUBSEQUENT requests follow the new
	// routing. Proven by setting a global classifier target via POST: before, a
	// classifier falls back to the AnyRouter prefix upstream; after, the same
	// request reroutes to the target — no process restart.
	t.Run("admin_hot_reload_reroutes_no_restart", func(t *testing.T) {
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

		cfg := baseAdminConfig()
		cfg.AnyRouter.Entrances = []string{anyUpstream.URL}
		server := newTestAdminServer(t, cfg, filepath.Join(t.TempDir(), "config.json"))

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

		// Before hot-reload: classifier falls back to the AnyRouter prefix upstream.
		if got := sendClassifier(); got != `{"ok":"any"}` {
			t.Fatalf("pre-reload classifier body = %s, want the AnyRouter prefix upstream's", got)
		}

		// Hot-reload: POST a config that sets the global classifier target, same listen_addr.
		c := *cfg
		c.Classifier = classifierRuntimeConfig{TargetBaseURL: target.URL}
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		rec := postAdminConfig(t, server, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /admin/config status = %d (%s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			RestartRequired bool `json:"restart_required"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode POST response: %v", err)
		}
		if resp.RestartRequired {
			t.Fatal("unchanged listen_addr must hot-apply without a restart (restart_required=false)")
		}

		// After hot-reload: the SAME classifier request now reroutes to the target.
		if got := sendClassifier(); got != `{"ok":"target"}` {
			t.Fatalf("post-reload classifier body = %s, want the global target's", got)
		}
		if anyHits != 1 || targetHits != 1 {
			t.Fatalf("hits after hot-reload: anyHits=%d targetHits=%d, want 1/1", anyHits, targetHits)
		}
	})

	// classifier.model_override: overriding a claude classifier body to an
	// o-series model (o3) flips it to the openai profile; routed to /cpa the
	// destination is CPA, so the session is isolated and the GPT-style response is
	// reassembled. Proves the override changes BOTH the request model and the
	// downstream profile/adaptation, not just the model string.
	t.Run("classifier_model_override_claude_to_o3_openai_isolation_reassemble", func(t *testing.T) {
		var gotSession string
		var gotBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotSession = r.Header.Get(sessionHeader)
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
		}))
		defer upstream.Close()

		server := classifierTestServer(t,
			classifierRuntimeConfig{ModelOverride: "o3"},
			nil,
			classifierRoute(cliproxyPrefix, "cliproxy", upstream, cliproxyStrategy{}),
		)
		proxy := httptest.NewServer(server)
		defer proxy.Close()

		// The client sends a CLAUDE body; the override rewrites it to o3.
		resp, err := http.Post(proxy.URL+"/cpa/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
		clientOut, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		// Override applied: the request model is o3, not the client's claude.
		if m["model"] != "o3" {
			t.Fatalf("upstream model = %v, want overridden o3", m["model"])
		}
		// openai profile (not anthropic): no identity marker cloak.
		if bytes.Contains(gotBody, []byte(identityMarker)) {
			t.Fatalf("override to o3 must use the openai profile (no anthropic cloak), got %s", gotBody)
		}
		// Session isolation (destination is CPA): header rewritten and consistent with body.
		if gotSession == "" || gotSession == "orig-session" {
			t.Fatalf("override to o3 on /cpa must isolate the session, header = %q", gotSession)
		}
		uidRaw, _ := m["metadata"].(map[string]any)["user_id"].(string)
		var uid map[string]any
		if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
			t.Fatalf("user_id not JSON: %v", err)
		}
		if uid["session_id"] != gotSession {
			t.Fatalf("body session_id %v != header session %q (isolation must be consistent)", uid["session_id"], gotSession)
		}
		// Reassembled response.
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
	})
}
