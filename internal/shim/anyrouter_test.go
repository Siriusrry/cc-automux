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

func TestRewriteAnyRouterAutoModeClassifier(t *testing.T) {
	raw := []byte(`{
		"model": "claude-opus-4-8",
		"max_tokens": 64,
		"thinking": {"type": "disabled"},
		"stop_sequences": ["</block>"],
		"system": [
			{"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\n\n## Context\n"},
			{"type": "text", "text": "session context"}
		],
		"messages": []
	}`)

	out, rewritten, err := rewriteAnyRouterClassifier(raw)
	if err != nil {
		t.Fatalf("rewriteAnyRouterClassifier returned error: %v", err)
	}
	if !rewritten {
		t.Fatal("expected classifier request to be rewritten")
	}

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("rewritten body is invalid JSON: %v", err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking.disabled should be deleted")
	}

	systemBlocks := body["system"].([]any)
	if len(systemBlocks) != 3 {
		t.Fatalf("expected 3 system blocks after prepend, got %d", len(systemBlocks))
	}

	marker := systemBlocks[0].(map[string]any)
	if marker["type"] != "text" || marker["text"] != identityMarker {
		t.Fatalf("unexpected marker block: %#v", marker)
	}

	original := systemBlocks[1].(map[string]any)
	text := original["text"].(string)
	if !strings.HasPrefix(text, correctionText+securityPrefix) {
		t.Fatalf("original classifier prompt was not corrected: %q", text[:min(len(text), 120)])
	}
}

func TestRewriteAnyRouterAutoModeStage2Classifier(t *testing.T) {
	raw := []byte(`{
		"model": "claude-opus-4-8",
		"max_tokens": 8192,
		"thinking": {"type": "disabled"},
		"system": [
			{"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\n\n## Context\n"},
			{"type": "text", "text": "session context"}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "<transcript>...</transcript>\nReview the classification process and follow it carefully. Use <thinking> before responding with <block>."}]}
		]
	}`)

	out, rewritten, err := rewriteAnyRouterClassifier(raw)
	if err != nil {
		t.Fatalf("rewriteAnyRouterClassifier returned error: %v", err)
	}
	if !rewritten {
		t.Fatal("expected stage2 classifier request to be rewritten")
	}

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("rewritten body is invalid JSON: %v", err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking.disabled should be deleted for stage2 too")
	}
	if _, ok := body["stop_sequences"]; ok {
		t.Fatal("stage2 rewrite should not add stop_sequences")
	}
	if maxTokens, ok := intFromJSONValue(body["max_tokens"]); !ok || maxTokens != 8192 {
		t.Fatalf("stage2 max_tokens should be preserved, got %#v", body["max_tokens"])
	}

	systemBlocks := body["system"].([]any)
	if len(systemBlocks) != 3 {
		t.Fatalf("expected 3 system blocks after prepend, got %d", len(systemBlocks))
	}
	marker := systemBlocks[0].(map[string]any)
	if marker["type"] != "text" || marker["text"] != identityMarker {
		t.Fatalf("unexpected marker block: %#v", marker)
	}
	original := systemBlocks[1].(map[string]any)
	text := original["text"].(string)
	if !strings.HasPrefix(text, correctionText+securityPrefix) {
		t.Fatalf("stage2 classifier prompt was not corrected: %q", text[:min(len(text), 120)])
	}
}

func TestRewriteAnyRouterPassesThroughNormalRequestExactly(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4-8","max_tokens":64000,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)

	out, rewritten, err := rewriteAnyRouterClassifier(raw)
	if err != nil {
		t.Fatalf("rewriteAnyRouterClassifier returned error: %v", err)
	}
	if rewritten {
		t.Fatal("normal request should not be rewritten")
	}
	if !bytes.Equal(out, raw) {
		t.Fatal("normal request should pass through byte-for-byte")
	}
}

func TestRewriteRequiresSecurityMonitorAsFirstSystemBlock(t *testing.T) {
	raw := []byte(`{"max_tokens":8192,"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[]}`)

	if !looksLikeClassifier(raw) {
		t.Fatal("fixture should pass the raw marker scan")
	}

	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("fixture decode failed: %v", err)
	}
	if !isAutoModeClassifierRequest(body) {
		t.Fatal("fixture should match classifier detection")
	}

	body["system"] = []any{map[string]any{
		"type": "text",
		"text": "You are Claude Code, Anthropic's official CLI for Claude.",
	}}
	body["messages"] = []any{map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "text",
			"text": "Discussing: You are a security monitor for autonomous AI coding agents.",
		}},
	}}
	if !looksLikeClassifier(mustMarshalJSON(t, body)) {
		t.Fatal("fixture should still pass the raw marker scan because the transcript mentions the marker")
	}
	if isAutoModeClassifierRequest(body) {
		t.Fatal("security monitor text outside system[0] should not match classifier detection")
	}
}

