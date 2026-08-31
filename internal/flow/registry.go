package flow

import (
	"context"
	"fmt"
	"reflect"

	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// Registry is an immutable RequestType -> Planner table.
type Registry struct {
	planners map[traffic.RequestType]Planner
}

// NewRegistry validates and snapshots the supplied planners. The variadic
// form keeps construction convenient while NewRegistryFromSlice supports
// callers that already hold a slice.
func NewRegistry(planners ...Planner) (*Registry, error) {
	entries := make(map[traffic.RequestType]Planner, len(planners))
	for _, planner := range planners {
		if isNilPlanner(planner) {
			return nil, fmt.Errorf("%w: nil planner", ErrDuplicatePlanner)
		}
		typeID, err := plannerType(planner)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnknownRequestType, err)
		}
		if !typeID.Valid() || typeID == traffic.RequestType("*") {
			return nil, fmt.Errorf("%w: %q", ErrUnknownRequestType, typeID)
		}
		if _, exists := entries[typeID]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicatePlanner, typeID)
		}
		entries[typeID] = planner
	}
	return &Registry{planners: entries}, nil
}

func NewRegistryFromSlice(planners []Planner) (*Registry, error) {
	return NewRegistry(planners...)
}

func (r *Registry) Lookup(requestType traffic.RequestType) (Planner, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.planners[requestType]
	return p, ok
}

func (r *Registry) List() []traffic.RequestType {
	if r == nil {
		return []traffic.RequestType{}
	}
	result := make([]traffic.RequestType, 0, len(r.planners))
	for typ := range r.planners {
		result = append(result, typ)
	}
	// Keep the result deterministic without exposing the mutable map.
	for i := 1; i < len(result); i++ {
		v := result[i]
		j := i
		for j > 0 && result[j-1] > v {
			result[j] = result[j-1]
			j--
		}
		result[j] = v
	}
	return result
}

// Dispatcher is a small stable facade over Registry. It is safe for
// concurrent reads because Registry never mutates after construction.
type Dispatcher struct {
	registry *Registry
}

func NewDispatcher(registry *Registry) *Dispatcher {
	return &Dispatcher{registry: registry}
}

func (d *Dispatcher) Dispatch(ctx context.Context, snapshot SnapshotView, ingress traffic.IngressRequest) (ExecutionPlan, error) {
	if d == nil || d.registry == nil {
		return ExecutionPlan{}, ErrMissingPlanner
	}
	planner, ok := d.registry.Lookup(ingress.RequestType)
	if !ok {
		return ExecutionPlan{}, fmt.Errorf("%w: %q", ErrMissingPlanner, ingress.RequestType)
	}
	plan, err := planner.Build(ctx, snapshot, ingress)
	if err != nil {
		return ExecutionPlan{}, err
	}
	if err := plan.Validate(); err != nil {
		// A planner may have already allocated a sealed request body before
		// returning a structurally invalid plan.  The dispatcher owns no
		// successful plan, so close that body on this rejection path to keep
		// request-scoped temporary files from escaping.
		if plan.PreparedRequest.BaseBody != nil {
			_ = plan.PreparedRequest.BaseBody.Close()
		}
		return ExecutionPlan{}, err
	}
	return plan, nil
}

func (r *Registry) Dispatch(ctx context.Context, snapshot SnapshotView, ingress traffic.IngressRequest) (ExecutionPlan, error) {
	return NewDispatcher(r).Dispatch(ctx, snapshot, ingress)
}

func isNilPlanner(planner Planner) bool {
	if planner == nil {
		return true
	}
	value := reflect.ValueOf(planner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func plannerType(planner Planner) (requestType traffic.RequestType, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("planner request type panic: %v", recovered)
		}
	}()
	return planner.RequestType(), nil
}
