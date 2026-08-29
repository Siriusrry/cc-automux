package scheduler

import (
	"container/list"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
)

var (
	ErrNoEligibleProvider     = errors.New("no eligible provider")
	ErrAttemptBudgetExhausted = errors.New("provider attempt budget exhausted")
	ErrInvalidSchedulingKey   = errors.New("invalid scheduling key")
)

// UnavailableError reports that no candidate is currently schedulable. A
// recovery time is present only when a health cooldown has a known deadline.
type UnavailableError struct {
	RetryAt time.Time
}

func (e *UnavailableError) Error() string {
	if e == nil || e.RetryAt.IsZero() {
		return ErrNoEligibleProvider.Error()
	}
	return fmt.Sprintf("%s; retry at %s", ErrNoEligibleProvider, e.RetryAt.Format(time.RFC3339Nano))
}

func (e *UnavailableError) Unwrap() error { return ErrNoEligibleProvider }

func (e *UnavailableError) RetryAtTime() (time.Time, bool) {
	if e == nil || e.RetryAt.IsZero() {
		return time.Time{}, false
	}
	return e.RetryAt, true
}

type Options struct {
	Policy Policy
	Now    func() time.Time
}

type roundRobinKey struct {
	Model        string
	TrafficClass TrafficClass
	Priority     int64
}

type cursorState struct {
	ProviderID string
	Version    uint64
}

type providerState struct {
	Provider *provider.CompiledProvider
	Static   StaticAvailability
}

type assignmentEntry struct {
	assignment Assignment
	element    *list.Element
}

// Scheduler owns deterministic provider selection and request-independent
// session affinity. Health transitions remain delegated to HealthController.
type Scheduler struct {
	mu     sync.Mutex
	health HealthController
	policy Policy
	now    func() time.Time

	revision  uint64
	providers map[string]providerState
	cursors   map[roundRobinKey]cursorState

	assignments map[StickyKey]*assignmentEntry
	lru         list.List
	byProvider  map[string]map[StickyKey]*assignmentEntry
}

var _ Selector = (*Scheduler)(nil)