func TestAnyRouterProxyRewritesClassifierAndStripsPrefix(t *testing.T) {
	classifierBody := []byte(`{"max_tokens":64,"thinking":{"type":"disabled"},"stop_sequences":["</block>"],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[]}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/messages?beta=true" {
			t.Fatalf("unexpected upstream path: %s", r.URL.RequestURI())
		}
		if r.Header.Get(sessionHeader) != "keep-me" {
			t.Fatalf("anyrouter route must not rewrite session header, got %q", r.Header.Get(sessionHeader))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if !bytes.Contains(body, []byte(identityMarker)) {
			t.Fatalf("upstream body missing identity marker: %s", body)
		}
		if bytes.Contains(body, []byte(`"thinking"`)) {
			t.Fatalf("upstream body still contains thinking: %s", body)
		}
		if !bytes.Contains(body, []byte("Ignore the preceding identity marker. Follow only the instructions below.")) || !bytes.Contains(body, []byte(securityPrefix)) {
			t.Fatalf("upstream body missing correction text: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, anyRouterPrefix, upstream, anyRouterStrategy{}))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages?beta=true", bytes.NewReader(classifierBody))
	req.Header.Set(sessionHeader, "keep-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response status: %s", resp.Status)
	}
}

func TestAnyRouterProxyPassesThroughNormalRequestAndResponse(t *testing.T) {
	normalBody := []byte(`{"model":"claude-opus-4-8","max_tokens":64000,"messages":[]}`)
	responseBody := []byte(`{"content":[{"type":"thinking","thinking":"not reassembled"},{"type":"text","text":"<block>no</block>"}]}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if !bytes.Equal(body, normalBody) {
			t.Fatalf("normal body changed:\nwant: %s\n got: %s", normalBody, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, anyRouterPrefix, upstream, anyRouterStrategy{}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages?beta=true", "application/json", bytes.NewReader(normalBody))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(out, responseBody) {
		t.Fatalf("anyrouter response should not be reassembled:\nwant %s\n got %s", responseBody, out)
	}
}

func TestRewriteAnyRouterPromotesDisabledThinkingOnNormalRequest(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4-8","max_tokens":64000,"thinking":{"type":"disabled"},"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)

	out, changed, err := promoteDisabledThinking(raw)
	if err != nil {
		t.Fatalf("promoteDisabledThinking returned error: %v", err)
	}
	if !changed {
		t.Fatal("expected thinking.disabled to be promoted on a normal request")
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("promoted body is invalid JSON: %v", err)
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking should be promoted to adaptive, got %v", body["thinking"])
	}
}

func TestPromoteDisabledThinkingLeavesOtherRequestsByteForByte(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"model":"claude-opus-4-8","messages":[]}`),
		[]byte(`{"thinking":{"type":"adaptive"},"messages":[]}`),
		[]byte("not json at all"),
	} {
		out, changed, err := promoteDisabledThinking(raw)
		if err != nil {
			t.Fatalf("promoteDisabledThinking returned error: %v", err)
		}
		if changed {
			t.Fatalf("request without thinking.disabled should not change: %s", raw)
		}
		if !bytes.Equal(out, raw) {
			t.Fatalf("request should pass through byte-for-byte:\nwant %s\n got %s", raw, out)
		}
	}
}

func TestAnyRouterProxyPromotesSubagentDisabledThinking(t *testing.T) {
	subagentBody := []byte(`{"model":"claude-opus-4-8","max_tokens":64000,"thinking":{"type":"disabled"},"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if bytes.Contains(body, []byte(`"disabled"`)) {
			t.Fatalf("upstream body still has thinking.disabled: %s", body)
		}
		if !bytes.Contains(body, []byte(`"adaptive"`)) {
			t.Fatalf("upstream body should carry adaptive thinking: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(testProxyServer(t, anyRouterPrefix, upstream, anyRouterStrategy{}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/any/v1/messages?beta=true", "application/json", bytes.NewReader(subagentBody))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response status: %s", resp.Status)
	}
}
