package health

import (
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// Clock supplies time to the health state machine.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

type machineState uint8

const (
	stateUnknown machineState = iota
	stateHealthy
	stateDegraded
	stateCooldown
	stateHalfOpen
	stateDisabled
)

type stateEntry struct {
	state               machineState
	backoffLevel        int
	consecutiveFailures int
	observedFailures    uint64
	lastHealthFailure   time.Time
	cooldownUntil       time.Time
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	probeToken          uint64
	lastUpstreamURL     string
	lastError           string
	lastSessionID       string
	activeLeases        int
}

type channelKey struct {
	model       string
	requestType traffic.RequestType
}

type scopeKey struct {
	providerID string
	generation scheduler.ProviderGeneration
}

type retiredKey struct {
	scopeKey
	disableHealth bool
}

type providerScope struct {
	key           scopeKey
	disableHealth bool
	active        bool
	models        []string
	modelSet      map[string]struct{}
	global        stateEntry
	channels      map[channelKey]*stateEntry
	activeLeases  int
}

type leaseRecord struct {
	scope        *providerScope
	key          scheduler.HealthKey
	globalProbe  bool
	channelProbe bool
	disabled     bool
}

// Store is a concurrency-safe in-memory health controller.
type Store struct {
	mu        sync.Mutex
	policy    scheduler.Policy
	clock     Clock
	active    map[string]*providerScope
	retired   map[retiredKey][]*providerScope
	leases    map[uint64]leaseRecord
	nextToken uint64
}

// New constructs a health store using an explicit policy and clock.
func New(policy scheduler.Policy, clock Clock) (*Store, error) {
	if policy == (scheduler.Policy{}) {
		policy = scheduler.DefaultPolicy()
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &Store{
		policy:  policy,
		clock:   clock,
		active:  make(map[string]*providerScope),
		retired: make(map[retiredKey][]*providerScope),
		leases:  make(map[uint64]leaseRecord),
	}, nil
}

// NewDefault constructs the production health store.
func NewDefault() *Store {
	store, err := New(scheduler.DefaultPolicy(), ClockFunc(time.Now))
	if err != nil {
		panic(err)
	}
	return store
}

func newScope(p *provider.CompiledProvider) *providerScope {
	disabled := p.DisableHealth
	scope := &providerScope{
		key: scopeKey{
			providerID: p.ID,
			generation: p.Generation,
		},
		disableHealth: disabled,
		active:        true,
		models:        append([]string(nil), p.Models...),
		modelSet:      make(map[string]struct{}, len(p.Models)),
		channels:      make(map[channelKey]*stateEntry),
	}
	scope.global.state = initialState(disabled)
	for _, model := range p.Models {
		scope.modelSet[model] = struct{}{}
		key := channelKey{model: model, requestType: traffic.RequestTypeNormal}
		scope.channels[key] = &stateEntry{state: initialState(disabled)}
	}
	return scope
}

func newRetiredScope(key scheduler.HealthKey, disableHealth bool) *providerScope {
	scope := &providerScope{
		key: scopeKey{
			providerID: key.ProviderID,
			generation: key.Generation,
		},
		disableHealth: disableHealth,
		modelSet:      map[string]struct{}{key.Model: {}},
		models:        []string{key.Model},
		channels:      make(map[channelKey]*stateEntry),
	}
	scope.global.state = initialState(disableHealth)
	scope.channels[channelKey{model: key.Model, requestType: key.RequestType}] = &stateEntry{state: initialState(disableHealth)}
	return scope
}

func initialState(disabled bool) machineState {
	if disabled {
		return stateDisabled
	}
	return stateUnknown
}

// Reconcile installs the current provider generations while isolating leases
// already issued for retired generations.
func (s *Store) Reconcile(providers []*provider.CompiledProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]struct{}, len(providers))
	for _, item := range providers {
		if item == nil {
			continue
		}
		seen[item.ID] = struct{}{}
		current := s.active[item.ID]
		if current != nil && current.key.generation == item.Generation && current.disableHealth == item.DisableHealth {
			s.reconcileModelsLocked(current, item.Models)
			continue
		}
		if current != nil {
			s.retireLocked(current)
		}
		fresh := newScope(item)
		if current != nil && current.key.generation == item.Generation {
			copyScopeDiagnostics(fresh, current)
		}
		s.active[item.ID] = fresh
	}
	for id, current := range s.active {
		if _, ok := seen[id]; ok {
			continue
		}
		delete(s.active, id)
		s.retireLocked(current)
	}
}

