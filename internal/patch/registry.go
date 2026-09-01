package patch

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

var (
	ErrUnknownPatch       = errors.New("unknown provider patch")
	ErrDuplicatePatch     = errors.New("duplicate provider patch")
	ErrPatchConflict      = errors.New("provider patch conflict")
	ErrPatchNotApplicable = errors.New("provider patch is not applicable to target request type")
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
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	RequestTypes []RequestType `json:"request_types"`
	Stages       []Stage       `json:"stages"`
	Conflicts    []string      `json:"conflicts"`
	Idempotence  Idempotence   `json:"idempotence"`
	// RequestPaths and ResponsePaths are internal selective-scan contracts.
	// They are not discovery/configuration fields: callers only need to know
	// that the compiled Plan can prepare the minimal index required by hooks.
	RequestPaths  []string        `json:"-"`
	ResponsePaths []string        `json:"-"`
	Factory       InstanceFactory `json:"-"`
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
	d.RequestPaths = append([]string(nil), d.RequestPaths...)
	d.ResponsePaths = append([]string(nil), d.ResponsePaths...)
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

// Services returns the immutable service handles captured when the registry
// was built.  The handles themselves (for example AliasStore) are process
// runtime resources and are intentionally shared across all compiled target
// snapshots; this method never creates or clones them.
func (r Registry) Services() Services { return r.services }

// AliasStore returns the shared alias map, if this registry was constructed
// with one.  It is primarily a diagnostics/test seam; production callers must
// obtain the registry from Runtime Manager rather than constructing another.
func (r Registry) AliasStore() *AliasStore { return r.services.AliasStore }

// RequiresAliasStore reports whether this registry contains the built-in
// session-isolation definition. RuntimeContext uses it to reject a production
// context that forgot to inject the central AliasStore.
func (r Registry) RequiresAliasStore() bool {
	_, ok := r.entries[CLIProxyAPIClassifierSessionID]
	return ok
}

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
		if err := validateScanPaths(d); err != nil {
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

// DefaultRegistry builds the one built-in registry from caller-owned shared
// services.  The caller (normally the application composition root) must
// create and retain the AliasStore; this constructor never allocates runtime
// state.  The four definitions are executable and therefore
// discoverable/configurable; no placeholder entries are exposed.
func DefaultRegistry(services Services) Registry {
	definitions := []PatchDefinition{
		newAnyRouterSubagentDefinition(),
		newAnyRouterClassifierDefinition(),
		newCLIProxyAPIClassifierDefinition(),
		newGPTClassifierResponseDefinition(),
	}
	if services.AliasStore == nil {
		panic(errors.New("alias store service is required"))
	}
	r, err := NewRegistryWithServices(definitions, services)
	if err != nil {
		// Built-in definitions are compile-time constants. A panic here is a
		// programmer error and is preferable to a partially populated registry.
		panic(err)
	}
	return r
}

// NewDefaultRegistry is the error-returning form of DefaultRegistry for
// startup paths that prefer to surface a construction error instead of
// panicking.  It still requires caller-owned services and never allocates an
// AliasStore itself.
func NewDefaultRegistry(services Services) (Registry, error) {
	if services.AliasStore == nil {
		return Registry{}, errors.New("alias store service is required")
	}
	definitions := []PatchDefinition{
		newAnyRouterSubagentDefinition(),
		newAnyRouterClassifierDefinition(),
		newCLIProxyAPIClassifierDefinition(),
		newGPTClassifierResponseDefinition(),
	}
	return NewRegistryWithServices(definitions, services)
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
			return Plan{}, fmt.Errorf("patch %q: %w", id, ErrPatchNotApplicable)
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
	return newPlan(selected, r.services)
}

func (r Registry) CompileForType(ids []string, requestType RequestType) (Plan, error) {
	return r.Compile(ids, requestType)
}

// CompileForTypes is the explicit slice variant used by fixed classifier
// targets and future request types.
func (r Registry) CompileForTypes(ids []string, requestTypes []RequestType) (Plan, error) {
	return r.compile(ids, requestTypes)
}

// RequiredPaths returns the union declared by definitions applicable to any
// requested type and stage. It is used before request-type classification to
// prepare one ingress scan that is sufficient for every reachable flow.
func (r Registry) RequiredPaths(stage Stage, requestTypes ...RequestType) ([]string, error) {
	if !validStage(stage) {
		return nil, fmt.Errorf("invalid patch stage %q", stage)
	}
	if len(requestTypes) == 0 {
		requestTypes = []RequestType{RequestTypeNormal, RequestTypeClassifier}
	}
	for _, requestType := range requestTypes {
		if !validRequestType(requestType) || requestType == RequestTypeAny {
			return nil, fmt.Errorf("invalid patch request type %q", requestType)
		}
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, id := range r.order {
		definition := r.entries[id]
		applies := false
		for _, requestType := range requestTypes {
			if definitionApplies(definition, requestType) {
				applies = true
				break
			}
		}
		if !applies || !hasStage(definition.Stages, stage) {
			continue
		}
		paths := definition.RequestPaths
		if stage == StageResponse {
			paths = definition.ResponsePaths
		}
		for _, path := range paths {
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			result = append(result, path)
		}
	}
	if result == nil {
		return []string{}, nil
	}
	return result, nil
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

func validateScanPaths(definition PatchDefinition) error {
	if len(definition.RequestPaths) > 0 && !hasStage(definition.Stages, StageRequest) {
		return fmt.Errorf("%w: request paths declared without request stage", ErrInvalidDefinition)
	}
	if len(definition.ResponsePaths) > 0 && !hasStage(definition.Stages, StageResponse) {
		return fmt.Errorf("%w: response paths declared without response stage", ErrInvalidDefinition)
	}
	if _, err := bodyfile.NewScanSpec(definition.RequestPaths...); err != nil {
		return fmt.Errorf("%w: invalid request paths: %v", ErrInvalidDefinition, err)
	}
	if _, err := bodyfile.NewScanSpec(definition.ResponsePaths...); err != nil {
		return fmt.Errorf("%w: invalid response paths: %v", ErrInvalidDefinition, err)
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
	d.RequestPaths = append([]string(nil), d.RequestPaths...)
	d.ResponsePaths = append([]string(nil), d.ResponsePaths...)
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
