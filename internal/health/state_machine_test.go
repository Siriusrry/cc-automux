package health

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestOutcomeClassesAffectOnlyTheirHealthLayer(t *testing.T) {
	cases := []struct {
		name        string
		outcome     scheduler.Outcome
		wantGlobal  scheduler.GlobalHealthState
		wantChannel scheduler.ChannelHealthState
	}{
		{name: "success", outcome: scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200}, wantGlobal: scheduler.GlobalHealthy, wantChannel: scheduler.ChannelHealthy},
		{name: "neutral", outcome: scheduler.Outcome{Class: scheduler.FailureNeutral, HTTPStatus: 400}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelUnknown},
		{name: "global immediate", outcome: scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, HTTPStatus: 401}, wantGlobal: scheduler.GlobalCooldown, wantChannel: scheduler.ChannelUnknown},
		{name: "global transient", outcome: scheduler.Outcome{Class: scheduler.FailureGlobalTransient}, wantGlobal: scheduler.GlobalDegraded, wantChannel: scheduler.ChannelUnknown},
		{name: "channel immediate", outcome: scheduler.Outcome{Class: scheduler.FailureChannelImmediate, HTTPStatus: 404}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelCooldown},
		{name: "channel transient", outcome: scheduler.Outcome{Class: scheduler.FailureChannelTransient, HTTPStatus: 429}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelDegraded},
		{name: "stream after response start", outcome: scheduler.Outcome{Class: scheduler.FailureChannelStream, ResponseStarted: true}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelDegraded},
		{name: "client canceled", outcome: scheduler.Outcome{Class: scheduler.FailureClientCanceled}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelUnknown},
		{name: "downstream", outcome: scheduler.Outcome{Class: scheduler.FailureDownstream, ResponseStarted: true}, wantGlobal: scheduler.GlobalUnknown, wantChannel: scheduler.ChannelUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock)
			p := testProvider("provider", "generation", false, "model")
			store.Reconcile([]*provider.CompiledProvider{p})
			key := testHealthKey(p, "model")
			lease := mustAcquire(t, store, key, false).Lease
			store.Report(lease, tc.outcome)
			got := mustProviderSnapshot(t, store, p)
			if got.Global.State != tc.wantGlobal || got.Channels[0].State != tc.wantChannel {
				t.Fatalf("states = %q/%q, want %q/%q", got.Global.State, got.Channels[0].State, tc.wantGlobal, tc.wantChannel)
			}
		})
	}
}

func TestNeutralUpstreamErrorIsObservedWithoutBreakerMutation(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	decision := mustAcquire(t, store, key, false)
	store.Report(decision.Lease, scheduler.Outcome{
		Class:       scheduler.FailureNeutral,
		HTTPStatus:  400,
		UpstreamURL: "https://provider.example/v1/messages",
		RawError:    "invalid request",
		SessionID:   "session-neutral",
	})
	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if channel.State != scheduler.ChannelUnknown || channel.ConsecutiveFailures != 0 || channel.ObservedFailures != 1 || channel.LastFailureAt == nil {
		t.Fatalf("neutral channel = %#v", channel)
	}
	if channel.LastUpstreamURL != "https://provider.example/v1/messages" || channel.LastError != "invalid request" || channel.LastSessionID != "session-neutral" {
		t.Fatalf("neutral diagnostics = %#v", channel)
	}
}

