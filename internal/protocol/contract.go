// Package protocol defines the narrow conversion boundary used by fixed
// classifier targets.  Adapters only translate Anthropic-shaped bodies and
// headers; routing, authentication, HTTP status handling, retries and health
// remain owned by the gateway.
package protocol

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

var (
	ErrNilAdapter       = errors.New("protocol: nil adapter")
	ErrDuplicateAdapter = errors.New("protocol: duplicate adapter")
	ErrInvalidAdapter   = errors.New("protocol: invalid adapter")
)

// ProtocolMessage is the result of a successful request or response
// conversion.  The Body is immutable and its ownership is transferred to the
// caller on success.  Headers are a private copy owned by the caller.
type ProtocolMessage struct {
	Body    bodyfile.Body
	Headers http.Header
}

// ProtocolAdapter converts the canonical Anthropic classifier representation
// to and from one fixed upstream protocol.  Implementations must not close
// input bodies or retain mutable headers after returning.
type ProtocolAdapter interface {
	Protocol() string
	EncodeRequest(bodyfile.Body, http.Header) (ProtocolMessage, error)
	DecodeResponse(bodyfile.Body, http.Header) (ProtocolMessage, error)
}

// Adapter is a short alias used by callers that do not need the longer name.
type Adapter = ProtocolAdapter

// ProtocolAdapterRegistry is the lookup boundary injected into fixed-target
// execution.  A registry is immutable after construction.
type ProtocolAdapterRegistry interface {
	Lookup(protocol string) (ProtocolAdapter, bool)
}

// Registry is the production immutable adapter table.  The application uses
// an empty Registry until a real protocol implementation is deliberately
// added; tests may inject a fake adapter through the same interface.
type Registry struct {
	entries map[string]ProtocolAdapter
	order   []string
}

// NewRegistry validates and snapshots adapters in registration order.
func NewRegistry(adapters ...ProtocolAdapter) (*Registry, error) {
	entries := make(map[string]ProtocolAdapter, len(adapters))
	order := make([]string, 0, len(adapters))
	for index, adapter := range adapters {
		if isNilAdapter(adapter) {
			return nil, fmt.Errorf("%w at index %d", ErrNilAdapter, index)
		}
		protocolID := strings.TrimSpace(adapter.Protocol())
		if protocolID == "" || strings.ContainsAny(protocolID, " \t\r\n") {
			return nil, fmt.Errorf("%w at index %d: protocol id must be non-empty and whitespace-free", ErrInvalidAdapter, index)
		}
		if _, exists := entries[protocolID]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateAdapter, protocolID)
		}
		entries[protocolID] = adapter
		order = append(order, protocolID)
	}
	return &Registry{entries: entries, order: order}, nil
}

// EmptyRegistry returns an empty immutable production registry.
func EmptyRegistry() *Registry { return &Registry{entries: map[string]ProtocolAdapter{}} }

func (r *Registry) Lookup(protocolID string) (ProtocolAdapter, bool) {
	if r == nil {
		return nil, false
	}
	adapter, ok := r.entries[protocolID]
	return adapter, ok
}

// List returns registered protocol identifiers in deterministic order.
func (r *Registry) List() []string {
	if r == nil {
		return []string{}
	}
	result := append([]string(nil), r.order...)
	if len(result) > 1 {
		// Registration order is useful for diagnostics, but sorting makes a
		// defensive discovery response deterministic for map-backed callers.
		sort.Strings(result)
	}
	return result
}

func isNilAdapter(adapter ProtocolAdapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ ProtocolAdapterRegistry = (*Registry)(nil)