func copyScopeDiagnostics(target, source *providerScope) {
	copyDiagnostics(&target.global, &source.global)
	for key, sourceEntry := range source.channels {
		if _, configured := target.modelSet[key.model]; !configured {
			continue
		}
		targetEntry := target.channels[key]
		if targetEntry == nil {
			// Classifier and future request-type channels are created lazily. Preserve
			// them across a disable_health toggle just like pre-created normal
			// channels, while resetting all breaker state to the target mode.
			targetEntry = &stateEntry{state: initialState(target.disableHealth)}
			target.channels[key] = targetEntry
		}
		copyDiagnostics(targetEntry, sourceEntry)
	}
}

func copyDiagnostics(target, source *stateEntry) {
	target.observedFailures = source.observedFailures
	target.lastSuccessAt = source.lastSuccessAt
	target.lastFailureAt = source.lastFailureAt
	target.lastUpstreamURL = source.lastUpstreamURL
	target.lastError = source.lastError
	target.lastSessionID = source.lastSessionID
}

func (s *Store) reconcileModelsLocked(scope *providerScope, models []string) {
	scope.models = append(scope.models[:0], models...)
	scope.modelSet = make(map[string]struct{}, len(models))
	for _, model := range models {
		scope.modelSet[model] = struct{}{}
		key := channelKey{model: model, requestType: traffic.RequestTypeNormal}
		if _, ok := scope.channels[key]; !ok {
			scope.channels[key] = &stateEntry{state: initialState(scope.disableHealth)}
		}
	}
	for key, entry := range scope.channels {
		if _, ok := scope.modelSet[key.model]; !ok && entry.activeLeases == 0 {
			delete(scope.channels, key)
		}
	}
}

func (s *Store) retireLocked(scope *providerScope) {
	scope.active = false
	if scope.activeLeases == 0 {
		return
	}
	key := retiredKey{scopeKey: scope.key, disableHealth: scope.disableHealth}
	for _, existing := range s.retired[key] {
		if existing == scope {
			return
		}
	}
	s.retired[key] = append(s.retired[key], scope)
}

// Acquire atomically checks composed health and reserves any required probes.
func (s *Store) Acquire(key scheduler.HealthKey, disableHealth bool) scheduler.HealthDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	if key.ProviderID == "" || key.Generation == "" || key.Model == "" || key.RequestType == "" {
		return scheduler.HealthDecision{
			GlobalState:  scheduler.GlobalUnknown,
			ChannelState: scheduler.ChannelUnknown,
		}
	}
	scope := s.scopeForAcquireLocked(key, disableHealth)
	entryKey := channelKey{model: key.Model, requestType: key.RequestType}
	channel := scope.channels[entryKey]
	if channel == nil {
		channel = &stateEntry{state: initialState(scope.disableHealth)}
		scope.channels[entryKey] = channel
	}

	now := s.clock.Now()
	advanceCooldown(&scope.global, now)
	advanceCooldown(channel, now)
	globalAvailable, globalProbe := available(&scope.global)
	channelAvailable, channelProbe := available(channel)
	decision := scheduler.HealthDecision{
		Available:    globalAvailable && channelAvailable,
		GlobalState:  globalState(scope.global.state),
		ChannelState: channelState(channel.state),
		RetryAt:      blockingRetryAt(now, &scope.global, channel),
	}
	if !decision.Available {
		return decision
	}

	token := s.allocateTokenLocked()
	if globalProbe {
		scope.global.probeToken = token
	}
	if channelProbe {
		channel.probeToken = token
	}
	scope.activeLeases++
	channel.activeLeases++
	record := leaseRecord{
		scope:        scope,
		key:          key,
		globalProbe:  globalProbe,
		channelProbe: channelProbe,
		disabled:     scope.disableHealth,
	}
	s.leases[token] = record
	decision.Lease = scheduler.HealthLease{
		Key:          key,
		GlobalProbe:  globalProbe,
		ChannelProbe: channelProbe,
		Disabled:     scope.disableHealth,
		Token:        token,
	}
	decision.RetryAt = nil
	return decision
}

