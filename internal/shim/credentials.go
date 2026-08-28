package shim

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// records the request's outcome, unifying static and rotating credentials behind
// one interface so the main auth path is uniform. AnyRouter
// multi-account rotation is the *accountPool implementation; a single-key route
// (CPA) is the degenerate one-element staticKeyProvider. assign picks the key for a
// request (sticky by session id where the implementation rotates) and returns the
// index identifying which credential served, for reportResult; ok is false only
// when there is nothing to assign (an empty accountPool), in which case the caller
// forwards the client's own auth. reportResult feeds the outcome back so a rotating
// provider can track per-account health; a static provider ignores it (a single
// configured key is never evicted). A classifier request redirected to the global
// target is handled outside this interface and never draws a provider key (see
// dispatchClassifier's globalTarget gate).
type credentialProvider interface {
	assign(sessionID string) (key string, idx int, ok bool)
	reportResult(idx, status int, transportErr bool)
}

// staticKeyProvider is the degenerate single-key credentialProvider for a route
// that owns exactly one upstream key (CPA's cpa.key). assign always returns that
// key at idx 0 — there is no rotation and no per-session stickiness to track — and
// reportResult is a no-op: a single configured key is never evicted (a bad CPA key
// is the operator's to fix; there is no peer to rotate to). An empty key is
// returned verbatim so applyAccountAuth becomes a no-op and the client's own auth
// is forwarded — today's default for /cpa with no cpa.key.
type staticKeyProvider struct {
	key string
}

func (s staticKeyProvider) assign(string) (string, int, bool) {
	return s.key, 0, true
}

func (staticKeyProvider) reportResult(int, int, bool) {}

// sessionIDForRequest derives the session id used to pin an account: the
// X-Claude-Code-Session-Id header when present, else metadata.user_id.session_id
// from the request body.
func sessionIDForRequest(r *http.Request, raw []byte) string {
	if sid := strings.TrimSpace(r.Header.Get(sessionHeader)); sid != "" {
		return sid
	}
	return sessionIDFromBody(raw)
}

func sessionIDFromBody(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return ""
	}
	meta, ok := body["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	uidRaw, ok := meta["user_id"].(string)
	if !ok || uidRaw == "" {
		return ""
	}
	var uid map[string]any
	if err := json.Unmarshal([]byte(uidRaw), &uid); err != nil {
		return ""
	}
	sid, _ := uid["session_id"].(string)
	return strings.TrimSpace(sid)
}

// applyAccountAuth overrides the upstream auth with a shim-owned key, stripping
// any client-forwarded credentials so the override always wins. It is the single
// mechanism for every shim-owned credential: an AnyRouter rotation account key, a
// configured CPA key, and a classifier global-target key. All three reach
// Anthropic-compatible upstreams the way Claude Code does — by the bearer token
// it sends as ANTHROPIC_AUTH_TOKEN — so the key is set on Authorization: Bearer
// and the alternate x-api-key header is removed. An empty key is a no-op (the
// client's own auth is forwarded unchanged).
func applyAccountAuth(h http.Header, key string) {
	if key == "" {
		return
	}
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Set("Authorization", "Bearer "+key)
}
