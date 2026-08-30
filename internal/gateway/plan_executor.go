package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// capturedFlowSnapshot keeps the request's validated normal attempt budget
// while preserving the richer classifier/Auto Mode view supplied by Runtime.
// A scheduler Snapshot predates flow.SnapshotView, so this adapter is also the
// compatibility boundary for focused tests that implement only the scheduler
// interface.
type capturedFlowSnapshot struct {
	snapshot scheduler.Snapshot
	view     flow.SnapshotView
	normal   scheduler.AttemptPolicy
}

func (s capturedFlowSnapshot) Revision() uint64 {
	if s.snapshot == nil {
		return 0
	}
	return s.snapshot.Revision()
}

func (s capturedFlowSnapshot) NormalAttemptPolicy() scheduler.AttemptPolicy { return s.normal }

func (s capturedFlowSnapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy {
	if s.view != nil {
		return s.view.ClassifierAttemptPolicy()
	}
	return scheduler.DefaultClassifierAttemptPolicy()
}

func (s capturedFlowSnapshot) AutoMode() flow.AutoModeSnapshot {
	if s.view != nil {
		return s.view.AutoMode()
	}
	return flow.AutoModeSnapshot{}
}

func capturedFlowView(runtimeSnapshot, capturedSnapshot scheduler.Snapshot, normal scheduler.AttemptPolicy) flow.SnapshotView {
	var view flow.SnapshotView
	if candidate, ok := runtimeSnapshot.(flow.SnapshotView); ok {
		view = candidate
	} else if candidate, ok := capturedSnapshot.(flow.SnapshotView); ok {
		view = candidate
	}
	return capturedFlowSnapshot{snapshot: capturedSnapshot, view: view, normal: normal}
}

func requestTypeTrafficClass(requestType traffic.RequestType) scheduler.TrafficClass {
	return scheduler.TrafficClass(requestType)
}

func (h *Handler) prepareIngress(body bodyfile.Body, index bodyfile.JSONIndex, request *http.Request) (traffic.IngressRequest, error) {
	model := index.ModelValue()
	session := singleSessionHeader(request.Header)
	detection := traffic.NewDetectionRequest(body, index, model, session, traffic.NewHeaderView(request.Header))
	if h.detectors == nil {
		return traffic.IngressRequest{}, traffic.ErrNilDetectorRegistry
	}
	return h.detectors.ClassifyDetection(detection)
}

func (h *Handler) dispatchIngress(ctx context.Context, snapshot flow.SnapshotView, ingress traffic.IngressRequest) (flow.ExecutionPlan, error) {
	if h.flows == nil {
		return flow.ExecutionPlan{}, flow.ErrMissingPlanner
	}
	return h.flows.Dispatch(ctx, snapshot, ingress)
}

// forwardExecution is the common plan executor. Request-type behavior belongs
// to planners; this function interprets only TargetMode and AttemptPolicy.
func (h *Handler) forwardExecution(w http.ResponseWriter, incoming *http.Request, snapshot scheduler.Snapshot, plan flow.ExecutionPlan) {
	prepared := plan.PreparedRequest
	defer prepared.BaseBody.Close() // idempotent fallback for every early return

	if plan.TargetMode != flow.TargetModeProviderPool {
		if err := prepared.BaseBody.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
			return
		}
		writeError(w, http.StatusNotImplemented, "target_not_implemented", "target mode is not implemented")
		return
	}
	if err := plan.AttemptPolicy.Validate(); err != nil {
		_ = prepared.BaseBody.Close()
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "invalid request attempt policy")
		return
	}

	trafficClass := requestTypeTrafficClass(prepared.Plan.RequestType)
	sticky := scheduler.StickyKey{
		SessionID:    prepared.Plan.OriginalSessionID,
		Model:        prepared.Plan.EffectiveModel,
		TrafficClass: trafficClass,
	}
	excluded := make(map[string]struct{})
	var last *capturedFailure
	for attempt := 1; attempt <= plan.AttemptPolicy.MaxAttempts; attempt++ {
		lease, err := h.selector.Acquire(snapshot, sticky, excluded)
		if err != nil {
			h.finishCapturedFailure(w, prepared.BaseBody, last, err)
			return
		}
		if lease.Provider == nil {
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.finishLocalSelectionFailure(w, prepared.BaseBody, last, "selected provider is unavailable")
			return
		}
		if _, duplicate := excluded[lease.Provider.ID]; duplicate {
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.finishLocalSelectionFailure(w, prepared.BaseBody, last, "provider selection repeated an attempt")
			return
		}
		if last != nil {
			h.recordFailover(last, lease, attempt, incoming)
			last = nil
		}
		failure, done := h.executeAttempt(w, incoming, prepared, lease, sticky.SessionID, attempt)
		if done {
			return
		}
		last = failure
		excluded[lease.Provider.ID] = struct{}{}
	}
	h.finishCapturedFailure(w, prepared.BaseBody, last, nil)
}