func TestFailureWindowThresholdAndLayerIsolation(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p})
	keyA := testHealthKey(p, "model-a")
	keyB := testHealthKey(p, "model-b")

	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	clock.Advance(2 * time.Minute)
	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	clock.Advance(2*time.Minute + time.Nanosecond)
	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	got := mustProviderSnapshot(t, store, p)
	channelA := findChannel(t, got, "model-a", scheduler.TrafficClassNormal)
	if channelA.State != scheduler.ChannelDegraded || channelA.ConsecutiveFailures != 1 || channelA.ObservedFailures != 3 {
		t.Fatalf("window reset channel = %#v", channelA)
	}

	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	update := report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	if !update.ChannelEnteredCooldown || update.GlobalEnteredCooldown {
		t.Fatalf("threshold update = %#v", update)
	}
	got = mustProviderSnapshot(t, store, p)
	channelA = findChannel(t, got, "model-a", scheduler.TrafficClassNormal)
	channelB := findChannel(t, got, "model-b", scheduler.TrafficClassNormal)
	if channelA.State != scheduler.ChannelCooldown || channelB.State != scheduler.ChannelUnknown || got.Global.State != scheduler.GlobalUnknown {
		t.Fatalf("isolated states = %#v", got)
	}
	decisionB := store.Acquire(keyB, false)
	if !decisionB.Available {
		t.Fatal("channel cooldown blocked a different model")
	}
	store.Report(decisionB.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral})
}

func TestGlobalCooldownBlocksEveryChannel(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p})
	keyA := testHealthKey(p, "model-a")

	for i := 0; i < 3; i++ {
		report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureGlobalTransient})
	}
	for _, model := range p.Models {
		decision := store.Acquire(testHealthKey(p, model), false)
		if decision.Available || decision.GlobalState != scheduler.GlobalCooldown || decision.RetryAt == nil {
			t.Fatalf("Acquire(%q) = %#v", model, decision)
		}
	}
	got := mustProviderSnapshot(t, store, p)
	if got.Global.ConsecutiveFailures != 3 || got.Global.ObservedFailures != 3 {
		t.Fatalf("global diagnostics = %#v", got.Global)
	}
	for _, channel := range got.Channels {
		if channel.State != scheduler.ChannelUnknown || channel.ObservedFailures != 0 {
			t.Fatalf("global failure polluted channel = %#v", channel)
		}
	}
}

func TestConcurrentSuccessRecoversCooldown(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")

	leases := make([]scheduler.HealthLease, 0, 4)
	for i := 0; i < 4; i++ {
		leases = append(leases, mustAcquire(t, store, key, false).Lease)
	}
	for i := 0; i < 3; i++ {
		store.Report(leases[i], scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	}
	if got := findChannel(t, mustProviderSnapshot(t, store, p), "model", scheduler.TrafficClassNormal); got.State != scheduler.ChannelCooldown {
		t.Fatalf("threshold state = %#v", got)
	}
	store.Report(leases[3], scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if got.Global.State != scheduler.GlobalHealthy || channel.State != scheduler.ChannelHealthy || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 {
		t.Fatalf("successful completion did not recover health = %#v", got)
	}
}

func TestHalfOpenSingleLeaseNeutralCancelFailureAndSuccess(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	first := report(t, store, key, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	if first.CooldownUntil == nil || first.CooldownUntil.Sub(clock.Now()) != time.Minute {
		t.Fatalf("first cooldown = %#v", first)
	}
	if decision := store.Acquire(key, false); decision.Available || decision.RetryAt == nil || !decision.RetryAt.Equal(*first.CooldownUntil) {
		t.Fatalf("cooldown decision = %#v", decision)
	}

	clock.Advance(time.Minute)
	probe := mustAcquire(t, store, key, false)
	if !probe.Lease.ChannelProbe || probe.Lease.GlobalProbe {
		t.Fatalf("probe lease = %#v", probe.Lease)
	}
	if other := store.Acquire(key, false); other.Available || other.ChannelState != scheduler.ChannelHalfOpen || other.RetryAt != nil {
		t.Fatalf("concurrent half-open decision = %#v", other)
	}
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral, HTTPStatus: 400})

	probe = mustAcquire(t, store, key, false)
	if !probe.Lease.ChannelProbe {
		t.Fatal("neutral response did not release the probe")
	}
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient, ClientCanceled: true})
	probe = mustAcquire(t, store, key, false)
	if !probe.Lease.ChannelProbe {
		t.Fatal("client cancellation did not release the probe")
	}
	second := store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	if !second.ChannelEnteredCooldown || second.CooldownUntil == nil || second.CooldownUntil.Sub(clock.Now()) != 2*time.Minute {
		t.Fatalf("half-open failure = %#v", second)
	}

	clock.Advance(2 * time.Minute)
	probe = mustAcquire(t, store, key, false)
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", scheduler.TrafficClassNormal)
	if got.State != scheduler.ChannelHealthy || got.BackoffLevel != 0 || got.ConsecutiveFailures != 0 || got.ProbeInFlight {
		t.Fatalf("half-open recovery = %#v", got)
	}

	again := report(t, store, key, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	if again.CooldownUntil == nil || again.CooldownUntil.Sub(clock.Now()) != time.Minute {
		t.Fatalf("reset backoff cooldown = %#v", again)
	}
}

