package scheduler

import (
	"errors"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

const (
	providerA = "11111111-1111-4111-8111-111111111111"
	providerB = "22222222-2222-4222-8222-222222222222"
	providerC = "33333333-3333-4333-8333-333333333333"
	providerD = "44444444-4444-4444-8444-444444444444"
)

type fakeSnapshot struct {
	revision  uint64
	attempts  AttemptPolicy
	providers []*provider.CompiledProvider
}

type countingAttemptSnapshot struct {
	*fakeSnapshot
	calls int
}

type typedAttemptSnapshot struct {
	*fakeSnapshot
	normal     AttemptPolicy
	classifier AttemptPolicy
}

func (s *typedAttemptSnapshot) NormalAttemptPolicy() AttemptPolicy { return s.normal }

func (s *typedAttemptSnapshot) ClassifierAttemptPolicy() AttemptPolicy { return s.classifier }

func (s *countingAttemptSnapshot) AttemptPolicy() AttemptPolicy {
	s.calls++
	return s.fakeSnapshot.AttemptPolicy()
}

func (s *fakeSnapshot) Revision() uint64   { return s.revision }
func (s *fakeSnapshot) GatewayKey() string { return "gateway-key" }
func (s *fakeSnapshot) AttemptPolicy() AttemptPolicy {
	if s.attempts == (AttemptPolicy{}) {
		return DefaultAttemptPolicy()
	}
	return s.attempts
}

func (s *fakeSnapshot) Candidates(model string) []*provider.CompiledProvider {
	result := make([]*provider.CompiledProvider, 0, len(s.providers))
	for _, item := range s.providers {
		if item.SupportsModel(model) {
			result = append(result, item.Clone())
		}
	}
	return result
}

func (s *fakeSnapshot) Providers() []*provider.CompiledProvider {
	result := make([]*provider.CompiledProvider, len(s.providers))
	for i, item := range s.providers {
		result[i] = item.Clone()
	}
	return result
}

type fakeHealth struct {
	mu                 sync.Mutex
	decisions          map[HealthKey]HealthDecision
	updates            map[HealthKey]HealthUpdate
	retryAt            time.Time
	earliestRetryCalls int
	acquired           []HealthKey
	reports            []HealthLease
	reconciled         [][]ProviderGeneration
}

func newFakeHealth() *fakeHealth {
	return &fakeHealth{
		decisions: make(map[HealthKey]HealthDecision),
		updates:   make(map[HealthKey]HealthUpdate),
	}
}

func (h *fakeHealth) Reconcile(items []*provider.CompiledProvider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	generations := make([]ProviderGeneration, len(items))
	for i, item := range items {
		generations[i] = item.Generation
	}
	h.reconciled = append(h.reconciled, generations)
}

func (h *fakeHealth) Acquire(key HealthKey, disabled bool) HealthDecision {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acquired = append(h.acquired, key)
	if disabled {
		return HealthDecision{
			Available:    true,
			GlobalState:  GlobalDisabled,
			ChannelState: ChannelDisabled,
			Lease:        HealthLease{Key: key, Disabled: true},
		}
	}
	if decision, ok := h.decisions[key]; ok {
		decision.Lease.Key = key
		return decision
	}
	return HealthDecision{
		Available:    true,
		GlobalState:  GlobalHealthy,
		ChannelState: ChannelHealthy,
		Lease:        HealthLease{Key: key},
	}
}

func (h *fakeHealth) Report(lease HealthLease, _ Outcome) HealthUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reports = append(h.reports, lease)
	return h.updates[lease.Key]
}

func (h *fakeHealth) EarliestRetry(keys []HealthKey) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.earliestRetryCalls++
	if len(keys) == 0 || h.retryAt.IsZero() {
		return time.Time{}, false
	}
	return h.retryAt, true
}

