package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/protocol"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type fixedDiagnosticContextKey struct{}

// fixedLifecycle carries request-local ownership and terminal-event state
// through the fixed execution helpers.  A fixed call has exactly one
// terminal outcome after EventForward; the lifecycle also lets that outcome
// include cleanup errors before a response is made visible.
type fixedLifecycle struct {
	mu sync.Mutex

	started         bool
	responseStarted bool
	gatewayStatus   int
	terminal        bool

	upstreamStatus  int
	upstreamHeaders http.Header
	upstreamBody    string
	upstreamBodySet bool
	upstreamSource  bodyfile.Body
	requestBody     io.ReadCloser
	requestBodyErr  error
	requestBodyOnce sync.Once

	cleanupOnce sync.Once
	cleanupErr  error
	bodyErr     error
	patchErr    error
	cleanup     func() error
}

type fixedLifecycleContextKey struct{}

func withFixedLifecycle(ctx context.Context, lifecycle *fixedLifecycle) context.Context {
	if ctx == nil || lifecycle == nil {
		return ctx
	}
	return context.WithValue(ctx, fixedLifecycleContextKey{}, lifecycle)
}

func fixedLifecycleFromContext(ctx context.Context) *fixedLifecycle {
	if ctx == nil {
		return nil
	}
	lifecycle, _ := ctx.Value(fixedLifecycleContextKey{}).(*fixedLifecycle)
	return lifecycle
}

func (l *fixedLifecycle) markStarted() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.started = true
	l.mu.Unlock()
}

func (l *fixedLifecycle) isStarted() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started
}

func (l *fixedLifecycle) markResponseStarted(status int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.responseStarted = true
	l.gatewayStatus = status
	l.mu.Unlock()
}

func (l *fixedLifecycle) hasResponseStarted() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.responseStarted
}

func (l *fixedLifecycle) writtenGatewayStatus() (int, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gatewayStatus, l.responseStarted
}

// beginTerminal returns false when another terminal outcome already won the
// race.  Normal fixed execution is single-goroutine, but the lock makes the
// invariant robust to custom response writers or test seams that callback.
func (l *fixedLifecycle) beginTerminal() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminal {
		return false
	}
	l.terminal = true
	return true
}

func (l *fixedLifecycle) hasTerminal() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.terminal
}

func (l *fixedLifecycle) observeResponse(status int, headers http.Header, source bodyfile.Body) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.upstreamStatus = status
	l.upstreamHeaders = cloneOrEmptyHeaders(headers)
	if source != nil {
		l.upstreamSource = source
	}
	l.mu.Unlock()
}

// setRequestBody transfers ownership of the outbound reader to the request
// lifecycle.  The reader may be closed by both net/http and the gateway; the
// fixedOnceReadCloser wrapper makes that hand-off idempotent, while keeping
// it here ensures early-return paths do not silently discard close errors.
func (l *fixedLifecycle) setRequestBody(reader io.ReadCloser) {
	if l == nil || reader == nil {
		return
	}
	l.mu.Lock()
	l.requestBody = reader
	l.mu.Unlock()
}

func (l *fixedLifecycle) closeRequestBody() error {
	if l == nil {
		return nil
	}
	l.requestBodyOnce.Do(func() {
		l.mu.Lock()
		reader := l.requestBody
		l.requestBody = nil
		l.mu.Unlock()
		if reader == nil {
			return
		}
		err := reader.Close()
		l.mu.Lock()
		l.requestBodyErr = err
		l.mu.Unlock()
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requestBodyErr
}

func (l *fixedLifecycle) observeBody(body string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.upstreamBody = body
	l.upstreamBodySet = true
	l.mu.Unlock()
}

func (l *fixedLifecycle) facts() (status int, headers http.Header, body string, bodySet bool, source bodyfile.Body) {
	if l == nil {
		return 0, nil, "", false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.upstreamStatus, cloneOrEmptyHeaders(l.upstreamHeaders), l.upstreamBody, l.upstreamBodySet, l.upstreamSource
}

func (l *fixedLifecycle) closeResources() error {
	if l == nil {
		return nil
	}
	l.cleanupOnce.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				l.cleanupErr = fmt.Errorf("fixed target cleanup panic: %v", recovered)
			}
		}()
		if l.cleanup != nil {
			l.cleanupErr = l.cleanup()
		}
	})
	return l.cleanupErr
}

func (l *fixedLifecycle) closeDetails() (all, bodyErr, patchErr error) {
	if l == nil {
		return nil, nil, nil
	}
	_ = l.closeResources()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleanupErr, l.bodyErr, l.patchErr
}

// fixedOnceReadCloser makes ownership transfer across http.Client.Do safe.
// The net/http RoundTripper contract permits a transport to close the request
// body asynchronously after RoundTrip returns, while the gateway also needs
// to observe a close error.  The wrapper ensures the underlying reader is
// closed exactly once and every caller sees the same error.
type fixedOnceReadCloser struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (r *fixedOnceReadCloser) Close() error {
	if r == nil || r.ReadCloser == nil {
		return nil
	}
	r.once.Do(func() { r.err = r.ReadCloser.Close() })
	return r.err
}

func closeFixedReader(reader io.ReadCloser) error {
	if reader == nil {
		return nil
	}
	return reader.Close()
}

type fixedResponseFacts struct {
	status   int
	headers  http.Header
	body     []byte
	readErr  error
	closeErr error
}

// consumeFixedResponse drains and closes a response exactly once from the
// gateway's point of view.  It is used only on terminal/error paths, where the
// raw upstream body is required for diagnostics.
func consumeFixedResponse(response *http.Response) fixedResponseFacts {
	if response == nil {
		return fixedResponseFacts{}
	}
	facts := fixedResponseFacts{status: response.StatusCode, headers: cloneOrEmptyHeaders(response.Header)}
	if response.Body == nil {
		return facts
	}
	facts.body, facts.readErr = io.ReadAll(response.Body)
	facts.closeErr = response.Body.Close()
	return facts
}