func (h *Handler) finishCapturedFailure(w http.ResponseWriter, base bodyfile.Body, last *capturedFailure, unavailable error) {
	if last != nil {
		h.recordCapturedFailure(last)
	}
	if err := base.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
		return
	}
	if last != nil {
		h.writeCapturedFailure(w, last)
		return
	}
	if unavailable != nil {
		h.writeUnavailable(w, unavailable)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "no provider is available")
}

func (h *Handler) finishLocalSelectionFailure(w http.ResponseWriter, base bodyfile.Body, last *capturedFailure, message string) {
	if last != nil {
		h.recordCapturedFailure(last)
	}
	if err := base.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
		return
	}
	writeError(w, http.StatusBadGateway, "bad_gateway", message)
}

func (h *Handler) executeAttempt(w http.ResponseWriter, incoming *http.Request, prepared traffic.PreparedRequest, lease scheduler.AttemptLease, sessionID string, attempt int) (*capturedFailure, bool) {
	item := lease.Provider
	if item == nil {
		return nil, true
	}
	url, err := upstreamURL(item, incoming.URL)
	if err != nil {
		return nil, h.terminalLocalAttempt(w, prepared.BaseBody, lease, sessionID, attempt, "", http.StatusBadGateway, "bad_gateway", "provider request could not be built", err, "", "")
	}
	headers, err := cleanUpstreamHeaders(incoming.Header)
	if err != nil {
		return nil, h.terminalLocalAttempt(w, prepared.BaseBody, lease, sessionID, attempt, url.String(), http.StatusBadGateway, "bad_gateway", "provider request could not be built", err, "", "")
	}

	patchContext := patch.PatchContext{
		RequestType:       prepared.Plan.RequestType,
		OriginalModel:     prepared.Plan.OriginalModel,
		EffectiveModel:    prepared.Plan.EffectiveModel,
		OriginalSessionID: prepared.Plan.OriginalSessionID,
		TargetID:          item.ID,
		Generation:        item.Generation.String(),
	}
	execution, err := item.PatchPlan.NewInstance(patchContext)
	if err != nil {
		return h.requestPatchFailure(w, prepared.BaseBody, lease, sessionID, attempt, url.String(), nil, nil, err)
	}

	base := prepared.BaseBody
	mutable := patch.NewMutableRequest(base, prepared.BaseIndex, patch.NewHTTPHeaderSet(headers))
	if err := execution.ApplyRequestOnly(mutable); err != nil {
		return h.requestPatchFailure(w, base, lease, sessionID, attempt, url.String(), execution, mutable.Body, err)
	}
	if mutable.Body == nil {
		return h.requestPatchFailure(w, base, lease, sessionID, attempt, url.String(), execution, nil, errors.New("patch returned nil body"))
	}
	outboundHeaders, err := mutableHTTPHeaders(mutable.Headers)
	if err != nil {
		return h.requestPatchFailure(w, base, lease, sessionID, attempt, url.String(), execution, mutable.Body, err)
	}
	if err := item.ApplyAuthHeaders(outboundHeaders); err != nil {
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusBadGateway, "bad_gateway", "provider request could not be authenticated", errors.Join(err, cleanupErr), "", "")
	}
	requestBody, err := mutable.Body.OpenReader()
	if err != nil {
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), errors.Join(err, cleanupErr), "", "")
	}
	request, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, url.String(), requestBody)
	if err != nil {
		closeErr := requestBody.Close()
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusBadGateway, "bad_gateway", "provider request could not be built", errors.Join(err, closeErr, cleanupErr), "", "")
	}
	request.Header = outboundHeaders
	request.ContentLength = mutable.Body.Size()
	request.Host = ""
	request.GetBody = nil
	h.record(Event{Kind: EventForward, Time: h.now().UTC(), ProviderID: item.ID, ProviderName: item.Name, SessionID: sessionID, Model: lease.Model, TrafficClass: lease.TrafficClass, Attempt: attempt, UpstreamURL: url.String()})

	client, err := h.clients.Client(item)
	if err != nil {
		closeErr := requestBody.Close()
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusInternalServerError, "gateway_unavailable", "provider client is unavailable", errors.Join(err, closeErr, cleanupErr), "", "")
	}
	response, requestErr := client.Do(request)
	requestCloseErr := requestBody.Close()
	if requestErr != nil {
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		combined := errors.Join(requestErr, requestCloseErr, cleanupErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), combined, "", "")
		}
		return h.handlePlanTransportError(w, incoming.Context(), base, lease, sessionID, attempt, url.String(), combined)
	}
	if requestCloseErr != nil {
		_ = response.Body.Close()
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), errors.Join(requestCloseErr, cleanupErr), "", "")
	}

	outcome := scheduler.Outcome{Class: scheduler.ClassifyHTTPStatus(response.StatusCode), HTTPStatus: response.StatusCode, UpstreamURL: url.String(), SessionID: sessionID, RetryAfter: parseRetryAfter(response.Header, h.now()), HasRetryAfter: hasValidRetryAfter(response.Header)}
	if outcome.ShouldFailover() {
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		bodyCleanupErr := closeRequestBodies(base, mutable.Body, false)
		patchCleanupErr := closePatchExecution(execution)
		if requestCanceled(incoming.Context()) {
			outcome.Class = scheduler.FailureClientCanceled
			outcome.ClientCanceled = true
			outcome.RawError = contextError(incoming.Context(), readErr).Error()
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
			_ = base.Close()
			return nil, true
		}
		if bodyCleanupErr != nil {
			return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), errors.Join(bodyCleanupErr, patchCleanupErr), "", "")
		}
		if patchCleanupErr != nil {
			return nil, h.terminalPatchFailure(w, base, lease, sessionID, attempt, url.String(), patchCleanupErr, "", patch.StageRequest)
		}
		if readErr != nil || closeErr != nil {
			if readErr == nil {
				readErr = closeErr
			}
			outcome.Class = scheduler.FailureChannelStream
			outcome.RawError = readErr.Error()
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
			_ = base.Close()
			writeError(w, http.StatusBadGateway, "bad_gateway", "provider response ended unexpectedly")
			return nil, true
		}
		outcome.RawError = string(data)
		update := h.selector.Report(lease, outcome)
		return &capturedFailure{lease: lease, outcome: outcome, update: update, attempt: attempt, headers: response.Header.Clone(), body: data}, false
	}

	// The request will not be retried. Release all request bodies before making
	// any terminal response visible to the client.
	if err := closeRequestBodies(base, mutable.Body, true); err != nil {
		_ = response.Body.Close()
		return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, url.String(), errors.Join(err, execution.Close()), "", "")
	}
	if hasResponseHooks(item.PatchPlan, prepared.Plan.RequestType) && response.StatusCode >= 200 && response.StatusCode < 300 {
		return h.executeBufferedResponse(w, incoming, lease, sessionID, attempt, response, execution, url.String())
	}
	if err := execution.Close(); err != nil {
		_ = response.Body.Close()
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, url.String(), err, "", patch.StageRequest)
	}
	h.streamResponse(w, incoming.Context(), response, lease, outcome, attempt)
	return nil, true
}

