package shim

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	sessionHeader = "X-Claude-Code-Session-Id"

	maxResponseBodyBytes int64 = 64 << 20
)

type cliproxyStrategy struct{}

func (cliproxyStrategy) rewriteRequest(r *http.Request, strippedPath string, raw []byte) ([]byte, requestContext, error) {
	if !isClaudeMessagesPath(strippedPath) {
		return raw, requestContext{}, nil
	}
	body, ok, err := parseAutoModeClassifier(raw)
	if err != nil {
		// Preserve the standalone cliproxy shim behavior: malformed /v1/messages
		// requests pass through to CLIProxyAPI instead of being rewritten, but log
		// enough structure to debug classifier shape changes.
		errorf("cliproxy classifier parse error for %s %s; passing through: %v", r.Method, r.URL.RequestURI(), err)
		logRequestDiagnostic("cliproxy", r, strippedPath, raw, "classifier parse failed; passing through unchanged", err)
		return raw, requestContext{}, nil
	}
	if !ok {
		return raw, requestContext{}, nil
	}

	sid, err := newUUID()
	if err != nil {
		return nil, requestContext{}, err
	}
	stopSequences := extractStopSequences(body)
	rewriteSessionInBody(body, sid)

	newBody, err := marshalJSON(body)
	if err != nil {
		return nil, requestContext{}, err
	}
	return newBody, requestContext{
		rewritten:     true,
		classifier:    true,
		stopSequences: stopSequences,
		sessionID:     sid,
	}, nil
}

func (cliproxyStrategy) applyRequestHeaders(dst, src http.Header, ctx requestContext) {
	copyRequestHeaders(dst, src)
	if ctx.classifier {
		dst.Set(sessionHeader, ctx.sessionID)
		// Force an uncompressed classifier response so we can reassemble it without decoding.
		dst.Del("Accept-Encoding")
	}
}

func (cliproxyStrategy) writeResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, ctx requestContext) error {
	if !ctx.classifier {
		return streamResponse(w, resp)
	}
	return writeReassembledClassifierResponse("cliproxy", w, r, resp, ctx)
}
func extractStopSequences(body map[string]any) []string {
	raw, ok := body["stop_sequences"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// rewriteSessionInBody replaces metadata.user_id.session_id with sid, preserving any
// other fields (device_id, account_uuid). It creates metadata/user_id if absent.
func rewriteSessionInBody(body map[string]any, sid string) {
	meta, ok := body["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		body["metadata"] = meta
	}
	if uidRaw, ok := meta["user_id"].(string); ok && uidRaw != "" {
		var uid map[string]any
		if err := json.Unmarshal([]byte(uidRaw), &uid); err == nil {
			uid["session_id"] = sid
			if encoded, err := json.Marshal(uid); err == nil {
				meta["user_id"] = string(encoded)
				return
			}
		}
	}
	encoded, _ := json.Marshal(map[string]any{"session_id": sid})
	meta["user_id"] = string(encoded)
}
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