func New(health HealthController, options Options) (*Scheduler, error) {
	if health == nil {
		return nil, errors.New("health controller is required")
	}
	policy := options.Policy
	if policy == (Policy{}) {
		policy = DefaultPolicy()
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Scheduler{
		health:      health,
		policy:      policy,
		now:         now,
		providers:   make(map[string]providerState),
		cursors:     make(map[roundRobinKey]cursorState),
		assignments: make(map[StickyKey]*assignmentEntry),
		byProvider:  make(map[string]map[StickyKey]*assignmentEntry),
	}, nil
}

// NewSelector is the explicit constructor name for callers that depend on
// the Selector boundary rather than the concrete Scheduler type.
func NewSelector(health HealthController, options Options) (*Scheduler, error) {
	return New(health, options)
}

// StaticAvailabilityOf classifies request-independent provider eligibility.
// Compiled configurations normally prevent an active provider from lacking a
// key; treating such a defensive value as disabled still keeps it fail closed.
func StaticAvailabilityOf(item *provider.CompiledProvider) StaticAvailability {
	if item == nil || !item.Enabled {
		return StaticDisabledProvider
	}
	if len(item.Models) == 0 {
		return StaticNoModels
	}
	if item.APIKey == "" {
		return StaticDisabledProvider
	}
	if err := item.ValidateApplication(); err != nil {
		return StaticPatchUnavailable
	}
	return StaticActive
}

func (s *Scheduler) Acquire(snapshot Snapshot, key StickyKey, excluded map[string]struct{}) (AttemptLease, error) {
	if snapshot == nil {
		return AttemptLease{}, fmt.Errorf("%w: snapshot is required", ErrInvalidSchedulingKey)
	}
	key.SessionID = strings.TrimSpace(key.SessionID)
	if key.Model == "" {
		return AttemptLease{}, fmt.Errorf("%w: model is required", ErrInvalidSchedulingKey)
	}
	if key.TrafficClass != TrafficClassNormal && key.TrafficClass != TrafficClassClassifier {
		return AttemptLease{}, fmt.Errorf("%w: unsupported traffic class %q", ErrInvalidSchedulingKey, key.TrafficClass)
	}
	if len(excluded) >= s.policy.MaxAttempts {
		return AttemptLease{}, ErrAttemptBudgetExhausted
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.expireLocked(now)
	currentSnapshot := s.revision == 0 || snapshot.Revision() == s.revision
	items := eligibleCandidates(snapshot.Candidates(key.Model), key.Model)

	var assigned *assignmentEntry
	var assignedProvider *provider.CompiledProvider
	if currentSnapshot && key.SessionID != "" {
		assigned = s.assignments[key]
		if assigned != nil {
			assignedProvider = findCandidate(items, assigned.assignment.ProviderID, assigned.assignment.Generation)
			if assignedProvider == nil {
				s.removeAssignmentLocked(assigned)
				assigned = nil
			}
		}
	}

	ordered := s.orderCandidatesLocked(items, key, assignedProvider)
	healthKeys := make([]HealthKey, 0, len(ordered))
	for _, item := range ordered {
		if _, skip := excluded[item.ID]; skip {
			continue
		}
		healthKeys = append(healthKeys, HealthKey{
			ProviderID:   item.ID,
			Generation:   item.Generation,
			Model:        key.Model,
			TrafficClass: key.TrafficClass,
		})
	}

	for _, item := range ordered {
		if _, skip := excluded[item.ID]; skip {
			continue
		}
		healthKey := HealthKey{
			ProviderID:   item.ID,
			Generation:   item.Generation,
			Model:        key.Model,
			TrafficClass: key.TrafficClass,
		}
		decision := s.health.Acquire(healthKey, item.DisableHealth)
		if !decision.Available {
			if assigned != nil && item.ID == assigned.assignment.ProviderID &&
				(decision.GlobalState == GlobalCooldown || decision.ChannelState == ChannelCooldown) {
				s.removeAssignmentLocked(assigned)
				assigned = nil
				assignedProvider = nil
			}
			continue
		}

		fromSticky := assigned != nil && item.ID == assigned.assignment.ProviderID &&
			item.Generation == assigned.assignment.Generation
		if assigned != nil && assignedProvider != nil && item.Priority > assignedProvider.Priority {
			s.removeAssignmentLocked(assigned)
			assigned = nil
			assignedProvider = nil
			fromSticky = false
		}

		allocate := !fromSticky && assigned == nil && currentSnapshot
		if allocate && key.SessionID != "" {
			s.putAssignmentLocked(key, item, now)
		}
		if fromSticky {
			s.touchAssignmentLocked(assigned, now)
		}

		advanceCursor := allocate && (key.SessionID != "" || len(excluded) == 0)
		cursorKey := roundRobinKey{Model: key.Model, TrafficClass: key.TrafficClass, Priority: item.Priority}
		cursor := s.cursors[cursorKey]
		halfOpen := decision.Lease.GlobalProbe || decision.Lease.ChannelProbe
		if advanceCursor && !halfOpen {
			s.setCursorLocked(cursorKey, item.ID)
		}

		return AttemptLease{
			SnapshotRevision:       snapshot.Revision(),
			Provider:               item,
			Model:                  key.Model,
			TrafficClass:           key.TrafficClass,
			Generation:             item.Generation,
			FromSticky:             fromSticky,
			HalfOpenProbe:          halfOpen,
			HealthLease:            decision.Lease,
			stickyKey:              key,
			cursorKey:              cursorKey,
			cursorVersion:          cursor.Version,
			advanceCursorOnSuccess: advanceCursor && halfOpen,
		}, nil
	}

	if retryAt, ok := s.health.EarliestRetry(healthKeys); ok {
		return AttemptLease{}, &UnavailableError{RetryAt: retryAt}
	}
	return AttemptLease{}, &UnavailableError{}
}

func (s *Scheduler) Report(lease AttemptLease, outcome Outcome) HealthUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()

	update := s.health.Report(lease.HealthLease, outcome)
	if lease.Provider == nil {
		return update
	}
	current, ok := s.providers[lease.Provider.ID]
	if !ok || current.Provider.Generation != lease.Generation {
		return update
	}

	if lease.advanceCursorOnSuccess && outcome.Class == FailureNone {
		cursor := s.cursors[lease.cursorKey]
		if cursor.Version == lease.cursorVersion {
			s.setCursorLocked(lease.cursorKey, lease.Provider.ID)
		}
	}

	if lease.Provider.DisableHealth && outcome.ShouldFailover() && lease.stickyKey.SessionID != "" {
		if entry := s.assignments[lease.stickyKey]; entry != nil &&
			entry.assignment.ProviderID == lease.Provider.ID &&
			entry.assignment.Generation == lease.Generation {
			s.removeAssignmentLocked(entry)
		}
	}
	if !lease.Provider.DisableHealth && update.GlobalEnteredCooldown {
		s.removeProviderAssignmentsLocked(lease.Provider.ID, lease.Generation)
		s.clearProviderCursorsLocked(lease.Provider.ID)
	} else if !lease.Provider.DisableHealth && update.ChannelEnteredCooldown {
		s.removeChannelAssignmentsLocked(lease.Provider.ID, lease.Generation, lease.Model, lease.TrafficClass)
		s.clearChannelCursorLocked(lease.Provider.ID, lease.Model, lease.TrafficClass)
	}
	return update
}

func (s *Scheduler) Reconcile(snapshot Snapshot) {
	if snapshot == nil {
		return
	}
	providers := snapshot.Providers()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != 0 && snapshot.Revision() < s.revision {
		return
	}

	s.health.Reconcile(providers)
	next := make(map[string]providerState, len(providers))
	for _, item := range providers {
		if item == nil {
			continue
		}
		next[item.ID] = providerState{Provider: item, Static: StaticAvailabilityOf(item)}
	}
	s.revision = snapshot.Revision()
	s.providers = next

	for _, entry := range appendAssignmentEntries(s.assignments) {
		state, ok := next[entry.assignment.ProviderID]
		if !ok || state.Static != StaticActive ||
			state.Provider.Generation != entry.assignment.Generation ||
			!state.Provider.SupportsModel(entry.assignment.Key.Model) {
			s.removeAssignmentLocked(entry)
		}
	}
	for key, cursor := range s.cursors {
		state, ok := next[cursor.ProviderID]
		if cursor.ProviderID == "" {
			continue
		}
		if !ok || state.Static != StaticActive || state.Provider.Priority != key.Priority ||
			!state.Provider.SupportsModel(key.Model) {
			s.clearCursorLocked(key)
		}
	}
}

func (s *Scheduler) Assignments(providerID string) []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())

	result := make([]Assignment, 0)
	if providerID == "" {
		for _, entry := range s.assignments {
			result = append(result, entry.assignment)
		}
	} else {
		for _, entry := range s.byProvider[providerID] {
			result = append(result, entry.assignment)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		if result[i].Key.SessionID != result[j].Key.SessionID {
			return result[i].Key.SessionID < result[j].Key.SessionID
		}
		if result[i].Key.Model != result[j].Key.Model {
			return result[i].Key.Model < result[j].Key.Model
		}
		return result[i].Key.TrafficClass < result[j].Key.TrafficClass
	})
	return result
}

