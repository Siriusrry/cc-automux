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
	"github.com/Siriusrry/cc-automux/internal/traffic"
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
	Model       string
	RequestType traffic.RequestType
	Priority    int64
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
	version    uint64
}

// pendingMigration is retained only across the remainder of a request's
// fallback chain when a health transition removes the source assignment before
// the replacement has succeeded. A later independent request clears it when it
// starts a new RequestSelection.
type pendingMigration struct {
	ProviderID string
	Generation ProviderGeneration
	CreatedAt  time.Time
	LastUsedAt time.Time
	Version    uint64
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
	pending     map[StickyKey]pendingMigration
	nextVersion uint64
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
		pending:     make(map[StickyKey]pendingMigration),
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
	return StaticActive
}

func (s *Scheduler) Acquire(snapshot Snapshot, key StickyKey, request *RequestSelection) (AttemptLease, error) {
	if snapshot == nil || request == nil {
		return AttemptLease{}, fmt.Errorf("%w: snapshot and request selection are required", ErrInvalidSchedulingKey)
	}
	key.SessionID = strings.TrimSpace(key.SessionID)
	if key.Model == "" || (key.RequestType != traffic.RequestTypeNormal && key.RequestType != traffic.RequestTypeClassifier) {
		return AttemptLease{}, ErrInvalidSchedulingKey
	}
	if err := request.policy.Validate(); err != nil {
		return AttemptLease{}, fmt.Errorf("%w: %w", ErrInvalidSchedulingKey, err)
	}
	if !request.HasBudget() {
		return AttemptLease{}, ErrAttemptBudgetExhausted
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	currentSnapshot := s.revision == 0 || snapshot.Revision() == s.revision
	first := !request.initialized
	var assigned *assignmentEntry
	if currentSnapshot && key.SessionID != "" {
		assigned = s.assignments[key]
	}
	if first {
		items := eligibleCandidates(snapshot.Candidates(key.Model), key.Model)
		var assignedProvider *provider.CompiledProvider
		if currentSnapshot {
			// A new request cannot revive a removed assignment from an older chain.
			delete(s.pending, key)
			if assigned != nil {
				assignedProvider = findCandidate(items, assigned.assignment.ProviderID, assigned.assignment.Generation)
				if assignedProvider == nil {
					s.removeAssignmentLocked(assigned)
					assigned = nil
				}
			}
		}
		if assigned != nil {
			request.source = migrationSource(assigned)
		}
		request.ordered = s.orderCandidatesLocked(items, key, assignedProvider)
		request.visited = make(map[string]bool, len(items))
		request.initialized = true
	}

	var earliestRetry time.Time
	for start := 0; start < len(request.ordered); {
		end := start + 1
		for end < len(request.ordered) && request.ordered[end].Priority == request.ordered[start].Priority {
			end++
		}
		group := request.ordered[start:end]
		blocked := false
		// Inspect unvisited targets first, then revisit this tier. Never skip a
		// tier merely because its providers have already served this request.
		for pass := 0; pass < 2; pass++ {
			for _, item := range group {
				if request.visited[item.ID] != (pass == 1) {
					continue
				}
				decision := s.health.Acquire(HealthKey{ProviderID: item.ID, Generation: item.Generation, Model: key.Model, RequestType: key.RequestType}, item.DisableHealth)
				if !decision.Available {
					if decision.RetryAt != nil && (earliestRetry.IsZero() || decision.RetryAt.Before(earliestRetry)) {
						earliestRetry = *decision.RetryAt
					}
					cooling := decision.GlobalState == GlobalCooldown || decision.ChannelState == ChannelCooldown
					// An occupied half-open probe blocks lower tiers without taking
					// another lease or waiting for its owner.
					blocked = blocked || !cooling
					continue
				}
				if pass == 1 {
					for _, member := range group {
						delete(request.visited, member.ID)
					}
				}
				request.visited[item.ID] = true
				fromSticky := assigned != nil && item.ID == assigned.assignment.ProviderID && item.Generation == assigned.assignment.Generation
				allocate := first && assigned == nil && request.source.ProviderID == "" && currentSnapshot
				if allocate && key.SessionID != "" {
					s.putAssignmentLocked(key, item, now)
					assigned = s.assignments[key]
					request.source = migrationSource(assigned)
				}
				if fromSticky {
					s.touchAssignmentLocked(assigned, now)
				}
				source := request.source
				migration := source.ProviderID != "" && (item.ID != source.ProviderID || item.Generation != source.Generation)
				cursorKey := roundRobinKey{Model: key.Model, RequestType: key.RequestType, Priority: item.Priority}
				cursor := s.cursors[cursorKey]
				halfOpen := decision.Lease.GlobalProbe || decision.Lease.ChannelProbe
				advanceCursor := first && !fromSticky && currentSnapshot
				if advanceCursor && !halfOpen {
					s.setCursorLocked(cursorKey, item.ID)
				}
				return AttemptLease{
					SnapshotRevision: snapshot.Revision(), Provider: item, Model: key.Model,
					RequestType: key.RequestType, Generation: item.Generation,
					FromSticky: fromSticky, HalfOpenProbe: halfOpen, HealthLease: decision.Lease,
					stickyKey: key, stickyMigration: migration,
					stickySourceProviderID: source.ProviderID, stickySourceGeneration: source.Generation,
					stickySourceVersion: source.Version, cursorKey: cursorKey, cursorVersion: cursor.Version,
					advanceCursorOnSuccess: advanceCursor && halfOpen,
				}, nil
			}
		}
		if blocked {
			return AttemptLease{}, &UnavailableError{RetryAt: earliestRetry}
		}
		start = end
	}
	return AttemptLease{}, &UnavailableError{RetryAt: earliestRetry}
}

func migrationSource(entry *assignmentEntry) pendingMigration {
	return pendingMigration{ProviderID: entry.assignment.ProviderID, Generation: entry.assignment.Generation,
		CreatedAt: entry.assignment.CreatedAt, LastUsedAt: entry.assignment.LastUsedAt, Version: entry.version}
}

func (s *Scheduler) Report(lease AttemptLease, outcome Outcome) (HealthUpdate, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())

	// A lease from an older published revision may still finish after a
	// reconcile. Consume it without applying the result to the current health
	// state; late results must not mutate the new scheduler/health view.
	if lease.Provider != nil {
		current, ok := s.providers[lease.Provider.ID]
		stale := lease.SnapshotRevision != 0 && s.revision != 0 && lease.SnapshotRevision != s.revision
		if !ok || current.Static != StaticActive || current.Provider.Generation != lease.Generation ||
			!current.Provider.SupportsModel(lease.Model) || stale {
			return s.health.Report(lease.HealthLease, Outcome{Class: FailureClientCanceled})
		}
	}
	update, observation := s.health.Report(lease.HealthLease, outcome)
	if lease.Provider == nil {
		return update, observation
	}

	if lease.advanceCursorOnSuccess && outcome.Class == FailureNone {
		cursor := s.cursors[lease.cursorKey]
		if cursor.Version == lease.cursorVersion {
			s.setCursorLocked(lease.cursorKey, lease.Provider.ID)
		}
	}

	// A fallback lease may have been acquired while the session still pointed
	// at an earlier provider. On a successful replacement, move that
	// assignment atomically, regardless of the source provider's health mode.
	if outcome.Class == FailureNone && lease.stickyMigration {
		s.migrateAssignmentLocked(lease)
	}
	if !lease.Provider.DisableHealth && update.GlobalEnteredCooldown {
		s.removeProviderAssignmentsLocked(lease.Provider.ID, lease.Generation)
		s.clearProviderCursorsLocked(lease.Provider.ID)
	} else if !lease.Provider.DisableHealth && update.ChannelEnteredCooldown {
		s.removeChannelAssignmentsLocked(lease.Provider.ID, lease.Generation, lease.Model, lease.RequestType)
		s.clearChannelCursorLocked(lease.Provider.ID, lease.Model, lease.RequestType)
	}
	return update, observation
}

