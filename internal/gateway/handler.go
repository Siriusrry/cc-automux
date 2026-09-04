package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/protocol"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type SnapshotFunc func() scheduler.Snapshot

type requestScanSnapshot interface {
	RequestScanSpec() *bodyfile.CompiledScanSpec
}

type Options struct {
	ClientPool       *ClientPool
	Recorder         EventRecorder
	ReplayDirectory  string
	Now              func() time.Time
	DetectorRegistry *traffic.Registry
	FlowDispatcher   *flow.Dispatcher
	// ProtocolAdapters is the process-owned registry for fixed classifier
	// targets. Production currently supplies an empty registry; tests may
	// inject a fake adapter through this same seam.
	ProtocolAdapters protocol.ProtocolAdapterRegistry
	// ProtocolRegistry is a descriptive compatibility alias for
	// ProtocolAdapters. If both are set, ProtocolAdapters wins.
	ProtocolRegistry protocol.ProtocolAdapterRegistry
	FixedDiagnostics *automode.Diagnostics
}

type Handler struct {
	snapshot         SnapshotFunc
	selector         scheduler.Selector
	clients          *ClientPool
	recorder         EventRecorder
	replayDirectory  string
	now              func() time.Time
	detectors        *traffic.Registry
	flows            *flow.Dispatcher
	protocols        protocol.ProtocolAdapterRegistry
	fixedDiagnostics *automode.Diagnostics
	active           atomic.Int64
	clientRevisionMu sync.Mutex
	clientRevision   uint64
	fixedScopeMu     sync.RWMutex
	fixedScope       automode.DiagnosticScope
	fixedScopeBound  bool
}

func New(snapshot SnapshotFunc, selector scheduler.Selector) *Handler {
	return NewWithOptions(snapshot, selector, Options{})
}