func fixedRequestCanceled(ctx context.Context, response *http.Response) bool {
	if requestCanceled(ctx) {
		return true
	}
	return response != nil && response.Request != nil && requestCanceled(response.Request.Context())
}

func fixedCancellationCause(ctx context.Context, response *http.Response, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		cause = errors.Join(cause, ctx.Err())
	}
	if response != nil && response.Request != nil && response.Request.Context() != nil && response.Request.Context().Err() != nil {
		cause = errors.Join(cause, response.Request.Context().Err())
	}
	return cause
}

func withFixedDiagnosticScope(ctx context.Context, scope automode.DiagnosticScope, scoped bool) context.Context {
	if ctx == nil || !scoped {
		return ctx
	}
	return context.WithValue(ctx, fixedDiagnosticContextKey{}, scope)
}

func fixedDiagnosticScopeFromContext(ctx context.Context) (automode.DiagnosticScope, bool) {
	if ctx == nil {
		return automode.DiagnosticScope{}, false
	}
	scope, ok := ctx.Value(fixedDiagnosticContextKey{}).(automode.DiagnosticScope)
	return scope, ok
}

// fixedUpstreamURL composes the fixed target endpoint from its base URL. The
// configured URL is always a base URL (it may contain a path prefix); Gateway
// appends the protocol-specific path. The client's raw query is preserved
// byte-for-byte.
func fixedUpstreamURL(target *provider.CompiledFixedTarget, incoming *url.URL) (*url.URL, error) {
	if target == nil || target.BaseURL == nil {
		return nil, errors.New("fixed target base URL is unavailable")
	}
	return targetUpstreamURL(target, incoming)
}