func (s *Store) scopeForAcquireLocked(key scheduler.HealthKey, disableHealth bool) *providerScope {
	if current := s.active[key.ProviderID]; current != nil && current.key.generation == key.Generation && current.disableHealth == disableHealth {
		return current
	}
	lookup := retiredKey{
		scopeKey: scopeKey{
			providerID: key.ProviderID,
			generation: key.Generation,
		},
		disableHealth: disableHealth,
	}
	if scopes := s.retired[lookup]; len(scopes) > 0 {
		return scopes[len(scopes)-1]
	}
	scope := newRetiredScope(key, disableHealth)
	s.retired[lookup] = append(s.retired[lookup], scope)
	return scope
}

func (s *Store) allocateTokenLocked() uint64 {
	for {
		s.nextToken++
		if s.nextToken == 0 {
			continue
		}
		if _, exists := s.leases[s.nextToken]; !exists {
			return s.nextToken
		}
	}
}

func advanceCooldown(entry *stateEntry, now time.Time) {
	if entry.state != stateCooldown || now.Before(entry.cooldownUntil) {
		return
	}
	entry.state = stateHalfOpen
	entry.cooldownUntil = time.Time{}
	entry.probeToken = 0
}

func available(entry *stateEntry) (bool, bool) {
	switch entry.state {
	case stateCooldown:
		return false, false
	case stateHalfOpen:
		return entry.probeToken == 0, entry.probeToken == 0
	default:
		return true, false
	}
}

func blockingRetryAt(now time.Time, entries ...*stateEntry) *time.Time {
	var latest time.Time
	for _, entry := range entries {
		if entry.state != stateCooldown || !entry.cooldownUntil.After(now) {
			continue
		}
		if latest.IsZero() || entry.cooldownUntil.After(latest) {
			latest = entry.cooldownUntil
		}
	}
	return timePointer(latest)
}

