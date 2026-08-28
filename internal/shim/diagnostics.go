package shim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	diagnosticTextPrefixChars = 300
	diagnosticRawPrefixBytes  = 4 << 10
)

var diagnosticHeaderNames = []string{
	"Content-Type",
	"Content-Length",
	"User-Agent",
	"Accept",
	"Accept-Encoding",
	"Anthropic-Version",
	"Anthropic-Beta",
	"X-Claude-Code-Session-Id",
}

func logRequestDiagnostic(route string, r *http.Request, strippedPath string, raw []byte, reason string, err error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s request diagnostic\n", route)
	fmt.Fprintf(&b, "reason: %s\n", reason)
	if err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	}
	fmt.Fprintf(&b, "method: %s\n", r.Method)
	fmt.Fprintf(&b, "path: %s\n", r.URL.RequestURI())
	fmt.Fprintf(&b, "stripped_path: %s\n", strippedPath)
	fmt.Fprintf(&b, "headers:\n%s", formatSelectedHeaders(r.Header))
	fmt.Fprintf(&b, "body:\n%s", formatRequestBodySummary(raw))
	stderrLogger.Print(strings.TrimRight(b.String(), "\n"))
}

func logResponseDiagnostic(route string, r *http.Request, resp *http.Response, respBody []byte, reason string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s response diagnostic\n", route)
	fmt.Fprintf(&b, "reason: %s\n", reason)
	fmt.Fprintf(&b, "method: %s\n", r.Method)
	fmt.Fprintf(&b, "path: %s\n", r.URL.RequestURI())
	fmt.Fprintf(&b, "status: %d\n", resp.StatusCode)
	fmt.Fprintf(&b, "content_type: %s\n", resp.Header.Get("Content-Type"))
	fmt.Fprintf(&b, "body:\n%s", formatResponseBodySummary(respBody))
	stderrLogger.Print(strings.TrimRight(b.String(), "\n"))
}

func formatSelectedHeaders(headers http.Header) string {
	var b strings.Builder
	wrote := false
	for _, name := range diagnosticHeaderNames {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		wrote = true
		fmt.Fprintf(&b, "  %s: %s\n", name, strings.Join(values, ", "))
	}
	if !wrote {
		b.WriteString("  (none)\n")
	}
	return b.String()
}

func formatRequestBodySummary(raw []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  size_bytes: %d\n", len(raw))

	body, err := decodeJSONObject(raw)
	if err != nil {
		fmt.Fprintf(&b, "  parse_error: %v\n", err)
		fmt.Fprintf(&b, "  raw_prefix: %s\n", quoteBytePrefix(raw, diagnosticRawPrefixBytes))
		return b.String()
	}

	fmt.Fprintf(&b, "  top_level_keys: %v\n", sortedKeys(body))
	writeScalarField(&b, body, "model")
	writeScalarField(&b, body, "max_tokens")
	writeScalarField(&b, body, "stream")
	if thinking, ok := body["thinking"].(map[string]any); ok {
		writeScalarFieldWithName(&b, "thinking.type", thinking["type"])
	}
	if stops, ok := stringSliceFromAny(body["stop_sequences"]); ok {
		fmt.Fprintf(&b, "  stop_sequences: %v\n", stops)
	}
	writeSystemSummary(&b, body["system"])
	writeMessagesSummary(&b, body["messages"])
	writeMetadataSummary(&b, body["metadata"])
	return b.String()
}