func compileProvider(t *testing.T, id, name string, models []string, priority int64) *provider.CompiledProvider {
	t.Helper()
	item, err := provider.Compile(config.ProviderConfig{
		ID:       id,
		Name:     name,
		BaseURL:  "https://" + name + ".example/anthropic",
		APIKey:   "key-" + name,
		Models:   append([]string(nil), models...),
		Priority: priority,
		Enabled:  true,
	}, testProviderRuntimeContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func compileProviderConfig(t *testing.T, input config.ProviderConfig) *provider.CompiledProvider {
	t.Helper()
	item, err := provider.Compile(input, testProviderRuntimeContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func testProviderRuntimeContext(t *testing.T) provider.RuntimeContext {
	t.Helper()
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func newTestScheduler(t *testing.T, health HealthController, policy Policy, now func() time.Time) *Scheduler {
	t.Helper()
	selector, err := New(health, Options{Policy: policy, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return selector
}

func normalKey(session, model string) StickyKey {
	return StickyKey{SessionID: session, Model: model, RequestType: traffic.RequestTypeNormal}
}

func providerID(t *testing.T, lease AttemptLease, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if lease.Provider == nil {
		t.Fatal("lease has no provider")
	}
	return lease.Provider.ID
}

func TestStaticAvailabilityOf(t *testing.T) {
	active := compileProvider(t, providerA, "active", []string{"Model"}, 0)
	if got := StaticAvailabilityOf(active); got != StaticActive {
		t.Fatalf("active availability = %q", got)
	}

	disabled := active.Clone()
	disabled.Enabled = false
	if got := StaticAvailabilityOf(disabled); got != StaticDisabledProvider {
		t.Fatalf("disabled availability = %q", got)
	}
	missingKey := active.Clone()
	missingKey.APIKey = ""
	if got := StaticAvailabilityOf(missingKey); got != StaticDisabledProvider {
		t.Fatalf("missing-key availability = %q", got)
	}

	empty := compileProviderConfig(t, config.ProviderConfig{
		ID: providerB, Name: "empty", BaseURL: "https://empty.example", Enabled: true, Models: []string{},
	})
	if got := StaticAvailabilityOf(empty); got != StaticNoModels {
		t.Fatalf("empty-model availability = %q", got)
	}

	patched := compileProviderConfig(t, config.ProviderConfig{
		ID: providerC, Name: "patched", BaseURL: "https://patched.example", APIKey: "key",
		Enabled: true, Models: []string{"Model"}, Patches: []string{patch.AnyRouterSubagentThinkingID},
	})
	if got := StaticAvailabilityOf(patched); got != StaticActive {
		t.Fatalf("executable-patch availability = %q", got)
	}
}

func TestPriorityRoundRobinStickyAndExactModel(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerD, "low", []string{"Model"}, math.MinInt64),
		compileProvider(t, providerA, "a", []string{"Model"}, math.MaxInt64),
		compileProvider(t, providerB, "b", []string{"Model"}, math.MaxInt64),
		compileProvider(t, providerC, "case", []string{"model"}, math.MaxInt64),
	}}
	selector.Reconcile(snapshot)

	lease1, err := selector.Acquire(snapshot, normalKey(" session-1 ", "Model"), nil)
	if got := providerID(t, lease1, err); got != providerA {
		t.Fatalf("first provider = %s", got)
	}
	if lease1.FromSticky {
		t.Fatal("first assignment marked sticky")
	}
	sticky, err := selector.Acquire(snapshot, normalKey("session-1", "Model"), nil)
	if got := providerID(t, sticky, err); got != providerA || !sticky.FromSticky {
		t.Fatalf("sticky lease = provider %s, sticky %v", got, sticky.FromSticky)
	}
	lease2, err := selector.Acquire(snapshot, normalKey("session-2", "Model"), nil)
	if got := providerID(t, lease2, err); got != providerB {
		t.Fatalf("second provider = %s", got)
	}
	lease3, err := selector.Acquire(snapshot, normalKey("", "Model"), nil)
	if got := providerID(t, lease3, err); got != providerA {
		t.Fatalf("sessionless provider = %s", got)
	}
	lease4, err := selector.Acquire(snapshot, normalKey("", "Model"), nil)
	if got := providerID(t, lease4, err); got != providerB {
		t.Fatalf("second sessionless provider = %s", got)
	}
	if selector.ActiveAssignmentCount() != 2 {
		t.Fatalf("assignment count = %d", selector.ActiveAssignmentCount())
	}

	caseLease, err := selector.Acquire(snapshot, normalKey("case-session", "model"), nil)
	if got := providerID(t, caseLease, err); got != providerC {
		t.Fatalf("case-sensitive provider = %s", got)
	}
	_, err = selector.Acquire(snapshot, normalKey("missing", "MODEL"), nil)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("missing exact model error = %v", err)
	}
}

func TestExecutablePatchCandidatesRemainEligible(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	patched := compileProviderConfig(t, config.ProviderConfig{
		ID: providerA, Name: "patched", BaseURL: "https://patched.example", APIKey: "key",
		Enabled: true, Models: []string{"m"}, Priority: 100, Patches: []string{patch.AnyRouterSubagentThinkingID},
	})
	active := compileProvider(t, providerB, "active", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{patched, active}}
	selector.Reconcile(snapshot)
	lease, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
	if got := providerID(t, lease, err); got != providerA {
		t.Fatalf("selected provider = %s", got)
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	if len(health.acquired) != 1 || health.acquired[0].ProviderID != providerA {
		t.Fatalf("health acquisitions = %#v", health.acquired)
	}
}

func TestInvalidSchedulingInputsAndPolicy(t *testing.T) {
	health := newFakeHealth()
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil health controller accepted")
	}
	invalidPolicy := DefaultPolicy()
	invalidPolicy.StickyCapacity = 0
	if _, err := New(health, Options{Policy: invalidPolicy}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	selector := newTestScheduler(t, health, Policy{}, nil)
	snapshot := &fakeSnapshot{revision: 1}
	for _, key := range []StickyKey{
		{RequestType: traffic.RequestTypeNormal},
		{Model: "m"},
		{Model: "m", RequestType: "unknown"},
	} {
		if _, err := selector.Acquire(snapshot, key, nil); !errors.Is(err, ErrInvalidSchedulingKey) {
			t.Fatalf("key %#v error = %v", key, err)
		}
	}
	if _, err := selector.Acquire(nil, normalKey("session", "m"), nil); !errors.Is(err, ErrInvalidSchedulingKey) {
		t.Fatalf("nil snapshot error = %v", err)
	}
}

func TestAttemptOrderAndMaximumDistinctProviders(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 10),
		compileProvider(t, providerB, "b", []string{"m"}, 10),
		compileProvider(t, providerC, "c", []string{"m"}, 0),
		compileProvider(t, providerD, "d", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	key := normalKey("sticky", "m")

	first, err := selector.Acquire(snapshot, key, nil)
	if got := providerID(t, first, err); got != providerA {
		t.Fatalf("first provider = %s", got)
	}
	second, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if got := providerID(t, second, err); got != providerB {
		t.Fatalf("same-priority fallback = %s", got)
	}
	third, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}, providerB: {}})
	if got := providerID(t, third, err); got != providerC {
		t.Fatalf("lower-priority fallback = %s", got)
	}
	_, err = selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}, providerB: {}, providerC: {}})
	if !errors.Is(err, ErrAttemptBudgetExhausted) {
		t.Fatalf("attempt budget error = %v", err)
	}

	assignments := selector.Assignments(providerA)
	if len(assignments) != 1 || assignments[0].Key != key {
		t.Fatalf("isolated fallback changed assignment: %#v", assignments)
	}
}