// Report consumes a lease exactly once and applies the outcome to its isolated
// provider generation.
func (s *Store) Report(lease scheduler.HealthLease, outcome scheduler.Outcome) scheduler.HealthUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.leases[lease.Token]
	if !ok {
		return scheduler.HealthUpdate{}
	}
	delete(s.leases, lease.Token)
	scope := record.scope
	entryKey := channelKey{model: record.key.Model, requestType: record.key.RequestType}
	channel := scope.channels[entryKey]
	if channel == nil || lease.Key != record.key ||
		lease.GlobalProbe != record.globalProbe ||
		lease.ChannelProbe != record.channelProbe ||
		lease.Disabled != record.disabled {
		releaseProbe(&scope.global, lease.Token)
		releaseProbe(channel, lease.Token)
		s.releaseLeaseLocked(scope, entryKey, channel)
		return scheduler.HealthUpdate{}
	}
	if !scope.active {
		update := scheduler.HealthUpdate{
			GlobalState:  globalState(scope.global.state),
			ChannelState: channelState(channel.state),
		}
		// A generation or health-mode change may retire a scope while one of
		// its half-open probes is still in flight. The late result is isolated
		// from current state, but its single-use lease must still release the
		// retired probe token so another old-snapshot request cannot remain
		// blocked behind a probe that has already completed.
		releaseProbe(&scope.global, lease.Token)
		releaseProbe(channel, lease.Token)
		s.releaseLeaseLocked(scope, entryKey, channel)
		return update
	}

	now := s.clock.Now()
	if outcome.ClientCanceled {
		outcome.Class = scheduler.FailureClientCanceled
	}
	update := scheduler.HealthUpdate{}
	switch outcome.Class {
	case scheduler.FailureNone:
		observeLatest(&scope.global, outcome)
		observeLatest(channel, outcome)
		applySuccess(&scope.global, now)
		applySuccess(channel, now)
	case scheduler.FailureGlobalImmediate:
		observeLatest(&scope.global, outcome)
		update.GlobalEnteredCooldown = s.applyFailureLocked(&scope.global, lease.Token, now, outcome, true)
		releaseProbe(channel, lease.Token)
	case scheduler.FailureGlobalTransient:
		observeLatest(&scope.global, outcome)
		update.GlobalEnteredCooldown = s.applyFailureLocked(&scope.global, lease.Token, now, outcome, false)
		releaseProbe(channel, lease.Token)
	case scheduler.FailureChannelImmediate:
		observeLatest(channel, outcome)
		update.ChannelEnteredCooldown = s.applyFailureLocked(channel, lease.Token, now, outcome, true)
		releaseProbe(&scope.global, lease.Token)
	case scheduler.FailureChannelTransient, scheduler.FailureChannelStream:
		observeLatest(channel, outcome)
		update.ChannelEnteredCooldown = s.applyFailureLocked(channel, lease.Token, now, outcome, false)
		releaseProbe(&scope.global, lease.Token)
	case scheduler.FailureNeutral:
		// A neutral upstream HTTP response is a model/request observation only.
		// It must not change any Provider-global diagnostic or breaker field.
		if outcome.HTTPStatus > 0 {
			observeNeutralFailure(channel, outcome, now)
		}
		releaseProbe(&scope.global, lease.Token)
		releaseProbe(channel, lease.Token)
	default:
		// Local preparation errors, client cancellation, and downstream write
		// failures do not describe Provider health.
		releaseProbe(&scope.global, lease.Token)
		releaseProbe(channel, lease.Token)
	}

	update.GlobalState = globalState(scope.global.state)
	update.ChannelState = channelState(channel.state)
	if update.GlobalEnteredCooldown {
		update.CooldownUntil = timePointer(scope.global.cooldownUntil)
	} else if update.ChannelEnteredCooldown {
		update.CooldownUntil = timePointer(channel.cooldownUntil)
	}
	s.releaseLeaseLocked(scope, entryKey, channel)
	return update
}

func observeLatest(entry *stateEntry, outcome scheduler.Outcome) {
	if entry == nil {
		return
	}
	entry.lastUpstreamURL = outcome.UpstreamURL
	entry.lastSessionID = outcome.SessionID
	// An empty upstream error body is still the latest complete observation.
	// Overwrite an older diagnostic instead of leaving stale text attached to
	// the new status/result.
	entry.lastError = outcome.RawError
}

func observeNeutralFailure(entry *stateEntry, outcome scheduler.Outcome, now time.Time) {
	if entry == nil {
		return
	}
	entry.lastUpstreamURL = outcome.UpstreamURL
	entry.lastSessionID = outcome.SessionID
	// An empty response body is still the latest observed error text and must
	// replace an older diagnostic rather than leaving stale text behind.
	entry.lastError = outcome.RawError
	recordNeutralFailure(entry, now)
}

func recordNeutralFailure(entry *stateEntry, now time.Time) {
	if entry.observedFailures != ^uint64(0) {
		entry.observedFailures++
	}
	entry.lastFailureAt = now
}

func applySuccess(entry *stateEntry, now time.Time) {
	if entry.state == stateDisabled {
		entry.lastSuccessAt = now
		return
	}
	entry.state = stateHealthy
	entry.backoffLevel = 0
	entry.consecutiveFailures = 0
	entry.lastHealthFailure = time.Time{}
	entry.cooldownUntil = time.Time{}
	entry.lastSuccessAt = now
	entry.probeToken = 0
}

func (s *Store) applyFailureLocked(entry *stateEntry, token uint64, now time.Time, outcome scheduler.Outcome, immediate bool) bool {
	if entry.observedFailures != ^uint64(0) {
		entry.observedFailures++
	}
	entry.lastFailureAt = now
	if entry.state == stateDisabled {
		return false
	}
	if entry.state == stateCooldown {
		return false
	}
	if entry.state == stateHalfOpen {
		if entry.probeToken != token {
			return false
		}
		entry.probeToken = 0
		recordConsecutiveFailure(entry, now, s.policy.FailureWindow)
		if entry.backoffLevel < len(s.policy.Cooldowns)-1 {
			entry.backoffLevel++
		}
		s.enterCooldownLocked(entry, now, outcome)
		return true
	}

	recordConsecutiveFailure(entry, now, s.policy.FailureWindow)
	if immediate || entry.consecutiveFailures >= s.policy.FailureThreshold {
		s.enterCooldownLocked(entry, now, outcome)
		return true
	}
	entry.state = stateDegraded
	return false
}