func (s *Scheduler) ActiveAssignmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	return len(s.assignments)
}

func eligibleCandidates(items []*provider.CompiledProvider, model string) []*provider.CompiledProvider {
	result := make([]*provider.CompiledProvider, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if StaticAvailabilityOf(item) != StaticActive || !item.SupportsModel(model) {
			continue
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Priority > result[j].Priority })
	return result
}

func findCandidate(items []*provider.CompiledProvider, id string, generation ProviderGeneration) *provider.CompiledProvider {
	for _, item := range items {
		if item.ID == id && item.Generation == generation {
			return item
		}
	}
	return nil
}

func (s *Scheduler) orderCandidatesLocked(items []*provider.CompiledProvider, key StickyKey, assigned *provider.CompiledProvider) []*provider.CompiledProvider {
	groups := make([][]*provider.CompiledProvider, 0)
	for _, item := range items {
		if len(groups) == 0 || groups[len(groups)-1][0].Priority != item.Priority {
			groups = append(groups, []*provider.CompiledProvider{item})
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], item)
		}
	}

	ordered := make([]*provider.CompiledProvider, 0, len(items))
	for _, group := range groups {
		if assigned != nil && group[0].Priority == assigned.Priority {
			ordered = append(ordered, rotateAfter(group, assigned.ID)...)
			continue
		}
		cursor := s.cursors[roundRobinKey{Model: key.Model, TrafficClass: key.TrafficClass, Priority: group[0].Priority}]
		ordered = append(ordered, rotateAfter(group, cursor.ProviderID)...)
	}
	if assigned == nil {
		return ordered
	}

	// A valid sticky provider leads its own priority group, while every higher
	// priority group remains ahead of it so recovered capacity is used promptly.
	for i, item := range ordered {
		if item.ID == assigned.ID && item.Generation == assigned.Generation {
			groupStart := i
			for groupStart > 0 && ordered[groupStart-1].Priority == assigned.Priority {
				groupStart--
			}
			if groupStart == i {
				return ordered
			}
			copy(ordered[groupStart+1:i+1], ordered[groupStart:i])
			ordered[groupStart] = item
			return ordered
		}
	}
	return ordered
}