func TestBackoffScheduleAndRetryAfterClamp(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")

	update := report(t, store, key, false, scheduler.Outcome{
		Class:         scheduler.FailureChannelImmediate,
		RetryAfter:    30 * time.Minute,
		HasRetryAfter: true,
	})
	if update.CooldownUntil == nil || update.CooldownUntil.Sub(clock.Now()) != 15*time.Minute {
		t.Fatalf("Retry-After clamp = %#v", update)
	}
	clock.Advance(15 * time.Minute)
	probe := mustAcquire(t, store, key, false)
	update = store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	if update.CooldownUntil == nil || update.CooldownUntil.Sub(clock.Now()) != 2*time.Minute {
		t.Fatalf("second cooldown = %#v", update)
	}

	want := []time.Duration{4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	current := 2 * time.Minute
	for _, duration := range want {
		clock.Advance(current)
		probe = mustAcquire(t, store, key, false)
		update = store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
		if update.CooldownUntil == nil || update.CooldownUntil.Sub(clock.Now()) != duration {
			t.Fatalf("cooldown after %v = %#v, want %v", current, update, duration)
		}
		current = duration
	}
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", scheduler.TrafficClassNormal)
	if got.BackoffLevel != 4 {
		t.Fatalf("final backoff level = %d", got.BackoffLevel)
	}
}

func TestComposedHalfOpenProbeResolvesLayersIndependently(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")

	leases := make([]scheduler.HealthLease, 0, 4)
	for i := 0; i < 4; i++ {
		leases = append(leases, mustAcquire(t, store, key, false).Lease)
	}
	store.Report(leases[0], scheduler.Outcome{Class: scheduler.FailureGlobalImmediate})
	store.Report(leases[1], scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	store.Report(leases[2], scheduler.Outcome{Class: scheduler.FailureGlobalTransient})
	store.Report(leases[3], scheduler.Outcome{Class: scheduler.FailureChannelTransient})

	clock.Advance(time.Minute)
	probe := mustAcquire(t, store, key, false)
	if !probe.Lease.GlobalProbe || !probe.Lease.ChannelProbe {
		t.Fatalf("composed probe = %#v", probe.Lease)
	}
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if got.Global.State != scheduler.GlobalHalfOpen || got.Global.ProbeInFlight || channel.State != scheduler.ChannelCooldown {
		t.Fatalf("cross-layer resolution = %#v", got)
	}

	clock.Advance(2 * time.Minute)
	probe = mustAcquire(t, store, key, false)
	if !probe.Lease.GlobalProbe || !probe.Lease.ChannelProbe {
		t.Fatalf("second composed probe = %#v", probe.Lease)
	}
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
	got = mustProviderSnapshot(t, store, p)
	channel = findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if got.Global.State != scheduler.GlobalHealthy || channel.State != scheduler.ChannelHealthy {
		t.Fatalf("composed recovery = %#v", got)
	}
}

func TestDisableHealthNeverSuppressesAndPreservesDiagnostics(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", true, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")

	outcomes := []scheduler.Outcome{
		{Class: scheduler.FailureGlobalImmediate, UpstreamURL: "https://one.example/v1/messages", RawError: "auth failed", SessionID: "session-global-1", HasRetryAfter: true, RetryAfter: time.Hour},
		{Class: scheduler.FailureGlobalTransient, UpstreamURL: "https://two.example/v1/messages", RawError: "dial failed", SessionID: "session-global-2"},
		{Class: scheduler.FailureChannelImmediate, UpstreamURL: "https://three.example/v1/messages", RawError: "model missing", SessionID: "session-channel-1"},
		{Class: scheduler.FailureChannelTransient, UpstreamURL: "https://four.example/v1/messages", RawError: "rate limited", SessionID: "session-channel-2"},
		{Class: scheduler.FailureChannelStream, UpstreamURL: "https://five.example/v1/messages", RawError: "unexpected EOF", SessionID: "session-channel-3", ResponseStarted: true},
	}
	for _, outcome := range outcomes {
		decision := mustAcquire(t, store, key, true)
		if !decision.Lease.Disabled || decision.Lease.GlobalProbe || decision.Lease.ChannelProbe {
			t.Fatalf("disabled lease = %#v", decision.Lease)
		}
		update := store.Report(decision.Lease, outcome)
		if update.GlobalEnteredCooldown || update.ChannelEnteredCooldown || update.GlobalState != scheduler.GlobalDisabled || update.ChannelState != scheduler.ChannelDisabled {
			t.Fatalf("disabled update = %#v", update)
		}
	}

	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if got.Global.State != scheduler.GlobalDisabled || got.Global.ObservedFailures != 2 || got.Global.ConsecutiveFailures != 0 || got.Global.CooldownUntil != nil {
		t.Fatalf("disabled global = %#v", got.Global)
	}
	if got.Global.LastUpstreamURL != "https://two.example/v1/messages" || got.Global.LastError != "dial failed" || got.Global.LastSessionID != "session-global-2" {
		t.Fatalf("disabled global diagnostics = %#v", got.Global)
	}
	if channel.State != scheduler.ChannelDisabled || channel.ObservedFailures != 3 || channel.ConsecutiveFailures != 0 || channel.CooldownUntil != nil {
		t.Fatalf("disabled channel = %#v", channel)
	}
	if channel.LastUpstreamURL != "https://five.example/v1/messages" || channel.LastError != "unexpected EOF" || channel.LastSessionID != "session-channel-3" {
		t.Fatalf("disabled channel diagnostics = %#v", channel)
	}
	if earliest, ok := store.EarliestRetry([]scheduler.HealthKey{key}); ok || !earliest.IsZero() {
		t.Fatalf("disabled earliest retry = %v, %v", earliest, ok)
	}
}

func TestDisableHealthToggleClearsBreakerState(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	enabled := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{enabled})
	key := testHealthKey(enabled, "model")
	report(t, store, key, false, scheduler.Outcome{
		Class:       scheduler.FailureChannelImmediate,
		RawError:    "model failed",
		SessionID:   "session",
		UpstreamURL: "https://provider.example/v1/messages",
	})

	disabled := testProvider("provider", "generation", true, "model")
	store.Reconcile([]*provider.CompiledProvider{disabled})
	got := mustProviderSnapshot(t, store, disabled)
	channel := findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if channel.State != scheduler.ChannelDisabled || channel.CooldownUntil != nil || channel.ProbeInFlight || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 {
		t.Fatalf("disabled state = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastError != "model failed" || channel.LastSessionID != "session" {
		t.Fatalf("disabled diagnostics = %#v", channel)
	}

	store.Reconcile([]*provider.CompiledProvider{enabled})
	got = mustProviderSnapshot(t, store, enabled)
	channel = findChannel(t, got, "model", scheduler.TrafficClassNormal)
	if channel.State != scheduler.ChannelUnknown || channel.CooldownUntil != nil || channel.ProbeInFlight || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 {
		t.Fatalf("re-enabled state = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastError != "model failed" {
		t.Fatalf("re-enabled diagnostics = %#v", channel)
	}
}

func TestReconcileGenerationModeAndModels(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p1 := testProvider("provider", "generation-1", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p1})
	keyA := testHealthKey(p1, "model-a")
	staleLease := mustAcquire(t, store, keyA, false).Lease
	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient, RawError: "first", SessionID: "session-1"})

	p1Disabled := testProvider("provider", "generation-1", true, "model-a", "model-c")
	store.Reconcile([]*provider.CompiledProvider{p1Disabled})
	got := mustProviderSnapshot(t, store, p1Disabled)
	channelA := findChannel(t, got, "model-a", scheduler.TrafficClassNormal)
	channelC := findChannel(t, got, "model-c", scheduler.TrafficClassNormal)
	if got.Global.State != scheduler.GlobalDisabled || channelA.State != scheduler.ChannelDisabled || channelA.ObservedFailures != 1 || channelA.LastError != "first" {
		t.Fatalf("mode toggle preserved diagnostics = %#v", got)
	}
	if channelC.State != scheduler.ChannelDisabled || channelC.ObservedFailures != 0 {
		t.Fatalf("new channel = %#v", channelC)
	}
	staleUpdate := store.Report(staleLease, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, RawError: "stale"})
	if staleUpdate.GlobalEnteredCooldown || staleUpdate.ChannelEnteredCooldown {
		t.Fatalf("stale mode update = %#v", staleUpdate)
	}
	got = mustProviderSnapshot(t, store, p1Disabled)
	if got.Global.ObservedFailures != 0 || got.Global.LastError == "stale" {
		t.Fatalf("old mode report polluted current mode = %#v", got.Global)
	}

	p2 := testProvider("provider", "generation-2", false, "model-a")
	store.Reconcile([]*provider.CompiledProvider{p2})
	got = mustProviderSnapshot(t, store, p2)
	if got.Global.State != scheduler.GlobalUnknown || got.Global.ObservedFailures != 0 || got.Channels[0].State != scheduler.ChannelUnknown || got.Channels[0].ObservedFailures != 0 {
		t.Fatalf("new generation did not reset = %#v", got)
	}
	oldDecision := mustAcquire(t, store, keyA, false)
	oldUpdate := store.Report(oldDecision.Lease, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, RawError: "old generation"})
	if oldUpdate.GlobalEnteredCooldown || oldUpdate.ChannelEnteredCooldown {
		t.Fatalf("old generation update = %#v", oldUpdate)
	}
	got = mustProviderSnapshot(t, store, p2)
	if got.Global.State != scheduler.GlobalUnknown || got.Global.ObservedFailures != 0 || got.Global.LastError != "" {
		t.Fatalf("old generation report polluted current = %#v", got.Global)
	}
}