func recordConsecutiveFailure(entry *stateEntry, now time.Time, window time.Duration) {
	gap := now.Sub(entry.lastHealthFailure)
	if entry.lastHealthFailure.IsZero() || gap < 0 || gap > window {
		entry.consecutiveFailures = 1
	} else if entry.consecutiveFailures < int(^uint(0)>>1) {
		entry.consecutiveFailures++
	}
	entry.lastHealthFailure = now
}

func (s *Store) enterCooldownLocked(entry *stateEntry, now time.Time, outcome scheduler.Outcome) {
	duration := s.policy.EffectiveCooldown(entry.backoffLevel, outcome.RetryAfter, outcome.HasRetryAfter)
	entry.state = stateCooldown
	entry.cooldownUntil = now.Add(duration)
	entry.probeToken = 0
}

func releaseProbe(entry *stateEntry, token uint64) {
	if entry == nil {
		return
	}
	if entry.state == stateHalfOpen && entry.probeToken == token {
		entry.probeToken = 0
	}
}

func (s *Store) releaseLeaseLocked(scope *providerScope, key channelKey, channel *stateEntry) {
	if scope.activeLeases > 0 {
		scope.activeLeases--
	}
	if channel != nil && channel.activeLeases > 0 {
		channel.activeLeases--
	}
	if channel != nil {
		if _, configured := scope.modelSet[key.model]; !configured && channel.activeLeases == 0 {
			delete(scope.channels, key)
		}
	}
	if scope.active || scope.activeLeases != 0 {
		return
	}
	lookup := retiredKey{scopeKey: scope.key, disableHealth: scope.disableHealth}
	if scopes := s.retired[lookup]; len(scopes) > 0 {
		for i := len(scopes) - 1; i >= 0; i-- {
			if scopes[i] != scope {
				continue
			}
			scopes = append(scopes[:i], scopes[i+1:]...)
			if len(scopes) == 0 {
				delete(s.retired, lookup)
			} else {
				s.retired[lookup] = scopes
			}
			break
		}
	}
}

// EarliestRetry finds the first future cooldown deadline among the supplied
// current or isolated generation keys without acquiring a probe.
func (s *Store) EarliestRetry(keys []scheduler.HealthKey) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	var earliest time.Time
	for _, key := range keys {
		for _, scope := range s.scopesForKeyLocked(key) {
			entry := scope.channels[channelKey{model: key.Model, requestType: key.RequestType}]
			readyAt := composedRetryAt(now, &scope.global, entry)
			if !readyAt.IsZero() && (earliest.IsZero() || readyAt.Before(earliest)) {
				earliest = readyAt
			}
		}
	}
	return earliest, !earliest.IsZero()
}

func composedRetryAt(now time.Time, global, channel *stateEntry) time.Time {
	var readyAt time.Time
	for _, entry := range []*stateEntry{global, channel} {
		if entry == nil || entry.state != stateCooldown || !entry.cooldownUntil.After(now) {
			continue
		}
		if readyAt.IsZero() || entry.cooldownUntil.After(readyAt) {
			readyAt = entry.cooldownUntil
		}
	}
	return readyAt
}

func (s *Store) scopesForKeyLocked(key scheduler.HealthKey) []*providerScope {
	result := make([]*providerScope, 0, 3)
	if current := s.active[key.ProviderID]; current != nil && current.key.generation == key.Generation {
		result = append(result, current)
	}
	for retiredKey, scopes := range s.retired {
		if retiredKey.providerID != key.ProviderID || retiredKey.generation != key.Generation {
			continue
		}
		for _, scope := range scopes {
			if scope != nil {
				result = append(result, scope)
			}
		}
	}
	return result
}

var _ scheduler.HealthController = (*Store)(nil)
