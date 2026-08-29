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

	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

type SnapshotFunc func() scheduler.Snapshot

type Options struct {
	ClientPool      *ClientPool
	Recorder        EventRecorder
	ReplayDirectory string
	Now             func() time.Time
}

type Handler struct {
	snapshot         SnapshotFunc
	selector         scheduler.Selector
	clients          *ClientPool
	recorder         EventRecorder
	replayDirectory  string
	now              func() time.Time
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
	return &Handler{
		snapshot:        snapshot,
		selector:        selector,
		clients:         clients,
		recorder:        recorder,
		replayDirectory: options.ReplayDirectory,
		now:             now,
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
	snapshot := h.snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	capturedSnapshot, attemptPolicy, err := scheduler.CaptureAttemptPolicy(snapshot)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", "gateway is unavailable")
		return
	}
	snapshot = capturedSnapshot
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
	replay, err := spoolRequestBody(r.Body, h.replayDirectory)
	if err != nil {
		if requestCanceled(r.Context()) {
			return
		}
		if isRequestBodyReadError(err) {
			writeError(w, http.StatusBadRequest, "invalid_body", "could not read request body")
			return
		}
		writeError(w, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		return
	}
	defer replay.Close()

	reader, err := replay.Reader()
	if err != nil {
		h.writeAfterReplayClose(w, replay, http.StatusInternalServerError, "replay_unavailable", "request could not be prepared")
		return
	}
	fields, parseErr := parseMessagesRequest(reader)
	_ = reader.Close()
	if parseErr != nil {
		var requestErr *requestParseError
		if errors.As(parseErr, &requestErr) {
			h.writeAfterReplayClose(w, replay, http.StatusBadRequest, requestErr.code, requestErr.message)
			return
		}
		h.writeAfterReplayClose(w, replay, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}

	sessionID := singleSessionHeader(r.Header)
	if len(snapshot.Candidates(fields.model)) == 0 {
		h.writeAfterReplayClose(w, replay, http.StatusNotFound, "model_not_configured", "model is not configured")
		return
	}
	h.reconcileClients(snapshot)
	h.forward(w, r, snapshot, replay, attemptPolicy, scheduler.StickyKey{
		SessionID:    sessionID,
		Model:        fields.model,
		TrafficClass: scheduler.TrafficClassNormal,
	})
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
	lease     scheduler.AttemptLease
	outcome   scheduler.Outcome
	update    scheduler.HealthUpdate
	attempt   int
	headers   http.Header
	body      []byte
	transport bool
}

func (h *Handler) forward(w http.ResponseWriter, incoming *http.Request, snapshot scheduler.Snapshot, replay *replayBody, attemptPolicy scheduler.AttemptPolicy, sticky scheduler.StickyKey) {
	excluded := make(map[string]struct{})
	var last *capturedFailure
	for attempt := 1; attempt <= attemptPolicy.MaxAttempts; attempt++ {
		lease, err := h.selector.Acquire(snapshot, sticky, excluded)
		if err != nil {
			if last != nil {
				h.recordCapturedFailure(last)
			}
			if closeErr := replay.Close(); closeErr != nil {
				writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
				return
			}
			if last != nil {
				h.writeCapturedFailure(w, last)
				return
			}
			h.writeUnavailable(w, err)
			return
		}
		if lease.Provider == nil {
			if last != nil {
				h.recordCapturedFailure(last)
			}
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "selected provider is unavailable")
			return
		}
		if _, duplicate := excluded[lease.Provider.ID]; duplicate {
			if last != nil {
				h.recordCapturedFailure(last)
			}
			h.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sticky.SessionID})
			h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider selection repeated an attempt")
			return
		}
		if last != nil {
			h.recordFailover(last, lease, attempt, incoming)
			last = nil
		}
		failure, done := h.attempt(w, incoming, replay, lease, sticky.SessionID, attempt)
		if done {
			return
		}
		last = failure
		excluded[lease.Provider.ID] = struct{}{}
	}
	if last != nil {
		h.recordCapturedFailure(last)
	}
	if closeErr := replay.Close(); closeErr != nil {
		writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
		return
	}
	if last != nil {
		h.writeCapturedFailure(w, last)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "no provider is available")
}