// forwardFixedExecution executes exactly one fixed classifier call. It never
// touches Scheduler or Health; target choice and policy are sealed in the plan.
func (h *Handler) forwardFixedExecution(w http.ResponseWriter, incoming *http.Request, snapshot scheduler.Snapshot, plan flow.ExecutionPlan) {
	prepared := plan.PreparedRequest
	model := prepared.Plan.EffectiveModel
	target := plan.FixedTarget
	sessionID := prepared.Plan.OriginalSessionID
	// Take ownership before any early return. Classifier planners may have
	// produced a BaseBody distinct from the captured ingress body, so even a
	// canceled or structurally invalid request must release it here.
	ownedBodies := make([]bodyfile.Body, 0, 5)
	own := func(body bodyfile.Body) {
		if body == nil {
			return
		}
		for _, prior := range ownedBodies {
			if sameBody(prior, body) {
				return
			}
		}
		ownedBodies = append(ownedBodies, body)
	}
	own(prepared.BaseBody)
	var execution *patch.Execution
	closeExecution := func() error {
		if execution == nil {
			return nil
		}
		current := execution
		execution = nil
		return current.Close()
	}
	lifecycle := &fixedLifecycle{}
	upstream := ""
	lifecycle.cleanup = func() error {
		patchErr := closeExecution()
		bodyErr := errors.Join(lifecycle.closeRequestBody(), closeFixedBodies(ownedBodies...))
		lifecycle.mu.Lock()
		lifecycle.bodyErr = bodyErr
		lifecycle.patchErr = patchErr
		lifecycle.mu.Unlock()
		return errors.Join(bodyErr, patchErr)
	}
	ctx := context.Background()
	if incoming != nil {
		ctx = incoming.Context()
	}
	// The caller supplies the immutable plan snapshot. Capture its target scope
	// once so a later hot update cannot make this request's diagnostics appear to
	// belong to a newer target.
	scope, scoped := h.fixedDiagnosticScope(snapshot, target)
	ctx = withFixedDiagnosticScope(ctx, scope, scoped)
	ctx = withFixedLifecycle(ctx, lifecycle)
	defer func() {
		cleanupErr := lifecycle.closeResources()
		if lifecycle.hasTerminal() {
			return
		}
		// A defensive fallback for an unexpected post-call return.  Do not emit a
		// pre-call cancellation diagnosis, but never leave an actual forwarded
		// call without a terminal failure event.
		if lifecycle.isStarted() {
			if requestCanceled(ctx) {
				h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, 0, nil, "", cleanupErr)
			} else {
				h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
					"bad_gateway", "fixed target execution failed", cleanupErr, 0, nil, "", "")
			}
		}
	}()
	// A request that is already canceled must not emit a response status.  The
	// caller (net/http) owns the closed connection in this case; there has been
	// no fixed-target call to diagnose yet.
	if requestCanceled(ctx) {
		return
	}
	if incoming == nil || incoming.URL == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, "", http.StatusInternalServerError,
			"request_prepare_failed", "fixed target request is unavailable", errors.New("incoming request is nil"), 0, nil, "", "")
		return
	}

	if target == nil || prepared.Plan.RequestType != traffic.RequestTypeClassifier {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, "", http.StatusInternalServerError,
			"request_prepare_failed", "fixed target is invalid", errors.New("fixed target is invalid"), 0, nil, "", "")
		return
	}
	endpoint, endpointErr := fixedUpstreamURL(target, requestURL(incoming))
	if endpoint != nil {
		upstream = endpoint.String()
	}
	if endpointErr != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target request could not be built", endpointErr, 0, nil, "", "")
		return
	}
	var adapter protocol.ProtocolAdapter
	if target.Protocol != config.ProtocolAnthropicMessages {
		if h.protocols == nil {
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusNotImplemented,
				"protocol_not_implemented", "fixed target protocol is not implemented", nil, 0, nil, "", "")
			return
		}
		var ok bool
		adapter, ok = h.protocols.Lookup(target.Protocol)
		if !ok || adapter == nil {
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusNotImplemented,
				"protocol_not_implemented", "fixed target protocol is not implemented", nil, 0, nil, "", "")
			return
		}
	}
	if requestCanceled(ctx) {
		return
	}

	headers, err := cleanUpstreamHeaders(incoming.Header)
	if err != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target request could not be built", err, 0, nil, "", "")
		return
	}
	patchContext := patch.PatchContext{
		RequestType:       prepared.Plan.RequestType,
		OriginalModel:     prepared.Plan.OriginalModel,
		EffectiveModel:    model,
		OriginalSessionID: prepared.Plan.OriginalSessionID,
		TargetID:          target.ID,
		Generation:        target.Generation.String(),
	}
	execution, err = target.PatchPlan.NewInstance(patchContext)
	if err != nil {
		patchID, stage := patchErrorDetails(err, patch.StageRequest)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target request patch failed", err, 0, nil, patchID+":"+string(stage), "")
		return
	}
	if requestCanceled(ctx) {
		return
	}

	mutable := patch.NewMutableRequest(prepared.BaseBody, prepared.BaseIndex, patch.NewHTTPHeaderSet(headers))
	requestPatchErr := execution.ApplyRequestOnly(mutable)
	// A hook may replace the body before returning an error. Register the
	// current value immediately so the request-scoped defer closes it too.
	own(mutable.Body)
	if requestPatchErr != nil {
		patchID, stage := patchErrorDetails(requestPatchErr, patch.StageRequest)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target request patch failed", requestPatchErr, 0, nil, patchID+":"+string(stage), "")
		return
	}
	if requestCanceled(ctx) {
		return
	}
	if mutable.Body == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target request patch failed", errors.New("patch returned nil body"), 0, nil, "", "")
		return
	}
	encodedHeaders, err := mutableHTTPHeaders(mutable.Headers)
	if err != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target request patch failed", err, 0, nil, "", "")
		return
	}
	if requestCanceled(ctx) {
		return
	}
	// A request patch is allowed to add ordinary protocol metadata, but it must
	// never smuggle a client/provider credential into either the Adapter boundary
	// or the direct Anthropic request.
	protocolHeaders := encodedHeaders.Clone()
	removeHopByHop(protocolHeaders)
	deleteCredentialHeaders(protocolHeaders)
	deleteHeaderFold(protocolHeaders, "Content-Length")
	deleteHeaderFold(protocolHeaders, "Host")
	var encoded protocol.ProtocolMessage
	var encodeErr error
	if adapter == nil {
		encoded = protocol.ProtocolMessage{Body: mutable.Body, Headers: protocolHeaders}
	} else {
		encoded, encodeErr = encodeFixedRequest(adapter, mutable.Body, protocolHeaders)
		// Ownership transfers only on success. On failure the Adapter remains
		// responsible for every temporary resource it created.
		if encodeErr != nil {
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
				"protocol_conversion_failed", "fixed target request conversion failed", encodeErr, 0, nil, "", "")
			return
		}
	}
	own(encoded.Body)
	if requestCanceled(ctx) {
		return
	}
	if encoded.Body == nil {
		if adapter == nil {
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
				"bad_gateway", "fixed target request body is unavailable", errors.New("fixed target request body is nil"), 0, nil, "", "")
			return
		}
		if encodeErr == nil {
			encodeErr = errors.New("protocol adapter returned a nil request body")
		}
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"protocol_conversion_failed", "fixed target request conversion failed", encodeErr, 0, nil, "", "")
		return
	}

	requestHeaders := cloneOrEmptyHeaders(encoded.Headers)
	removeHopByHop(requestHeaders)
	deleteCredentialHeaders(requestHeaders)
	deleteHeaderFold(requestHeaders, "Content-Length")
	deleteHeaderFold(requestHeaders, "Host")
	if err := target.ApplyAuthHeaders(requestHeaders); err != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target request could not be authenticated", err, 0, nil, "", "")
		return
	}
	if requestCanceled(ctx) {
		return
	}
	requestBody, err := encoded.Body.OpenReader()
	if err != nil {
		err = errors.Join(err, closeFixedReader(requestBody))
		code, status := fixedLocalBodyError(err)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code,
			"fixed target request could not be replayed", err, 0, nil, "", "")
		return
	}
	// Once handed to http.Client, the transport may close the request body
	// asynchronously.  Keep one idempotent close surface so the explicit
	// pre-Do cleanup and post-Do error observation cannot close it twice.
	requestBody = &fixedOnceReadCloser{ReadCloser: requestBody}
	lifecycle.setRequestBody(requestBody)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, requestBody)
	if err != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target request could not be built", err, 0, nil, "", "")
		return
	}
	request.Header = requestHeaders
	request.ContentLength = encoded.Body.Size()
	request.Host = ""
	// Cancellation during patching, conversion, authentication, or client
	// lookup is still pre-call: clean up and return without EventForward or a
	// client response status.
	if requestCanceled(ctx) {
		return
	}
	if h.clients == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusInternalServerError,
			"gateway_unavailable", "fixed target client is unavailable", errors.New("provider client pool is nil"), 0, nil, "", "")
		return
	}
	clientLease, err := h.clients.AcquireTarget(target)
	if err != nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusInternalServerError,
			"gateway_unavailable", "fixed target client is unavailable", err, 0, nil, "", "")
		return
	}
	defer clientLease.Release()
	// Record forwarding only once a client is available and immediately before
	// the actual upstream call; a local client-pool failure is not a call.
	if requestCanceled(ctx) {
		return
	}
	lifecycle.markStarted()
	h.recordFixedEvent(EventForward, target, sessionID, model, upstream, 1, 0, "")
	response, requestErr := clientLease.Client().Do(request)
	requestCloseErr := lifecycle.closeRequestBody()
	if requestErr != nil {
		facts := consumeFixedResponse(response)
		lifecycle.observeResponse(facts.status, facts.headers, nil)
		lifecycle.observeBody(string(facts.body))
		// requestCloseErr is retained by the lifecycle cleanup and is therefore
		// intentionally not joined here a second time.
		requestErr = errors.Join(requestErr, facts.readErr, facts.closeErr)
		if fixedRequestCanceled(ctx, response) {
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, facts.status, facts.headers, string(facts.body), requestErr)
			return
		}
		code, status := fixedTransportError(requestErr)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code,
			"fixed target request failed", requestErr, facts.status, facts.headers, "", string(facts.body))
		return
	}
	if requestCloseErr != nil {
		facts := consumeFixedResponse(response)
		lifecycle.observeResponse(facts.status, facts.headers, nil)
		lifecycle.observeBody(string(facts.body))
		// The lifecycle owns requestCloseErr; terminal cleanup appends it to the
		// diagnostic exactly once along with any response read/close errors.
		cause := errors.Join(facts.readErr, facts.closeErr)
		if fixedRequestCanceled(ctx, response) {
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, facts.status, facts.headers, string(facts.body), cause)
			return
		}
		code, status := fixedLocalBodyError(requestCloseErr)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code,
			"fixed target request could not be replayed", cause, facts.status, facts.headers, "", string(facts.body))
		return
	}
	if response == nil {
		if fixedRequestCanceled(ctx, response) {
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, 0, nil, "", errors.New("nil upstream response"))
			return
		}
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target returned no response", errors.New("nil upstream response"), 0, nil, "", "")
		return
	}
	lifecycle.observeResponse(response.StatusCode, response.Header, nil)
	if response.Body == nil {
		if fixedRequestCanceled(ctx, response) {
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, response.StatusCode, response.Header, "", errors.New("nil upstream response body"))
			return
		}
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target returned no response body", errors.New("nil upstream response body"), response.StatusCode, response.Header, "", "")
		return
	}
	// A cancellation observed after Do has started is a terminal fixed-call
	// failure.  Consume the response before returning so any status, headers,
	// and raw body already delivered by the target remain diagnosable.
	if fixedRequestCanceled(ctx, response) {
		facts := consumeFixedResponse(response)
		lifecycle.observeResponse(facts.status, facts.headers, nil)
		lifecycle.observeBody(string(facts.body))
		cause := errors.Join(facts.readErr, facts.closeErr, contextError(ctx, nil))
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, facts.status, facts.headers, string(facts.body), cause)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.handleFixedUpstreamFailure(ctx, w, response, target, sessionID, model, upstream)
		return
	}

	// Capture and close the raw response before applying response conversion or
	// response patches; capture errors are body/transport failures.
	rawBody, captureErr := bodyfile.Capture(response.Body, h.replayDirectory)
	responseCloseErr := response.Body.Close()
	own(rawBody)
	lifecycle.observeResponse(response.StatusCode, response.Header, rawBody)
	if captureErr != nil || responseCloseErr != nil {
		cause := errors.Join(captureErr, responseCloseErr)
		if requestCanceled(ctx) {
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, response.StatusCode, response.Header, "", cause)
			return
		}
		code, status := fixedResponseBodyError(cause)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code,
			"fixed target response could not be read", cause, response.StatusCode, response.Header, "", "")
		return
	}
	var decoded protocol.ProtocolMessage
	if adapter == nil {
		decoded = protocol.ProtocolMessage{Body: rawBody, Headers: response.Header.Clone()}
	} else {
		var decodeErr error
		decoded, decodeErr = decodeFixedResponse(adapter, rawBody, response.Header.Clone())
		if decodeErr != nil {
			rawText, rawErr := fixedBodyText(rawBody)
			decodeErr = errors.Join(decodeErr, rawErr)
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
				"protocol_conversion_failed", "fixed target response conversion failed", decodeErr, response.StatusCode, response.Header, "", rawText)
			return
		}
	}
	own(decoded.Body)
	if decoded.Body == nil {
		rawText, rawErr := fixedBodyText(rawBody)
		if adapter == nil {
			h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
				"bad_gateway", "fixed target response body is unavailable", errors.Join(errors.New("fixed target response body is nil"), rawErr), response.StatusCode, response.Header, "", rawText)
			return
		}
		decodeErr := errors.Join(errors.New("protocol adapter returned a nil response body"), rawErr)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"protocol_conversion_failed", "fixed target response conversion failed", decodeErr, response.StatusCode, response.Header, "", rawText)
		return
	}

	if target.PatchPlan.HasStage(patch.StageResponse, prepared.Plan.RequestType) {
		h.finishFixedPatchedResponse(w, ctx, target, sessionID, model, upstream,
			response.StatusCode, response.Header, decoded, rawBody, execution, closeExecution, own)
		return
	}
	if err := closeExecution(); err != nil {
		rawText, rawErr := fixedBodyText(rawBody)
		err = errors.Join(err, rawErr)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch failed", err, response.StatusCode, response.Header, "", rawText)
		return
	}
	responseHeaders := decoded.Headers
	if adapter != nil {
		responseHeaders = fixedRepresentationHeaders(responseHeaders, decoded.Body)
	}
	h.streamFixedBody(w, ctx, target, sessionID, model, upstream, response.StatusCode, response.StatusCode, decoded.Body, responseHeaders)
}