func TestEarliestRetryAndDiagnosticAggregate(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p1 := testProvider("a", "generation-a", false, "model-a")
	p2 := testProvider("b", "generation-b", false, "model-b")
	p3 := testProvider("c", "generation-c", true, "model-c", "model-d")
	store.Reconcile([]*provider.CompiledProvider{p2, p3, p1})
	key1 := testHealthKey(p1, "model-a")
	key2 := testHealthKey(p2, "model-b")
	first := report(t, store, key1, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	clock.Advance(10 * time.Second)
	second := report(t, store, key2, false, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, HasRetryAfter: true, RetryAfter: 2 * time.Minute})
	earliest, ok := store.EarliestRetry([]scheduler.HealthKey{key2, key1})
	if !ok || first.CooldownUntil == nil || !earliest.Equal(*first.CooldownUntil) {
		t.Fatalf("earliest retry = %v, %v; first %#v second %#v", earliest, ok, first, second)
	}

	snapshot := store.Snapshot()
	if gotIDs := []string{snapshot.Providers[0].ProviderID, snapshot.Providers[1].ProviderID, snapshot.Providers[2].ProviderID}; !reflect.DeepEqual(gotIDs, []string{"a", "b", "c"}) {
		t.Fatalf("snapshot order = %v", gotIDs)
	}
	aggregate := store.Aggregate()
	want := Aggregate{
		Global:                      StateCounts{Unknown: 1, Cooldown: 1, Disabled: 1},
		Channels:                    StateCounts{Unknown: 1, Cooldown: 1, Disabled: 2},
		HealthDisabledProviderCount: 1,
	}
	if !reflect.DeepEqual(aggregate, want) {
		t.Fatalf("aggregate = %#v, want %#v", aggregate, want)
	}

	snapshot.Providers[0].Channels[0].LastError = "mutated"
	snapshot.Providers[0].Channels = nil
	fresh, ok := store.ProviderSnapshot(p1.ID, p1.Generation)
	if !ok || len(fresh.Channels) != 1 || fresh.Channels[0].LastError == "mutated" {
		t.Fatalf("snapshot was not detached = %#v, %v", fresh, ok)
	}
	if _, ok := store.ProviderSnapshot(p1.ID, provider.ProviderGeneration("stale")); ok {
		t.Fatal("stale generation returned as current")
	}
}

