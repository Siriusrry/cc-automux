package shim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func classifierBodyWithModel(model string) []byte {
	return []byte(`{"model":"` + model + `","max_tokens":64,"thinking":{"type":"disabled"},` +
		`"stop_sequences":["</block>"],` +
		`"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"session_id\":\"orig-session\"}"},` +
		`"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],` +
		`"messages":[]}`)
}

func classifierRoute(prefix, name string, upstream *httptest.Server, strategy proxyStrategy) proxyRoute {
	u, err := url.Parse(upstream.URL)
	if err != nil {
		panic(err)
	}
	return proxyRoute{
		name:      name,
		prefix:    prefix,
		upstreams: newUpstreamPool([]*url.URL{u}),
		client:    upstream.Client(),
		strategy:  strategy,
	}
}

func classifierTestServer(t *testing.T, cfg classifierRuntimeConfig, target *httptest.Server, routes ...proxyRoute) *proxyServer {
	t.Helper()
	table := &routingTable{routes: routes}
	if target != nil {
		u, err := url.Parse(target.URL)
		if err != nil {
			t.Fatalf("failed to parse classifier target URL: %v", err)
		}
		table.classifierTarget = newUpstreamPool([]*url.URL{u})
		table.classifierClient = target.Client()
		// Resolve the global target's platform from cfg.TargetType exactly as
		// compileRuntimeConfig would, so a test can DECLARE its target type via the
		// config field instead of poking table.classifierTargetType afterwards. With
		// no configured AnyRouter/CPA upstreams to match against, an empty/"auto" type
		// resolves to platformGeneric — the GENERIC third-party target these tests
		// model (an httptest server that is neither a configured AnyRouter entrance nor
		// the CPA upstream): no CPA isolation, no AnyRouter cloak. An explicit
		// "generic"/"anyrouter"/"cpa" forces that platform. Callers that omit
		// TargetType therefore model a generic third-party target.
		platform, err := resolveConfiguredClassifierPlatform(cfg.TargetType, u, nil, nil)
		if err != nil {
			t.Fatalf("resolve classifier target platform: %v", err)
		}
		table.classifierTargetType = platform
	}
	server := &proxyServer{}
	server.state.Store(&proxyState{
		runtime: &runtimeConfig{ListenAddr: defaultListenAddr, Classifier: cfg},
		table:   table,
	})
	return server
}