func NewWithOptions(snapshot SnapshotFunc, selector scheduler.Selector, options Options) *Handler {
	clients := options.ClientPool
	if clients == nil {
		clients = NewClientPool()
	}
	recorder := options.Recorder
	if recorder == nil {
		recorder = discardRecorder{}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	detectors := options.DetectorRegistry
	if detectors == nil {
		detectors = traffic.DefaultRegistry()
	}
	flows := options.FlowDispatcher
	if flows == nil {
		normal, err := flow.NewRegistry(flow.NewNormalPlanner())
		if err != nil {
			// The built-in normal planner is static and cannot fail in normal
			// operation; retain a nil dispatcher so requests fail closed if a
			// future constructor change introduces an error.
			flows = nil
		} else {
			flows = flow.NewDispatcher(normal)
		}
	}
	protocols := options.ProtocolAdapters
	if protocols == nil {
		protocols = options.ProtocolRegistry
	}
	if protocols == nil {
		protocols = protocol.EmptyRegistry()
	}
	diagnostics := options.FixedDiagnostics
	if diagnostics == nil {
		diagnostics = automode.NewDiagnostics()
	}
	return &Handler{
		snapshot:         snapshot,
		selector:         selector,
		clients:          clients,
		recorder:         recorder,
		replayDirectory:  options.ReplayDirectory,
		now:              now,
		detectors:        detectors,
		flows:            flows,
		protocols:        protocols,
		fixedDiagnostics: diagnostics,
	}
}

func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	return h.clients.Close()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || r == nil || r.URL == nil || r.URL.Path != MessagesPath || r.URL.EscapedPath() != MessagesPath {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if h.snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	runtimeSnapshot := h.snapshot()
	if runtimeSnapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	snapshot, attemptPolicy, err := scheduler.CaptureAttemptPolicy(runtimeSnapshot)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	plannerSnapshot := capturedFlowView(runtimeSnapshot, snapshot, attemptPolicy)
	key := snapshot.GatewayKey()
	if key == "" {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured", "gateway key is not configured")
		return
	}
	if !authorized(r, key) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	h.active.Add(1)
	defer h.active.Add(-1)
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is required")
		return
	}
	defer r.Body.Close()
	scanSnapshot, ok := runtimeSnapshot.(requestScanSnapshot)
	if !ok {
		writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request scan could not be prepared")
		return
	}
	requestScan := scanSnapshot.RequestScanSpec()
	if requestScan == nil {
		writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request scan could not be prepared")
		return
	}
	// Capture and JSON scanning consume exactly the same ingress chunks.  The
	// sealed body is never reopened merely to build the index.
	captured, index, err := bodyfile.CaptureAndScanCompiled(r.Body, requestScan, h.replayDirectory)
	if err != nil {
		if requestCanceled(r.Context()) {
			return
		}
		if bodyfile.IsReadError(err) {
			writeError(w, http.StatusBadRequest, "invalid_body", "could not read request body")
			return
		}
		if errors.Is(err, bodyfile.ErrLocalIO) {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		} else if errors.Is(err, bodyfile.ErrModelMissing) || errors.Is(err, bodyfile.ErrModelRepeated) || errors.Is(err, bodyfile.ErrModelNotString) || errors.Is(err, bodyfile.ErrModelEmpty) || errors.Is(err, bodyfile.ErrModelTooLong) {
			writeError(w, http.StatusBadRequest, "invalid_model", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		}
		return
	}
	defer captured.Close()
	ingress, err := h.prepareIngress(captured, index, r)
	if err != nil {
		if errors.Is(err, traffic.ErrAmbiguousRequestType) {
			writeError(w, http.StatusBadRequest, "ambiguous_request_type", "request type is ambiguous")
		} else if errors.Is(err, traffic.ErrDetectionFailed) {
			writeError(w, http.StatusInternalServerError, "request_detection_failed", "request type detection failed")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_body", "request body could not be prepared")
		}
		return
	}
	// Keep the selective union captured at ingress. Classification does not
	// reread Providers or copy/filter the index; every possible request hook was
	// already accounted for before the body was received.
	plan, err := h.dispatchIngress(r.Context(), plannerSnapshot, ingress)
	if err != nil {
		if requestCanceled(r.Context()) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		} else if errors.Is(err, automode.ErrAutoModeNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, "auto_mode_not_configured", "auto mode is not configured")
		} else if errors.Is(err, bodyfile.ErrLocalIO) {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		} else if errors.Is(err, flow.ErrMissingPlanner) {
			writeError(w, http.StatusServiceUnavailable, "request_flow_unavailable", "request flow is unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request could not be prepared")
		}
		return
	}
	// Reconcile against the rich runtime view, not the attempt-policy wrapper,
	// so a fixed target remains part of the active transport set.
	h.reconcileClients(runtimeSnapshot)
	h.forwardExecution(w, r, snapshot, plan)
}

func (h *Handler) reconcileClients(snapshot scheduler.Snapshot) {
	if h == nil || snapshot == nil {
		return
	}
	h.clientRevisionMu.Lock()
	defer h.clientRevisionMu.Unlock()
	if snapshot.Revision() <= h.clientRevision {
		return
	}
	providers := snapshot.Providers()
	var fixed *provider.CompiledFixedTarget
	if view, ok := snapshot.(flow.SnapshotView); ok {
		fixed = view.AutoMode().FixedTarget
	}
	h.clients.ReconcileTargets(providers, fixed)
	h.clientRevision = snapshot.Revision()
}

// ActiveRequests reports data-plane requests currently being processed.
func (h *Handler) ActiveRequests() int64 {
	if h == nil {
		return 0
	}
	return h.active.Load()
}

// FixedTargetDiagnostics returns the latest fixed classifier target
// observation, detached from the handler's internal state.
func (h *Handler) FixedTargetDiagnostics() *automode.FixedTargetCall {
	if h == nil || h.fixedDiagnostics == nil {
		return nil
	}
	return h.fixedDiagnostics.Snapshot()
}