func TestAttemptBudgetComesFromRequestSnapshot(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, DefaultPolicy(), nil)
	snapshot := &fakeSnapshot{
		revision: 1,
		attempts: AttemptPolicy{MaxAttempts: 2},
		providers: []*provider.CompiledProvider{
			compileProvider(t, providerA, "a", []string{"m"}, 0),
			compileProvider(t, providerB, "b", []string{"m"}, 0),
			compileProvider(t, providerC, "c", []string{"m"}, 0),
		},
	}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")
	if _, err := selector.Acquire(snapshot, key, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}}); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}, providerB: {}}); !errors.Is(err, ErrAttemptBudgetExhausted) {
		t.Fatalf("snapshot attempt budget error = %v", err)
	}
}

func TestClassifierAcquireUsesTypedRequestBudget(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, DefaultPolicy(), nil)
	snapshot := &typedAttemptSnapshot{
		fakeSnapshot: &fakeSnapshot{
			revision: 1,
			// The legacy ordinary budget is intentionally larger than the
			// classifier budget; classifier selection must not read it.
			attempts: AttemptPolicy{MaxAttempts: 3},
			providers: []*provider.CompiledProvider{
				compileProvider(t, providerA, "a", []string{"m"}, 0),
				compileProvider(t, providerB, "b", []string{"m"}, 0),
			},
		},
		normal:     AttemptPolicy{MaxAttempts: 3},
		classifier: AttemptPolicy{MaxAttempts: 1},
	}
	selector.Reconcile(snapshot)
	key := StickyKey{SessionID: "classifier-session", Model: "m", RequestType: traffic.RequestTypeClassifier}
	first, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Provider.ID; got != providerA {
		t.Fatalf("first classifier provider = %s", got)
	}
	_, err = selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if !errors.Is(err, ErrAttemptBudgetExhausted) {
		t.Fatalf("classifier budget error = %v", err)
	}
}

func TestAcquireWithPolicyUsesExplicitExecutionPlanBudget(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, DefaultPolicy(), nil)
	snapshot := &fakeSnapshot{revision: 1, attempts: AttemptPolicy{MaxAttempts: 3}, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
		compileProvider(t, providerB, "b", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	key := StickyKey{SessionID: "classifier-session", Model: "m", RequestType: traffic.RequestTypeClassifier}
	first, err := selector.AcquireWithPolicy(snapshot, key, nil, AttemptPolicy{MaxAttempts: 1})
	if err != nil || first.Provider == nil {
		t.Fatalf("explicit classifier acquire = %#v, %v", first, err)
	}
	_, err = selector.AcquireWithPolicy(snapshot, key, map[string]struct{}{first.Provider.ID: {}}, AttemptPolicy{MaxAttempts: 1})
	if !errors.Is(err, ErrAttemptBudgetExhausted) {
		t.Fatalf("explicit budget error = %v", err)
	}
	if _, err := selector.AcquireWithPolicy(snapshot, key, nil, AttemptPolicy{}); !errors.Is(err, ErrInvalidSchedulingKey) {
		t.Fatalf("invalid explicit budget error = %v", err)
	}
}

func TestClassifierAcquireFailsClosedWithoutTypedBudget(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, DefaultPolicy(), nil)
	snapshot := &fakeSnapshot{revision: 1, attempts: AttemptPolicy{MaxAttempts: 3}, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	key := StickyKey{SessionID: "classifier-session", Model: "m", RequestType: traffic.RequestTypeClassifier}
	_, err := selector.Acquire(snapshot, key, nil)
	if !errors.Is(err, ErrInvalidSchedulingKey) || !errors.Is(err, ErrRequestAttemptPolicyUnavailable) {
		t.Fatalf("untyped classifier acquire error = %v", err)
	}
}

func TestCaptureAttemptPolicyFreezesOneSnapshotRead(t *testing.T) {
	source := &countingAttemptSnapshot{fakeSnapshot: &fakeSnapshot{
		revision: 1,
		attempts: AttemptPolicy{MaxAttempts: 5},
	}}
	captured, policy, err := CaptureAttemptPolicy(source)
	if err != nil || policy.MaxAttempts != 5 {
		t.Fatalf("CaptureAttemptPolicy() = %#v, %v", policy, err)
	}
	for i := 0; i < 3; i++ {
		got, err := ResolveAttemptPolicy(captured)
		if err != nil || got != policy {
			t.Fatalf("captured policy = %#v, %v", got, err)
		}
	}
	if source.calls != 1 {
		t.Fatalf("source AttemptPolicy() calls = %d", source.calls)
	}
}

func TestCaptureAttemptPolicyPreservesTypedClassifierBudget(t *testing.T) {
	source := &typedAttemptSnapshot{
		fakeSnapshot: &fakeSnapshot{revision: 1, attempts: AttemptPolicy{MaxAttempts: 3}},
		normal:       AttemptPolicy{MaxAttempts: 3},
		classifier:   AttemptPolicy{MaxAttempts: 1},
	}
	captured, normal, err := CaptureAttemptPolicy(source)
	if err != nil || normal.MaxAttempts != 3 {
		t.Fatalf("CaptureAttemptPolicy() = %#v, %v", normal, err)
	}
	got, err := ResolveRequestAttemptPolicy(captured, traffic.RequestTypeClassifier)
	if err != nil || got.MaxAttempts != 1 {
		t.Fatalf("captured classifier policy = %#v, %v", got, err)
	}
}

func TestConcurrentFirstAssignmentIsSingleAndStable(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
		compileProvider(t, providerB, "b", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)

	const workers = 128
	results := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := selector.Acquire(snapshot, normalKey("one-session", "m"), nil)
			if err != nil {
				errs <- err
				return
			}
			results <- lease.Provider.ID
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for id := range results {
		if id != providerA {
			t.Fatalf("concurrent assignment used %s", id)
		}
	}
	if selector.ActiveAssignmentCount() != 1 {
		t.Fatalf("assignment count = %d", selector.ActiveAssignmentCount())
	}
	assignments := selector.Assignments(providerA)
	if len(assignments) != 1 || assignments[0].Key.SessionID != "one-session" {
		t.Fatalf("assignments = %#v", assignments)
	}
}

func TestAssignmentsAreReturnedDeterministically(t *testing.T) {
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, func() time.Time { return now })
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	for _, session := range []string{"z", "a", "m"} {
		if _, err := selector.Acquire(snapshot, normalKey(session, "m"), nil); err != nil {
			t.Fatal(err)
		}
	}
	assignments := selector.Assignments(providerA)
	sessions := make([]string, len(assignments))
	for i := range assignments {
		sessions[i] = assignments[i].Key.SessionID
	}
	if !sort.StringsAreSorted(sessions) {
		t.Fatalf("assignment order = %v", sessions)
	}
}

