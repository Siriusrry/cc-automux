package shim

import (
	"bytes"
	"encoding/json"
	"strings"
)

const securityPrefix = "You are a security monitor for autonomous AI coding agents"

func parseAutoModeClassifier(raw []byte) (map[string]any, bool, error) {
	if !looksLikeClassifier(raw) {
		return nil, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, false, err
	}
	if !isAutoModeClassifierRequest(body) {
		return nil, false, nil
	}
	return body, true, nil
}

func looksLikeClassifier(raw []byte) bool {
	return bytes.Contains(raw, []byte(securityPrefix))
}

func isAutoModeClassifierRequest(body map[string]any) bool {
	systemBlocks, ok := body["system"].([]any)
	if !ok || len(systemBlocks) == 0 {
		return false
	}
	firstSystemBlock, ok := systemBlocks[0].(map[string]any)
	if !ok {
		return false
	}
	text, ok := firstSystemBlock["text"].(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(text, securityPrefix)
}

func isClaudeMessagesPath(path string) bool {
	return path == "/v1/messages"
}

func intFromJSONValue(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return i, true
		}
		f, err := v.Float64()
		if err != nil || f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	case float64:
		if v != float64(int64(v)) {
			return 0, false
		}
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
