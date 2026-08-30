// Package patch contains the immutable provider-patch registry and the
// request/response hook contracts used by the gateway.  A patch deliberately
// has no access to provider credentials, URL, transport, scheduler state, or
// health state; those concerns remain outside this package.
package patch

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// RequestType is kept as an alias so callers can use either patch.RequestType
// or traffic.RequestType without conversions.
type RequestType = traffic.RequestType

const (
	RequestTypeNormal     RequestType = "normal"
	RequestTypeClassifier RequestType = "classifier"
	RequestTypeAny        RequestType = "*"
)

// Stage identifies the side of an upstream call on which a hook runs.
type Stage string

const (
	StageRequest  Stage = "request"
	StageResponse Stage = "response"
)

// Idempotence describes whether applying a hook to an already transformed
// value is safe.  The execution object still guarantees at-most-once hook
// invocation for per_execution patches.
type Idempotence string

const (
	Idempotent   Idempotence = "idempotent"
	PerExecution Idempotence = "per_execution"
)

// PatchMetadata is the discovery/configuration representation of a patch.
// Slices returned by the registry are defensive copies.
type PatchMetadata struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	RequestTypes []RequestType `json:"request_types"`
	Stages       []Stage       `json:"stages"`
	Conflicts    []string      `json:"conflicts"`
	Idempotence  Idempotence   `json:"idempotence"`
}

// Metadata is the shorter name used by a few integration callers.
type Metadata = PatchMetadata

// PatchContext contains only request facts that are safe for a patch to see.
// Generation is intentionally an opaque string: importing provider here would
// introduce a dependency from the patch layer back into provider compilation.
type PatchContext struct {
	RequestType       RequestType
	OriginalModel     string
	EffectiveModel    string
	OriginalSessionID string
	TargetID          string
	Generation        string
}

// Validate rejects malformed context values before invoking a hook.  Empty
// model/session values are allowed because a particular flow may not expose
// them; the fields that identify a target/request type are checked when the
// execution is created.
func (c PatchContext) Validate() error {
	if !c.RequestType.Valid() || c.RequestType == RequestTypeAny {
		return fmt.Errorf("invalid patch request type %q", c.RequestType)
	}
	if strings.TrimSpace(c.TargetID) == "" {
		return fmt.Errorf("patch target id must not be empty")
	}
	if strings.TrimSpace(c.Generation) == "" {
		return fmt.Errorf("patch target generation must not be empty")
	}
	return nil
}

// RequestPatch mutates an outbound request body/header set.
type RequestPatch interface {
	ApplyRequest(PatchContext, *MutableRequest) error
}

// ResponsePatch mutates the actual terminal upstream response.  A response
// hook is never called for a response that the gateway discards while failing
// over.
type ResponsePatch interface {
	ApplyResponse(PatchContext, *MutableResponse) error
}

// MutableRequest is the only mutable request surface exposed to hooks.
type MutableRequest struct {
	Body    bodyfile.Body
	Headers MutableHeaderSet

	index bodyfile.JSONIndex
}

// NewMutableRequest binds the PreparedRequest index to the immutable BaseBody
// for the first request hook. If a hook replaces Body, later hooks rebuild an
// attempt-level index only when they actually need one.
func NewMutableRequest(body bodyfile.Body, index bodyfile.JSONIndex, headers MutableHeaderSet) *MutableRequest {
	return &MutableRequest{Body: body, Headers: headers, index: index}
}

// MutableResponse is the only mutable response surface exposed to hooks.
type MutableResponse struct {
	Status  int
	Body    bodyfile.Body
	Headers MutableHeaderSet
}

// HeaderView and MutableHeaderSet intentionally mirror the traffic contracts,
// but are declared here as structural interfaces to keep patch independent of
// concrete HTTP types and to make tests easy to inject.
type HeaderView interface {
	Get(string) (string, bool)
	Values(string) []string
}

type MutableHeaderSet interface {
	HeaderView
	Set(string, string)
	Delete(string)
}

// HTTPHeaderSet adapts net/http headers to MutableHeaderSet.  It performs
// case-insensitive lookups/deletes while preserving all values and caller
// ordering.  It is useful at the gateway seam and in focused tests.
type HTTPHeaderSet struct{ Header http.Header }

func NewHTTPHeaderSet(h http.Header) *HTTPHeaderSet {
	if h == nil {
		h = make(http.Header)
	}
	return &HTTPHeaderSet{Header: h}
}

func (h *HTTPHeaderSet) Get(name string) (string, bool) {
	if h == nil || h.Header == nil {
		return "", false
	}
	for key, values := range h.Header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0], true
		}
	}
	return "", false
}

func (h *HTTPHeaderSet) Values(name string) []string {
	if h == nil || h.Header == nil {
		return nil
	}
	var out []string
	for key, values := range h.Header {
		if strings.EqualFold(key, name) {
			out = append(out, values...)
		}
	}
	return append([]string(nil), out...)
}

func (h *HTTPHeaderSet) Set(name, value string) {
	if h == nil {
		return
	}
	if h.Header == nil {
		h.Header = make(http.Header)
	}
	h.Delete(name)
	h.Header[name] = []string{value}
}

func (h *HTTPHeaderSet) Delete(name string) {
	if h == nil || h.Header == nil {
		return
	}
	for key := range h.Header {
		if strings.EqualFold(key, name) {
			delete(h.Header, key)
		}
	}
}

// Hooks is a convenient factory result.  Either side may be nil, but the
// definition metadata must not claim a stage for which the factory cannot
// supply a hook (the registry checks this when it can inspect the result).
type Hooks struct {
	Request  RequestPatch
	Response ResponsePatch
}

// PatchInstance is the result of a Definition factory.
type PatchInstance interface {
	RequestPatch() RequestPatch
	ResponsePatch() ResponsePatch
}

type hooksInstance struct{ Hooks }

func (i hooksInstance) RequestPatch() RequestPatch   { return i.Request }
func (i hooksInstance) ResponsePatch() ResponsePatch { return i.Response }

func newHooksInstance(h Hooks) PatchInstance { return hooksInstance{Hooks: h} }

// NewHooksInstance adapts simple hooks to a fresh patch instance for custom
// registries and focused tests.
func NewHooksInstance(h Hooks) PatchInstance { return newHooksInstance(h) }

func validRequestType(t RequestType) bool {
	// The registry currently has two concrete flows plus the wildcard metadata
	// value. Future request types must be added with their traffic/flow contracts
	// before a patch definition can reference them; silently accepting an
	// arbitrary string would create a discoverable-but-unexecutable patch.
	switch t {
	case RequestTypeNormal, RequestTypeClassifier, RequestTypeAny:
		return true
	default:
		return false
	}
}

func validStage(s Stage) bool { return s == StageRequest || s == StageResponse }