// SetRuntimeSnapshot advances the fixed-target diagnostic scope when a new
// immutable runtime revision is published. It is called by the application
// composition root, never by an individual request.
func (h *Handler) SetRuntimeSnapshot(snapshot scheduler.Snapshot) {
	if h == nil || h.fixedDiagnostics == nil || snapshot == nil {
		return
	}
	scope := automode.DiagnosticScope{Revision: snapshot.Revision()}
	if view, ok := snapshot.(flow.SnapshotView); ok {
		if target := view.AutoMode().FixedTarget; target != nil {
			scope.Configured = true
			scope.Generation = target.Generation.String()
		}
	}
	h.fixedScopeMu.Lock()
	h.fixedScope = scope
	h.fixedScopeBound = true
	h.fixedScopeMu.Unlock()
	h.fixedDiagnostics.SetScope(scope)
}

func (h *Handler) fixedDiagnosticScope(snapshot scheduler.Snapshot, target *provider.CompiledFixedTarget) (automode.DiagnosticScope, bool) {
	if h == nil || h.fixedDiagnostics == nil || snapshot == nil || target == nil {
		return automode.DiagnosticScope{}, false
	}
	scope := automode.DiagnosticScope{Revision: snapshot.Revision(), Generation: target.Generation.String(), Configured: true}
	h.fixedScopeMu.RLock()
	bound := h.fixedScopeBound
	h.fixedScopeMu.RUnlock()
	return scope, bound
}

type capturedFailure struct {
	lease        scheduler.AttemptLease
	outcome      scheduler.Outcome
	update       scheduler.HealthUpdate
	attempt      int
	headers      http.Header
	body         []byte
	transport    bool
	patchFailure bool
	patchID      string
	patchStage   string
}

func (h *Handler) streamResponse(w http.ResponseWriter, ctx context.Context, response *http.Response, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int) {
	// A response body is owned by this function from the moment it is passed in.
	// Close it exactly once on every path, including cancellation, while keeping
	// the health lease's terminal report exactly once.
	if response == nil || response.Body == nil {
		if requestCanceled(ctx) {
			h.reportClientCanceledWithAttempt(lease, outcome.SessionID, outcome.UpstreamURL, responseStatus(response), attempt, nil)
			return
		}
		outcome.Class = scheduler.FailureNeutral
		outcome.RawError = "provider returned no response body"
		if h != nil && h.selector != nil {
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		}
		return
	}
	closed := false
	closeBody := func() error {
		if closed {
			return nil
		}
		closed = true
		return response.Body.Close()
	}
	defer func() { _ = closeBody() }()
	report := func(kind EventKind, value scheduler.Outcome) {
		if h == nil || h.selector == nil {
			return
		}
		update := h.selector.Report(lease, value)
		h.recordOutcome(kind, lease, value, attempt, update)
	}
	cancel := func(cause error) {
		value := outcome
		value.Class = scheduler.FailureClientCanceled
		value.ClientCanceled = true
		value.RawError = contextError(ctx, cause).Error()
		value.ResponseStarted = outcome.ResponseStarted
		value.RawError = errors.Join(contextError(ctx, cause), closeBody()).Error()
		report(EventFailure, value)
	}
	if requestCanceled(ctx) {
		cancel(nil)
		return
	}
	copyResponseHeaders(w.Header(), response.Header)
	if requestCanceled(ctx) {
		cancel(nil)
		return
	}
	// The context check immediately before WriteHeader is the last point at
	// which a canceled client can be guaranteed not to receive a status line.
	w.WriteHeader(response.StatusCode)
	outcome.ResponseStarted = true
	if requestCanceled(ctx) {
		cancel(nil)
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	var raw strings.Builder
	captureError := outcome.Class != scheduler.FailureNone
	buffer := make([]byte, 32*1024)
	for {
		if requestCanceled(ctx) {
			cancel(nil)
			return
		}
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if captureError {
				_, _ = raw.Write(buffer[:n])
			}
			if requestCanceled(ctx) {
				cancel(nil)
				return
			}
			written, writeErr := w.Write(buffer[:n])
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				if requestCanceled(ctx) {
					cancel(writeErr)
					return
				}
				value := outcome
				value.Class = scheduler.FailureDownstream
				value.RawError = errors.Join(writeErr, closeBody()).Error()
				report(EventFailure, value)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				closeErr := closeBody()
				if requestCanceled(ctx) {
					cancel(closeErr)
					return
				}
				if closeErr != nil {
					value := outcome
					value.Class = scheduler.FailureNeutral
					value.RawError = closeErr.Error()
					report(EventFailure, value)
					return
				}
				if captureError {
					outcome.RawError = raw.String()
					report(EventFailure, outcome)
				} else {
					report(EventSuccess, outcome)
				}
				return
			}
			if requestCanceled(ctx) {
				cancel(readErr)
				return
			}
			value := outcome
			value.Class = scheduler.FailureChannelStream
			value.RawError = errors.Join(readErr, closeBody()).Error()
			report(EventFailure, value)
			// Headers and a prefix of the body may already be visible to the
			// caller. Returning leaves that partial stream intact and prevents a
			// retry that could duplicate side effects.
			return
		}
	}
}