func TestStickyTTLAndLRUEviction(t *testing.T) {
	now := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	policy.StickyCapacity = 2
	health := newFakeHealth()
	selector := newTestScheduler(t, health, policy, func() time.Time { return now })
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
		compileProvider(t, providerB, "b", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)

	if _, err := selector.Acquire(snapshot, normalKey("s1", "m"), nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := selector.Acquire(snapshot, normalKey("s2", "m"), nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := selector.Acquire(snapshot, normalKey("s1", "m"), nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := selector.Acquire(snapshot, normalKey("s3", "m"), nil); err != nil {
		t.Fatal(err)
	}

	assignments := selector.Assignments("")
	got := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		got[assignment.Key.SessionID] = true
	}
	if len(got) != 2 || !got["s1"] || !got["s3"] || got["s2"] {
		t.Fatalf("LRU survivors = %#v", got)
	}

	now = now.Add(policy.StickyTTL)
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("assignment count after TTL = %d", got)
	}
}

func TestStickyTTLRefreshesFromLastAccess(t *testing.T) {
	now := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	health := newFakeHealth()
	selector := newTestScheduler(t, health, policy, func() time.Time { return now })
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	if _, err := selector.Acquire(snapshot, normalKey("sliding", "m"), nil); err != nil {
		t.Fatal(err)
	}
	// A hit just before the original deadline must move the deadline forward.
	now = now.Add(policy.StickyTTL - time.Minute)
	lease, err := selector.Acquire(snapshot, normalKey("sliding", "m"), nil)
	if err != nil || !lease.FromSticky {
		t.Fatalf("refreshing sticky lease = %#v, %v", lease, err)
	}
	now = now.Add(2 * time.Minute)
	if got := selector.ActiveAssignmentCount(); got != 1 {
		t.Fatalf("assignment expired despite recent access: %d", got)
	}
	now = now.Add(policy.StickyTTL)
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("assignment survived sliding TTL: %d", got)
	}
}

func TestDefaultStickyCapacityIsEnforcedAt8192(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, func() time.Time { return now })
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{
		compileProvider(t, providerA, "a", []string{"m"}, 0),
	}}
	selector.Reconcile(snapshot)
	for i := 0; i <= DefaultPolicy().StickyCapacity; i++ {
		session := "session-" + time.Unix(int64(i), 0).UTC().Format("150405.000000000")
		if _, err := selector.Acquire(snapshot, normalKey(session, "m"), nil); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Nanosecond)
	}
	if got := selector.ActiveAssignmentCount(); got != 8192 {
		t.Fatalf("assignment count = %d", got)
	}
	for _, assignment := range selector.Assignments(providerA) {
		if assignment.Key.SessionID == "session-000000.000000000" {
			t.Fatal("oldest assignment was not evicted")
		}
	}
}

func TestHigherPriorityDegradationAndRecovery(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	high := compileProvider(t, providerA, "high", []string{"m"}, 10)
	low := compileProvider(t, providerB, "low", []string{"m"}, -10)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{low, high}}
	selector.Reconcile(snapshot)
	highKey := HealthKey{ProviderID: high.ID, Generation: high.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	health.decisions[highKey] = HealthDecision{
		Available: false, GlobalState: GlobalHealthy, ChannelState: ChannelCooldown,
		Lease: HealthLease{Key: highKey},
	}

	lowLease, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
	if got := providerID(t, lowLease, err); got != providerB {
		t.Fatalf("degraded provider = %s", got)
	}
	if assignments := selector.Assignments(providerB); len(assignments) != 1 {
		t.Fatalf("low-priority assignments = %#v", assignments)
	}

	health.mu.Lock()
	delete(health.decisions, highKey)
	health.mu.Unlock()
	recovered, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
	if got := providerID(t, recovered, err); got != providerA || recovered.FromSticky {
		t.Fatalf("recovered lease = provider %s, sticky %v", got, recovered.FromSticky)
	}
	if len(selector.Assignments(providerB)) != 0 || len(selector.Assignments(providerA)) != 1 {
		t.Fatalf("assignments after recovery: high=%#v low=%#v", selector.Assignments(providerA), selector.Assignments(providerB))
	}
}

func TestUnavailableErrorCarriesEarliestRetry(t *testing.T) {
	health := newFakeHealth()
	health.retryAt = time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	selector := newTestScheduler(t, health, Policy{}, nil)
	item := compileProvider(t, providerA, "a", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{item}}
	selector.Reconcile(snapshot)
	key := HealthKey{ProviderID: item.ID, Generation: item.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	retryAt := health.retryAt
	health.decisions[key] = HealthDecision{
		Available: false, GlobalState: GlobalCooldown, ChannelState: ChannelHealthy,
		RetryAt: &retryAt,
		Lease:   HealthLease{Key: key},
	}

	_, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("unavailable error = %v", err)
	}
	timed, ok := err.(interface{ RetryAtTime() (time.Time, bool) })
	if !ok {
		t.Fatalf("error does not expose retry time: %T", err)
	}
	if retryAt, present := timed.RetryAtTime(); !present || !retryAt.Equal(health.retryAt) {
		t.Fatalf("retry time = %v, %v", retryAt, present)
	}
	health.mu.Lock()
	deferredRetryCalls := health.earliestRetryCalls
	health.mu.Unlock()
	if deferredRetryCalls != 0 {
		t.Fatalf("Acquire re-queried EarliestRetry for an old scope: %d calls", deferredRetryCalls)
	}
}