func requestURL(request *http.Request) *url.URL {
	if request == nil {
		return nil
	}
	return request.URL
}

// handleFixedUpstreamFailure reads the complete non-2xx response, records raw
// upstream facts, and either preserves or maps the client-facing status.
// DecodeResponse and response patches are deliberately not called here.
func (h *Handler) handleFixedUpstreamFailure(ctx context.Context, w http.ResponseWriter, response *http.Response, target *provider.CompiledFixedTarget, sessionID, model, upstream string) {
	if response == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target returned no response", errors.New("nil upstream response"), 0, nil, "", "")
		return
	}
	facts := consumeFixedResponse(response)
	data := facts.body
	if fixedRequestCanceled(ctx, response) {
		cause := fixedCancellationCause(ctx, response, errors.Join(facts.readErr, facts.closeErr))
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, facts.status, facts.headers, string(data), cause)
		return
	}
	if response.Body == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"bad_gateway", "fixed target returned no response body", errors.New("nil upstream response body"), facts.status, facts.headers, "", string(data))
		return
	}
	if facts.readErr != nil || facts.closeErr != nil {
		cause := errors.Join(facts.readErr, facts.closeErr)
		code, status := fixedResponseBodyError(cause)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code,
			"fixed target response failed", cause, facts.status, facts.headers, "", string(data))
		return
	}
	status := facts.status
	code := "upstream_error"
	message := "fixed target returned an error"
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusMethodNotAllowed || (status >= 300 && status < 400) {
		status = http.StatusBadGateway
		code = "bad_gateway"
		message = "fixed target rejected the gateway request"
	}
	h.fixedRawTerminal(ctx, w, model, target, sessionID, upstream, status, code, message,
		facts.status, facts.headers, string(data))
}

