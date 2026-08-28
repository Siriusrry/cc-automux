package shim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriteSessionPreservesOtherFields(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal(cliproxyStage1Body(), &body); err != nil {
		t.Fatalf("fixture decode failed: %v", err)
	}
	rewriteSessionInBody(body, "new-session-xyz")

	uidRaw := body["metadata"].(map[string]any)["user_id"].(string)
	var uid map[string]any
	if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
		t.Fatalf("user_id is not valid JSON: %v", err)
	}
	if uid["session_id"] != "new-session-xyz" {
		t.Fatalf("session_id = %v, want new-session-xyz", uid["session_id"])
	}
	if uid["device_id"] != "dev-123" {
		t.Fatalf("device_id should be preserved, got %v", uid["device_id"])
	}
}

func TestParseClassifierOnlyMatchesSystemZero(t *testing.T) {
	if _, ok, err := parseAutoModeClassifier(cliproxyStage1Body()); !ok || err != nil {
		t.Fatalf("stage1 fixture should be detected as a classifier request, ok=%v err=%v", ok, err)
	}

	// Security-monitor text only mentioned inside the transcript, not in system[0].
	notClassifier := []byte(`{"model":"gpt-5.5","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"talking about You are a security monitor for autonomous AI coding agents"}]}]}`)
	if _, ok, err := parseAutoModeClassifier(notClassifier); ok || err != nil {
		t.Fatalf("security-monitor text outside system[0] must not match, ok=%v err=%v", ok, err)
	}
}

func TestCliproxyProxyClassifierRewritesSessionAndReassembles(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/messages?beta=true" {
			t.Fatalf("unexpected upstream path: %s", r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("upstream body not JSON: %v", err)
		}

		// model and thinking must be left untouched.
		if m["model"] != "gpt-5.5" {
			t.Fatalf("model changed to %v, want gpt-5.5", m["model"])
		}
		if th, ok := m["thinking"].(map[string]any); !ok || th["type"] != "disabled" {
			t.Fatalf("thinking should be untouched (disabled), got %v", m["thinking"])
		}
		if bytes.Contains(body, []byte(identityMarker)) || bytes.Contains(body, []byte(correctionText)) {
			t.Fatalf("cliproxy route must not apply anyrouter prompt rewrite: %s", body)
		}

		// session id must be rewritten in both the header and metadata, and consistent.
		hdrSID := r.Header.Get(sessionHeader)
		if hdrSID == "" || hdrSID == "orig-session" {
			t.Fatalf("header session id not rewritten: %q", hdrSID)
		}
		uidRaw := m["metadata"].(map[string]any)["user_id"].(string)
		var uid map[string]any
		if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
			t.Fatalf("user_id not JSON: %v", err)
		}
		if uid["session_id"] == "orig-session" {
			t.Fatal("body session_id was not rewritten")
		}
		if uid["session_id"] != hdrSID {
			t.Fatalf("header session %q != body session %v", hdrSID, uid["session_id"])
		}
		if uid["device_id"] != "dev-123" {
			t.Fatalf("device_id should be preserved, got %v", uid["device_id"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>yes</block><reason>destructive</reason>"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, cliproxyPrefix, upstream, cliproxyStrategy{}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/cpa/v1/messages?beta=true", "application/json", bytes.NewReader(cliproxyStage1Body()))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client response not JSON: %v", err)
	}
	content := m["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected a single text block, got %v", content)
	}
	if content[0].(map[string]any)["text"] != "<block>yes" {
		t.Fatalf("client text = %q, want <block>yes", content[0].(map[string]any)["text"])
	}
	if m["stop_reason"] != "stop_sequence" {
		t.Fatalf("stop_reason = %v, want stop_sequence", m["stop_reason"])
	}
}

func TestCliproxyProxyPassthroughLeavesNonClassifierUnchanged(t *testing.T) {
	normalBody := []byte(`{"model":"gpt-5.5","max_tokens":64000,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if r.URL.RequestURI() != "/v1/messages?beta=true" {
			t.Fatalf("unexpected upstream path: %s", r.URL.RequestURI())
		}
		if !bytes.Equal(body, normalBody) {
			t.Fatalf("passthrough body changed:\n want %s\n got  %s", normalBody, body)
		}
		if r.Header.Get(sessionHeader) != "keep-me" {
			t.Fatalf("passthrough must not alter session header, got %q", r.Header.Get(sessionHeader))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, cliproxyPrefix, upstream, cliproxyStrategy{}))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/messages?beta=true", bytes.NewReader(normalBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sessionHeader, "keep-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestCliproxyStage2ProxyKeepsReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cliproxyGPTStyleResponse("<block>yes</block><reason>real reason here</reason>"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, cliproxyPrefix, upstream, cliproxyStrategy{}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/cpa/v1/messages?beta=true", "application/json", bytes.NewReader(cliproxyStage2Body()))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client response not JSON: %v", err)
	}
	text := m["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "<reason>real reason here</reason>") {
		t.Fatalf("stage 2 must keep the full reason, got %q", text)
	}
}

func TestCliproxyNonMessagesPathPassthrough(t *testing.T) {
	bodyWithSecurityPrefix := []byte(`{"model":"gpt-5.5","input":"You are a security monitor for autonomous AI coding agents","stream":true}`)
	upstreamResponse := cliproxyGPTStyleResponse("<block>yes</block><reason>should remain raw</reason>")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/responses" {
			t.Fatalf("unexpected upstream path: %s", r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, bodyWithSecurityPrefix) {
			t.Fatalf("non-/v1/messages body changed:\nwant %s\n got %s", bodyWithSecurityPrefix, body)
		}
		if r.Header.Get(sessionHeader) != "keep-me" {
			t.Fatalf("non-/v1/messages path must not rewrite session header, got %q", r.Header.Get(sessionHeader))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamResponse)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, cliproxyPrefix, upstream, cliproxyStrategy{}))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/cpa/v1/responses", bytes.NewReader(bodyWithSecurityPrefix))
	req.Header.Set(sessionHeader, "keep-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(out, upstreamResponse) {
		t.Fatalf("non-/v1/messages response should not be reassembled:\nwant %s\n got %s", upstreamResponse, out)
	}
}
