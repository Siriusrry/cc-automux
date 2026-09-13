package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func httpStatusText(status int) string {
	return fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
}
func mappedHTTPError(status int) bool {
	return status == 401 || status == 403 || status == 405 || status >= 300 && status < 400
}

func (h *Handler) fillFailure(f *capturedFailure, event Event, raw string, incomplete incompleteMark) {
	event.RawError = raw
	event.RawErrorIncomplete = incomplete
	event.EndReason = endHTTPError
	if updater, ok := h.selector.(scheduler.ErrorUpdater); ok {
		updater.UpdateError(f.lease.AttemptLease, f.observation, raw, incomplete == incompleteTimeout || incomplete == incompleteInterrupted || incomplete == incompleteCanceled, incomplete == incompleteTruncated)
	}
	h.record(event)
}
func (h *Handler) backgroundFailure(f *capturedFailure, event Event) {
	f.finishOnce.Do(func() {
		f.lease.control.readBackground()
		go func() {
			defer f.release()
			defer f.lease.control.close()
			raw, incomplete := h.readErrorBody(f.response.Body, f.lease.control)
			h.fillFailure(f, event, raw, incomplete)
		}()
	})
}
func (h *Handler) readErrorBody(body io.ReadCloser, control *upstreamAttempt) (string, incompleteMark) {
	defer body.Close()
	raw := boundedText{limit: h.limits.ErrorTextBytes}
	buffer := make([]byte, min(32*1024, raw.limit))
	for {
		remaining := raw.limit - len(raw.data)
		if remaining == 0 {
			raw.truncated = true
			return raw.text(), incompleteTruncated
		}
		n, err := body.Read(buffer[:min(len(buffer), remaining)])
		if n > 0 {
			raw.add(buffer[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return raw.text(), ""
			}
			if control.reason() == stopErrorBodyTimeout {
				return raw.text(), incompleteTimeout
			}
			return raw.text(), incompleteInterrupted
		}
	}
}
func (h *Handler) finishHTTPFailure(w http.ResponseWriter, ctx context.Context, f *capturedFailure) {
	event := h.outcomeEvent(EventFailure, f.lease, f.outcome, f.attempt, f.update)
	if requestCanceled(ctx) {
		h.backgroundFailure(f, event)
		h.recordCopyCancellation(event, streamCopyResult{cancel: &copyCancellation{reason: canceledByClient}})
		return
	}
	if mappedHTTPError(f.response.StatusCode) {
		h.backgroundFailure(f, event)
		if !requestCanceled(ctx) {
			writeError(w, 502, "bad_gateway", "provider rejected the gateway request")
		}
		return
	}
	f.finishOnce.Do(func() {
		defer f.release()
		defer f.lease.control.close()
		f.lease.control.readFinal(ctx)
		result := h.copyUpstream(w, ctx, f.response, f.lease.control, false)
		h.fillFailure(f, event, result.verdict.raw, result.verdict.incomplete)
		h.recordCopyCancellation(event, result)
		if result.abort {
			panic(http.ErrAbortHandler)
		}
	})
}

func (h *Handler) finishFixedHTTPFailure(ctx context.Context, w http.ResponseWriter, response *http.Response, target *provider.CompiledFixedTarget, session, model, upstream string, release func()) {
	lifecycle := fixedLifecycleFromContext(ctx)
	control := lifecycle.control
	if !lifecycle.beginTerminal() {
		release()
		control.close()
		response.Body.Close()
		return
	}
	_ = lifecycle.closeResources()
	status := response.StatusCode
	gatewayStatus := status
	code := "upstream_error"
	if mappedHTTPError(status) {
		gatewayStatus = 502
		code = "bad_gateway"
	}
	record := func(raw string, incomplete incompleteMark) Event {
		h.recordFixedCall(ctx, target, automode.FixedTargetCall{UpstreamURL: upstream, GatewayStatus: gatewayStatus, GatewayError: code, UpstreamStatus: status, UpstreamHeaders: response.Header.Clone(), UpstreamBody: raw, SessionID: session, Error: raw, UpstreamBodyTruncated: incomplete == incompleteTruncated})
		event := Event{Kind: EventFailure, ProviderID: target.ID, SessionID: session, Model: model, RequestType: traffic.RequestTypeClassifier, Stream: lifecycle.stream, Attempt: 1, UpstreamURL: upstream, HTTPStatus: status, EndReason: endHTTPError, RawError: raw, RawErrorIncomplete: incomplete}
		h.record(event)
		return event
	}
	if mappedHTTPError(status) || requestCanceled(ctx) {
		control.readBackground()
		go func() {
			defer release()
			defer control.close()
			raw, incomplete := h.readErrorBody(response.Body, control)
			record(raw, incomplete)
		}()
		if !requestCanceled(ctx) {
			writeError(w, 502, "bad_gateway", "fixed target rejected the gateway request")
		} else {
			h.recordFixedEvent(ctx, EventCanceled, target, session, model, upstream, 1, status, "")
		}
		return
	}
	defer release()
	defer control.close()
	control.readFinal(ctx)
	result := h.copyUpstream(w, ctx, response, control, false)
	event := record(result.verdict.raw, result.verdict.incomplete)
	h.recordCopyCancellation(event, result)
	if result.abort {
		panic(http.ErrAbortHandler)
	}
}
