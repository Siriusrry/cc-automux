package shim

import (
	"encoding/json"
	"testing"
)

func cliproxyGPTStyleResponse(verdictText string) []byte {
	return []byte(`{"id":"resp_x","type":"message","role":"assistant","model":"gpt-5.5",` +
		`"content":[{"type":"thinking","thinking":"weighing the action","signature":"sig"},` +
		`{"type":"text","text":` + mustJSONString(verdictText) + `}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":100,"output_tokens":3817}}`)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func cliproxyStage1Body() []byte {
	return []byte(`{
		"model": "gpt-5.5",
		"max_tokens": 64,
		"thinking": {"type": "disabled"},
		"stop_sequences": ["</block>"],
		"system": [
			{"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\n\n## Context\n"},
			{"type": "text", "text": "session context"}
		],
		"metadata": {"user_id": "{\"device_id\":\"dev-123\",\"account_uuid\":\"\",\"session_id\":\"orig-session\"}"},
		"messages": [{"role": "user", "content": [{"type":"text","text":"<transcript>...</transcript>"}]}]
	}`)
}

func cliproxyStage2Body() []byte {
	return []byte(`{
		"model": "gpt-5.5",
		"max_tokens": 8192,
		"thinking": {"type": "disabled"},
		"system": [
			{"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\n\n## Context\n"},
			{"type": "text", "text": "session context"}
		],
		"metadata": {"user_id": "{\"device_id\":\"dev-123\",\"account_uuid\":\"\",\"session_id\":\"orig-session\"}"},
		"messages": [{"role": "user", "content": [{"type":"text","text":"<transcript>...</transcript>"}]}]
	}`)
}

func TestReassembleStage1StripsThinkingAndTruncates(t *testing.T) {
	resp := cliproxyGPTStyleResponse("<block>yes</block><reason>destructive delete</reason>")
	out, ok := reassembleClassifierResponse(resp, []string{"</block>"})
	if !ok {
		t.Fatal("expected reassembly to succeed")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid reassembled JSON: %v", err)
	}
	if m["stop_reason"] != "stop_sequence" {
		t.Fatalf("stop_reason = %v, want stop_sequence", m["stop_reason"])
	}
	if m["stop_sequence"] != "</block>" {
		t.Fatalf("stop_sequence = %v, want </block>", m["stop_sequence"])
	}
	content := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block after stripping thinking, got %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("remaining block type = %v, want text", block["type"])
	}
	if block["text"] != "<block>yes" {
		t.Fatalf("text = %q, want %q", block["text"], "<block>yes")
	}
}

func TestReassembleStage2StripsThinkingNoTruncate(t *testing.T) {
	resp := cliproxyGPTStyleResponse("<block>no</block>")
	out, ok := reassembleClassifierResponse(resp, nil) // stage 2 has no stop_sequences
	if !ok {
		t.Fatal("expected reassembly to succeed")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid reassembled JSON: %v", err)
	}
	if m["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn (unchanged)", m["stop_reason"])
	}
	content := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["text"] != "<block>no</block>" {
		t.Fatalf("text = %q, want full %q", block["text"], "<block>no</block>")
	}
	for _, b := range content {
		if b.(map[string]any)["type"] == "thinking" {
			t.Fatal("thinking block should have been stripped")
		}
	}
}

func TestReassembleFailsClosedOnNonMessage(t *testing.T) {
	errBody := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"boom"}}`)
	if _, ok := reassembleClassifierResponse(errBody, []string{"</block>"}); ok {
		t.Fatal("error responses must not be reassembled (fail closed -> passthrough)")
	}
}
