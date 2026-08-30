package patch

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrUnknownPatch       = errors.New("unknown provider patch")
	ErrDuplicatePatch     = errors.New("duplicate provider patch")
	ErrPatchConflict      = errors.New("provider patch conflict")
	ErrInvalidDefinition  = errors.New("invalid provider patch definition")
	ErrFactoryUnavailable = errors.New("provider patch factory unavailable")
	ErrHookUnavailable    = errors.New("provider patch hook unavailable")
)

// Services are immutable/shared services that may be injected into factories.
// The AliasStore is intentionally the only built-in shared service today.
// Additional services should be added here rather than making hooks depend on
// gateway/runtime/provider implementation packages.
type Services struct {
	AliasStore *AliasStore
}

// FactoryContext is passed to a patch factory for one upstream execution.
// Context embeds the read-only PatchContext; Services contains process-local
// helpers and never credentials.
type FactoryContext struct {
	PatchContext
	Services Services
}

// InstanceFactory is the single supported factory shape.
type InstanceFactory func(FactoryContext) (PatchInstance, error)

// PatchDefinition combines immutable metadata and an executable factory.
type PatchDefinition struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	RequestTypes []RequestType   `json:"request_types"`
	Stages       []Stage         `json:"stages"`
	Conflicts    []string        `json:"conflicts"`
	Idempotence  Idempotence     `json:"idempotence"`
	Factory      InstanceFactory `json:"-"`
}

// Definition is a concise alias used by callers that do not need the
// Patch-prefixed name.
type Definition = PatchDefinition

func (d PatchDefinition) metadata() PatchMetadata {
	return PatchMetadata{
		ID:           d.ID,
		Name:         d.Name,
		Description:  d.Description,
		RequestTypes: append([]RequestType(nil), d.RequestTypes...),
		Stages:       append([]Stage(nil), d.Stages...),
		Conflicts:    append([]string(nil), d.Conflicts...),
		Idempotence:  d.Idempotence,
	}
}

func (d PatchDefinition) normalized() PatchDefinition {
	m := d.metadata()
	d.ID, d.Name, d.Description = m.ID, m.Name, m.Description
	d.RequestTypes = append([]RequestType(nil), m.RequestTypes...)
	d.Stages = append([]Stage(nil), m.Stages...)
	d.Conflicts = append([]string(nil), m.Conflicts...)
	d.Idempotence = m.Idempotence
	return d
}

// Registry is an immutable definition table. The map and ordered slice are
// never exposed; methods return defensive copies. A zero Registry is empty.
type Registry struct {
	entries  map[string]PatchDefinition
	order    []string
	services Services
}

func (r Registry) Empty() bool { return len(r.entries) == 0 }

// NewRegistry validates and freezes definitions. It rejects unknown request
// types/stages, malformed metadata, duplicate IDs, one-way/self conflicts,
// missing factories, and factory capability mismatches.
func NewRegistry(definitions []PatchDefinition) (Registry, error) {
	return newRegistry(definitions, Services{})
}

func newRegistry(definitions []PatchDefinition, services Services) (Registry, error) {
	r := Registry{
		entries:  make(map[string]PatchDefinition, len(definitions)),
		order:    make([]string, 0, len(definitions)),
		services: services,
	}
	for i, raw := range definitions {
		d := raw.normalized()
		if err := validateMetadata(d.metadata()); err != nil {
			return Registry{}, fmt.Errorf("definition[%d]: %w", i, err)
		}
		if d.Factory == nil {
			return Registry{}, fmt.Errorf("definition %q: %w", d.ID, ErrFactoryUnavailable)
		}
		if _, exists := r.entries[d.ID]; exists {
			return Registry{}, fmt.Errorf("definition %q: %w", d.ID, ErrDuplicatePatch)
		}
		r.entries[d.ID] = d
		r.order = append(r.order, d.ID)
	}
	for _, id := range r.order {
		d := r.entries[id]
		if err := validateFactoryCapabilities(d, r.services); err != nil {
			return Registry{}, err
		}
		seen := make(map[string]struct{}, len(d.Conflicts))
		for _, conflict := range d.Conflicts {
			if conflict == id {
				return Registry{}, fmt.Errorf("definition %q: %w with itself", id, ErrPatchConflict)
			}
			if _, duplicate := seen[conflict]; duplicate {
				return Registry{}, fmt.Errorf("definition %q: duplicate conflict %q", id, conflict)
			}
			seen[conflict] = struct{}{}
			other, ok := r.entries[conflict]
			if !ok {
				return Registry{}, fmt.Errorf("definition %q: %w conflict target %q", id, ErrUnknownPatch, conflict)
			}
			if !containsString(other.Conflicts, id) {
				return Registry{}, fmt.Errorf("definition %q: conflict with %q must be symmetric", id, conflict)
			}
		}
	}
	return r, nil
}

