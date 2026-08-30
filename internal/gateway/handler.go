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

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type SnapshotFunc func() scheduler.Snapshot

type Options struct {
	ClientPool       *ClientPool
	Recorder         EventRecorder
	ReplayDirectory  string
	Now              func() time.Time
	DetectorRegistry *traffic.Registry
	FlowDispatcher   *flow.Dispatcher
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
	active           atomic.Int64
	clientRevisionMu sync.Mutex
	clientRevision   uint64
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
	return &Handler{
		snapshot:        snapshot,
		selector:        selector,
		clients:         clients,
		recorder:        recorder,
		replayDirectory: options.ReplayDirectory,
		now:             now,
		detectors:       detectors,
		flows:           flows,
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
	if h.snapshot == nil || h.selector == nil {
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
	captured, err := bodyfile.Capture(r.Body, h.replayDirectory)
	if err != nil {
		if requestCanceled(r.Context()) {
			return
		}
		if bodyfile.IsReadError(err) {
			writeError(w, http.StatusBadRequest, "invalid_body", "could not read request body")
			return
		}
		writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		return
	}
	defer captured.Close()
	index, err := bodyfile.Index(captured)
	if err != nil {
		if errors.Is(err, bodyfile.ErrLocalIO) {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		} else if errors.Is(err, bodyfile.ErrModelMissing) || errors.Is(err, bodyfile.ErrModelRepeated) || errors.Is(err, bodyfile.ErrModelNotString) || errors.Is(err, bodyfile.ErrModelEmpty) {
			writeError(w, http.StatusBadRequest, "invalid_model", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		}
		return
	}
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
	plan, err := h.dispatchIngress(r.Context(), plannerSnapshot, ingress)
	if err != nil {
		if errors.Is(err, bodyfile.ErrLocalIO) {
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		} else if errors.Is(err, flow.ErrMissingPlanner) {
			writeError(w, http.StatusServiceUnavailable, "request_flow_unavailable", "request flow is unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, "request_prepare_failed", "request could not be prepared")
		}
		return
	}
	if plan.TargetMode == flow.TargetModeProviderPool && len(snapshot.Candidates(plan.PreparedRequest.Plan.EffectiveModel)) == 0 {
		writeError(w, http.StatusNotFound, "model_not_configured", "model is not configured")
		return
	}
	h.reconcileClients(snapshot)
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
	h.clients.Reconcile(snapshot.Providers())
	h.clientRevision = snapshot.Revision()
}

// ActiveRequests reports data-plane requests currently being processed.
func (h *Handler) ActiveRequests() int64 {
	if h == nil {
		return 0
	}
	return h.active.Load()
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
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	outcome.ResponseStarted = true
	var raw strings.Builder
	captureError := outcome.Class != scheduler.FailureNone
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if captureError {
				_, _ = raw.Write(buffer[:n])
			}
			written, writeErr := w.Write(buffer[:n])
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				outcome.Class = scheduler.FailureDownstream
				outcome.RawError = writeErr.Error()
				if requestCanceled(ctx) {
					outcome.Class = scheduler.FailureClientCanceled
					outcome.ClientCanceled = true
				}
				update := h.selector.Report(lease, outcome)
				h.recordOutcome(EventFailure, lease, outcome, attempt, update)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if captureError {
					outcome.RawError = raw.String()
					update := h.selector.Report(lease, outcome)
					h.recordOutcome(EventFailure, lease, outcome, attempt, update)
				} else {
					update := h.selector.Report(lease, outcome)
					h.recordOutcome(EventSuccess, lease, outcome, attempt, update)
				}
				return
			}
			if requestCanceled(ctx) {
				outcome.Class = scheduler.FailureClientCanceled
				outcome.ClientCanceled = true
				outcome.RawError = contextError(ctx, readErr).Error()
				update := h.selector.Report(lease, outcome)
				h.recordOutcome(EventFailure, lease, outcome, attempt, update)
				return
			}
			outcome.Class = scheduler.FailureChannelStream
			outcome.RawError = readErr.Error()
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
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
		Time:                   h.now().UTC(),
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
