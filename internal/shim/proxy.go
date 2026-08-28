package shim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxRequestBodyBytes int64 = 64 << 20

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

type preparedRequest struct {
	body []byte
	ctx  requestContext
}

func (p *proxyServer) prepareProxyRequest(profile proxyStrategy, r *http.Request, strippedPath string, rawBody []byte) (preparedRequest, error) {
	body, ctx, err := profile.rewriteRequest(r, strippedPath, rawBody)
	if err != nil {
		return preparedRequest{}, err
	}
	return preparedRequest{body: body, ctx: ctx}, nil
}

// resolveTarget picks the upstream pool, client, and strategy for a request. By
// default it is the matched prefix's route; for a detected auto-mode classifier
// request, dispatchClassifier may override the strategy (by model family) and
// the destination (to a configured global target).
func (p *proxyServer) attemptTarget(w http.ResponseWriter, r *http.Request, target resolvedTarget, strippedPath string, body []byte, ctx requestContext) (resp *http.Response, servedIdx int, ok bool, upstreamExhausted bool) {
	order := target.baseURLs.attemptOrder()
	for n, idx := range order {
		upstream := target.baseURLs.url(idx)
		upstreamURL := resolveUpstreamURL(upstream, r, strippedPath)
		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
			errorf("%s failed to create upstream request for %s %s: %v", target.routeName, r.Method, r.URL.RequestURI(), err)
			return nil, -1, false, false
		}
		upstreamReq.ContentLength = int64(len(body))
		target.profile.applyRequestHeaders(upstreamReq.Header, r.Header, ctx)
		// Override the upstream auth with the shim-owned account key, replayed
		// identically on every entrance attempt. Empty key ⇒ forward client auth.
		applyAccountAuth(upstreamReq.Header, target.key)

		attemptResp, err := target.client.Do(upstreamReq)
		if err != nil {
			if r.Context().Err() != nil {
				// The client canceled or disconnected; that is not an
				// entrance failure, so do not fail over or move the pointer.
				errorf("%s request canceled by client for %s %s via %s: %v",
					target.routeName, r.Method, r.URL.RequestURI(), upstream.Host, err)
				return nil, -1, false, false
			}
			if n < len(order)-1 {
				next := target.baseURLs.url(order[n+1])
				errorf("%s upstream %s failed for %s %s (%v); failing over to %s%s",
					target.routeName, upstream.Host, r.Method, r.URL.RequestURI(), err, next.Host, failoverDiagnostic(ctx))
				continue
			}
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			errorf("%s upstream request failed for %s %s via %s: %v", target.routeName, r.Method, r.URL.RequestURI(), upstream.Host, err)
			return nil, -1, false, true
		}

		if n < len(order)-1 && isFailoverStatus(attemptResp.StatusCode) {
			next := target.baseURLs.url(order[n+1])
			errorf("%s upstream %s returned %d for %s %s; failing over to %s%s",
				target.routeName, upstream.Host, attemptResp.StatusCode, r.Method, r.URL.RequestURI(), next.Host, failoverDiagnostic(ctx))
			// Close without draining: reusing a connection to an entrance
			// being abandoned is worthless, and draining could block forever
			// on a stalled error body, which would defeat the failover.
			_ = attemptResp.Body.Close()
			continue
		}

		return attemptResp, idx, true, false
	}
	// Unreachable for any non-empty entrance list (the loop returns on its last
	// iteration, and the config is normalized to >=1 entrance). If it ever runs,
	// no entrance was contacted, so this is a shim/config fault, not an account
	// fault: report not-exhausted so the account is not penalized.
	return nil, -1, false, false
}

func (p *proxyServer) recordServedEntrance(resp *http.Response, target resolvedTarget, servedIdx int) {
	// A non-failover status means this entrance is alive: it becomes (or stays)
	// the current entrance. A 429/5xx served from the last entrance is not
	// success, so the pointer is left where it was.
	if !isFailoverStatus(resp.StatusCode) {
		if target.baseURLs.promote(servedIdx) {
			infof("%s now serving via %s", target.routeName, target.baseURLs.url(servedIdx).Host)
		}
	}
}