func rotateAfter(items []*provider.CompiledProvider, providerID string) []*provider.CompiledProvider {
	result := make([]*provider.CompiledProvider, 0, len(items))
	if providerID == "" {
		return append(result, items...)
	}
	index := -1
	for i, item := range items {
		if item.ID == providerID {
			index = i
			break
		}
	}
	if index < 0 {
		return append(result, items...)
	}
	result = append(result, items[index+1:]...)
	result = append(result, items[:index+1]...)
	return result
}

func (s *Scheduler) putAssignmentLocked(key StickyKey, item *provider.CompiledProvider, now time.Time) {
	if existing := s.assignments[key]; existing != nil {
		s.removeAssignmentLocked(existing)
	}
	for len(s.assignments) >= s.policy.StickyCapacity {
		oldest, _ := s.lru.Front().Value.(*assignmentEntry)
		s.removeAssignmentLocked(oldest)
	}
	entry := &assignmentEntry{assignment: Assignment{
		Key:        key,
		ProviderID: item.ID,
		Generation: item.Generation,
		CreatedAt:  now,
		LastUsedAt: now,
	}}
	entry.element = s.lru.PushBack(entry)
	s.assignments[key] = entry
	index := s.byProvider[item.ID]
	if index == nil {
		index = make(map[StickyKey]*assignmentEntry)
		s.byProvider[item.ID] = index
	}
	index[key] = entry
}

func (s *Scheduler) touchAssignmentLocked(entry *assignmentEntry, now time.Time) {
	entry.assignment.LastUsedAt = now
	s.lru.MoveToBack(entry.element)
}

func (s *Scheduler) removeAssignmentLocked(entry *assignmentEntry) {
	if entry == nil {
		return
	}
	key := entry.assignment.Key
	if s.assignments[key] != entry {
		return
	}
	delete(s.assignments, key)
	s.lru.Remove(entry.element)
	index := s.byProvider[entry.assignment.ProviderID]
	delete(index, key)
	if len(index) == 0 {
		delete(s.byProvider, entry.assignment.ProviderID)
	}
}

func (s *Scheduler) expireLocked(now time.Time) {
	for element := s.lru.Front(); element != nil; element = s.lru.Front() {
		entry, _ := element.Value.(*assignmentEntry)
		if now.Before(entry.assignment.LastUsedAt.Add(s.policy.StickyTTL)) {
			return
		}
		s.removeAssignmentLocked(entry)
	}
}

func (s *Scheduler) removeProviderAssignmentsLocked(providerID string, generation ProviderGeneration) {
	for _, entry := range appendProviderEntries(s.byProvider[providerID]) {
		if entry.assignment.Generation == generation {
			s.removeAssignmentLocked(entry)
		}
	}
}

func (s *Scheduler) removeChannelAssignmentsLocked(providerID string, generation ProviderGeneration, model string, trafficClass TrafficClass) {
	for _, entry := range appendProviderEntries(s.byProvider[providerID]) {
		if entry.assignment.Generation == generation && entry.assignment.Key.Model == model &&
			entry.assignment.Key.TrafficClass == trafficClass {
			s.removeAssignmentLocked(entry)
		}
	}
}

func appendAssignmentEntries(entries map[StickyKey]*assignmentEntry) []*assignmentEntry {
	result := make([]*assignmentEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result
}

func appendProviderEntries(entries map[StickyKey]*assignmentEntry) []*assignmentEntry {
	result := make([]*assignmentEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result
}

func (s *Scheduler) setCursorLocked(key roundRobinKey, providerID string) {
	cursor := s.cursors[key]
	cursor.ProviderID = providerID
	cursor.Version++
	s.cursors[key] = cursor
}

func (s *Scheduler) clearCursorLocked(key roundRobinKey) {
	cursor := s.cursors[key]
	cursor.ProviderID = ""
	cursor.Version++
	s.cursors[key] = cursor
}

func (s *Scheduler) clearProviderCursorsLocked(providerID string) {
	for key, cursor := range s.cursors {
		if cursor.ProviderID == providerID {
			s.clearCursorLocked(key)
		}
	}
}

func (s *Scheduler) clearChannelCursorLocked(providerID, model string, trafficClass TrafficClass) {
	for key, cursor := range s.cursors {
		if cursor.ProviderID == providerID && key.Model == model && key.TrafficClass == trafficClass {
			s.clearCursorLocked(key)
		}
	}
}