func TestClassifierProfileSelectsByModelFamily(t *testing.T) {
	cases := map[string]classifierProfileKind{
		"gpt-5.5":         openAIClassifierProfile,
		"GPT-4o":          openAIClassifierProfile,
		"o1-preview":      openAIClassifierProfile,
		"o2":              openAIClassifierProfile,
		"o3-mini":         openAIClassifierProfile,
		"o4-experimental": openAIClassifierProfile,
		"o5-pro":          openAIClassifierProfile,
		"claude-opus-4-8": anthropicClassifierProfile,
		"claude-sonnet-4": anthropicClassifierProfile,
		"gemini-1.5-pro":  anthropicClassifierProfile,
		"":                anthropicClassifierProfile,
	}
	for model, want := range cases {
		if got := classifierProfile(model); got != want {
			t.Errorf("classifierProfile(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestClassifierRequestModelHonorsOverride(t *testing.T) {
	body := map[string]any{"model": "claude-opus-4-8"}
	if got := classifierRequestModel(body, ""); got != "claude-opus-4-8" {
		t.Fatalf("no override should keep body model, got %q", got)
	}
	if got := classifierRequestModel(body, "gpt-5.5"); got != "gpt-5.5" {
		t.Fatalf("override should win, got %q", got)
	}
	if got := classifierRequestModel(map[string]any{}, ""); got != "" {
		t.Fatalf("missing model with no override should be empty, got %q", got)
	}
}

// TestClassifierModelOverrideReachesUpstream confirms the override rewrites the
// request model while keeping the anthropic profile rewrite (identity marker).
func TestClassifierModelOverrideReachesUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		if m["model"] != "claude-sonnet-4" {
			t.Fatalf("model = %v, want overridden claude-sonnet-4", m["model"])
		}
		if !bytes.Contains(body, []byte(identityMarker)) {
			t.Fatalf("anthropic profile should still add the identity marker: %s", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := classifierTestServer(t,
		classifierRuntimeConfig{ModelOverride: "claude-sonnet-4"},
		nil,
		classifierRoute(anyRouterPrefix, "anyrouter", upstream, anyRouterStrategy{}),
	)
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClassifierModelOverrideSwitchesProfile confirms overriding a Claude
// classifier to a GPT model flips it to the openai profile, and because the
// destination is AnyRouter (not CPA) no session isolation is applied while the
// response is still reassembled.
func TestClassifierModelOverrideSwitchesProfile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}
		if m["model"] != "gpt-5.5" {
			t.Fatalf("model = %v, want overridden gpt-5.5", m["model"])
		}
		if bytes.Contains(body, []byte(identityMarker)) {
			t.Fatalf("openai profile must not add the anthropic identity marker: %s", body)
		}
		if r.Header.Get(sessionHeader) != "" {
			t.Fatalf("GPT classifier on AnyRouter must not get CPA session header, got %q", r.Header.Get(sessionHeader))
		}
		uidRaw := m["metadata"].(map[string]any)["user_id"].(string)
		if !strings.Contains(uidRaw, "orig-session") {
			t.Fatalf("session must not be rewritten when destination is not CPA, got %s", uidRaw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>yes</block><reason>x</reason>"))
	}))
	defer upstream.Close()

	server := classifierTestServer(t,
		classifierRuntimeConfig{ModelOverride: "gpt-5.5"},
		nil,
		classifierRoute(anyRouterPrefix, "anyrouter", upstream, anyRouterStrategy{}),
	)
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client response not JSON (should be reassembled): %v", err)
	}
	if m["stop_reason"] != "stop_sequence" {
		t.Fatalf("openai profile response should be reassembled, stop_reason = %v", m["stop_reason"])
	}
}

// TestOpenAIClassifierIsolationOnlyForCPA covers the platform-keyed adaptation
// rules: openai session isolation applies only when the destination platform is
// CPA, and the AnyRouter cloak/delete-thinking applies only when the destination
// platform is AnyRouter. dispatch tags the strategy with the destination's
// platform — the matched prefix's platform when no global target is set, else the
// global target's resolved target_type.
func TestOpenAIClassifierIsolationOnlyForCPA(t *testing.T) {
	noTarget := &proxyState{
		runtime: &runtimeConfig{Classifier: classifierRuntimeConfig{}},
		table:   &routingTable{},
	}
	cpaRoute := &proxyRoute{name: "cliproxy", prefix: cliproxyPrefix}
	anyRoute := &proxyRoute{name: "anyrouter", prefix: anyRouterPrefix}

	// GPT classifier on /cpa with no global target → openai profile, platform CPA
	// (⇒ isolate). CPA is not AnyRouter, and the openai profile never cloaks anyway.
	got := dispatchClassifier(noTarget, cpaRoute, resolvedTarget{routeName: cpaRoute.name}, "/v1/messages", classifierBodyWithModel("gpt-5.5"))
	ps, ok := got.profile.(classifierProfileStrategy)
	if !ok || ps.profile != openAIClassifierProfile || ps.platform != platformCPA {
		t.Fatalf("GPT classifier to CPA: profile=%v platform=%s, want openai+cpa (isolate)", ps.profile, platformName(ps.platform))
	}

	// Claude classifier on /cpa → anthropic profile, platform CPA. The anthropic
	// profile never isolates, and platform != AnyRouter means no cloak: the body
	// is forwarded unchanged.
	got = dispatchClassifier(noTarget, cpaRoute, resolvedTarget{routeName: cpaRoute.name}, "/v1/messages", classifierBodyWithModel("claude-opus-4-8"))
	ps = got.profile.(classifierProfileStrategy)
	if ps.profile != anthropicClassifierProfile || ps.platform != platformCPA {
		t.Fatalf("Claude classifier to CPA: profile=%v platform=%s, want anthropic+cpa (no isolate, no cloak)", ps.profile, platformName(ps.platform))
	}

	// Claude classifier on /any with no global target → anthropic profile, platform
	// AnyRouter ⇒ the cloak path is enabled.
	got = dispatchClassifier(noTarget, anyRoute, resolvedTarget{routeName: anyRoute.name}, "/v1/messages", classifierBodyWithModel("claude-opus-4-8"))
	ps = got.profile.(classifierProfileStrategy)
	if ps.profile != anthropicClassifierProfile || ps.platform != platformAnyRouter {
		t.Fatalf("Claude classifier to /any: profile=%v platform=%s, want anthropic+anyrouter (cloak)", ps.profile, platformName(ps.platform))
	}

	// GPT classifier on /any routed to a GENERIC global target (not CPA, not
	// AnyRouter) → openai, platform generic ⇒ no isolation, no cloak, key filled.
	// The matched prefix is /any (platformAnyRouter), but the configured global
	// target's resolved platform (platformGeneric, set explicitly to lock this
	// intent) must win over the prefix — proving the platform override.
	target := &proxyState{
		runtime: &runtimeConfig{Classifier: classifierRuntimeConfig{TargetBaseURL: "https://c.example", TargetKey: "sk-target"}},
		table: &routingTable{
			classifierTarget:     newUpstreamPool([]*url.URL{mustURL(t, "https://c.example")}),
			classifierClient:     &http.Client{},
			classifierTargetType: platformGeneric,
		},
	}
	got = dispatchClassifier(target, anyRoute, resolvedTarget{routeName: anyRoute.name}, "/v1/messages", classifierBodyWithModel("gpt-5.5"))
	ps = got.profile.(classifierProfileStrategy)
	if ps.profile != openAIClassifierProfile || ps.platform != platformGeneric {
		t.Fatalf("GPT classifier to generic global target: profile=%v platform=%s, want openai+generic (no isolate, no cloak)", ps.profile, platformName(ps.platform))
	}
	if got.key != "sk-target" {
		t.Fatalf("global-target key = %q, want sk-target filled into resolvedTarget.key", got.key)
	}
	if got.baseURLs.url(0).String() != "https://c.example" {
		t.Fatalf("global-target upstream = %q, want https://c.example", got.baseURLs.url(0).String())
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// TestClassifierGlobalTargetCapturesBothProviders confirms a configured target
// captures classifier traffic from both prefixes, each keeping its model-family
// profile, and that the prefix's own upstream is bypassed. Because the global
// target is not AnyRouter, a claude classifier reaching it must be forwarded
// UNCHANGED (no identity-marker cloak, thinking.disabled preserved, system block
// count unchanged) — the platform-aware behavior orthogonal-decoupling behavior.
func TestClassifierGlobalTargetCapturesBothProviders(t *testing.T) {
	var targetHits int
	var claudeBody []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Branch by model family, not by marker presence: after platform-aware behavior the claude
		// classifier to this non-AnyRouter target is no longer cloaked, so the
		// marker is absent from both branches.
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if model, _ := m["model"].(string); strings.HasPrefix(model, "claude") {
			// Claude classifier (anthropic profile) — capture the body for the
			// no-cloak assertions below and stream a plain body back.
			claudeBody = body
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		// GPT classifier (openai profile) — return a GPT-style body to reassemble.
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
	}))
	defer target.Close()

	deadHit := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("%s default upstream must not be hit when a global target is set", name)
		}
	}
	anyUpstream := httptest.NewServer(deadHit("anyrouter"))
	defer anyUpstream.Close()
	cpaUpstream := httptest.NewServer(deadHit("cpa"))
	defer cpaUpstream.Close()

	server := classifierTestServer(t,
		// target_type:"generic" is set explicitly: this test models a generic
		// third-party global target (the platform-aware behavior no-cloak/no-isolation path). The field now
		// drives the resolved platform through classifierTestServer, so the generic
		// route is exercised by declared intent, not merely the helper's old default.
		classifierRuntimeConfig{TargetBaseURL: target.URL, TargetKey: "sk-target", TargetType: "generic"},
		target,
		classifierRoute(anyRouterPrefix, "anyrouter", anyUpstream, anyRouterStrategy{}),
		classifierRoute(cliproxyPrefix, "cliproxy", cpaUpstream, cliproxyStrategy{}),
	)
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Claude classifier via /any → anthropic profile to a NON-AnyRouter target:
	// passed through verbatim (streamed, not reassembled). The client must receive
	// the upstream body byte-for-byte with no stop_reason/stop_sequence injection.
	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("any request failed: %v", err)
	}
	anyOut, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("any classifier status = %d, want 200 from global target", resp.StatusCode)
	}
	if string(anyOut) != `{"ok":true}` {
		t.Fatalf("anthropic profile must pass the upstream body through verbatim, got %s", anyOut)
	}
	if bytes.Contains(anyOut, []byte("stop_reason")) || bytes.Contains(anyOut, []byte("stop_sequence")) {
		t.Fatalf("anthropic profile must not inject reassembly fields, got %s", anyOut)
	}

	// The claude classifier reached the global target UNCHANGED: no identity-marker
	// cloak, no correction text, thinking.disabled preserved, and the same number
	// of system blocks the client sent (the cloak would have prepended a marker
	// block, making 2).
	if bytes.Contains(claudeBody, []byte(identityMarker)) || bytes.Contains(claudeBody, []byte(correctionText)) {
		t.Fatalf("claude classifier to a non-AnyRouter target must not be cloaked, got %s", claudeBody)
	}
	var cm map[string]any
	if err := json.Unmarshal(claudeBody, &cm); err != nil {
		t.Fatalf("claude upstream body not JSON: %v", err)
	}
	if th, _ := cm["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("claude classifier to a non-AnyRouter target must keep thinking.disabled, got %v", cm["thinking"])
	}
	if sys, _ := cm["system"].([]any); len(sys) != 1 {
		t.Fatalf("claude classifier system blocks = %d, want 1 (unchanged from client), body %s", len(sys), claudeBody)
	}

	// GPT classifier via /cpa → openai profile, reassembled.
	resp2, err := http.Post(proxy.URL+"/cpa/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
	if err != nil {
		t.Fatalf("cpa request failed: %v", err)
	}
	out, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("cpa client response not JSON: %v", err)
	}
	if _, ok := m["content"]; !ok {
		t.Fatalf("openai-profile response should be reassembled message, got %s", out)
	}
	if targetHits != 2 {
		t.Fatalf("global target hits = %d, want 2 (both providers)", targetHits)
	}
}