func TestUnavailableErrorUsesEarliestCurrentHealthDecision(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 1)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	keyA := HealthKey{ProviderID: a.ID, Generation: a.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	keyB := HealthKey{ProviderID: b.ID, Generation: b.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	later := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Minute)
	health.decisions[keyA] = HealthDecision{Available: false, GlobalState: GlobalCooldown, RetryAt: &later, Lease: HealthLease{Key: keyA}}
	health.decisions[keyB] = HealthDecision{Available: false, GlobalState: GlobalCooldown, RetryAt: &earlier, Lease: HealthLease{Key: keyB}}

	_, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("unavailable error = %v", err)
	}
	timed, ok := err.(interface{ RetryAtTime() (time.Time, bool) })
	if !ok {
		t.Fatalf("error does not expose retry time: %T", err)
	}
	if got, present := timed.RetryAtTime(); !present || !got.Equal(earlier) {
		t.Fatalf("retry time = %v, %v; want %v", got, present, earlier)
	}
	health.mu.Lock()
	deferredRetryCalls := health.earliestRetryCalls
	health.mu.Unlock()
	if deferredRetryCalls != 0 {
		t.Fatalf("Acquire re-queried EarliestRetry: %d calls", deferredRetryCalls)
	}
}

func TestHalfOpenSelectionDefersCursorWithoutOverwritingNewerRotation(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	c := compileProvider(t, providerC, "c", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b, c}}
	selector.Reconcile(snapshot)
	aKey := HealthKey{ProviderID: a.ID, Generation: a.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	bKey := HealthKey{ProviderID: b.ID, Generation: b.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
	health.decisions[aKey] = HealthDecision{Available: false, GlobalState: GlobalCooldown, ChannelState: ChannelHealthy}
	health.decisions[bKey] = HealthDecision{
		Available: true, GlobalState: GlobalHalfOpen, ChannelState: ChannelHealthy,
		Lease: HealthLease{Key: bKey, GlobalProbe: true},
	}

	probe, err := selector.Acquire(snapshot, normalKey("probe", "m"), nil)
	if got := providerID(t, probe, err); got != providerB || !probe.HalfOpenProbe {
		t.Fatalf("half-open lease = provider %s probe %v", got, probe.HalfOpenProbe)
	}
	health.decisions[bKey] = HealthDecision{Available: false, GlobalState: GlobalHalfOpen, ChannelState: ChannelHealthy}
	concurrent, err := selector.Acquire(snapshot, normalKey("other", "m"), nil)
	if got := providerID(t, concurrent, err); got != providerC {
		t.Fatalf("selection while probe is in flight = %s", got)
	}

	selector.Report(probe, Outcome{Class: FailureNone, SessionID: "probe"})
	delete(health.decisions, bKey)
	next, err := selector.Acquire(snapshot, normalKey("next", "m"), nil)
	if got := providerID(t, next, err); got != providerB {
		t.Fatalf("late probe success overwrote newer cursor: selected %s", got)
	}
}

func TestCooldownUpdatesRemoveOnlyAffectedAssignments(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	item := compileProvider(t, providerA, "a", []string{"m", "n"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{item}}
	selector.Reconcile(snapshot)
	mLease, err := selector.Acquire(snapshot, normalKey("m-session", "m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	nLease, err := selector.Acquire(snapshot, normalKey("n-session", "n"), nil)
	if err != nil {
		t.Fatal(err)
	}

	health.updates[mLease.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(mLease, Outcome{Class: FailureChannelImmediate, SessionID: "m-session"})
	assignments := selector.Assignments(providerA)
	if len(assignments) != 1 || assignments[0].Key.SessionID != "n-session" {
		t.Fatalf("channel cooldown assignments = %#v", assignments)
	}

	health.updates[nLease.HealthLease.Key] = HealthUpdate{GlobalEnteredCooldown: true, GlobalState: GlobalCooldown}
	selector.Report(nLease, Outcome{Class: FailureGlobalImmediate, SessionID: "n-session"})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("global cooldown assignment count = %d", got)
	}
}

func TestCooldownFallbackCreatesAssignmentOnlyAfterSuccess(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	failed, err := selector.Acquire(snapshot, key, nil)
	if got := providerID(t, failed, err); got != providerA {
		t.Fatalf("initial provider = %s", got)
	}
	health.updates[failed.HealthLease.Key] = HealthUpdate{
		ChannelEnteredCooldown: true,
		ChannelState:           ChannelCooldown,
	}
	selector.Report(failed, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("source assignment survived cooldown: %d", got)
	}

	fallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if got := providerID(t, fallback, err); got != providerB {
		t.Fatalf("fallback provider = %s", got)
	}
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("fallback assignment created before success: %d", got)
	}
	selector.Report(fallback, Outcome{Class: FailureNone, SessionID: key.SessionID})
	assignments := selector.Assignments(providerB)
	if len(assignments) != 1 || assignments[0].Key != key {
		t.Fatalf("successful fallback assignment = %#v", assignments)
	}
}

func TestCooldownFallbackFailureDoesNotCreateAssignment(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	failed, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.updates[failed.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(failed, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	fallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if got := providerID(t, fallback, err); got != providerB {
		t.Fatalf("fallback provider = %s", got)
	}
	selector.Report(fallback, Outcome{Class: FailureChannelTransient, SessionID: key.SessionID})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("failed fallback created assignment: %d", got)
	}
}

func TestCooldownRemovesAssignmentsButPreservesPendingMigrationForEverySession(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	// A has the higher priority so both sessions initially bind to it. B is
	// still an eligible fallback for the same model.
	a := compileProvider(t, providerA, "a", []string{"m"}, 10)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)

	s1, err := selector.Acquire(snapshot, normalKey("s1", "m"), nil)
	if err != nil || s1.Provider.ID != providerA {
		t.Fatalf("s1 initial lease = %#v, %v", s1, err)
	}
	s2, err := selector.Acquire(snapshot, normalKey("s2", "m"), nil)
	if err != nil || s2.Provider.ID != providerA {
		t.Fatalf("s2 initial lease = %#v, %v", s2, err)
	}

	// S2 has already acquired its replacement while its A assignment still
	// exists. A later global cooldown removes both A assignments; both source
	// identities must remain available for their respective migrations.
	s2Fallback, err := selector.Acquire(snapshot, normalKey("s2", "m"), map[string]struct{}{providerA: {}})
	if err != nil || s2Fallback.Provider.ID != providerB {
		t.Fatalf("s2 fallback lease = %#v, %v", s2Fallback, err)
	}
	health.updates[s1.HealthLease.Key] = HealthUpdate{GlobalEnteredCooldown: true, GlobalState: GlobalCooldown}
	selector.Report(s1, Outcome{Class: FailureGlobalImmediate, SessionID: "s1"})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("assignments after global cooldown = %d", got)
	}

	selector.Report(s2Fallback, Outcome{Class: FailureNone, SessionID: "s2"})
	assignments := selector.Assignments(providerB)
	if len(assignments) != 1 || assignments[0].Key != normalKey("s2", "m") {
		t.Fatalf("s2 migration assignment = %#v", assignments)
	}
}

func TestLateFallbackSuccessCannotOverwriteNewerAssignment(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	c := compileProvider(t, providerC, "c", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b, c}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	failed, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.updates[failed.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(failed, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	aKey := failed.HealthLease.Key
	health.decisions[aKey] = HealthDecision{Available: false, GlobalState: GlobalHealthy, ChannelState: ChannelCooldown}

	late, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if got := providerID(t, late, err); got != providerB {
		t.Fatalf("late fallback provider = %s", got)
	}
	bKey := late.HealthLease.Key
	health.decisions[bKey] = HealthDecision{Available: false, GlobalState: GlobalHealthy, ChannelState: ChannelCooldown}
	newer, err := selector.Acquire(snapshot, key, nil)
	if got := providerID(t, newer, err); got != providerC {
		t.Fatalf("newer provider = %s", got)
	}
	selector.Report(newer, Outcome{Class: FailureNone, SessionID: key.SessionID})
	selector.Report(late, Outcome{Class: FailureNone, SessionID: key.SessionID})

	assignments := selector.Assignments(providerC)
	if len(assignments) != 1 || assignments[0].Key != key || len(selector.Assignments(providerB)) != 0 {
		t.Fatalf("late result overwrote newer assignment: B=%#v C=%#v", selector.Assignments(providerB), assignments)
	}
}

func TestPendingMigrationExpiresWithStickyTTL(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	health := newFakeHealth()
	selector := newTestScheduler(t, health, policy, func() time.Time { return now })
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	failed, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.updates[failed.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(failed, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	if len(selector.pending) != 1 {
		t.Fatalf("pending migrations = %#v", selector.pending)
	}
	now = now.Add(policy.StickyTTL)
	fallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if got := providerID(t, fallback, err); got != providerB {
		t.Fatalf("fallback provider = %s", got)
	}
	selector.Report(fallback, Outcome{Class: FailureNone, SessionID: key.SessionID})
	if len(selector.pending) != 0 || selector.ActiveAssignmentCount() != 0 {
		t.Fatalf("expired source created affinity: pending=%#v assignments=%#v", selector.pending, selector.Assignments(""))
	}
}

func TestPendingMigrationExpiresBeforeLateReport(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()
	health := newFakeHealth()
	selector := newTestScheduler(t, health, policy, func() time.Time { return now })
	a := compileProvider(t, providerA, "a", []string{"m"}, 10)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	initial, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.updates[initial.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(initial, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	fallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if err != nil || fallback.Provider.ID != providerB {
		t.Fatalf("fallback lease = %#v, %v", fallback, err)
	}

	// No intervening Scheduler read occurs. Report itself must expire the
	// pending tombstone before applying a late success, otherwise it revives an
	// assignment whose source affinity has exceeded the sliding TTL.
	now = now.Add(policy.StickyTTL + time.Nanosecond)
	selector.Report(fallback, Outcome{Class: FailureNone, SessionID: key.SessionID})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("late success revived expired assignment: %d", got)
	}
}

func TestFreshRequestClearsPendingMigrationEvenWhenNoCandidateIsAvailable(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 10)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")
	initial, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.updates[initial.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(initial, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	fallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if err != nil || fallback.Provider.ID != providerB {
		t.Fatalf("fallback lease = %#v, %v", fallback, err)
	}

	// A new request begins while both candidates are unavailable. Its empty
	// exclusion set must invalidate the old request's pending tombstone even
	// though no replacement assignment can be selected in this call.
	health.decisions[fallback.HealthLease.Key] = HealthDecision{
		Available:    false,
		GlobalState:  GlobalHealthy,
		ChannelState: ChannelCooldown,
	}
	health.decisions[initial.HealthLease.Key] = HealthDecision{
		Available:    false,
		GlobalState:  GlobalHealthy,
		ChannelState: ChannelCooldown,
	}
	if _, err := selector.Acquire(snapshot, key, nil); !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("fresh unavailable request error = %v", err)
	}
	delete(health.decisions, fallback.HealthLease.Key)
	delete(health.decisions, initial.HealthLease.Key)
	selector.Report(fallback, Outcome{Class: FailureNone, SessionID: key.SessionID})
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("cleared pending migration revived assignment: %d", got)
	}
}

func TestSameGenerationLateFallbackCannotPassAssignmentABA(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 10)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)
	key := normalKey("session", "m")

	initial, err := selector.Acquire(snapshot, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldFallback, err := selector.Acquire(snapshot, key, map[string]struct{}{providerA: {}})
	if err != nil || oldFallback.Provider.ID != providerB {
		t.Fatalf("old fallback lease = %#v, %v", oldFallback, err)
	}

	// Remove A through a real health cooldown, then make the same-generation A
	// candidate available again and start a fresh request. This creates a new
	// A assignment with the same provider ID/generation (an ABA cycle), but a
	// newer internal assignment version.
	health.updates[initial.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(initial, Outcome{Class: FailureChannelImmediate, SessionID: key.SessionID})
	delete(health.decisions, initial.HealthLease.Key)
	// The fake health controller returns the configured update for every lease;
	// clear the one-shot cooldown before the replacement A request succeeds.
	health.updates[initial.HealthLease.Key] = HealthUpdate{}
	fresh, err := selector.Acquire(snapshot, key, nil)
	if err != nil || fresh.Provider.ID != providerA {
		t.Fatalf("fresh A lease = %#v, %v", fresh, err)
	}
	selector.Report(fresh, Outcome{Class: FailureNone, SessionID: key.SessionID})

	// The old B success must not migrate the replacement A assignment. A
	// provider ID/generation-only comparison would incorrectly accept it.
	selector.Report(oldFallback, Outcome{Class: FailureNone, SessionID: key.SessionID})
	assignments := selector.Assignments("")
	if len(assignments) != 1 || assignments[0].ProviderID != providerA {
		t.Fatalf("same-generation ABA assignment = %#v", assignments)
	}
}

func TestLateLeaseCannotRebindAfterStaticOrModelInvalidation(t *testing.T) {
	cases := []struct {
		name   string
		update func(*provider.CompiledProvider)
	}{
		{name: "disabled", update: func(item *provider.CompiledProvider) { item.Enabled = false }},
		{name: "model removed", update: func(item *provider.CompiledProvider) { item.Models = []string{"other"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			health := newFakeHealth()
			selector := newTestScheduler(t, health, Policy{}, nil)
			a := compileProvider(t, providerA, "a", []string{"m"}, 10)
			b := compileProvider(t, providerB, "b", []string{"m"}, 0)
			first := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
			selector.Reconcile(first)
			key := normalKey("session", "m")
			initial, err := selector.Acquire(first, key, nil)
			if err != nil {
				t.Fatal(err)
			}
			fallback, err := selector.Acquire(first, key, map[string]struct{}{providerA: {}})
			if err != nil || fallback.Provider.ID != providerB {
				t.Fatalf("fallback lease = %#v, %v", fallback, err)
			}

			updatedB := b.Clone()
			tc.update(updatedB)
			second := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{a, updatedB}}
			selector.Reconcile(second)
			selector.Report(fallback, Outcome{Class: FailureNone, SessionID: key.SessionID})

			assignments := selector.Assignments("")
			if len(assignments) != 1 || assignments[0].ProviderID != providerA {
				t.Fatalf("late invalid lease changed assignments = %#v", assignments)
			}
			// The source lease is intentionally left in flight; reporting it after
			// the reconcile must not create a second assignment or alter B state.
			selector.Report(initial, Outcome{Class: FailureNone, SessionID: key.SessionID})
			if got := selector.ActiveAssignmentCount(); got != 1 {
				t.Fatalf("assignment count after source report = %d", got)
			}
		})
	}
}

func TestDisableHealthFailureKeepsAssignmentUntilSuccessfulMigration(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	a.DisableHealth = true
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	b.DisableHealth = true
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(snapshot)

	failed, err := selector.Acquire(snapshot, normalKey("s1", "m"), nil)
	if got := providerID(t, failed, err); got != providerA {
		t.Fatalf("s1 provider = %s", got)
	}
	second, err := selector.Acquire(snapshot, normalKey("s2", "m"), nil)
	if got := providerID(t, second, err); got != providerB {
		t.Fatalf("s2 provider = %s", got)
	}
	third, err := selector.Acquire(snapshot, normalKey("s3", "m"), nil)
	if got := providerID(t, third, err); got != providerA {
		t.Fatalf("s3 provider = %s", got)
	}

	selector.Report(failed, Outcome{Class: FailureChannelTransient, SessionID: "s1"})
	aAssignments := selector.Assignments(providerA)
	if len(aAssignments) != 2 {
		t.Fatalf("provider A assignments after failure = %#v", aAssignments)
	}
	if len(selector.Assignments(providerB)) != 1 {
		t.Fatalf("provider B assignment changed: %#v", selector.Assignments(providerB))
	}

	retry, err := selector.Acquire(snapshot, normalKey("s1", "m"), map[string]struct{}{providerA: {}})
	if got := providerID(t, retry, err); got != providerB {
		t.Fatalf("failed session fallback = %s", got)
	}
	selector.Report(retry, Outcome{Class: FailureNone, SessionID: "s1"})
	if assignments := selector.Assignments(providerA); len(assignments) != 1 || assignments[0].Key.SessionID != "s3" {
		t.Fatalf("provider A assignments after migration = %#v", assignments)
	}
	if assignments := selector.Assignments(providerB); len(assignments) != 2 {
		t.Fatalf("provider B assignments after migration = %#v", assignments)
	}
	afterRound, err := selector.Acquire(snapshot, normalKey("s4", "m"), nil)
	if got := providerID(t, afterRound, err); got != providerB {
		t.Fatalf("new session did not continue round-robin after migration: %s", got)
	}
}

func TestDisableHealthStreamFailureKeepsAssignmentAfterResponseStarts(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	item := compileProvider(t, providerA, "a", []string{"m"}, 0)
	item.DisableHealth = true
	snapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{item}}
	selector.Reconcile(snapshot)
	lease, err := selector.Acquire(snapshot, normalKey("stream-session", "m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	selector.Report(lease, Outcome{
		Class:           FailureChannelStream,
		SessionID:       "stream-session",
		ResponseStarted: true,
	})
	if assignments := selector.Assignments(providerA); len(assignments) != 1 || assignments[0].Key.SessionID != "stream-session" {
		t.Fatalf("stream failure changed disabled-health assignment: %#v", assignments)
	}
}

func TestReconcilePrunesAssignmentsAndInvalidCursors(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	b := compileProvider(t, providerB, "b", []string{"m"}, 0)
	c := compileProvider(t, providerC, "c", []string{"m"}, 0)
	first := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b, c}}
	selector.Reconcile(first)
	if _, err := selector.Acquire(first, normalKey("s1", "m"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.Acquire(first, normalKey("s2", "m"), nil); err != nil {
		t.Fatal(err)
	}

	second := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{a, c}}
	selector.Reconcile(second)
	if len(selector.Assignments(providerB)) != 0 {
		t.Fatalf("removed provider assignments = %#v", selector.Assignments(providerB))
	}
	lease, err := selector.Acquire(second, normalKey("s3", "m"), nil)
	if got := providerID(t, lease, err); got != providerA {
		t.Fatalf("selection after cursor provider removal = %s", got)
	}

	withoutModelConfig := a.Config()
	withoutModelConfig.Models = []string{"other"}
	withoutModel := compileProviderConfig(t, withoutModelConfig)
	third := &fakeSnapshot{revision: 3, providers: []*provider.CompiledProvider{withoutModel, c}}
	selector.Reconcile(third)
	if len(selector.Assignments(providerA)) != 0 {
		t.Fatalf("model-removed assignments = %#v", selector.Assignments(providerA))
	}
	if _, err := selector.Acquire(third, normalKey("s4", "m"), nil); err != nil {
		t.Fatal(err)
	}
	disabled := c.Clone()
	disabled.Enabled = false
	fourth := &fakeSnapshot{revision: 4, providers: []*provider.CompiledProvider{withoutModel, disabled}}
	selector.Reconcile(fourth)
	if len(selector.Assignments(providerC)) != 0 {
		t.Fatalf("disabled provider assignments = %#v", selector.Assignments(providerC))
	}
}

func TestReconcilePreservesIdentityCompatibleAssignments(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"m"}, 0)
	first := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a}}
	selector.Reconcile(first)
	if _, err := selector.Acquire(first, normalKey("session", "m"), nil); err != nil {
		t.Fatal(err)
	}

	updated := a.Clone()
	updated.Name = "renamed"
	updated.Priority = math.MinInt64
	updated.DisableHealth = true
	second := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{updated}}
	selector.Reconcile(second)
	assignments := selector.Assignments(providerA)
	if len(assignments) != 1 || assignments[0].Generation != updated.Generation {
		t.Fatalf("compatible update assignments = %#v", assignments)
	}
}