func (h *Handler) requestPatchFailure(w http.ResponseWriter, base bodyfile.Body, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, execution *patch.Execution, attemptBody bodyfile.Body, err error) (*capturedFailure, bool) {
	patchID, stage := patchErrorDetails(err, patch.StageRequest)
	cleanupErr := closeRequestAttempt(base, attemptBody, execution, false)
	if cleanupErr != nil {
		combined := errors.Join(err, cleanupErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, upstream, combined, patchID, stage)
		}
		return nil, h.terminalPatchFailure(w, base, lease, sessionID, attempt, upstream, combined, patchID, stage)
	}
	if errors.Is(err, bodyfile.ErrLocalIO) {
		return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, upstream, err, patchID, stage)
	}
	outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: upstream, RawError: err.Error()}
	update := h.selector.Report(lease, outcome)
	return &capturedFailure{lease: lease, outcome: outcome, update: update, attempt: attempt, transport: true, patchFailure: true, patchID: patchID, patchStage: string(stage)}, false
}

func (h *Handler) handlePlanTransportError(w http.ResponseWriter, ctx context.Context, base bodyfile.Body, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, err error) (*capturedFailure, bool) {
	outcome := scheduler.Outcome{Class: scheduler.FailureGlobalTransient, UpstreamURL: upstream, RawError: err.Error(), SessionID: sessionID}
	if requestCanceled(ctx) {
		outcome.Class = scheduler.FailureClientCanceled
		outcome.ClientCanceled = true
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		_ = base.Close()
		return nil, true
	}
	update := h.selector.Report(lease, outcome)
	return &capturedFailure{lease: lease, outcome: outcome, update: update, attempt: attempt, transport: true}, false
}