func (h *Handler) writeCapturedFailure(w http.ResponseWriter, failure *capturedFailure) {
	if failure != nil && (failure.patchFailure || failure.patchID != "") {
		writeError(w, http.StatusBadGateway, "patch_failed", "provider request patch failed")
		return
	}
	if failure == nil || failure.transport {
		writeError(w, http.StatusBadGateway, "bad_gateway", "provider request failed")
		return
	}
	status := failure.outcome.HTTPStatus
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusMethodNotAllowed || status >= 300 && status < 400 {
		writeError(w, http.StatusBadGateway, "bad_gateway", "provider rejected the gateway request")
		return
	}
	copyResponseHeaders(w.Header(), failure.headers)
	w.WriteHeader(status)
	_, _ = w.Write(failure.body)
}

func (h *Handler) writeUnavailable(w http.ResponseWriter, err error) {
	if retry, ok := err.(interface{ RetryAtTime() (time.Time, bool) }); ok {
		if retryAt, exists := retry.RetryAtTime(); exists {
			seconds := int64((retryAt.Sub(h.now()) + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
	}
	writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "no provider is available")
}

func (h *Handler) record(event Event) {
	if h != nil && h.recorder != nil {
		h.recorder.RecordGatewayEvent(event)
	}
}

func (h *Handler) recordOutcome(kind EventKind, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int, update scheduler.HealthUpdate) {
	h.record(h.outcomeEvent(kind, lease, outcome, attempt, update))
}

func (h *Handler) recordOutcomeWithPatch(kind EventKind, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int, update scheduler.HealthUpdate, patchID string, stage patch.Stage) {
	event := h.outcomeEvent(kind, lease, outcome, attempt, update)
	event.PatchID = patchID
	event.PatchStage = string(stage)
	h.record(event)
}

func (h *Handler) outcomeEvent(kind EventKind, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int, update scheduler.HealthUpdate) Event {
	item := lease.Provider
	event := Event{
		Kind:                   kind,
		SessionID:              outcome.SessionID,
		Model:                  lease.Model,
		RequestType:            lease.RequestType,
		Attempt:                attempt,
		UpstreamURL:            outcome.UpstreamURL,
		HTTPStatus:             outcome.HTTPStatus,
		RawError:               outcome.RawError,
		GlobalHealth:           update.GlobalState,
		ChannelHealth:          update.ChannelState,
		GlobalEnteredCooldown:  update.GlobalEnteredCooldown,
		ChannelEnteredCooldown: update.ChannelEnteredCooldown,
		CooldownUntil:          update.CooldownUntil,
	}
	if item != nil {
		event.ProviderID = item.ID
		event.ProviderName = item.Name
	}
	return event
}

func (h *Handler) recordCapturedFailure(failure *capturedFailure) {
	if failure == nil {
		return
	}
	event := h.outcomeEvent(EventFailure, failure.lease, failure.outcome, failure.attempt, failure.update)
	event.PatchID = failure.patchID
	event.PatchStage = failure.patchStage
	h.record(event)
}

func (h *Handler) recordFailover(failure *capturedFailure, next scheduler.AttemptLease, nextAttempt int, incoming *http.Request) {
	if failure == nil || next.Provider == nil {
		return
	}
	event := h.outcomeEvent(EventFailover, failure.lease, failure.outcome, failure.attempt, failure.update)
	event.PatchID = failure.patchID
	event.PatchStage = failure.patchStage
	event.NextProviderID = next.Provider.ID
	event.NextProviderName = next.Provider.Name
	event.NextAttempt = nextAttempt
	if incoming != nil {
		if nextURL, err := upstreamURL(next.Provider, incoming.URL); err == nil {
			event.NextUpstreamURL = nextURL.String()
		}
	}
	h.record(event)
}

func singleSessionHeader(headers http.Header) string {
	values := headerValuesFold(headers, "X-Claude-Code-Session-Id")
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func requestCanceled(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func responseStatus(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

// reportClientCanceledWithAttempt records the terminal outcome for a leased
// normal or classifier pool attempt.  Cancellation is deliberately kept
// outside the Provider health failure classes, but the lease still must be
// reported so a half-open probe is released and the event stream has one
// matching failure.  The helper never writes a client response.
func (h *Handler) reportClientCanceledWithAttempt(lease scheduler.AttemptLease, sessionID, upstream string, status, attempt int, cause error) {
	h.reportClientCanceledState(lease, sessionID, upstream, status, attempt, false, cause)
}

func (h *Handler) reportClientCanceledState(lease scheduler.AttemptLease, sessionID, upstream string, status, attempt int, responseStarted bool, cause error) {
	if h == nil || h.selector == nil {
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	outcome := scheduler.Outcome{
		Class:           scheduler.FailureClientCanceled,
		HTTPStatus:      status,
		UpstreamURL:     upstream,
		RawError:        cause.Error(),
		SessionID:       sessionID,
		ResponseStarted: responseStarted,
		ClientCanceled:  true,
	}
	update := h.selector.Report(lease, outcome)
	h.recordOutcome(EventFailure, lease, outcome, attempt, update)
}

func contextError(ctx context.Context, fallback error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if fallback != nil {
		return fallback
	}
	return context.Canceled
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: code, Message: message})
}

func parseRetryAfter(headers http.Header, now time.Time) time.Duration {
	value, ok := retryAfterValue(headers)
	if !ok {
		return 0
	}
	if seconds, numeric := parseRetryAfterSeconds(value); numeric {
		return seconds
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return when.Sub(now)
}

func hasValidRetryAfter(headers http.Header) bool {
	value, ok := retryAfterValue(headers)
	if !ok {
		return false
	}
	if _, numeric := parseRetryAfterSeconds(value); numeric {
		return true
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func retryAfterValue(headers http.Header) (string, bool) {
	values := headerValuesFold(headers, "Retry-After")
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != ""
}

func parseRetryAfterSeconds(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	const maxSeconds = int64(^uint64(0)>>1) / int64(time.Second)
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds > maxSeconds {
		return time.Duration(1<<63 - 1), true
	}
	return time.Duration(seconds) * time.Second, true
}

func (f *capturedFailure) Error() string {
	if f == nil {
		return "provider attempt failed"
	}
	return fmt.Sprintf("provider attempt failed: %s", f.outcome.RawError)
}