// migrateAssignmentLocked replaces a session assignment only when it still
// refers to the source observed by Acquire. This compare-and-swap-like check
// prevents a late fallback result from overwriting a newer assignment.
func (s *Scheduler) migrateAssignmentLocked(lease AttemptLease) {
	if lease.Provider == nil || lease.stickyKey.SessionID == "" ||
		lease.stickySourceProviderID == "" {
		return
	}
	pending, hasPending := s.pending[lease.stickyKey]
	if hasPending && (pending.ProviderID != lease.stickySourceProviderID || pending.Generation != lease.stickySourceGeneration || pending.Version != lease.stickySourceVersion) {
		return
	}
	entry := s.assignments[lease.stickyKey]
	if entry != nil {
		if entry.assignment.ProviderID != lease.stickySourceProviderID ||
			entry.assignment.Generation != lease.stickySourceGeneration || entry.version != lease.stickySourceVersion {
			return
		}
	} else if !hasPending {
		// A late fallback result must not create affinity after its source
		// assignment has been replaced or otherwise invalidated.
		return
	}
	if entry == nil {
		// Health cooldown removed the source assignment. Create the replacement
		// only after its request has reported success.
		createdAt := pending.CreatedAt
		if createdAt.IsZero() {
			createdAt = s.now()
		}
		entry = &assignmentEntry{assignment: Assignment{
			Key:        lease.stickyKey,
			ProviderID: lease.Provider.ID,
			Generation: lease.Provider.Generation,
			CreatedAt:  createdAt,
			LastUsedAt: s.now(),
		}, version: s.allocateAssignmentVersionLocked()}
		for len(s.assignments) >= s.policy.StickyCapacity {
			oldest, _ := s.lru.Front().Value.(*assignmentEntry)
			s.removeAssignmentLocked(oldest)
		}
		entry.element = s.lru.PushBack(entry)
		s.assignments[lease.stickyKey] = entry
		index := s.byProvider[lease.Provider.ID]
		if index == nil {
			index = make(map[StickyKey]*assignmentEntry)
			s.byProvider[lease.Provider.ID] = index
		}
		index[lease.stickyKey] = entry
		delete(s.pending, lease.stickyKey)
		return
	}
	oldProviderID := entry.assignment.ProviderID
	delete(s.byProvider[oldProviderID], lease.stickyKey)
	if len(s.byProvider[oldProviderID]) == 0 {
		delete(s.byProvider, oldProviderID)
	}
	entry.assignment.ProviderID = lease.Provider.ID
	entry.assignment.Generation = lease.Provider.Generation
	entry.version = s.allocateAssignmentVersionLocked()
	s.touchAssignmentLocked(entry, s.now())
	index := s.byProvider[lease.Provider.ID]
	if index == nil {
		index = make(map[StickyKey]*assignmentEntry)
		s.byProvider[lease.Provider.ID] = index
	}
	index[lease.stickyKey] = entry
	delete(s.pending, lease.stickyKey)
}