// TestClassifierEmptyTargetFallsBackPerPrefix confirms that with no global
// target, classifier traffic stays on its prefix's upstream with the
// prefix-appropriate profile — exactly today's behavior.
func TestClassifierEmptyTargetFallsBackPerPrefix(t *testing.T) {
	var anyHits, cpaHits int
	anyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anyHits++
		body, _ := io.ReadAll(r.Body)
		// /any with no global target → destination IS AnyRouter, so the anthropic
		// profile must still cloak: identity marker prepended AND thinking.disabled
		// deleted (the AnyRouter strict-path quirk this rewrite exists for).
		if !bytes.Contains(body, []byte(identityMarker)) {
			t.Fatalf("anyrouter fallback should apply anthropic profile: %s", body)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("anyrouter upstream body not JSON: %v", err)
		}
		if _, ok := m["thinking"]; ok {
			t.Fatalf("anyrouter fallback should delete thinking.disabled, got %s", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer anyUpstream.Close()
	cpaUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpaHits++
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("cpa upstream body not JSON: %v", err)
		}
		if model, _ := m["model"].(string); strings.HasPrefix(model, "claude") {
			// Claude classifier on /cpa (anthropic profile): the destination is CPA,
			// not AnyRouter, so the cloak is not applied — the body must reach
			// the upstream UNCHANGED (no identity marker, no correction text,
			// thinking.disabled preserved, single system block), and the openai-only CPA
			// session isolation must not run, so no session header is set. This is the
			// Claude-to-CPA counterpart of the /any cloak arm above.
			if bytes.Contains(body, []byte(identityMarker)) || bytes.Contains(body, []byte(correctionText)) {
				t.Fatalf("claude classifier to /cpa must not be cloaked (platform-aware behavior), got %s", body)
			}
			if th, _ := m["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
				t.Fatalf("claude classifier to /cpa must keep thinking.disabled, got %v", m["thinking"])
			}
			if sys, _ := m["system"].([]any); len(sys) != 1 {
				t.Fatalf("claude classifier to /cpa system blocks = %d, want 1 (unchanged), body %s", len(sys), body)
			}
			if r.Header.Get(sessionHeader) != "" {
				t.Fatalf("claude classifier (anthropic profile) to /cpa must not get CPA session isolation, header = %q", r.Header.Get(sessionHeader))
			}
			// anthropic profile streams the upstream body through verbatim.
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		// GPT classifier on /cpa (openai profile): session isolation applies.
		if r.Header.Get(sessionHeader) == "" || r.Header.Get(sessionHeader) == "orig-session" {
			t.Fatalf("cpa fallback should isolate the session, header = %q", r.Header.Get(sessionHeader))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
	}))
	defer cpaUpstream.Close()

	server := classifierTestServer(t,
		classifierRuntimeConfig{},
		nil,
		classifierRoute(anyRouterPrefix, "anyrouter", anyUpstream, anyRouterStrategy{}),
		classifierRoute(cliproxyPrefix, "cliproxy", cpaUpstream, cliproxyStrategy{}),
	)
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("any request failed: %v", err)
	}
	anyOut, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// anthropic profile must stream the upstream body through verbatim, not
	// reassemble it (exact bytes, no stop_reason/stop_sequence injection).
	if string(anyOut) != `{"ok":true}` {
		t.Fatalf("anthropic fallback must pass the upstream body through verbatim, got %s", anyOut)
	}
	if bytes.Contains(anyOut, []byte("stop_reason")) || bytes.Contains(anyOut, []byte("stop_sequence")) {
		t.Fatalf("anthropic fallback must not inject reassembly fields, got %s", anyOut)
	}
	resp2, err := http.Post(proxy.URL+"/cpa/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
	if err != nil {
		t.Fatalf("cpa request failed: %v", err)
	}
	resp2.Body.Close()
	// Claude classifier via /cpa → anthropic profile to CPA (not AnyRouter): after
	// platform-aware behavior it is forwarded UNCHANGED (no cloak; the in-handler assertions above prove
	// the upstream body) and streamed back verbatim.
	resp3, err := http.Post(proxy.URL+"/cpa/v1/messages", "application/json", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("cpa claude request failed: %v", err)
	}
	claudeOut, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if string(claudeOut) != `{"ok":true}` {
		t.Fatalf("claude classifier to /cpa must pass the upstream body through verbatim, got %s", claudeOut)
	}
	if bytes.Contains(claudeOut, []byte("stop_reason")) || bytes.Contains(claudeOut, []byte("stop_sequence")) {
		t.Fatalf("claude classifier to /cpa must not inject reassembly fields, got %s", claudeOut)
	}
	if anyHits != 1 || cpaHits != 2 {
		t.Fatalf("fallback hits = any %d cpa %d, want 1/2", anyHits, cpaHits)
	}
}