func (h *Handler) finishFixedPatchedResponse(w http.ResponseWriter, ctx context.Context, target *provider.CompiledFixedTarget, sessionID, model, upstream string, status int, upstreamHeaders http.Header, decoded protocol.ProtocolMessage, rawUpstream bodyfile.Body, execution *patch.Execution, closeExecution func() error, own func(bodyfile.Body)) {
	if decoded.Body == nil {
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"protocol_conversion_failed", "fixed target response conversion returned no body", errors.Join(errors.New("nil decoded response body"), rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	if requestCanceled(ctx) {
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, status, upstreamHeaders, "", contextError(ctx, nil))
		return
	}
	responseSpec, err := responseScanSpecForFixed(target)
	if err != nil {
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch could not be prepared", errors.Join(err, rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	reader, err := decoded.Body.OpenReader()
	if err != nil {
		_ = closeFixedReader(reader)
		code, localStatus := fixedLocalBodyError(err)
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, localStatus, code,
			"fixed target response could not be replayed", errors.Join(err, rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	body, index, captureErr := bodyfile.CaptureAndScan(reader, responseSpec, h.replayDirectory)
	readerCloseErr := closeFixedReader(reader)
	if own != nil {
		own(body)
	}
	if captureErr != nil || readerCloseErr != nil {
		cause := errors.Join(captureErr, readerCloseErr)
		code, localStatus := fixedResponseBodyError(cause)
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, localStatus, code,
			"fixed target response patch could not be read", errors.Join(cause, rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	if requestCanceled(ctx) {
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, status, upstreamHeaders, "", contextError(ctx, nil))
		return
	}
	mutable := patch.NewMutableResponse(status, body, index, patch.NewHTTPHeaderSet(cloneOrEmptyHeaders(decoded.Headers)))
	responsePatchErr := execution.ApplyResponseOnly(mutable)
	// As with request hooks, a response hook can replace Body before returning
	// an error; retain that value for deterministic cleanup.
	if own != nil {
		own(mutable.Body)
	}
	if responsePatchErr != nil {
		patchID, stage := patchErrorDetails(responsePatchErr, patch.StageResponse)
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch failed", errors.Join(responsePatchErr, rawErr), status, upstreamHeaders, patchID+":"+string(stage), rawText)
		return
	}
	if mutable.Body == nil {
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch failed", errors.Join(errors.New("patch returned nil response body"), rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	if own != nil {
		own(mutable.Body)
	}
	headers, err := mutableHTTPHeaders(mutable.Headers)
	if err != nil {
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch failed", errors.Join(err, rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	if err := closeExecution(); err != nil {
		rawText, rawErr := fixedBodyText(rawUpstream)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusBadGateway,
			"patch_failed", "fixed target response patch failed", errors.Join(err, rawErr), status, upstreamHeaders, "", rawText)
		return
	}
	if requestCanceled(ctx) {
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, status, upstreamHeaders, "", contextError(ctx, nil))
		return
	}
	headers = fixedRepresentationHeaders(headers, mutable.Body)
	h.streamFixedBody(w, ctx, target, sessionID, model, upstream, mutable.Status, status, mutable.Body, headers)
}

func fixedRepresentationHeaders(headers http.Header, body bodyfile.Body) http.Header {
	headers = cloneOrEmptyHeaders(headers)
	deleteHeaderFold(headers, "Content-Encoding")
	deleteHeaderFold(headers, "Content-Length")
	headers.Set("Content-Length", strconv.FormatInt(body.Size(), 10))
	return headers
}

func responseScanSpecForFixed(target *provider.CompiledFixedTarget) (bodyfile.ScanSpec, error) {
	if target == nil {
		return bodyfile.ScanSpec{}, errors.New("fixed target is nil")
	}
	paths, err := target.PatchPlan.RequiredPaths(patch.StageResponse, traffic.RequestTypeClassifier)
	if err != nil {
		return bodyfile.ScanSpec{}, err
	}
	return bodyfile.ResponseScanSpec(paths...)
}

func cloneOrEmptyHeaders(headers http.Header) http.Header {
	if headers == nil {
		return make(http.Header)
	}
	return headers.Clone()
}

func (h *Handler) streamFixedBody(w http.ResponseWriter, ctx context.Context, target *provider.CompiledFixedTarget, sessionID, model, upstream string, status, upstreamStatus int, body bodyfile.Body, headers http.Header) {
	h.streamFixedBodyWithFacts(w, ctx, target, sessionID, model, upstream, status, upstreamStatus, body, headers)
}

func (h *Handler) streamFixedBodyWithFacts(w http.ResponseWriter, ctx context.Context, target *provider.CompiledFixedTarget, sessionID, model, upstream string, status, upstreamStatus int, body bodyfile.Body, headers http.Header) {
	if requestCanceled(ctx) {
		cancelHeaders := headers
		if fixedLifecycleFromContext(ctx) != nil {
			cancelHeaders = nil
		}
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, cancelHeaders, "", contextError(ctx, nil))
		return
	}
	if body == nil {
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, http.StatusInternalServerError,
			"replay_unavailable", "fixed target response body is unavailable", nil, upstreamStatus, headers, "", "")
		return
	}
	reader, err := body.OpenReader()
	if err != nil {
		_ = closeFixedReader(reader)
		code, localStatus := fixedLocalBodyError(err)
		h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, localStatus, code,
			"fixed target response could not be replayed", err, upstreamStatus, headers, "", "")
		return
	}
	if requestCanceled(ctx) {
		_ = closeFixedReader(reader)
		cancelHeaders := headers
		if fixedLifecycleFromContext(ctx) != nil {
			cancelHeaders = nil
		}
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, cancelHeaders, "", contextError(ctx, nil))
		return
	}
	if lifecycle := fixedLifecycleFromContext(ctx); lifecycle != nil {
		lifecycle.markResponseStarted(status)
	}
	copyResponseHeaders(w.Header(), headers)
	w.WriteHeader(status)
	_, copyErr := io.Copy(w, reader)
	closeErr := closeFixedReader(reader)
	if copyErr != nil || closeErr != nil || requestCanceled(ctx) {
		cause := errors.Join(copyErr, closeErr)
		if requestCanceled(ctx) {
			cause = fixedCancellationCause(ctx, nil, cause)
			cancelHeaders := headers
			if fixedLifecycleFromContext(ctx) != nil {
				cancelHeaders = nil
			}
			h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, cancelHeaders, "", cause)
			return
		}
		cancelHeaders := headers
		if fixedLifecycleFromContext(ctx) != nil {
			cancelHeaders = nil
		}
		h.fixedTerminalAfterWriteWithFacts(ctx, target, sessionID, model, upstream, status, upstreamStatus, cancelHeaders, "", cause)
		return
	}
	h.fixedSuccess(ctx, target, sessionID, model, upstream, status, upstreamStatus)
}