func (s *Scheduler) markPendingEntryLocked(entry *assignmentEntry) {
	if entry == nil || entry.assignment.Key.SessionID == "" {
		return
	}
	if s.pending == nil {
		s.pending = make(map[StickyKey]pendingMigration)
	}
	s.pending[entry.assignment.Key] = pendingMigration{
		ProviderID: entry.assignment.ProviderID,
		Generation: entry.assignment.Generation,
		CreatedAt:  entry.assignment.CreatedAt,
		LastUsedAt: entry.assignment.LastUsedAt,
		Version:    entry.version,
	}
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
	for key, pending := range s.pending {
		state, ok := next[pending.ProviderID]
		if !ok || state.Static != StaticActive || state.Provider.Generation != pending.Generation || !state.Provider.SupportsModel(key.Model) {
			delete(s.pending, key)
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
		return result[i].Key.RequestType < result[j].Key.RequestType
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
		cursor := s.cursors[roundRobinKey{Model: key.Model, RequestType: key.RequestType, Priority: group[0].Priority}]
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
	}, version: s.allocateAssignmentVersionLocked()}
	delete(s.pending, key)
	entry.element = s.lru.PushBack(entry)
	s.assignments[key] = entry
	index := s.byProvider[item.ID]
	if index == nil {
		index = make(map[StickyKey]*assignmentEntry)
		s.byProvider[item.ID] = index
	}
	index[key] = entry
}

func (s *Scheduler) allocateAssignmentVersionLocked() uint64 {
	for {
		s.nextVersion++
		if s.nextVersion != 0 {
			return s.nextVersion
		}
	}
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
			break
		}
		s.removeAssignmentLocked(entry)
	}
	for key, pending := range s.pending {
		lastUsed := pending.LastUsedAt
		if lastUsed.IsZero() {
			lastUsed = pending.CreatedAt
		}
		if lastUsed.IsZero() || !now.Before(lastUsed.Add(s.policy.StickyTTL)) {
			delete(s.pending, key)
		}
	}
}

func (s *Scheduler) removeProviderAssignmentsLocked(providerID string, generation ProviderGeneration) {
	for _, entry := range appendProviderEntries(s.byProvider[providerID]) {
		if entry.assignment.Generation == generation {
			s.markPendingEntryLocked(entry)
			s.removeAssignmentLocked(entry)
		}
	}
}

func (s *Scheduler) removeChannelAssignmentsLocked(providerID string, generation ProviderGeneration, model string, requestType traffic.RequestType) {
	for _, entry := range appendProviderEntries(s.byProvider[providerID]) {
		if entry.assignment.Generation == generation && entry.assignment.Key.Model == model &&
			entry.assignment.Key.RequestType == requestType {
			s.markPendingEntryLocked(entry)
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

func (s *Scheduler) clearChannelCursorLocked(providerID, model string, requestType traffic.RequestType) {
	for key, cursor := range s.cursors {
		if cursor.ProviderID == providerID && key.Model == model && key.RequestType == requestType {
			s.clearCursorLocked(key)
		}
	}
}

// UpdateError forwards a diagnostic-only update for the same published lease.
func (s *Scheduler) UpdateError(lease AttemptLease, observation uint64, raw string, incomplete, truncated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease.SnapshotRevision != 0 && s.revision != lease.SnapshotRevision {
		return
	}
	if updater, ok := s.health.(HealthErrorUpdater); ok {
		updater.UpdateError(lease.HealthLease, observation, raw, incomplete, truncated)
	}
}

var _ ErrorUpdater = (*Scheduler)(nil)