// TestOpenAIClassifierStripsAcceptEncoding proves the openai profile strips
// Accept-Encoding for EVERY classifier destination, not only the CPA-isolating
// one. The openai profile always reassembles the response (JSON-parses the
// body), and the real non-CPA / CPA clients set DisableCompression, so a
// forwarded Accept-Encoding: gzip would not be auto-decoded and reassembly
// would choke on gzip bytes. Because httptest's default client auto-decompresses
// (masking the bug), the assertion is on the header the UPSTREAM receives, using
// a DisableCompression client that mirrors the real upstream clients (otherwise
// the transport would re-add gzip after the Del, hiding the strip).
func TestOpenAIClassifierStripsAcceptEncoding(t *testing.T) {
	cases := []struct {
		name        string
		prefix      string
		strategy    proxyStrategy
		wantSession bool
	}{
		{name: "non-cpa-anyrouter", prefix: anyRouterPrefix, strategy: anyRouterStrategy{}, wantSession: false},
		{name: "cpa-isolate", prefix: cliproxyPrefix, strategy: cliproxyStrategy{}, wantSession: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			var gotAcceptEncoding, gotSession string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				gotAcceptEncoding = r.Header.Get("Accept-Encoding")
				gotSession = r.Header.Get(sessionHeader)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cliproxyGPTStyleResponse("<block>no</block>"))
			}))
			defer upstream.Close()

			route := proxyRoute{
				name:      tc.name,
				prefix:    tc.prefix,
				upstreams: newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
				// DisableCompression mirrors the real AnyRouter / CPA clients: a
				// Del'd Accept-Encoding is NOT re-added by the transport, so the
				// upstream faithfully shows whether the strip happened.
				client:   &http.Client{Transport: &http.Transport{DisableCompression: true}},
				strategy: tc.strategy,
			}
			server := classifierTestServer(t, classifierRuntimeConfig{}, nil, route)
			proxy := httptest.NewServer(server)
			defer proxy.Close()

			req, err := http.NewRequest(http.MethodPost, proxy.URL+tc.prefix+"/v1/messages", bytes.NewReader(classifierBodyWithModel("gpt-5.5")))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			// Explicitly request gzip so there is something to strip.
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy request failed: %v", err)
			}
			resp.Body.Close()

			if !seen {
				t.Fatalf("upstream was not reached")
			}
			if gotAcceptEncoding != "" {
				t.Fatalf("openai-profile classifier (%s) must strip Accept-Encoding so the reassembler gets an uncompressed body, upstream saw %q", tc.name, gotAcceptEncoding)
			}
			if tc.wantSession && gotSession == "" {
				t.Fatalf("cpa-isolate path must still set the session header")
			}
			if !tc.wantSession && gotSession != "" {
				t.Fatalf("non-CPA destination must not get the CPA session header, got %q", gotSession)
			}
		})
	}
}