func TestOnlyOneConcurrentHalfOpenProbe(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	report(t, store, key, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	clock.Advance(time.Minute)

	const goroutines = 64
	var available atomic.Int64
	var winnerMu sync.Mutex
	var winner scheduler.HealthLease
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision := store.Acquire(key, false)
			if !decision.Available {
				return
			}
			available.Add(1)
			winnerMu.Lock()
			winner = decision.Lease
			winnerMu.Unlock()
		}()
	}
	wg.Wait()
	if available.Load() != 1 {
		t.Fatalf("available probes = %d", available.Load())
	}
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", scheduler.TrafficClassNormal)
	if got.State != scheduler.ChannelHalfOpen || !got.ProbeInFlight {
		t.Fatalf("half-open snapshot = %#v", got)
	}
	store.Report(winner, scheduler.Outcome{Class: scheduler.FailureNeutral})
	nextProbe := mustAcquire(t, store, key, false)
	if !nextProbe.Lease.ChannelProbe {
		t.Fatal("released probe was not available again")
	}
	store.Report(nextProbe.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral})
}

func TestConcurrentAcquireReportReconcileAndSnapshots(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p1 := testProvider("provider", "generation-1", false, "model")
	p2 := testProvider("provider", "generation-2", true, "model")
	store.Reconcile([]*provider.CompiledProvider{p1})

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				p := p1
				if (worker+i)%2 == 0 {
					p = p2
				}
				decision := store.Acquire(testHealthKey(p, "model"), p.DisableHealth)
				if decision.Available {
					class := scheduler.FailureNone
					if i%3 != 0 {
						class = scheduler.FailureChannelTransient
					}
					store.Report(decision.Lease, scheduler.Outcome{Class: class})
				}
				if i%10 == 0 {
					store.Snapshot()
					store.Aggregate()
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				store.Reconcile([]*provider.CompiledProvider{p2})
			} else {
				store.Reconcile([]*provider.CompiledProvider{p1})
			}
			clock.Advance(time.Second)
		}
	}()
	wg.Wait()
}