// validateFactoryCapabilities probes every concrete request type declared by a
// definition. A factory is allowed to inspect the type and return different
// instances, so checking only the first metadata entry would let a wildcard or
// multi-type definition hide a missing hook for another applicable flow.
func validateFactoryCapabilities(definition PatchDefinition, services Services) error {
	for _, requestType := range definitionValidationTypes(definition.RequestTypes) {
		instance, factoryErr := invokeFactory(definition.Factory, FactoryContext{
			PatchContext: PatchContext{
				RequestType: requestType,
				TargetID:    "registry-validation",
				Generation:  "registry-validation",
			},
			Services: services,
		})
		if factoryErr != nil {
			return fmt.Errorf("definition %q factory for %q: %w", definition.ID, requestType, factoryErr)
		}
		requestHook, responseHook, hookErr := instanceHooks(instance)
		if hookErr != nil {
			closeInstance(instance)
			return fmt.Errorf("definition %q capabilities for %q: %w", definition.ID, requestType, hookErr)
		}
		requestDeclared := hasStage(definition.Stages, StageRequest)
		responseDeclared := hasStage(definition.Stages, StageResponse)
		if requestDeclared != (requestHook != nil) {
			closeInstance(instance)
			return fmt.Errorf("definition %q: %w (request capability for %q)", definition.ID, ErrHookUnavailable, requestType)
		}
		if responseDeclared != (responseHook != nil) {
			closeInstance(instance)
			return fmt.Errorf("definition %q: %w (response capability for %q)", definition.ID, ErrHookUnavailable, requestType)
		}
		closeInstance(instance)
	}
	return nil
}

func definitionValidationTypes(requestTypes []RequestType) []RequestType {
	result := make([]RequestType, 0, len(requestTypes)+1)
	seen := make(map[RequestType]struct{}, len(requestTypes)+1)
	add := func(requestType RequestType) {
		if _, exists := seen[requestType]; exists {
			return
		}
		seen[requestType] = struct{}{}
		result = append(result, requestType)
	}
	for _, requestType := range requestTypes {
		if requestType == RequestTypeAny {
			add(RequestTypeNormal)
			add(RequestTypeClassifier)
			continue
		}
		add(requestType)
	}
	return result
}

// NewRegistryWithServices validates definitions against the exact shared
// services that their factories will receive at runtime.
func NewRegistryWithServices(definitions []PatchDefinition, services Services) (Registry, error) {
	return newRegistry(definitions, services)
}

// DefaultRegistry is the one built-in registry. The four definitions are
// executable and therefore discoverable/configurable; no placeholder entries
// are exposed.
func DefaultRegistry() Registry {
	definitions := []PatchDefinition{
		newAnyRouterSubagentDefinition(),
		newAnyRouterClassifierDefinition(),
		newCLIProxyAPIClassifierDefinition(),
		newGPTClassifierResponseDefinition(),
	}
	r, err := NewRegistryWithServices(definitions, Services{AliasStore: NewAliasStore()})
	if err != nil {
		// Built-in definitions are compile-time constants. A panic here is a
		// programmer error and is preferable to a partially populated registry.
		panic(err)
	}
	return r
}

// Lookup returns a defensive definition copy. The bool is false for unknown
// IDs.
func (r Registry) Lookup(id string) (PatchDefinition, bool) {
	d, ok := r.entries[id]
	if !ok {
		return PatchDefinition{}, false
	}
	return cloneDefinition(d), true
}

