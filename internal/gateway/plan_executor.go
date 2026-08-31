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
	"github.com/Siriusrry/cc-automux/internal/provider"
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

func (h *Handler) prepareIngress(body bodyfile.Body, index bodyfile.JSONIndex, request *http.Request) (traffic.IngressRequest, error) {
	model := index.ModelValue()
	session := singleSessionHeader(request.Header)
	detection := traffic.NewDetectionRequest(body, index, model, session, traffic.NewHeaderView(request.Header))
	if h.detectors == nil {
		return traffic.IngressRequest{}, traffic.ErrNilDetectorRegistry
	}
	return h.detectors.ClassifyDetection(detection)
}

// requestScanSpec computes the single ingress scan contract before the model
// and request type are known. Normal fallback and every specialised detector
// type are possible at this point, so the scanner retains the union of their
// request-patch paths (plus detector-declared paths). The resulting index is
// retained after classification; the request path never performs a second
// projection pass.
func (h *Handler) requestScanSpec(snapshot scheduler.Snapshot) (bodyfile.ScanSpec, error) {
	spec, _, err := h.requestScanSpecWithProviders(snapshot)
	return spec, err
}

// requestScanSpecWithProviders returns the ingress scan contract and the one
// defensive Provider snapshot used to compute it. The Gateway reuses that
// exact slice for client-pool reconciliation, avoiding a second provider-list
// copy on accepted requests.
func (h *Handler) requestScanSpecWithProviders(snapshot scheduler.Snapshot) (bodyfile.ScanSpec, []*provider.CompiledProvider, error) {
	types := []traffic.RequestType{traffic.RequestTypeNormal}
	paths := make([]string, 0)
	var providers []*provider.CompiledProvider
	if h != nil && h.detectors != nil {
		paths = appendUniquePaths(paths, h.detectors.RequiredPaths()...)
		types = append(types, h.detectors.Types()...)
	}
	if snapshot != nil {
		providers = snapshot.Providers()
		for _, item := range providers {
			// Only providers that can be selected in this immutable snapshot
			// contribute to the ingress union. Disabled/no-model entries are not
			// reachable and must not force unrelated patch fields into every index.
			if item == nil || !item.Enabled || len(item.Models) == 0 {
				continue
			}
			for _, requestType := range types {
				required, err := item.PatchPlan.RequiredPaths(patch.StageRequest, requestType)
				if err != nil {
					return bodyfile.ScanSpec{}, providers, err
				}
				paths = appendUniquePaths(paths, required...)
			}
		}
	}
	// A fixed classifier target is outside the scheduler Provider catalog, but
	// its request hooks are still a possible ingress consumer. Include its
	// classifier paths in the same pre-receive union so fixed-target execution
	// never depends on a post-capture rescan.
	if flowView, ok := snapshot.(flow.SnapshotView); ok {
		fixed := flowView.AutoMode().FixedTarget
		if fixed != nil {
			// Fixed targets are classifier-only by contract. Their request paths
			// belong to the same ingress union even though the target is outside
			// scheduler.Snapshot.Providers().
			required, err := fixed.PatchPlan.RequiredPaths(patch.StageRequest, traffic.RequestTypeClassifier)
			if err != nil {
				return bodyfile.ScanSpec{}, providers, err
			}
			paths = appendUniquePaths(paths, required...)
		}
	}
	var markers []string
	if h != nil && h.detectors != nil {
		markers = h.detectors.RequiredRawMarkers()
	}
	spec, err := bodyfile.RequestScanSpecWithRawMarkers(paths, markers...)
	return spec, providers, err
}