func (h *Handler) attempt(w http.ResponseWriter, incoming *http.Request, replay *replayBody, lease scheduler.AttemptLease, sessionID string, attempt int) (*capturedFailure, bool) {
	item := lease.Provider
	if err := item.ValidateApplication(); err != nil {
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider cannot process this request")
		return nil, true
	}
	url, err := upstreamURL(item, incoming.URL)
	if err != nil {
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider request could not be built")
		return nil, true
	}
	headers, err := prepareUpstreamHeaders(incoming.Header, item)
	if err != nil {
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: url.String(), RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider request could not be authenticated")
		return nil, true
	}
	body, err := replay.Reader()
	if err != nil {
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: url.String(), RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusInternalServerError, "replay_unavailable", "request could not be replayed")
		return nil, true
	}
	request, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, url.String(), body)
	if err != nil {
		_ = body.Close()
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: url.String(), RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider request could not be built")
		return nil, true
	}
	request.Header = headers
	request.ContentLength = replay.Size()
	request.Host = ""
	request.GetBody = nil

	h.record(Event{
		Kind:         EventForward,
		Time:         h.now().UTC(),
		ProviderID:   item.ID,
		ProviderName: item.Name,
		SessionID:    sessionID,
		Model:        lease.Model,
		TrafficClass: lease.TrafficClass,
		Attempt:      attempt,
		UpstreamURL:  url.String(),
	})
	client, err := h.clients.Client(item)
	if err != nil {
		closeRequestBody(request)
		outcome := scheduler.Outcome{Class: scheduler.FailureNeutral, SessionID: sessionID, UpstreamURL: url.String(), RawError: err.Error()}
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusInternalServerError, "gateway_unavailable", "provider client is unavailable")
		return nil, true
	}
	response, requestErr := client.Do(request)
	closeRequestBody(request)
	if requestErr != nil {
		return h.handleTransportError(w, incoming.Context(), replay, lease, sessionID, attempt, url.String(), requestErr)
	}
	class := scheduler.ClassifyHTTPStatus(response.StatusCode)
	outcome := scheduler.Outcome{
		Class:         class,
		HTTPStatus:    response.StatusCode,
		UpstreamURL:   url.String(),
		SessionID:     sessionID,
		RetryAfter:    parseRetryAfter(response.Header, h.now()),
		HasRetryAfter: hasValidRetryAfter(response.Header),
	}
	if !outcome.ShouldFailover() {
		if closeErr := replay.Close(); closeErr != nil {
			_ = response.Body.Close()
			outcome.Class = scheduler.FailureNeutral
			outcome.RawError = closeErr.Error()
			update := h.selector.Report(lease, outcome)
			h.recordOutcome(EventFailure, lease, outcome, attempt, update)
			writeError(w, http.StatusInternalServerError, "replay_unavailable", "request replay cleanup failed")
			return nil, true
		}
		h.streamResponse(w, incoming.Context(), response, lease, outcome, attempt)
		return nil, true
	}
	bodyBytes, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if requestCanceled(incoming.Context()) {
		outcome.Class = scheduler.FailureClientCanceled
		outcome.ClientCanceled = true
		outcome.RawError = contextError(incoming.Context(), readErr).Error()
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		_ = replay.Close()
		return nil, true
	}
	if readErr != nil || closeErr != nil {
		if readErr == nil {
			readErr = closeErr
		}
		outcome.Class = scheduler.FailureChannelStream
		outcome.RawError = readErr.Error()
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusBadGateway, "bad_gateway", "provider response ended unexpectedly")
		return nil, true
	}
	outcome.RawError = string(bodyBytes)
	update := h.selector.Report(lease, outcome)
	return &capturedFailure{
		lease:   lease,
		outcome: outcome,
		update:  update,
		attempt: attempt,
		headers: response.Header.Clone(),
		body:    bodyBytes,
	}, false
}

func (h *Handler) handleTransportError(w http.ResponseWriter, ctx context.Context, replay *replayBody, lease scheduler.AttemptLease, sessionID string, attempt int, upstream string, err error) (*capturedFailure, bool) {
	outcome := scheduler.Outcome{
		Class:       scheduler.FailureGlobalTransient,
		UpstreamURL: upstream,
		RawError:    err.Error(),
		SessionID:   sessionID,
	}
	if requestCanceled(ctx) {
		outcome.Class = scheduler.FailureClientCanceled
		outcome.ClientCanceled = true
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		_ = replay.Close()
		return nil, true
	}
	if requestBodyReadFailed(err) {
		outcome.Class = scheduler.FailureNeutral
		update := h.selector.Report(lease, outcome)
		h.recordOutcome(EventFailure, lease, outcome, attempt, update)
		h.writeAfterReplayClose(w, replay, http.StatusInternalServerError, "replay_unavailable", "request could not be replayed")
		return nil, true
	}
	update := h.selector.Report(lease, outcome)
	return &capturedFailure{lease: lease, outcome: outcome, update: update, attempt: attempt, transport: true}, false
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

func (h *Handler) writeAfterReplayClose(w http.ResponseWriter, replay *replayBody, status int, code, message string) {
	if replay != nil {
		if err := replay.Close(); err != nil {
			status = http.StatusInternalServerError
			code = "replay_unavailable"
			message = "request replay cleanup failed"
		}
	}
	writeError(w, status, code, message)
}

func (h *Handler) record(event Event) {
	if h != nil && h.recorder != nil {
		h.recorder.RecordGatewayEvent(event)
	}
}

func (h *Handler) recordOutcome(kind EventKind, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int, update scheduler.HealthUpdate) {
	h.record(h.outcomeEvent(kind, lease, outcome, attempt, update))
}

func (h *Handler) outcomeEvent(kind EventKind, lease scheduler.AttemptLease, outcome scheduler.Outcome, attempt int, update scheduler.HealthUpdate) Event {
	item := lease.Provider
	event := Event{
		Kind:                   kind,
		Time:                   h.now().UTC(),
		SessionID:              outcome.SessionID,
		Model:                  lease.Model,
		TrafficClass:           lease.TrafficClass,
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
	h.recordOutcome(EventFailure, failure.lease, failure.outcome, failure.attempt, failure.update)
}

func (h *Handler) recordFailover(failure *capturedFailure, next scheduler.AttemptLease, nextAttempt int, incoming *http.Request) {
	if failure == nil || next.Provider == nil {
		return
	}
	event := h.outcomeEvent(EventFailover, failure.lease, failure.outcome, failure.attempt, failure.update)
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