func encodeFixedRequest(adapter protocol.ProtocolAdapter, body bodyfile.Body, headers http.Header) (message protocol.ProtocolMessage, err error) {
	if adapter == nil {
		return protocol.ProtocolMessage{}, errors.New("protocol adapter is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			message = protocol.ProtocolMessage{}
			err = fmt.Errorf("protocol adapter encode panic: %v", recovered)
		}
	}()
	return adapter.EncodeRequest(body, headers)
}

func decodeFixedResponse(adapter protocol.ProtocolAdapter, body bodyfile.Body, headers http.Header) (message protocol.ProtocolMessage, err error) {
	if adapter == nil {
		return protocol.ProtocolMessage{}, errors.New("protocol adapter is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			message = protocol.ProtocolMessage{}
			err = fmt.Errorf("protocol adapter decode panic: %v", recovered)
		}
	}()
	return adapter.DecodeResponse(body, headers)
}

func closeFixedBodies(bodies ...bodyfile.Body) error {
	var result error
	closed := make([]bodyfile.Body, 0, len(bodies))
	for _, body := range bodies {
		if body == nil {
			continue
		}
		duplicate := false
		for _, prior := range closed {
			if sameBody(prior, body) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		closed = append(closed, body)
		result = errors.Join(result, body.Close())
	}
	return result
}

func fixedLocalBodyError(err error) (string, int) {
	if errors.Is(err, bodyfile.ErrLocalIO) {
		return "replay_unavailable", http.StatusInternalServerError
	}
	return "bad_gateway", http.StatusBadGateway
}

func fixedResponseBodyError(err error) (string, int) {
	if errors.Is(err, bodyfile.ErrLocalIO) {
		return "replay_unavailable", http.StatusInternalServerError
	}
	return "bad_gateway", http.StatusBadGateway
}

func fixedCleanupBodyError(err error) (code string, status int, message string) {
	if errors.Is(err, bodyfile.ErrLocalIO) {
		return "replay_unavailable", http.StatusInternalServerError, "fixed target replay cleanup failed"
	}
	return "bad_gateway", http.StatusBadGateway, "fixed target response cleanup failed"
}

func fixedErrorText(text string) error {
	if text == "" {
		return nil
	}
	return errors.New(text)
}

// fixedLifecycleFacts merges the best upstream facts accumulated so far into
// a terminal diagnostic.  A successful response is deliberately not read into
// memory during normal operation; if the request later fails or is canceled,
// the already-captured source body may be read once for the required failure
// diagnostic.  The source is read before lifecycle cleanup closes it.
func fixedLifecycleFacts(lifecycle *fixedLifecycle, upstreamStatus int, upstreamHeaders http.Header, upstreamBody string, cause error) (int, http.Header, string, error) {
	if lifecycle == nil {
		return upstreamStatus, upstreamHeaders, upstreamBody, cause
	}
	observedStatus, observedHeaders, observedBody, bodySet, source := lifecycle.facts()
	if upstreamStatus == 0 {
		upstreamStatus = observedStatus
	}
	if upstreamHeaders == nil {
		upstreamHeaders = observedHeaders
	}
	if upstreamBody == "" {
		if bodySet {
			upstreamBody = observedBody
		} else if source != nil {
			text, readErr := fixedBodyText(source)
			upstreamBody = text
			cause = errors.Join(cause, readErr)
		}
	}
	return upstreamStatus, upstreamHeaders, upstreamBody, cause
}

func (h *Handler) fixedRawTerminal(ctx context.Context, w http.ResponseWriter, model string, target *provider.CompiledFixedTarget, sessionID, upstream string, status int, code, message string, upstreamStatus int, upstreamHeaders http.Header, upstreamBody string) {
	// Keep the raw body as the diagnostic error when present, while allowing an
	// empty upstream error body to remain an empty Error field.
	h.fixedTerminalForContext(ctx, w, model, target, sessionID, upstream, status, code, message,
		fixedErrorText(upstreamBody), upstreamStatus, upstreamHeaders, "", upstreamBody)
}

func fixedTransportError(error) (string, int) {
	return "bad_gateway", http.StatusBadGateway
}

// fixedTerminalForContext applies the client-cancellation boundary before a
// local error is exposed. Once a fixed call has started, cancellation is still
// recorded as a failure, but no response status is written to the closed
// client connection.
func (h *Handler) fixedTerminalForContext(ctx context.Context, w http.ResponseWriter, model string, target *provider.CompiledFixedTarget, sessionID, upstream string, status int, code, message string, cause error, upstreamStatus int, upstreamHeaders http.Header, patchInfo, upstreamBody string) {
	if requestCanceled(ctx) {
		lifecycle := fixedLifecycleFromContext(ctx)
		if lifecycle != nil && !lifecycle.isStarted() {
			return
		}
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, upstreamHeaders, upstreamBody, contextError(ctx, cause))
		return
	}
	h.fixedTerminalForModelWithRaw(ctx, w, model, target, sessionID, upstream, status, code, message, cause, upstreamStatus, upstreamHeaders, patchInfo, upstreamBody)
}

