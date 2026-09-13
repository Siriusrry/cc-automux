package gateway

import (
	"context"
	"errors"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"io"
	"net/http"
)

type streamCopyResult struct {
	verdict        responseVerdict
	started, abort bool
	post           string
}

func (h *Handler) copyUpstream(w http.ResponseWriter, ctx context.Context, response *http.Response, control *upstreamAttempt, sse bool) (result streamCopyResult) {
	if control == nil {
		control = h.newUpstreamAttempt(ctx, false)
		defer control.close()
		response, _ = control.receiveHeaders(response, nil)
	}
	if response == nil || response.Body == nil {
		result.verdict = control.claim(responseVerdict{reason: "local_error", class: scheduler.FailureNeutral, raw: "provider returned no response body"})
		return
	}
	defer response.Body.Close()
	control.observe(response, sse)
	raw := boundedText{limit: h.limits.ErrorTextBytes}
	finish := func(v responseVerdict) {
		result.verdict = control.claim(v)
		if result.verdict.reason == "http_error" {
			result.verdict.raw = raw.text()
			if raw.truncated {
				result.verdict.incomplete = "truncated"
			} else {
				switch v.reason {
				case "response_idle_timeout":
					result.verdict.incomplete = "timeout"
				case "stream_interrupted":
					result.verdict.incomplete = "interrupted"
				case "client_canceled":
					result.verdict.incomplete = "canceled"
				}
			}
		}
	}
	canceled := func() { finish(responseVerdict{reason: "client_canceled", class: scheduler.FailureClientCanceled}) }
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
				finish(responseVerdict{reason: "client_canceled", class: scheduler.FailureDownstream, raw: writeErr.Error()})
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
			finish(responseVerdict{reason: "completed", class: scheduler.FailureNone})
			return
		}
		reason := control.reason()
		if reason == "client_canceled" || (reason == "" && requestCanceled(ctx)) {
			canceled()
			return
		}
		if _, message, timeout := timeoutResponse(err); timeout {
			finish(responseVerdict{reason: "response_idle_timeout", class: scheduler.FailureChannelImmediate, raw: message})
			if result.verdict.reason == "completed" {
				result.post = "idle_timeout"
			}
		} else {
			finish(responseVerdict{reason: "stream_interrupted", class: scheduler.FailureChannelStream, raw: err.Error()})
			if result.verdict.reason == "completed" {
				result.post = "connection_error"
			}
		}
		result.abort = !requestCanceled(ctx)
		return
	}
}
