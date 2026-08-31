package automode

import (
	"net/http"
	"sync"
	"time"
)

// FixedTargetCall is the in-memory diagnostic record for the most recent
// fixed classifier target call. It intentionally keeps the complete upstream
// failure response while never persisting it or logging request/success body
// data.
type FixedTargetCall struct {
	ObservedAt      time.Time   `json:"observed_at"`
	UpstreamURL     string      `json:"upstream_url"`
	GatewayStatus   int         `json:"gateway_status"`
	GatewayError    string      `json:"gateway_error,omitempty"`
	UpstreamStatus  int         `json:"upstream_status,omitempty"`
	SessionID       string      `json:"session_id"`
	Error           string      `json:"error,omitempty"`
	UpstreamHeaders http.Header `json:"upstream_headers,omitempty"`
	UpstreamBody    string      `json:"upstream_body,omitempty"`
}

// DiagnosticScope identifies the published fixed-target runtime identity that
// produced an observation. It is intentionally kept out of the JSON payload:
// scope is an internal freshness guard, not a new management surface.
type DiagnosticScope struct {
	Revision   uint64
	Generation string
	Configured bool
}

// Diagnostics owns the latest fixed-target observation. The value is detached
// on both write and read so callers cannot mutate shared runtime state.
type Diagnostics struct {
	mu         sync.RWMutex
	last       *FixedTargetCall
	scope      DiagnosticScope
	scopeBound bool
}

func NewDiagnostics() *Diagnostics { return &Diagnostics{} }

// SetScope advances the published fixed-target identity. Any observation from
// an older revision/generation is no longer a current status value, so it is
// discarded when the scope changes or fixed mode is disabled.
func (d *Diagnostics) SetScope(scope DiagnosticScope) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.scopeBound || d.scope != scope {
		d.last = nil
	}
	d.scope = scope
	d.scopeBound = true
	d.mu.Unlock()
}

func (d *Diagnostics) Record(call FixedTargetCall) {
	if d == nil {
		return
	}
	call.ObservedAt = call.ObservedAt.UTC()
	call.UpstreamHeaders = cloneHeaders(call.UpstreamHeaders)
	d.mu.Lock()
	d.last = &call
	d.mu.Unlock()
}

// RecordScoped records a call only if it still belongs to the currently
// published scope. The call is silently ignored when a hot update has already
// advanced the runtime identity, preventing late requests from overwriting
// current diagnostics.
func (d *Diagnostics) RecordScoped(scope DiagnosticScope, call FixedTargetCall) {
	if d == nil {
		return
	}
	call.ObservedAt = call.ObservedAt.UTC()
	call.UpstreamHeaders = cloneHeaders(call.UpstreamHeaders)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.scopeBound || d.scope != scope || !scope.Configured {
		return
	}
	d.last = &call
}

func (d *Diagnostics) Snapshot() *FixedTargetCall {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.last == nil {
		return nil
	}
	copy := *d.last
	copy.UpstreamHeaders = cloneHeaders(d.last.UpstreamHeaders)
	return &copy
}

func cloneHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}