func (h *Handler) fixedTerminalForModelWithRaw(ctx context.Context, w http.ResponseWriter, model string, target *provider.CompiledFixedTarget, sessionID, upstream string, status int, code, message string, cause error, upstreamStatus int, upstreamHeaders http.Header, patchInfo, upstreamBody string) {
	lifecycle := fixedLifecycleFromContext(ctx)
	writtenStatus, responseStarted := 0, false
	if lifecycle != nil {
		if !lifecycle.beginTerminal() {
			return
		}
		writtenStatus, responseStarted = lifecycle.writtenGatewayStatus()
		upstreamStatus, upstreamHeaders, upstreamBody, cause = fixedLifecycleFacts(lifecycle, upstreamStatus, upstreamHeaders, upstreamBody, cause)
		cleanupErr, bodyErr, patchErr := lifecycle.closeDetails()
		if cleanupErr != nil {
			cause = errors.Join(cause, cleanupErr)
			if bodyErr != nil {
				cleanupCode, cleanupStatus, cleanupMessage := fixedCleanupBodyError(bodyErr)
				code, message = cleanupCode, cleanupMessage
				if !responseStarted {
					status = cleanupStatus
				}
			} else if patchErr != nil {
				code, message = "patch_failed", "fixed target response patch failed"
				if !responseStarted {
					status = http.StatusBadGateway
				}
			}
		}
	}
	if responseStarted {
		status = writtenStatus
	}
	if requestCanceled(ctx) && lifecycle != nil && lifecycle.isStarted() {
		if !responseStarted {
			status = 0
		}
		code = "client_canceled"
		message = ""
	}
	errText := ""
	if cause != nil {
		errText = cause.Error()
	}
	if patchInfo != "" {
		if errText == "" {
			errText = patchInfo
		} else {
			errText = strings.TrimSpace(patchInfo) + ": " + errText
		}
	}
	call := automode.FixedTargetCall{
		UpstreamURL:     upstream,
		GatewayStatus:   status,
		GatewayError:    code,
		UpstreamStatus:  upstreamStatus,
		UpstreamHeaders: cloneOrEmptyHeaders(upstreamHeaders),
		UpstreamBody:    upstreamBody,
		SessionID:       sessionID,
		Error:           errText,
	}
	h.recordFixedCall(ctx, target, call)
	h.recordFixedEvent(EventFailure, target, sessionID, model, upstream, 1, upstreamStatus, errText)
	if status > 0 && w != nil && (lifecycle == nil || !lifecycle.hasResponseStarted()) && !requestCanceled(ctx) {
		if code == "upstream_error" && upstreamStatus == status {
			copyResponseHeaders(w.Header(), upstreamHeaders)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(upstreamBody))
		} else {
			writeError(w, status, code, message)
		}
	}
}