// List returns definitions in the documented built-in order, followed by
// custom IDs in lexical order. This makes discovery deterministic and keeps
// map iteration out of API responses.
func (r Registry) List() []PatchMetadata {
	if len(r.entries) == 0 {
		return []PatchMetadata{}
	}
	preferred := []string{
		"anyrouter-subagent-thinking",
		"anyrouter-classifier-request-compat",
		"cliproxyapi-classifier-session-isolation",
		"gpt-classifier-response-reassembly",
	}
	result := make([]PatchMetadata, 0, len(r.entries))
	seen := make(map[string]struct{}, len(r.entries))
	for _, id := range preferred {
		if d, ok := r.entries[id]; ok {
			result = append(result, cloneMetadata(d.metadata()))
			seen[id] = struct{}{}
		}
	}
	custom := make([]string, 0, len(r.entries)-len(result))
	for id := range r.entries {
		if _, ok := seen[id]; !ok {
			custom = append(custom, id)
		}
	}
	sort.Strings(custom)
	for _, id := range custom {
		result = append(result, cloneMetadata(r.entries[id].metadata()))
	}
	return result
}

// Validate checks IDs without applying request-type filtering. It is useful at
// config/schema boundaries where the target type is not yet known.
func (r Registry) Validate(ids []string) error {
	_, err := r.Compile(ids)
	return err
}

// Compile builds an immutable target plan in configured order. Optional target
// request types filter definitions before conflict checks.
func (r Registry) Compile(ids []string, targetTypes ...RequestType) (Plan, error) {
	return r.compile(ids, targetTypes)
}

func (r Registry) compile(ids []string, targetTypes []RequestType) (Plan, error) {
	types, err := normalizeTargetTypes(targetTypes)
	if err != nil {
		return Plan{}, err
	}
	selected := make([]PatchDefinition, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return Plan{}, fmt.Errorf("patch %q: %w", id, ErrDuplicatePatch)
		}
		seen[id] = struct{}{}
		d, ok := r.entries[id]
		if !ok {
			return Plan{}, fmt.Errorf("patch %q: %w", id, ErrUnknownPatch)
		}
		if len(types) > 0 && !definitionAppliesToAny(d, types) {
			// An ID that is valid globally but not applicable to this target is
			// a static configuration error, not a silently ignored patch.
			return Plan{}, fmt.Errorf("patch %q: not applicable to target request type", id)
		}
		selected = append(selected, cloneDefinition(d))
	}
	if err := validateSelectedConflicts(selected, types); err != nil {
		return Plan{}, err
	}
	if len(types) > 0 {
		filtered := selected[:0]
		for _, d := range selected {
			if definitionAppliesToAny(d, types) {
				filtered = append(filtered, d)
			}
		}
		selected = filtered
	}
	return newPlan(selected, r.services), nil
}

func (r Registry) CompileForType(ids []string, requestType RequestType) (Plan, error) {
	return r.Compile(ids, requestType)
}

// CompileForTypes is the explicit slice variant used by fixed classifier
// targets and future request types.
func (r Registry) CompileForTypes(ids []string, requestTypes []RequestType) (Plan, error) {
	return r.compile(ids, requestTypes)
}

