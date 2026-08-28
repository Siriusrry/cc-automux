package shim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// writeReassembledClassifierResponse buffers a classifier response and reassembles
// a successful, parseable GPT-style body into the auto-mode shape Claude Code
// expects. On any error or unexpected shape it passes the upstream response back
// unchanged so Claude Code sees the classifier failure instead of a synthesized
// allow verdict. label tags diagnostics with the route/profile that served the
// request. It is shared by the cliproxy route and the openai classifier profile.
func writeReassembledClassifierResponse(label string, w http.ResponseWriter, r *http.Request, resp *http.Response, ctx requestContext) error {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return err
	}

	out := respBody
	if resp.StatusCode == http.StatusOK {
		if reassembled, ok := reassembleClassifierResponse(respBody, ctx.stopSequences); ok {
			out = reassembled
		} else {
			errorf("%s classifier response reassembly failed for %s %s", label, r.Method, r.URL.RequestURI())
			logResponseDiagnostic(label, r, resp, respBody, "upstream returned 200 but classifier response was not reassembled")
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(out)
	return err
}

// reassembleClassifierResponse rewrites a gpt-style Anthropic message response into the
// shape Claude Code stage-1/stage-2 expects:
//   - drop thinking / redacted_thinking blocks (Claude Code asked for thinking disabled);
//   - if the request carried stop_sequences, truncate the first text block at the
//     earliest stop sequence, drop any later blocks, and set stop_reason="stop_sequence"
//     and stop_sequence to the matched value (emulating Anthropic stop behavior).
//
// Returns (nil, false) when the body is not a parseable message or stripping would leave
// no content, so the caller can pass the upstream response back unchanged.
func reassembleClassifierResponse(respBody []byte, stopSequences []string) ([]byte, bool) {
	var m map[string]any
	decoder := json.NewDecoder(bytes.NewReader(respBody))
	decoder.UseNumber()
	if err := decoder.Decode(&m); err != nil {
		return nil, false
	}
	if t, _ := m["type"].(string); t != "message" {
		return nil, false
	}
	content, ok := m["content"].([]any)
	if !ok {
		return nil, false
	}

	kept := make([]any, 0, len(content))
	for _, blk := range content {
		if b, ok := blk.(map[string]any); ok {
			if t, _ := b["type"].(string); t == "thinking" || t == "redacted_thinking" {
				continue
			}
		}
		kept = append(kept, blk)
	}
	if len(kept) == 0 {
		return nil, false
	}

	if len(stopSequences) > 0 {
		final := make([]any, 0, len(kept))
		truncated := false
		matched := ""
		for _, blk := range kept {
			if b, ok := blk.(map[string]any); ok {
				if t, _ := b["type"].(string); t == "text" {
					text, _ := b["text"].(string)
					if idx, stop := earliestStop(text, stopSequences); idx >= 0 {
						b["text"] = text[:idx]
						final = append(final, b)
						truncated = true
						matched = stop
						break
					}
				}
			}
			final = append(final, blk)
		}
		if truncated {
			kept = final
			m["stop_reason"] = "stop_sequence"
			m["stop_sequence"] = matched
		}
	}

	m["content"] = kept
	out, err := marshalJSON(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// earliestStop returns the index and value of the first stop sequence found in text,
// or (-1, "") if none is present.
func earliestStop(text string, stops []string) (int, string) {
	best := -1
	bestStop := ""
	for _, s := range stops {
		if s == "" {
			continue
		}
		if i := strings.Index(text, s); i >= 0 && (best == -1 || i < best) {
			best = i
			bestStop = s
		}
	}
	return best, bestStop
}