// fixedBodyText reads a request-scoped body only for a failure diagnostic. It
// is deliberately absent from successful fixed responses so normal traffic is
// never duplicated into an in-memory string.
func fixedBodyText(body bodyfile.Body) (string, error) {
	if body == nil {
		return "", nil
	}
	reader, err := body.OpenReader()
	if err != nil {
		_ = closeFixedReader(reader)
		return "", err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	return string(data), errors.Join(readErr, closeErr)
}

func (h *Handler) fixedTerminalAfterWriteWithFacts(ctx context.Context, target *provider.CompiledFixedTarget, sessionID, model, upstream string, status, upstreamStatus int, upstreamHeaders http.Header, upstreamBody string, cause error) {
	lifecycle := fixedLifecycleFromContext(ctx)
	if lifecycle != nil && !lifecycle.beginTerminal() {
		return
	}
	cleanupErr := error(nil)
	upstreamStatus, upstreamHeaders, upstreamBody, cause = fixedLifecycleFacts(lifecycle, upstreamStatus, upstreamHeaders, upstreamBody, cause)
	if lifecycle != nil {
		cleanupErr = lifecycle.closeResources()
	}
	if cleanupErr != nil {
		cause = errors.Join(cause, cleanupErr)
	}
	errText := ""
	if cause != nil {
		errText = cause.Error()
	}
	code := "downstream_error"
	gatewayStatus := status
	if writtenStatus, started := lifecycle.writtenGatewayStatus(); started {
		gatewayStatus = writtenStatus
	}
	if requestCanceled(ctx) {
		code = "client_canceled"
	} else if cleanupErr != nil {
		code, _, _ = fixedCleanupBodyError(cleanupErr)
	}
	call := automode.FixedTargetCall{UpstreamURL: upstream, GatewayStatus: gatewayStatus, GatewayError: code, UpstreamStatus: upstreamStatus, UpstreamHeaders: cloneOrEmptyHeaders(upstreamHeaders), UpstreamBody: upstreamBody, SessionID: sessionID, Error: errText}
	h.recordFixedCall(ctx, target, call)
	h.recordFixedEvent(EventFailure, target, sessionID, model, upstream, 1, upstreamStatus, errText)
}

func (h *Handler) fixedSuccess(ctx context.Context, target *provider.CompiledFixedTarget, sessionID, model, upstream string, status, upstreamStatus int) {
	lifecycle := fixedLifecycleFromContext(ctx)
	// A cancellation racing the final copy must win over a success event.  The
	// stream path checks the context after io.Copy as well, but this second
	// boundary closes the small gap between that check and terminal recording.
	if requestCanceled(ctx) {
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, nil, "", contextError(ctx, nil))
		return
	}
	cleanupErr := error(nil)
	if lifecycle != nil {
		cleanupErr = lifecycle.closeResources()
	}
	if requestCanceled(ctx) {
		// closeResources is idempotent; fixedCanceledWithFacts will retain any
		// cleanup error and record the sole terminal failure without writing a
		// second response status.
		h.fixedCanceledWithFacts(ctx, target, model, sessionID, upstream, upstreamStatus, nil, "", errors.Join(contextError(ctx, nil), cleanupErr))
		return
	}
	if lifecycle != nil && !lifecycle.beginTerminal() {
		return
	}
	if cleanupErr != nil {
		code, _, _ := fixedCleanupBodyError(cleanupErr)
		gatewayStatus := status
		if writtenStatus, started := lifecycle.writtenGatewayStatus(); started {
			gatewayStatus = writtenStatus
		}
		call := automode.FixedTargetCall{UpstreamURL: upstream, GatewayStatus: gatewayStatus, GatewayError: code, UpstreamStatus: upstreamStatus, SessionID: sessionID, Error: cleanupErr.Error()}
		h.recordFixedCall(ctx, target, call)
		h.recordFixedEvent(EventFailure, target, sessionID, model, upstream, 1, upstreamStatus, cleanupErr.Error())
		return
	}
	h.recordFixedCall(ctx, target, automode.FixedTargetCall{UpstreamURL: upstream, GatewayStatus: status, UpstreamStatus: upstreamStatus, SessionID: sessionID})
	h.recordFixedEvent(EventSuccess, target, sessionID, model, upstream, 1, upstreamStatus, "")
}

func (h *Handler) fixedCanceledWithFacts(ctx context.Context, target *provider.CompiledFixedTarget, model, sessionID, upstream string, upstreamStatus int, upstreamHeaders http.Header, upstreamBody string, cause error) {
	lifecycle := fixedLifecycleFromContext(ctx)
	if lifecycle != nil {
		if !lifecycle.isStarted() || !lifecycle.beginTerminal() {
			return
		}
		upstreamStatus, upstreamHeaders, upstreamBody, cause = fixedLifecycleFacts(lifecycle, upstreamStatus, upstreamHeaders, upstreamBody, cause)
		if cleanupErr := lifecycle.closeResources(); cleanupErr != nil {
			cause = errors.Join(cause, cleanupErr)
		}
	}
	errText := ""
	if cause != nil {
		errText = cause.Error()
	}
	gatewayStatus := 0
	if lifecycle != nil {
		if writtenStatus, started := lifecycle.writtenGatewayStatus(); started {
			gatewayStatus = writtenStatus
		}
	}
	h.recordFixedCall(ctx, target, automode.FixedTargetCall{UpstreamURL: upstream, GatewayStatus: gatewayStatus, GatewayError: "client_canceled", UpstreamStatus: upstreamStatus, UpstreamHeaders: cloneOrEmptyHeaders(upstreamHeaders), UpstreamBody: upstreamBody, SessionID: sessionID, Error: errText})
	h.recordFixedEvent(EventFailure, target, sessionID, model, upstream, 1, upstreamStatus, errText)
}

func (h *Handler) recordFixedCall(ctx context.Context, target *provider.CompiledFixedTarget, call automode.FixedTargetCall) {
	if h == nil || h.fixedDiagnostics == nil {
		return
	}
	if call.ObservedAt.IsZero() {
		call.ObservedAt = h.now().UTC()
	}
	if scope, ok := fixedDiagnosticScopeFromContext(ctx); ok {
		h.fixedDiagnostics.RecordScoped(scope, call)
		return
	}
	// Direct package-local callers without a runtime snapshot retain the simple
	// unscoped behavior used by focused tests.
	_ = target
	h.fixedDiagnostics.Record(call)
}

func (h *Handler) recordFixedEvent(kind EventKind, target *provider.CompiledFixedTarget, sessionID, model, upstream string, attempt, status int, raw string) {
	event := Event{Kind: kind, Time: h.now().UTC(), SessionID: sessionID, Model: model, RequestType: traffic.RequestTypeClassifier, Attempt: attempt, UpstreamURL: upstream, HTTPStatus: status, RawError: raw}
	if target != nil {
		event.ProviderID = target.ID
	}
	h.record(event)
}