func formatResponseBodySummary(raw []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  size_bytes: %d\n", len(raw))

	body, err := decodeJSONObject(raw)
	if err != nil {
		fmt.Fprintf(&b, "  parse_error: %v\n", err)
		fmt.Fprintf(&b, "  raw_prefix: %s\n", quoteBytePrefix(raw, diagnosticRawPrefixBytes))
		return b.String()
	}

	fmt.Fprintf(&b, "  top_level_keys: %v\n", sortedKeys(body))
	writeScalarField(&b, body, "type")
	writeScalarField(&b, body, "model")
	writeScalarField(&b, body, "stop_reason")
	writeScalarField(&b, body, "stop_sequence")
	content, ok := body["content"].([]any)
	if !ok {
		b.WriteString("  content: not_array_or_absent\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  content_count: %d\n", len(content))
	for i, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			fmt.Fprintf(&b, "  content[%d]: non_object\n", i)
			continue
		}
		typeValue, _ := block["type"].(string)
		line := fmt.Sprintf("  content[%d]: type=%s", i, typeValue)
		if text, ok := block["text"].(string); ok {
			line += fmt.Sprintf(" text_len=%d text_prefix=%s", len(text), quoteTextPrefix(text, diagnosticTextPrefixChars))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeScalarField(b *strings.Builder, body map[string]any, key string) {
	value, ok := body[key]
	if !ok {
		return
	}
	writeScalarFieldWithName(b, key, value)
}

func writeScalarFieldWithName(b *strings.Builder, name string, value any) {
	switch v := value.(type) {
	case string:
		fmt.Fprintf(b, "  %s: %q\n", name, v)
	case json.Number, bool, nil:
		fmt.Fprintf(b, "  %s: %v\n", name, v)
	case float64:
		fmt.Fprintf(b, "  %s: %v\n", name, v)
	}
}

func writeSystemSummary(b *strings.Builder, raw any) {
	system, ok := raw.([]any)
	if !ok {
		b.WriteString("  system: not_array_or_absent\n")
		return
	}
	fmt.Fprintf(b, "  system_count: %d\n", len(system))
	for i, item := range system {
		block, ok := item.(map[string]any)
		if !ok {
			fmt.Fprintf(b, "  system[%d]: non_object\n", i)
			continue
		}
		typeValue, _ := block["type"].(string)
		line := fmt.Sprintf("  system[%d]: type=%s", i, typeValue)
		if text, ok := block["text"].(string); ok {
			line += fmt.Sprintf(" text_len=%d text_prefix=%s", len(text), quoteTextPrefix(text, diagnosticTextPrefixChars))
		}
		b.WriteString(line + "\n")
	}
}

func writeMessagesSummary(b *strings.Builder, raw any) {
	messages, ok := raw.([]any)
	if !ok {
		b.WriteString("  messages: not_array_or_absent\n")
		return
	}
	fmt.Fprintf(b, "  messages_count: %d\n", len(messages))
	for i, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			fmt.Fprintf(b, "  messages[%d]: non_object\n", i)
			continue
		}
		role, _ := message["role"].(string)
		blocks, textLen := messageContentShape(message["content"])
		fmt.Fprintf(b, "  messages[%d]: role=%s content_blocks=%d text_total_len=%d\n", i, role, blocks, textLen)
	}
}

func messageContentShape(raw any) (int, int) {
	switch content := raw.(type) {
	case string:
		return 1, len(content)
	case []any:
		textLen := 0
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					textLen += len(text)
				}
			}
		}
		return len(content), textLen
	default:
		return 0, 0
	}
}

func writeMetadataSummary(b *strings.Builder, raw any) {
	metadata, ok := raw.(map[string]any)
	if !ok {
		b.WriteString("  metadata: not_object_or_absent\n")
		return
	}
	fmt.Fprintf(b, "  metadata_keys: %v\n", sortedKeys(metadata))
	uidRaw, ok := metadata["user_id"].(string)
	if !ok || uidRaw == "" {
		return
	}
	var uid map[string]any
	if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
		fmt.Fprintf(b, "  metadata.user_id_parse_error: %v\n", err)
		return
	}
	fmt.Fprintf(b, "  metadata.user_id_keys: %v\n", sortedKeys(uid))
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringSliceFromAny(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

func quoteTextPrefix(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return strconv.Quote(string(runes))
}

func quoteBytePrefix(raw []byte, maxBytes int) string {
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	return strconv.Quote(string(raw))
}