func TestWrongLeaseKeyConsumesAndReleasesActualLease(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	lease := mustAcquire(t, store, key, false).Lease
	wrong := lease
	wrong.Key.Model = "other-model"
	if update := store.Report(wrong, scheduler.Outcome{Class: scheduler.FailureChannelImmediate}); update != (scheduler.HealthUpdate{}) {
		t.Fatalf("wrong lease update = %#v", update)
	}
	got := mustProviderSnapshot(t, store, p)
	if got.Global.State != scheduler.GlobalUnknown || got.Channels[0].State != scheduler.ChannelUnknown || got.Channels[0].ProbeInFlight {
		t.Fatalf("wrong lease changed state = %#v", got)
	}
	// The actual lease was consumed, so a second report must be ignored and a
	// future acquire must remain possible.
	if update := store.Report(lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate}); update != (scheduler.HealthUpdate{}) {
		t.Fatalf("replayed lease update = %#v", update)
	}
	next := mustAcquire(t, store, key, false)
	store.Report(next.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral})

	lease = mustAcquire(t, store, key, false).Lease
	wrong = lease
	wrong.ChannelProbe = !lease.ChannelProbe
	if update := store.Report(wrong, scheduler.Outcome{Class: scheduler.FailureChannelImmediate}); update != (scheduler.HealthUpdate{}) {
		t.Fatalf("wrong lease flags update = %#v", update)
	}
	if next := mustAcquire(t, store, key, false); !next.Available {
		t.Fatalf("wrong lease flags left state unavailable = %#v", next)
	} else {
		store.Report(next.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral})
	}
}