func (h *Handler) executeBufferedResponse(w http.ResponseWriter, incoming *http.Request, lease scheduler.AttemptLease, sessionID string, attempt int, response *http.Response, execution *patch.Execution, upstream string) (*capturedFailure, bool) {
	body, err := bodyfile.Capture(response.Body, h.replayDirectory)
	closeErr := response.Body.Close()
	if requestCanceled(incoming.Context()) {
		if body != nil {
			_ = body.Close()
		}
		_ = execution.Close()
		outcome := scheduler.Outcome{Class: scheduler.FailureClientCanceled, HTTPStatus: response.StatusCode, UpstreamURL: upstream, SessionID: sessionID, RawError: contextError(incoming.Context(), err).Error(), ClientCanceled: true}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		return nil, true
	}
	if err != nil || closeErr != nil {
		executionErr := execution.Close()
		cleanupErr := error(nil)
		if body != nil {
			cleanupErr = body.Close()
		}
		combined := errors.Join(err, closeErr, cleanupErr, executionErr)
		if errors.Is(err, bodyfile.ErrLocalIO) || errors.Is(closeErr, bodyfile.ErrLocalIO) || errors.Is(cleanupErr, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, combined, "", patch.StageResponse)
		}
		class := scheduler.FailureNeutral
		if closeErr != nil || bodyfile.IsReadError(err) && !errors.Is(err, bodyfile.ErrLocalIO) {
			class = scheduler.FailureChannelStream
		}
		outcome := scheduler.Outcome{Class: class, HTTPStatus: response.StatusCode, UpstreamURL: upstream, SessionID: sessionID, RawError: safeBodyfileError(combined)}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		if bodyfile.IsReadError(err) || closeErr != nil {
			writeError(w, http.StatusBadGateway, "bad_gateway", "provider response ended unexpectedly")
		} else {
			writeError(w, http.StatusBadGateway, "patch_failed", "response patch failed")
		}
		return nil, true
	}

	mutable := &patch.MutableResponse{Status: response.StatusCode, Body: body, Headers: patch.NewHTTPHeaderSet(response.Header.Clone())}
	if err := execution.ApplyResponseOnly(mutable); err != nil {
		patchID, stage := patchErrorDetails(err, patch.StageResponse)
		cleanupErr := closeResponseBodies(body, mutable.Body)
		executionErr := execution.Close()
		combined := errors.Join(err, cleanupErr, executionErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, combined, patchID, stage)
		}
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, combined, patchID, stage)
	}
	responseHeaders, err := mutableHTTPHeaders(mutable.Headers)
	if err != nil {
		cleanupErr := closeResponseBodies(body, mutable.Body)
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr, execution.Close()), "", patch.StageResponse)
	}
	if mutable.Body == nil {
		_ = body.Close()
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(errors.New("patch returned nil response body"), execution.Close()), "", patch.StageResponse)
	}
	if err := execution.Close(); err != nil {
		cleanupErr := closeResponseBodies(body, mutable.Body)
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
	}
	deleteHeaderFold(responseHeaders, "Content-Encoding")
	deleteHeaderFold(responseHeaders, "Content-Length")
	responseHeaders.Set("Content-Length", strconv.FormatInt(mutable.Body.Size(), 10))
	reader, err := mutable.Body.OpenReader()
	if err != nil {
		cleanupErr := closeResponseBodies(body, mutable.Body)
		if errors.Is(err, bodyfile.ErrLocalIO) || errors.Is(cleanupErr, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
		}
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
	}

	copyResponseHeaders(w.Header(), responseHeaders)
	w.WriteHeader(mutable.Status)
	_, copyErr := io.Copy(w, reader)
	readerCloseErr := reader.Close()
	bodyCloseErr := closeResponseBodies(body, mutable.Body)
	outcome := scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: mutable.Status, UpstreamURL: upstream, SessionID: sessionID, ResponseStarted: true}
	if copyErr != nil {
		outcome.Class = scheduler.FailureDownstream
		outcome.RawError = copyErr.Error()
		if requestCanceled(incoming.Context()) {
			outcome.Class = scheduler.FailureClientCanceled
			outcome.ClientCanceled = true
		}
	} else if readerCloseErr != nil || bodyCloseErr != nil {
		outcome.Class = scheduler.FailureNeutral
		outcome.RawError = safeBodyfileError(errors.Join(readerCloseErr, bodyCloseErr))
	}
	update := h.selector.Report(lease, outcome)
	if outcome.Class == scheduler.FailureNone {
		h.recordOutcome(EventSuccess, lease, outcome, attempt, update)
	} else {
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
	}
	return nil, true
}

