package shim

import "net/http"

type requestContext struct {
	rewritten     bool
	classifier    bool
	stopSequences []string
	sessionID     string
}

type proxyStrategy interface {
	rewriteRequest(r *http.Request, strippedPath string, raw []byte) ([]byte, requestContext, error)
	applyRequestHeaders(dst, src http.Header, ctx requestContext)
	writeResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, ctx requestContext) error
}

// passthroughStrategy is the proxyStrategy used when the global feature switch is
// off (runtimeConfig.enabled == false): the shim degrades to a plain reverse
// proxy. It performs no body rewrite (raw bytes forwarded verbatim), copies the
// client's request headers unchanged so the client's own Authorization/x-api-key
// reach the upstream (servePassthrough invokes it with an empty key, making
// applyAccountAuth a no-op), and streams the response back without buffering. The
// /any double-entrance sticky failover is unaffected because attemptTarget drives
// it independently of the strategy.
type passthroughStrategy struct{}

func (passthroughStrategy) rewriteRequest(_ *http.Request, _ string, raw []byte) ([]byte, requestContext, error) {
	return raw, requestContext{}, nil
}

func (passthroughStrategy) applyRequestHeaders(dst, src http.Header, _ requestContext) {
	copyRequestHeaders(dst, src)
}

func (passthroughStrategy) writeResponse(w http.ResponseWriter, _ *http.Request, resp *http.Response, _ requestContext) error {
	return streamResponse(w, resp)
}