func appendUniquePaths(paths []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(additions))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range additions {
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
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
	if prepared.BaseBody == nil {
		if incoming == nil || !requestCanceled(incoming.Context()) {
			writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request body is unavailable")
		}
		return
	}
	if err := plan.Validate(); err != nil {
		_ = prepared.BaseBody.Close()
		if incoming == nil || !requestCanceled(incoming.Context()) {
			writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request execution plan is invalid")
		}
		return
	}

	if plan.TargetMode == flow.TargetModeFixedTarget {
		// The fixed executor owns the prepared body and every conversion/response
		// body it creates.  Keeping its ownership separate avoids a second Close
		// call from this common executor (custom Body implementations may expose
		// observable close errors even though production file bodies are idempotent).
		h.forwardFixedExecution(w, incoming, snapshot, plan)
		return
	}
	defer prepared.BaseBody.Close() // idempotent fallback for every early return
	// A cancellation observed after planning but before target selection must
	// stop the request without even acquiring a health lease.  Once a lease has
	// been acquired the loop below reports the cancellation so the lease is
	// released exactly once.
	if incoming != nil && requestCanceled(incoming.Context()) {
		return
	}
	if plan.TargetMode != flow.TargetModeProviderPool {
		if err := prepared.BaseBody.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
			return
		}
		writeError(w, http.StatusNotImplemented, "target_not_implemented", "target mode is not implemented")
		return
	}
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	if len(snapshot.Candidates(prepared.Plan.EffectiveModel)) == 0 {
		writeError(w, http.StatusNotFound, "model_not_configured", "model is not configured")
		return
	}
	if incoming == nil || incoming.URL == nil {
		writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request is unavailable")
		return
	}
	if h == nil || h.selector == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	if err := plan.AttemptPolicy.Validate(); err != nil {
		_ = prepared.BaseBody.Close()
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "invalid request attempt policy")
		return
	}

	sticky := scheduler.StickyKey{
		SessionID:   prepared.Plan.OriginalSessionID,
		Model:       prepared.Plan.EffectiveModel,
		RequestType: prepared.Plan.RequestType,
	}
	excluded := make(map[string]struct{})
	var last *capturedFailure
	for attempt := 1; attempt <= plan.AttemptPolicy.MaxAttempts; attempt++ {
		if requestCanceled(incoming.Context()) {
			// A retryable response may already have been reported to Health, but its
			// event is intentionally delayed until the gateway knows whether a
			// replacement will be attempted.  Cancellation removes that decision:
			// finish it as a normal terminal failure and never emit EventFailover.
			if last != nil {
				h.recordCapturedFailure(last)
			}
			return
		}
		lease, err := acquireWithPlanPolicy(h.selector, snapshot, sticky, excluded, plan.AttemptPolicy)
		if err != nil {
			if requestCanceled(incoming.Context()) {
				if last != nil {
					h.recordCapturedFailure(last)
				}
				return
			}
			h.finishCapturedFailure(w, prepared.BaseBody, last, err)
			return
		}
		if lease.Provider == nil {
			if requestCanceled(incoming.Context()) {
				if last != nil {
					h.recordCapturedFailure(last)
				}
				return
			}
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.finishLocalSelectionFailure(w, prepared.BaseBody, last, "selected provider is unavailable")
			return
		}
		if requestCanceled(incoming.Context()) {
			// The lease is real even though no upstream call has started.  Report a
			// client cancellation to release its health token, then let the deferred
			// BaseBody cleanup finish the request.
			if last != nil {
				h.recordCapturedFailure(last)
			}
			h.reportClientCanceledWithAttempt(lease, sticky.SessionID, "", 0, attempt, nil)
			return
		}
		if _, duplicate := excluded[lease.Provider.ID]; duplicate {
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.finishLocalSelectionFailure(w, prepared.BaseBody, last, "provider selection repeated an attempt")
			return
		}
		if last != nil {
			if requestCanceled(incoming.Context()) {
				h.recordCapturedFailure(last)
				h.reportClientCanceledWithAttempt(lease, sticky.SessionID, "", 0, attempt, nil)
				return
			}
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

// attemptPolicySnapshot adapts the immutable plan budget to the legacy
// scheduler.Snapshot interface. The concrete Scheduler reads AttemptPolicy
// from this wrapper, so classifier requests cannot accidentally consume the
// ordinary three-attempt default.
type attemptPolicySnapshot struct {
	scheduler.Snapshot
	policy scheduler.AttemptPolicy
}

func (s attemptPolicySnapshot) AttemptPolicy() scheduler.AttemptPolicy { return s.policy }

func (s attemptPolicySnapshot) AttemptPolicyFor(traffic.RequestType) scheduler.AttemptPolicy {
	return s.policy
}

func snapshotWithAttemptPolicy(snapshot scheduler.Snapshot, policy scheduler.AttemptPolicy) scheduler.Snapshot {
	if snapshot == nil {
		return nil
	}
	return attemptPolicySnapshot{Snapshot: snapshot, policy: policy}
}

func acquireWithPlanPolicy(selector scheduler.Selector, snapshot scheduler.Snapshot, key scheduler.StickyKey, excluded map[string]struct{}, policy scheduler.AttemptPolicy) (scheduler.AttemptLease, error) {
	if requestSelector, ok := selector.(scheduler.RequestPolicySelector); ok {
		return requestSelector.AcquireWithPolicy(snapshot, key, excluded, policy)
	}
	return selector.Acquire(snapshotWithAttemptPolicy(snapshot, policy), key, excluded)
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
	if item == nil || incoming == nil || incoming.URL == nil {
		return nil, true
	}
	ctx := incoming.Context()
	if requestCanceled(ctx) {
		_ = prepared.BaseBody.Close()
		h.reportClientCanceledWithAttempt(lease, sessionID, "", 0, attempt, contextError(ctx, nil))
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
	// Every cancellation after a lease has been acquired is terminal for this
	// request.  Close all attempt-scoped resources before reporting the outcome;
	// the caller's deferred BaseBody close remains an idempotent safety net.
	cancelAttempt := func(attemptBody bodyfile.Body, requestBody io.ReadCloser, extra error) (*capturedFailure, bool) {
		closeErr := error(nil)
		if requestBody != nil {
			closeErr = requestBody.Close()
		}
		cleanupErr := closeRequestAttempt(base, attemptBody, execution, true)
		h.reportClientCanceledWithAttempt(lease, sessionID, url.String(), 0, attempt, errors.Join(contextError(ctx, nil), extra, closeErr, cleanupErr))
		return nil, true
	}
	if requestCanceled(ctx) {
		return cancelAttempt(nil, nil, nil)
	}
	mutable := patch.NewMutableRequest(base, prepared.BaseIndex, patch.NewHTTPHeaderSet(headers))
	if err := execution.ApplyRequestOnly(mutable); err != nil {
		return h.requestPatchFailure(w, base, lease, sessionID, attempt, url.String(), execution, mutable.Body, err)
	}
	if requestCanceled(ctx) {
		return cancelAttempt(mutable.Body, nil, nil)
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
	if requestCanceled(ctx) {
		return cancelAttempt(mutable.Body, requestBody, nil)
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
	if requestCanceled(ctx) {
		return cancelAttempt(mutable.Body, requestBody, nil)
	}

	if h.clients == nil {
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusInternalServerError, "gateway_unavailable", "provider client is unavailable", errors.New("provider client pool is nil"), "", "")
	}
	clientLease, err := h.clients.Acquire(item)
	if err != nil {
		closeErr := requestBody.Close()
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusInternalServerError, "gateway_unavailable", "provider client is unavailable", errors.Join(err, closeErr, cleanupErr), "", "")
	}
	defer clientLease.Release()
	if requestCanceled(ctx) {
		return cancelAttempt(mutable.Body, requestBody, nil)
	}
	h.record(Event{Kind: EventForward, Time: h.now().UTC(), ProviderID: item.ID, ProviderName: item.Name, SessionID: sessionID, Model: lease.Model, RequestType: lease.RequestType, Attempt: attempt, UpstreamURL: url.String()})
	response, requestErr := clientLease.Client().Do(request)
	requestCloseErr := requestBody.Close()
	if requestErr != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		combined := errors.Join(requestErr, requestCloseErr, cleanupErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), combined, "", "")
		}
		return h.handlePlanTransportError(w, incoming.Context(), base, lease, sessionID, attempt, url.String(), combined)
	}
	if requestCanceled(ctx) {
		var responseCloseErr error
		if response != nil && response.Body != nil {
			responseCloseErr = response.Body.Close()
		}
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, true)
		h.reportClientCanceledWithAttempt(lease, sessionID, url.String(), responseStatus(response), attempt, errors.Join(requestCloseErr, responseCloseErr, cleanupErr))
		return nil, true
	}
	if requestCloseErr != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cleanupErr := closeRequestAttempt(base, mutable.Body, execution, false)
		return nil, h.terminalReplayFailure(w, base, lease, sessionID, attempt, url.String(), errors.Join(requestCloseErr, cleanupErr), "", "")
	}

	if response == nil {
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusBadGateway, "bad_gateway", "provider returned no response", errors.New("nil upstream response"), "", "")
	}
	if response.Body == nil {
		return nil, h.terminalLocalAttempt(w, base, lease, sessionID, attempt, url.String(), http.StatusBadGateway, "bad_gateway", "provider returned no response body", errors.New("nil upstream response body"), "", "")
	}
	outcome := scheduler.Outcome{Class: scheduler.ClassifyHTTPStatus(response.StatusCode), HTTPStatus: response.StatusCode, UpstreamURL: url.String(), SessionID: sessionID, RetryAfter: parseRetryAfter(response.Header, h.now()), HasRetryAfter: hasValidRetryAfter(response.Header)}
	if outcome.ShouldFailover() {
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		bodyCleanupErr := closeRequestBodies(base, mutable.Body, false)
		patchCleanupErr := closePatchExecution(execution)
		if requestCanceled(ctx) {
			outcome.Class = scheduler.FailureClientCanceled
			outcome.ClientCanceled = true
			outcome.RawError = errors.Join(contextError(ctx, readErr), bodyCleanupErr, patchCleanupErr, closeErr).Error()
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
		// Cancellation can race the final read/close boundary.  Re-check before
		// exposing this captured failure to the fallback loop so it cannot trigger
		// a second Provider call after the client has gone away.
		if requestCanceled(ctx) {
			outcome.Class = scheduler.FailureClientCanceled
			outcome.ClientCanceled = true
			outcome.RawError = contextError(ctx, nil).Error()
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
			_ = base.Close()
			return nil, true
		}
		update := h.selector.Report(lease, outcome)
		return &capturedFailure{lease: lease, outcome: outcome, update: update, attempt: attempt, headers: response.Header.Clone(), body: data}, false
	}

	// The request will not be retried. Release all request bodies before making
	// any terminal response visible to the client.
	if err := closeRequestBodies(base, mutable.Body, true); err != nil {
		_ = response.Body.Close()
		if requestCanceled(ctx) {
			h.reportClientCanceledWithAttempt(lease, sessionID, url.String(), response.StatusCode, attempt, err)
			return nil, true
		}
		return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, url.String(), errors.Join(err, execution.Close()), "", "")
	}
	if requestCanceled(ctx) {
		_ = response.Body.Close()
		_ = execution.Close()
		h.reportClientCanceledWithAttempt(lease, sessionID, url.String(), response.StatusCode, attempt, nil)
		return nil, true
	}
	if item.PatchPlan.HasStage(patch.StageResponse, prepared.Plan.RequestType) && response.StatusCode >= 200 && response.StatusCode < 300 {
		responseSpec, specErr := responseScanSpec(item.PatchPlan, prepared.Plan.RequestType)
		if specErr != nil {
			_ = response.Body.Close()
			_ = execution.Close()
			return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, url.String(), specErr, "", patch.StageResponse)
		}
		return h.executeBufferedResponse(w, incoming, lease, sessionID, attempt, response, execution, url.String(), responseSpec)
	}
	if err := execution.Close(); err != nil {
		_ = response.Body.Close()
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, url.String(), err, "", patch.StageRequest)
	}
	h.streamResponse(w, ctx, response, lease, outcome, attempt)
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
	// Request patch failures are terminal and must never trigger a Provider
	// failover. The patch may have partially transformed the attempt body, so all
	// cleanup happens before the single failure event/response is emitted.
	return nil, h.terminalPatchFailure(w, base, lease, sessionID, attempt, upstream, err, patchID, stage)
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