func hasResponseHooks(plan patch.Plan, requestType traffic.RequestType) bool {
	for _, metadata := range plan.List() {
		applies := false
		for _, typ := range metadata.RequestTypes {
			if typ == patch.RequestTypeAny || typ == requestType {
				applies = true
				break
			}
		}
		if !applies {
			continue
		}
		for _, stage := range metadata.Stages {
			if stage == patch.StageResponse {
				return true
			}
		}
	}
	return false
}

func cleanUpstreamHeaders(source http.Header) (http.Header, error) {
	headers := source.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	removeHopByHop(headers)
	deleteHeaderFold(headers, "Content-Length")
	deleteHeaderFold(headers, "Host")
	deleteHeaderFold(headers, "Authorization")
	deleteHeaderFold(headers, "X-Api-Key")
	return headers, nil
}

func mutableHTTPHeaders(headers patch.MutableHeaderSet) (http.Header, error) {
	set, ok := headers.(*patch.HTTPHeaderSet)
	if !ok || set == nil || set.Header == nil {
		return nil, errors.New("patch returned an unsupported header set")
	}
	return set.Header, nil
}

func sameBody(a, b bodyfile.Body) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	if va.Type().Comparable() {
		return va.Interface() == vb.Interface()
	}
	if va.Kind() == reflect.Ptr {
		return va.Pointer() == vb.Pointer()
	}
	return false
}

func closeRequestAttempt(base, attempt bodyfile.Body, execution *patch.Execution, closeBase bool) error {
	return errors.Join(closeRequestBodies(base, attempt, closeBase), closePatchExecution(execution))
}

func closeRequestBodies(base, attempt bodyfile.Body, closeBase bool) error {
	var result error
	if attempt != nil && !sameBody(base, attempt) {
		result = errors.Join(result, attempt.Close())
	}
	if closeBase && base != nil {
		result = errors.Join(result, base.Close())
	}
	return result
}

func closeResponseBodies(original, current bodyfile.Body) error {
	var result error
	if current != nil && !sameBody(original, current) {
		result = errors.Join(result, current.Close())
	}
	if original != nil {
		result = errors.Join(result, original.Close())
	}
	return result
}

func closePatchExecution(execution *patch.Execution) error {
	if execution == nil {
		return nil
	}
	return execution.Close()
}

func patchErrorDetails(err error, fallback patch.Stage) (string, patch.Stage) {
	var hookErr *patch.HookError
	if errors.As(err, &hookErr) {
		return hookErr.PatchID, hookErr.Stage
	}
	return "", fallback
}

func safeBodyfileError(err error) string {
	if errors.Is(err, bodyfile.ErrLocalIO) {
		return bodyfile.ErrLocalIO.Error()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *Handler) terminalReplayFailure(w http.ResponseWriter, base bodyfile.Body, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, err error, patchID string, stage patch.Stage) bool {
	if base != nil {
		err = errors.Join(err, base.Close())
	}
	outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: upstream, RawError: safeBodyfileError(err)}
	update := h.selector.Report(lease, outcome)
	h.recordOutcomeWithPatch(EventFailure, lease, outcome, attempt, update, patchID, stage)
	writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be replayed")
	return true
}

func (h *Handler) terminalLocalAttempt(w http.ResponseWriter, base bodyfile.Body, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, status int, code, message string, err error, patchID string, stage patch.Stage) bool {
	if base != nil {
		err = errors.Join(err, base.Close())
	}
	outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: upstream, RawError: safeBodyfileError(err)}
	update := h.selector.Report(lease, outcome)
	h.recordOutcomeWithPatch(EventFailure, lease, outcome, attempt, update, patchID, stage)
	writeError(w, status, code, message)
	return true
}

func (h *Handler) terminalPatchFailure(w http.ResponseWriter, base bodyfile.Body, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, err error, patchID string, stage patch.Stage) bool {
	if base != nil {
		err = errors.Join(err, base.Close())
	}
	outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: upstream, RawError: safeBodyfileError(err)}
	update := h.selector.Report(lease, outcome)
	h.recordOutcomeWithPatch(EventFailure, lease, outcome, attempt, update, patchID, stage)
	writeError(w, http.StatusBadGateway, "patch_failed", "provider patch failed")
	return true
}