func TestOldGenerationReportCannotRemoveNewAssignment(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	oldProvider := compileProvider(t, providerA, "a", []string{"m"}, 0)
	oldSnapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{oldProvider}}
	selector.Reconcile(oldSnapshot)
	oldLease, err := selector.Acquire(oldSnapshot, normalKey("session", "m"), nil)
	if err != nil {
		t.Fatal(err)
	}

	newConfig := oldProvider.Config()
	newConfig.APIKey = "replacement-key"
	newProvider := compileProviderConfig(t, newConfig)
	if newProvider.Generation == oldProvider.Generation {
		t.Fatal("test provider generation did not change")
	}
	newSnapshot := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{newProvider}}
	selector.Reconcile(newSnapshot)
	if _, err := selector.Acquire(newSnapshot, normalKey("session", "m"), nil); err != nil {
		t.Fatal(err)
	}
	health.updates[oldLease.HealthLease.Key] = HealthUpdate{GlobalEnteredCooldown: true, GlobalState: GlobalCooldown}
	selector.Report(oldLease, Outcome{Class: FailureGlobalImmediate, SessionID: "session"})

	assignments := selector.Assignments(providerA)
	if len(assignments) != 1 || assignments[0].Generation != newProvider.Generation {
		t.Fatalf("new assignment after old report = %#v", assignments)
	}
}