func (h *Handler) executeBufferedResponse(w http.ResponseWriter, incoming *http.Request, lease scheduler.AttemptLease, sessionID string, attempt int, response *http.Response, execution *patch.Execution, upstream string, spec bodyfile.ScanSpec) (*capturedFailure, bool) {
	if response == nil || response.Body == nil {
		if requestCanceled(incomingContext(incoming)) {
			h.reportClientCanceledWithAttempt(lease, sessionID, upstream, responseStatus(response), attempt, nil)
			return nil, true
		}
		return nil, h.terminalLocalAttempt(w, nil, lease, sessionID, attempt, upstream, http.StatusBadGateway, "bad_gateway", "provider returned no response body", errors.New("provider returned no response body"), "", patch.StageResponse)
	}
	ctx := incomingContext(incoming)
	responseStatusCode := response.StatusCode
	responseHeaders := response.Header.Clone()
	closedResponse := false
	closeResponse := func() error {
		if closedResponse {
			return nil
		}
		closedResponse = true
		return response.Body.Close()
	}
	defer func() { _ = closeResponse() }()
	report := func(kind EventKind, value scheduler.Outcome) {
		if h == nil || h.selector == nil {
			return
		}
		update := h.selector.Report(lease, value)
		h.recordOutcome(kind, lease, value, attempt, update)
	}
	cancel := func(cause error, responseStarted bool) {
		cleanupErr := errors.Join(closeResponse(), execution.Close())
		value := scheduler.Outcome{
			Class:           scheduler.FailureClientCanceled,
			HTTPStatus:      responseStatusCode,
			UpstreamURL:     upstream,
			SessionID:       sessionID,
			ResponseStarted: responseStarted,
			ClientCanceled:  true,
			RawError:        errors.Join(contextError(ctx, cause), cleanupErr).Error(),
		}
		report(EventFailure, value)
	}
	if requestCanceled(ctx) {
		cancel(nil, false)
		return nil, true
	}
	body, index, captureErr := bodyfile.CaptureAndScan(response.Body, spec, h.replayDirectory)
	closeErr := closeResponse()
	if requestCanceled(ctx) {
		bodyErr := error(nil)
		if body != nil {
			bodyErr = body.Close()
		}
		cancel(errors.Join(captureErr, closeErr, bodyErr), false)
		return nil, true
	}
	if captureErr != nil || closeErr != nil {
		executionErr := execution.Close()
		cleanupErr := error(nil)
		if body != nil {
			cleanupErr = body.Close()
		}
		combined := errors.Join(captureErr, closeErr, cleanupErr, executionErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, combined, "", patch.StageResponse)
		}
		class := scheduler.FailureNeutral
		if closeErr != nil || bodyfile.IsReadError(captureErr) && !errors.Is(captureErr, bodyfile.ErrLocalIO) {
			class = scheduler.FailureChannelStream
		}
		outcome := scheduler.Outcome{Class: class, HTTPStatus: responseStatusCode, UpstreamURL: upstream, SessionID: sessionID, RawError: safeBodyfileError(combined)}
		report(EventFailure, outcome)
		if bodyfile.IsReadError(captureErr) || closeErr != nil {
			writeError(w, http.StatusBadGateway, "bad_gateway", "provider response ended unexpectedly")
		} else {
			writeError(w, http.StatusBadGateway, "patch_failed", "response patch failed")
		}
		return nil, true
	}

	mutable := patch.NewMutableResponse(responseStatusCode, body, index, patch.NewHTTPHeaderSet(responseHeaders))
	var err error
	if err := execution.ApplyResponseOnly(mutable); err != nil {
		if requestCanceled(ctx) {
			cleanupErr := closeResponseBodies(body, mutable.Body)
			cancel(errors.Join(err, cleanupErr), false)
			return nil, true
		}
		patchID, stage := patchErrorDetails(err, patch.StageResponse)
		cleanupErr := closeResponseBodies(body, mutable.Body)
		executionErr := execution.Close()
		combined := errors.Join(err, cleanupErr, executionErr)
		if errors.Is(combined, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, combined, patchID, stage)
		}
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, combined, patchID, stage)
	}
	responseHeaders, err = mutableHTTPHeaders(mutable.Headers)
	if err != nil {
		if requestCanceled(ctx) {
			cancel(err, false)
			return nil, true
		}
		cleanupErr := closeResponseBodies(body, mutable.Body)
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr, execution.Close()), "", patch.StageResponse)
	}
	if mutable.Body == nil {
		err = errors.New("patch returned nil response body")
		if requestCanceled(ctx) {
			cancel(err, false)
			return nil, true
		}
		_ = body.Close()
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, execution.Close()), "", patch.StageResponse)
	}
	if err := execution.Close(); err != nil {
		if requestCanceled(ctx) {
			cancel(err, false)
			return nil, true
		}
		cleanupErr := closeResponseBodies(body, mutable.Body)
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
	}
	deleteHeaderFold(responseHeaders, "Content-Encoding")
	deleteHeaderFold(responseHeaders, "Content-Length")
	responseHeaders.Set("Content-Length", strconv.FormatInt(mutable.Body.Size(), 10))
	reader, err := mutable.Body.OpenReader()
	if err != nil {
		if requestCanceled(ctx) {
			cancel(err, false)
			return nil, true
		}
		cleanupErr := closeResponseBodies(body, mutable.Body)
		if errors.Is(err, bodyfile.ErrLocalIO) || errors.Is(cleanupErr, bodyfile.ErrLocalIO) {
			return nil, h.terminalReplayFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
		}
		return nil, h.terminalPatchFailure(w, nil, lease, sessionID, attempt, upstream, errors.Join(err, cleanupErr), "", patch.StageResponse)
	}
	readerClosed := false
	closeReader := func() error {
		if readerClosed {
			return nil
		}
		readerClosed = true
		return reader.Close()
	}
	defer func() { _ = closeReader() }()
	if requestCanceled(ctx) {
		cancel(closeReader(), false)
		_ = closeResponseBodies(body, mutable.Body)
		return nil, true
	}
	copyResponseHeaders(w.Header(), responseHeaders)
	if requestCanceled(ctx) {
		cancel(closeReader(), false)
		_ = closeResponseBodies(body, mutable.Body)
		return nil, true
	}
	w.WriteHeader(mutable.Status)
	responseStarted := true
	if requestCanceled(ctx) {
		cancel(closeReader(), responseStarted)
		_ = closeResponseBodies(body, mutable.Body)
		return nil, true
	}
	_, copyErr := io.Copy(w, reader)
	readerCloseErr := closeReader()
	bodyCloseErr := closeResponseBodies(body, mutable.Body)
	if requestCanceled(ctx) {
		cancel(errors.Join(copyErr, readerCloseErr, bodyCloseErr), responseStarted)
		return nil, true
	}
	outcome := scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: mutable.Status, UpstreamURL: upstream, SessionID: sessionID, ResponseStarted: responseStarted}
	if copyErr != nil {
		outcome.Class = scheduler.FailureDownstream
		outcome.RawError = copyErr.Error()
	} else if readerCloseErr != nil || bodyCloseErr != nil {
		outcome.Class = scheduler.FailureNeutral
		outcome.RawError = safeBodyfileError(errors.Join(readerCloseErr, bodyCloseErr))
	}
	if outcome.Class == scheduler.FailureNone {
		report(EventSuccess, outcome)
	} else {
		report(EventFailure, outcome)
	}
	return nil, true
}

func incomingContext(incoming *http.Request) context.Context {
	if incoming == nil {
		return context.Background()
	}
	return incoming.Context()
}

func responseScanSpec(plan patch.Plan, requestType traffic.RequestType) (bodyfile.ScanSpec, error) {
	paths, err := plan.RequiredPaths(patch.StageResponse, requestType)
	if err != nil {
		return bodyfile.ScanSpec{}, err
	}
	return bodyfile.ResponseScanSpec(paths...)
}

func cleanUpstreamHeaders(source http.Header) (http.Header, error) {
	headers := source.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	removeHopByHop(headers)
	deleteHeaderFold(headers, "Content-Length")
	deleteHeaderFold(headers, "Host")
	deleteCredentialHeaders(headers)
	return headers, nil
}

// deleteCredentialHeaders removes every credential-shaped header that can be
// supplied by a client or an adapter. Gateway writes the selected target key
// only after request patches/encoding complete.
func deleteCredentialHeaders(headers http.Header) {
	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key",
		"X-Gateway-Key", "X-Management-Key", "X-Provider-Key",
	} {
		deleteHeaderFold(headers, name)
	}
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