func (p *proxyServer) writeTargetResponse(w http.ResponseWriter, r *http.Request, target resolvedTarget, strippedPath string, rawBody []byte, resp *http.Response, servedIdx int, ctx requestContext) {
	if err := target.profile.writeResponse(w, r, resp, ctx); err != nil {
		errorf("%s response write failed for %s %s via %s: %v", target.routeName, r.Method, r.URL.RequestURI(), target.baseURLs.url(servedIdx).Host, err)
		// The response broke after bytes reached the client, so this request
		// cannot fail over — but if the break came from the upstream side
		// (not a client disconnect), point the next request, typically the
		// client's own retry, at the next entrance.
		if isUpstreamStreamError(err) && r.Context().Err() == nil {
			if next, moved := target.baseURLs.advanceFrom(servedIdx); moved {
				errorf("%s upstream %s broke mid-stream; switching current entrance to %s",
					target.routeName, target.baseURLs.url(servedIdx).Host, target.baseURLs.url(next).Host)
			}
		}
	}
	if ctx.rewritten {
		// Log every rewritten request (classifier cloak/isolation/reassembly AND the
		// non-classifier thinking.disabled→adaptive promotion) — this is the single
		// point rewritten results are logged. The rewrites counter surfaced by
		// GET /admin/status, however, is scoped to CLASSIFIER rewrites only: every
		// classifier-rewrite path sets ctx.classifier alongside ctx.rewritten, and
		// only the thinking promotion leaves classifier false, so gating the
		// increment on ctx.classifier counts classifier fixes exactly and excludes
		// the thinking promotion.
		kind := "auto-mode classifier request"
		if !ctx.classifier {
			kind = "thinking.disabled→adaptive request"
		}
		logRewrittenResult(target.routeName, kind, r, strippedPath, rawBody, resp.StatusCode, target.baseURLs.url(servedIdx).Host)
		if ctx.classifier {
			p.rewriteCount.Add(1)
		}
	}
}

// isFailoverStatus reports whether a complete upstream response should be
// treated as "this entrance is unavailable, try the next one". 429 is included
// deliberately: the AnyRouter entrances front the same backend, so a genuine
// backend 429 (rate limit, request-shape rejection) reproduces on the other
// entrance and is still returned to the client, while an edge-level 429 from one
// entrance is exactly the failure mode failover exists for. Other 4xx are passed
// through without switching — same backend, same answer.
func isFailoverStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// failoverDiagnostic compactly tags failover log lines for rewritten requests
// so a thinking-fix regression (both entrances rejecting rewritten bodies) is
// distinguishable from an entrance outage in stderr alone.
func failoverDiagnostic(ctx requestContext) string {
	if !ctx.rewritten {
		return ""
	}
	if ctx.classifier {
		return " (rewritten classifier request)"
	}
	return " (rewritten thinking.disabled→adaptive request)"
}

func logRewrittenResult(route, kind string, r *http.Request, strippedPath string, rawBody []byte, statusCode int, upstreamHost string) {
	message := "%s rewrote %s: %s %s (status %d via %s)"
	if statusCode >= 200 && statusCode < 400 {
		infof(message, route, kind, r.Method, r.URL.RequestURI(), statusCode, upstreamHost)
		return
	}
	errorf(message, route, kind, r.Method, r.URL.RequestURI(), statusCode, upstreamHost)
	if statusCode >= 500 {
		logRequestDiagnostic(route, r, strippedPath, rawBody, fmt.Sprintf("rewritten %s returned upstream status %d", kind, statusCode), nil)
	}
}

var errBodyTooLarge = errors.New("request body is too large")

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()

	limited := io.LimitReader(r.Body, maxRequestBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	_, ok := hopByHopHeaders[strings.ToLower(key)]
	return ok
}

func streamResponse(w http.ResponseWriter, resp *http.Response) error {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return copyBodyWithFlush(w, resp.Body)
}

// upstreamReadError marks a streaming failure as coming from the upstream side
// (resp.Body), as opposed to a write failure toward the client. Only
// upstream-side breaks are evidence that an entrance is unhealthy.
type upstreamReadError struct{ err error }

func (e *upstreamReadError) Error() string { return e.err.Error() }
func (e *upstreamReadError) Unwrap() error { return e.err }

func isUpstreamStreamError(err error) bool {
	var u *upstreamReadError
	return errors.As(err, &u)
}

func copyBodyWithFlush(w http.ResponseWriter, r io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return &upstreamReadError{err: readErr}
		}
	}
}