func TestOldSnapshotAcquireDoesNotPersistStateAfterReconcile(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	oldProvider := compileProvider(t, providerA, "old", []string{"m"}, 0)
	oldSnapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{oldProvider}}
	selector.Reconcile(oldSnapshot)

	newProvider := compileProvider(t, providerB, "new", []string{"m"}, 0)
	newSnapshot := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{newProvider}}
	selector.Reconcile(newSnapshot)
	lease, err := selector.Acquire(oldSnapshot, normalKey("old-request", "m"), nil)
	if got := providerID(t, lease, err); got != providerA {
		t.Fatalf("old snapshot provider = %s", got)
	}
	if got := selector.ActiveAssignmentCount(); got != 0 {
		t.Fatalf("old snapshot persisted %d assignments", got)
	}
}

func TestConcurrentAcquireReportAndGenerationReconcile(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	oldProvider := compileProvider(t, providerA, "a", []string{"m"}, 0)
	oldSnapshot := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{oldProvider}}
	selector.Reconcile(oldSnapshot)

	newConfig := oldProvider.Config()
	newConfig.APIKey = "new-key"
	newProvider := compileProviderConfig(t, newConfig)
	newSnapshot := &fakeSnapshot{revision: 2, providers: []*provider.CompiledProvider{newProvider}}

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot := Snapshot(oldSnapshot)
			if i%2 == 0 {
				snapshot = newSnapshot
			}
			lease, err := selector.Acquire(snapshot, normalKey("session", "m"), nil)
			if err == nil {
				selector.Report(lease, Outcome{Class: FailureNone, SessionID: "session"})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		selector.Reconcile(newSnapshot)
	}()
	wg.Wait()

	for _, assignment := range selector.Assignments(providerA) {
		if assignment.Generation != newProvider.Generation {
			t.Fatalf("stale assignment survived reconcile: %#v", assignment)
		}
	}
}