func TestGenerationRetirementKeepsOnlyLiveScopes(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p1 := testProvider("provider", "generation-1", false, "model")
	p2 := testProvider("provider", "generation-2", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p1})
	lease := mustAcquire(t, store, testHealthKey(p1, "model"), false).Lease
	store.Reconcile([]*provider.CompiledProvider{p2})
	if len(store.retired) != 1 {
		t.Fatalf("retired scopes = %#v", store.retired)
	}
	store.Report(lease, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate})
	if len(store.retired) != 0 {
		t.Fatalf("retired scope leaked = %#v", store.retired)
	}
	if got := mustProviderSnapshot(t, store, p2); got.Global.State != scheduler.GlobalUnknown {
		t.Fatalf("current generation changed = %#v", got.Global)
	}
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)}
}

func newTestStore(t *testing.T, clock Clock) *Store {
	t.Helper()
	store, err := New(scheduler.DefaultPolicy(), clock)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testHealthKey(p *provider.CompiledProvider, model string) scheduler.HealthKey {
	return scheduler.HealthKey{
		ProviderID:   p.ID,
		Generation:   p.Generation,
		Model:        model,
		TrafficClass: scheduler.TrafficClassNormal,
	}
}

func mustAcquire(t *testing.T, store *Store, key scheduler.HealthKey, disableHealth bool) scheduler.HealthDecision {
	t.Helper()
	decision := store.Acquire(key, disableHealth)
	if !decision.Available || decision.Lease.Token == 0 {
		t.Fatalf("Acquire(%#v, %v) = %#v", key, disableHealth, decision)
	}
	return decision
}

func report(t *testing.T, store *Store, key scheduler.HealthKey, disableHealth bool, outcome scheduler.Outcome) scheduler.HealthUpdate {
	t.Helper()
	return store.Report(mustAcquire(t, store, key, disableHealth).Lease, outcome)
}

func mustProviderSnapshot(t *testing.T, store *Store, p *provider.CompiledProvider) ProviderSnapshot {
	t.Helper()
	snapshot, ok := store.ProviderSnapshot(p.ID, p.Generation)
	if !ok {
		t.Fatalf("missing provider snapshot for %q generation %q", p.ID, p.Generation)
	}
	return snapshot
}

func findChannel(t *testing.T, snapshot ProviderSnapshot, model string, trafficClass scheduler.TrafficClass) ChannelSnapshot {
	t.Helper()
	for _, channel := range snapshot.Channels {
		if channel.Model == model && channel.TrafficClass == trafficClass {
			return channel
		}
	}
	t.Fatalf("missing channel %q/%q in %#v", model, trafficClass, snapshot.Channels)
	return ChannelSnapshot{}
}
