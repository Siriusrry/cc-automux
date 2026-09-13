package gateway

import (
	"github.com/Siriusrry/cc-automux/internal/traffic"
	"io"
)

func cancelPhase(started bool, status int) cancellationPhase {
	if status != 0 {
		return cancelReceivingResponse
	}
	if started {
		return cancelAwaitingResponse
	}
	return cancelBeforeUpstream
}

// clientWriteError distinguishes a failed downstream write from a read or cleanup error.
type clientWriteError struct{ error }

type downstreamWriter struct {
	io.Writer
	err error
}

func (w *downstreamWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = clientWriteError{err}
	}
	return n, err
}

func (h *Handler) recordBetweenAttemptCanceled(plan traffic.RequestPlan, attempt int) {
	h.record(Event{Kind: EventCanceled, SessionID: plan.OriginalSessionID, Model: plan.EffectiveModel, RequestType: plan.RequestType, Stream: plan.Stream, TraceID: plan.TraceID, Attempt: attempt, CancelReason: canceledByClient, CancelPhase: cancelBeforeUpstream})
}

type copyCancellation struct {
	reason cancellationReason
	raw    string
}

func (h *Handler) recordCopyCancellation(base Event, result streamCopyResult) {
	if result.cancel == nil || base.Kind == EventCanceled {
		return
	}
	base.Kind = EventCanceled
	base.EndReason = ""
	base.RawErrorIncomplete = ""
	base.PostCompletion = ""
	base.CancelReason = result.cancel.reason
	base.CancelPhase = cancelReceivingResponse
	base.ResponseStarted = result.started
	base.RawError = result.cancel.raw
	base.ErrorCode = ""
	base.PatchID = ""
	base.PatchStage = ""
	base.NextProviderID = ""
	base.NextProviderName = ""
	base.NextAttempt = 0
	base.NextUpstreamURL = ""
	base.GlobalEnteredCooldown = false
	base.ChannelEnteredCooldown = false
	base.CooldownUntil = nil
	h.record(base)
}
