package patch

import (
	"fmt"
)

// Plan is an immutable ordered collection of patch definitions. It can be
// shared by all requests targeting the same compiled target. Request-level
// mutable state is created only by NewInstance. The services handle captured
// here belongs to the process-owned RuntimeContext; Plan construction never
// allocates a replacement AliasStore.
type Plan struct {
	definitions []PatchDefinition
	services    Services
}

func newPlan(definitions []PatchDefinition, services Services) Plan {
	cloned := make([]PatchDefinition, len(definitions))
	for i, definition := range definitions {
		cloned[i] = cloneDefinition(definition)
	}
	return Plan{definitions: cloned, services: services}
}

func (p Plan) Empty() bool { return len(p.definitions) == 0 }

func (p Plan) Len() int { return len(p.definitions) }

// IDs returns the configured order of this plan.
func (p Plan) IDs() []string {
	ids := make([]string, len(p.definitions))
	for i, definition := range p.definitions {
		ids[i] = definition.ID
	}
	return ids
}

// List returns immutable metadata in plan order.
func (p Plan) List() []PatchMetadata {
	result := make([]PatchMetadata, len(p.definitions))
	for i, definition := range p.definitions {
		result[i] = cloneMetadata(definition.metadata())
	}
	if result == nil {
		return []PatchMetadata{}
	}
	return result
}

// Definitions returns defensive definition copies. It is primarily useful to
// diagnostics/tests; the execution path should use NewInstance.
func (p Plan) Definitions() []PatchDefinition {
	result := make([]PatchDefinition, len(p.definitions))
	for i, definition := range p.definitions {
		result[i] = cloneDefinition(definition)
	}
	if result == nil {
		return []PatchDefinition{}
	}
	return result
}

// ForRequestType creates a filtered immutable plan. Definitions that do not
// apply to the supplied type are omitted; conflicts are rechecked over the
// resulting set.
func (p Plan) ForRequestType(requestType RequestType) (Plan, error) {
	if !validRequestType(requestType) || requestType == RequestTypeAny {
		return Plan{}, fmt.Errorf("invalid patch request type %q", requestType)
	}
	selected := make([]PatchDefinition, 0, len(p.definitions))
	for _, definition := range p.definitions {
		if definitionApplies(definition, requestType) {
			selected = append(selected, cloneDefinition(definition))
		}
	}
	if err := validateSelectedConflicts(selected, []RequestType{requestType}); err != nil {
		return Plan{}, err
	}
	return newPlan(selected, p.services), nil
}

// NewInstance creates one request-scoped execution. Every selected definition
// receives a fresh instance, so response hooks can safely retain state from
// their own request hook without leaking across concurrent requests.
func (p Plan) NewInstance(context PatchContext) (*Execution, error) {
	if err := context.Validate(); err != nil {
		return nil, err
	}
	entries := make([]executionEntry, 0, len(p.definitions))
	for _, definition := range p.definitions {
		if !definitionApplies(definition, context.RequestType) {
			continue
		}
		instance, err := invokeFactory(definition.Factory, FactoryContext{
			PatchContext: context,
			Services:     p.services,
		})
		if err != nil {
			closeEntries(entries)
			return nil, fmt.Errorf("patch %q instance: %w", definition.ID, err)
		}
		requestHook, responseHook, hookErr := instanceHooks(instance)
		if hookErr != nil {
			closeEntries(entries)
			closeInstance(instance)
			return nil, fmt.Errorf("patch %q capabilities: %w", definition.ID, hookErr)
		}
		if hasStage(definition.Stages, StageRequest) && requestHook == nil {
			closeEntries(entries)
			closeInstance(instance)
			return nil, fmt.Errorf("patch %q: %w (request stage)", definition.ID, ErrHookUnavailable)
		}
		if hasStage(definition.Stages, StageResponse) && responseHook == nil {
			closeEntries(entries)
			closeInstance(instance)
			return nil, fmt.Errorf("patch %q: %w (response stage)", definition.ID, ErrHookUnavailable)
		}
		entries = append(entries, executionEntry{
			definition: definition,
			instance:   instance,
			request:    requestHook,
			response:   responseHook,
		})
	}
	return newExecution(context, entries), nil
}

// NewExecution is an alias retained for callers that use the longer name.
func (p Plan) NewExecution(context PatchContext) (*Execution, error) {
	return p.NewInstance(context)
}

func hasStage(stages []Stage, want Stage) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
}
