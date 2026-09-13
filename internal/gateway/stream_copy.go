package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

type streamCopyResult struct {
	cancel         *copyCancellation
	verdict        responseVerdict
	started, abort bool
	post           postCompletion
}

func (h *Handler) copyUpstream(w http.ResponseWriter, ctx context.Context, response *http.Response, control *upstreamAttempt, sse bool) (result streamCopyResult) {
	if response == nil || response.Body == nil {
		result.verdict = control.claim(responseVerdict{reason: endLocalError, class: scheduler.FailureNeutral, raw: "provider returned no response body"})
		return
	}
	defer response.Body.Close()
	defer func() {
		if result.cancel == nil && requestCanceled(ctx) {
			result.cancel = &copyCancellation{reason: canceledByClient}
		}
	}()
	control.observe(response, sse)
	raw := boundedText{limit: h.limits.ErrorTextBytes}
	finish := func(v responseVerdict) {
		result.verdict = control.claim(v)
		if result.verdict.reason == endHTTPError {
			result.verdict.raw = raw.text()
			if raw.truncated {
				result.verdict.incomplete = incompleteTruncated
			} else {
				switch v.reason {
				case endResponseIdleTimeout:
					result.verdict.incomplete = incompleteTimeout
				case endStreamInterrupted:
					result.verdict.incomplete = incompleteInterrupted
				case endClientCanceled:
					result.verdict.incomplete = incompleteCanceled
				}
			}
		}
	}
	canceled := func() {
		result.cancel = &copyCancellation{reason: canceledByClient}
		finish(responseVerdict{reason: endClientCanceled, class: scheduler.FailureClientCanceled})
	}
	if requestCanceled(ctx) {
		canceled()
		return
	}
	copyResponseHeaders(w.Header(), response.Header)
	if requestCanceled(ctx) {
		canceled()
		return
	}
	w.WriteHeader(response.StatusCode)
	result.started = true
	if requestCanceled(ctx) {
		canceled()
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	buffer := make([]byte, 32*1024)
	for {
		if requestCanceled(ctx) {
			canceled()
			return
		}
		n, err := response.Body.Read(buffer)
		if n > 0 {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				raw.add(buffer[:n])
			}
			if requestCanceled(ctx) {
				canceled()
				return
			}
			written, writeErr := w.Write(buffer[:n])
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				result.cancel = &copyCancellation{reason: canceledByDisconnect, raw: writeErr.Error()}
				finish(responseVerdict{reason: endClientCanceled, class: scheduler.FailureDownstream, raw: writeErr.Error()})
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			finish(responseVerdict{reason: endCompleted, class: scheduler.FailureNone})
			return
		}
		reason := control.reason()
		if reason == stopClientCanceled || (reason == "" && requestCanceled(ctx)) {
			canceled()
			return
		}
		if _, message, timeout := timeoutResponse(err); timeout {
			finish(responseVerdict{reason: endResponseIdleTimeout, class: scheduler.FailureChannelImmediate, raw: message})
			if result.verdict.reason == endCompleted {
				result.post = postIdleTimeout
			}
		} else {
			finish(responseVerdict{reason: endStreamInterrupted, class: scheduler.FailureChannelStream, raw: err.Error()})
			if result.verdict.reason == endCompleted {
				result.post = postConnectionError
			}
		}
		result.abort = !requestCanceled(ctx)
		return
	}
}