func validateMetadata(m PatchMetadata) error {
	if strings.TrimSpace(m.ID) == "" || strings.ContainsAny(m.ID, " \t\r\n") {
		return fmt.Errorf("%w: id must be non-empty and contain no whitespace", ErrInvalidDefinition)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalidDefinition)
	}
	if m.RequestTypes == nil || len(m.RequestTypes) == 0 {
		return fmt.Errorf("%w: request_types must not be empty", ErrInvalidDefinition)
	}
	seenTypes := make(map[RequestType]struct{}, len(m.RequestTypes))
	for _, typ := range m.RequestTypes {
		if !validRequestType(typ) {
			return fmt.Errorf("%w: unknown request type %q", ErrInvalidDefinition, typ)
		}
		if _, duplicate := seenTypes[typ]; duplicate {
			return fmt.Errorf("%w: duplicate request type %q", ErrInvalidDefinition, typ)
		}
		seenTypes[typ] = struct{}{}
	}
	if m.Stages == nil || len(m.Stages) == 0 {
		return fmt.Errorf("%w: stages must not be empty", ErrInvalidDefinition)
	}
	seenStages := make(map[Stage]struct{}, len(m.Stages))
	for _, stage := range m.Stages {
		if !validStage(stage) {
			return fmt.Errorf("%w: unknown stage %q", ErrInvalidDefinition, stage)
		}
		if _, duplicate := seenStages[stage]; duplicate {
			return fmt.Errorf("%w: duplicate stage %q", ErrInvalidDefinition, stage)
		}
		seenStages[stage] = struct{}{}
	}
	if m.Idempotence != Idempotent && m.Idempotence != PerExecution {
		return fmt.Errorf("%w: idempotence must be %q or %q", ErrInvalidDefinition, Idempotent, PerExecution)
	}
	return nil
}

func validateSelectedConflicts(selected []PatchDefinition, targetTypes []RequestType) error {
	for i := range selected {
		for j := i + 1; j < len(selected); j++ {
			a, b := selected[i], selected[j]
			if !containsString(a.Conflicts, b.ID) && !containsString(b.Conflicts, a.ID) {
				continue
			}
			if len(targetTypes) > 0 && !definitionAppliesToAny(a, targetTypes) {
				continue
			}
			if len(targetTypes) > 0 && !definitionAppliesToAny(b, targetTypes) {
				continue
			}
			if !typesIntersect(a.RequestTypes, b.RequestTypes) || !stagesIntersect(a.Stages, b.Stages) {
				continue
			}
			return fmt.Errorf("patch %q and %q: %w", a.ID, b.ID, ErrPatchConflict)
		}
	}
	return nil
}

func definitionAppliesToAny(d PatchDefinition, types []RequestType) bool {
	for _, typ := range types {
		if definitionApplies(d, typ) {
			return true
		}
	}
	return false
}

func definitionApplies(d PatchDefinition, typ RequestType) bool {
	return containsRequestType(d.RequestTypes, RequestTypeAny) || containsRequestType(d.RequestTypes, typ)
}

func typesIntersect(a, b []RequestType) bool {
	for _, left := range a {
		for _, right := range b {
			if left == RequestTypeAny || right == RequestTypeAny || left == right {
				return true
			}
		}
	}
	return false
}

func stagesIntersect(a, b []Stage) bool {
	for _, left := range a {
		for _, right := range b {
			if left == right {
				return true
			}
		}
	}
	return false
}

func containsRequestType(items []RequestType, target RequestType) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func cloneMetadata(m PatchMetadata) PatchMetadata {
	m.RequestTypes = append([]RequestType(nil), m.RequestTypes...)
	m.Stages = append([]Stage(nil), m.Stages...)
	m.Conflicts = append([]string(nil), m.Conflicts...)
	if m.RequestTypes == nil {
		m.RequestTypes = []RequestType{}
	}
	if m.Stages == nil {
		m.Stages = []Stage{}
	}
	if m.Conflicts == nil {
		m.Conflicts = []string{}
	}
	return m
}

func cloneDefinition(d PatchDefinition) PatchDefinition {
	d.RequestTypes = append([]RequestType(nil), d.RequestTypes...)
	d.Stages = append([]Stage(nil), d.Stages...)
	d.Conflicts = append([]string(nil), d.Conflicts...)
	return d
}

func normalizeTargetTypes(types []RequestType) ([]RequestType, error) {
	if len(types) == 0 {
		return nil, nil
	}
	seen := make(map[RequestType]struct{}, len(types))
	for _, typ := range types {
		if !validRequestType(typ) || typ == RequestTypeAny {
			return nil, fmt.Errorf("invalid patch target request type %q", typ)
		}
		if _, ok := seen[typ]; !ok {
			seen[typ] = struct{}{}
		}
	}
	ordered := make([]RequestType, 0, len(seen))
	for _, typ := range types {
		if _, ok := seen[typ]; ok {
			ordered = append(ordered, typ)
			delete(seen, typ)
		}
	}
	return ordered, nil
}
