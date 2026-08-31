package health

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
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
	// Establish non-default state in both layers so the neutral observation can
	// be checked for complete global immutability and unchanged breaker counts.
	globalLease := mustAcquire(t, store, key, false)
	store.Report(globalLease.Lease, scheduler.Outcome{
		Class:       scheduler.FailureGlobalTransient,
		UpstreamURL: "https://provider.example/global",
		RawError:    "global transient",
		SessionID:   "global-session",
	})
	channelLease := mustAcquire(t, store, key, false)
	store.Report(channelLease.Lease, scheduler.Outcome{
		Class:       scheduler.FailureChannelTransient,
		UpstreamURL: "https://provider.example/channel",
		RawError:    "channel transient",
		SessionID:   "channel-session",
	})
	before := mustProviderSnapshot(t, store, p)
	decision := mustAcquire(t, store, key, false)
	store.Report(decision.Lease, scheduler.Outcome{
		Class:       scheduler.FailureNeutral,
		HTTPStatus:  400,
		UpstreamURL: "https://provider.example/v1/messages",
		RawError:    "invalid request",
		SessionID:   "session-neutral",
	})
	got := mustProviderSnapshot(t, store, p)
	if !reflect.DeepEqual(got.Global, before.Global) {
		t.Fatalf("neutral response changed global diagnostics: before=%#v after=%#v", before.Global, got.Global)
	}
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
	beforeChannel := findChannel(t, before, "model", traffic.RequestTypeNormal)
	if channel.State != beforeChannel.State || channel.ConsecutiveFailures != beforeChannel.ConsecutiveFailures ||
		channel.ObservedFailures != beforeChannel.ObservedFailures+1 || channel.LastFailureAt == nil ||
		!channel.LastFailureAt.Equal(clock.Now()) {
		t.Fatalf("neutral channel = %#v", channel)
	}
	if channel.LastUpstreamURL != "https://provider.example/v1/messages" || channel.LastError != "invalid request" || channel.LastSessionID != "session-neutral" {
		t.Fatalf("neutral diagnostics = %#v", channel)
	}
	// A subsequent empty 400 body is also a real latest observation and clears
	// stale error text without affecting either layer's breaker counters.
	before = got
	decision = mustAcquire(t, store, key, false)
	store.Report(decision.Lease, scheduler.Outcome{
		Class:       scheduler.FailureNeutral,
		HTTPStatus:  400,
		UpstreamURL: "https://provider.example/empty",
		SessionID:   "session-empty",
	})
	got = mustProviderSnapshot(t, store, p)
	if !reflect.DeepEqual(got.Global, before.Global) {
		t.Fatalf("empty neutral response changed global diagnostics: before=%#v after=%#v", before.Global, got.Global)
	}
	channel = findChannel(t, got, "model", traffic.RequestTypeNormal)
	if channel.LastError != "" || channel.ObservedFailures != beforeChannel.ObservedFailures+2 || channel.ConsecutiveFailures != beforeChannel.ConsecutiveFailures {
		t.Fatalf("empty neutral diagnostics = %#v", channel)
	}
}

func TestHealthFailureWithEmptyRawErrorClearsPreviousDiagnostic(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", true, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")

	report(t, store, key, true, scheduler.Outcome{
		Class:       scheduler.FailureGlobalImmediate,
		UpstreamURL: "https://provider.example/first-global",
		RawError:    "old global error",
		SessionID:   "global-old",
	})
	report(t, store, key, true, scheduler.Outcome{
		Class:       scheduler.FailureGlobalTransient,
		UpstreamURL: "https://provider.example/empty-global",
		SessionID:   "global-empty",
	})
	report(t, store, key, true, scheduler.Outcome{
		Class:       scheduler.FailureChannelImmediate,
		HTTPStatus:  404,
		UpstreamURL: "https://provider.example/first-channel",
		RawError:    "old channel error",
		SessionID:   "channel-old",
	})
	report(t, store, key, true, scheduler.Outcome{
		Class:       scheduler.FailureChannelTransient,
		HTTPStatus:  503,
		UpstreamURL: "https://provider.example/empty-channel",
		SessionID:   "channel-empty",
	})

	snapshot := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, snapshot, "model", traffic.RequestTypeNormal)
	if snapshot.Global.LastError != "" || snapshot.Global.LastUpstreamURL != "https://provider.example/empty-global" || snapshot.Global.LastSessionID != "global-empty" {
		t.Fatalf("latest empty global diagnostic = %#v", snapshot.Global)
	}
	if channel.LastError != "" || channel.LastUpstreamURL != "https://provider.example/empty-channel" || channel.LastSessionID != "channel-empty" {
		t.Fatalf("latest empty channel diagnostic = %#v", channel)
	}
}

func TestLocalNeutralErrorDoesNotPolluteProviderDiagnostics(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	decision := mustAcquire(t, store, key, false)
	store.Report(decision.Lease, scheduler.Outcome{
		Class:     scheduler.FailureNeutral,
		RawError:  "local replay failure",
		SessionID: "session-local",
	})
	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
	if channel.ObservedFailures != 0 || channel.LastError != "" || channel.LastSessionID != "" || channel.LastFailureAt != nil {
		t.Fatalf("local error polluted provider diagnostics: %#v", channel)
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
	channelA := findChannel(t, got, "model-a", traffic.RequestTypeNormal)
	if channelA.State != scheduler.ChannelDegraded || channelA.ConsecutiveFailures != 1 || channelA.ObservedFailures != 3 {
		t.Fatalf("window reset channel = %#v", channelA)
	}

	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	update := report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelTransient})
	if !update.ChannelEnteredCooldown || update.GlobalEnteredCooldown {
		t.Fatalf("threshold update = %#v", update)
	}
	got = mustProviderSnapshot(t, store, p)
	channelA = findChannel(t, got, "model-a", traffic.RequestTypeNormal)
	channelB := findChannel(t, got, "model-b", traffic.RequestTypeNormal)
	if channelA.State != scheduler.ChannelCooldown || channelB.State != scheduler.ChannelUnknown || got.Global.State != scheduler.GlobalUnknown {
		t.Fatalf("isolated states = %#v", got)
	}
	decisionB := store.Acquire(keyB, false)
	if !decisionB.Available {
		t.Fatal("channel cooldown blocked a different model")
	}
	store.Report(decisionB.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral})
}

func TestClassifierChannelHealthIsolatedFromNormalChannel(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	classifierKey := scheduler.HealthKey{
		ProviderID:  p.ID,
		Generation:  p.Generation,
		Model:       "model",
		RequestType: traffic.RequestTypeClassifier,
	}
	normalKey := testHealthKey(p, "model")

	update := report(t, store, classifierKey, false, scheduler.Outcome{
		Class:       scheduler.FailureChannelImmediate,
		HTTPStatus:  404,
		UpstreamURL: "https://provider.example/v1/messages",
		RawError:    "classifier model missing",
		SessionID:   "classifier-session",
	})
	if !update.ChannelEnteredCooldown || update.GlobalEnteredCooldown || update.ChannelState != scheduler.ChannelCooldown {
		t.Fatalf("classifier channel update = %#v", update)
	}
	if normal := store.Acquire(normalKey, false); !normal.Available {
		t.Fatalf("classifier cooldown blocked normal channel = %#v", normal)
	} else {
		store.Report(normal.Lease, scheduler.Outcome{Class: scheduler.FailureNeutral, HTTPStatus: 400})
	}
	if next := store.Acquire(classifierKey, false); next.Available || next.ChannelState != scheduler.ChannelCooldown {
		t.Fatalf("classifier cooldown decision = %#v", next)
	}

	snapshot := mustProviderSnapshot(t, store, p)
	classifier := findChannel(t, snapshot, "model", traffic.RequestTypeClassifier)
	normal := findChannel(t, snapshot, "model", traffic.RequestTypeNormal)
	if classifier.State != scheduler.ChannelCooldown || classifier.LastError != "classifier model missing" ||
		classifier.LastSessionID != "classifier-session" || normal.State == scheduler.ChannelCooldown {
		t.Fatalf("classifier/normal channel states = %#v / %#v", classifier, normal)
	}
}

func TestDisableHealthClassifierChannelNeverCoolsDown(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "generation", true, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := scheduler.HealthKey{
		ProviderID:  p.ID,
		Generation:  p.Generation,
		Model:       "model",
		RequestType: traffic.RequestTypeClassifier,
	}
	for i := 0; i < 4; i++ {
		decision := mustAcquire(t, store, key, true)
		update := store.Report(decision.Lease, scheduler.Outcome{
			Class:      scheduler.FailureChannelTransient,
			HTTPStatus: 429,
			RawError:   "classifier rate limited",
			SessionID:  "classifier-session",
		})
		if update.ChannelEnteredCooldown || update.ChannelState != scheduler.ChannelDisabled {
			t.Fatalf("disabled classifier update %d = %#v", i, update)
		}
	}
	snapshot := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, snapshot, "model", traffic.RequestTypeClassifier)
	if channel.State != scheduler.ChannelDisabled || channel.ConsecutiveFailures != 0 || channel.ObservedFailures != 4 ||
		channel.LastError != "classifier rate limited" {
		t.Fatalf("disabled classifier channel = %#v", channel)
	}
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
	if got := findChannel(t, mustProviderSnapshot(t, store, p), "model", traffic.RequestTypeNormal); got.State != scheduler.ChannelCooldown {
		t.Fatalf("threshold state = %#v", got)
	}
	store.Report(leases[3], scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
	got := mustProviderSnapshot(t, store, p)
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
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
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", traffic.RequestTypeNormal)
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
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", traffic.RequestTypeNormal)
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
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
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
	channel = findChannel(t, got, "model", traffic.RequestTypeNormal)
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
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
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
	channel := findChannel(t, got, "model", traffic.RequestTypeNormal)
	if channel.State != scheduler.ChannelDisabled || channel.CooldownUntil != nil || channel.ProbeInFlight || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 {
		t.Fatalf("disabled state = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastError != "model failed" || channel.LastSessionID != "session" {
		t.Fatalf("disabled diagnostics = %#v", channel)
	}

	store.Reconcile([]*provider.CompiledProvider{enabled})
	got = mustProviderSnapshot(t, store, enabled)
	channel = findChannel(t, got, "model", traffic.RequestTypeNormal)
	if channel.State != scheduler.ChannelUnknown || channel.CooldownUntil != nil || channel.ProbeInFlight || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 {
		t.Fatalf("re-enabled state = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastError != "model failed" {
		t.Fatalf("re-enabled diagnostics = %#v", channel)
	}
}

func TestDisableHealthTogglePreservesClassifierDiagnostics(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	enabled := testProvider("provider", "generation", false, "model")
	store.Reconcile([]*provider.CompiledProvider{enabled})
	classifierKey := scheduler.HealthKey{
		ProviderID:  enabled.ID,
		Generation:  enabled.Generation,
		Model:       "model",
		RequestType: traffic.RequestTypeClassifier,
	}
	report(t, store, classifierKey, false, scheduler.Outcome{
		Class:       scheduler.FailureChannelImmediate,
		HTTPStatus:  404,
		UpstreamURL: "https://provider.example/v1/messages",
		RawError:    "classifier endpoint missing",
		SessionID:   "classifier-session",
	})

	// Toggling disable_health keeps the same target generation. Breaker fields
	// must reset to the new mode, but the dynamically-created classifier channel
	// and its latest observation remain available for diagnostics.
	disabled := testProvider("provider", "generation", true, "model")
	store.Reconcile([]*provider.CompiledProvider{disabled})
	snapshot := mustProviderSnapshot(t, store, disabled)
	channel := findChannel(t, snapshot, "model", traffic.RequestTypeClassifier)
	if channel.State != scheduler.ChannelDisabled || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 || channel.CooldownUntil != nil {
		t.Fatalf("classifier breaker state after disable toggle = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastUpstreamURL != "https://provider.example/v1/messages" ||
		channel.LastError != "classifier endpoint missing" || channel.LastSessionID != "classifier-session" {
		t.Fatalf("classifier diagnostics after disable toggle = %#v", channel)
	}

	// Switching back must preserve the same diagnostic fields while resetting
	// the disabled state again to an ordinary unknown channel.
	store.Reconcile([]*provider.CompiledProvider{enabled})
	snapshot = mustProviderSnapshot(t, store, enabled)
	channel = findChannel(t, snapshot, "model", traffic.RequestTypeClassifier)
	if channel.State != scheduler.ChannelUnknown || channel.ConsecutiveFailures != 0 || channel.BackoffLevel != 0 || channel.CooldownUntil != nil {
		t.Fatalf("classifier breaker state after re-enable = %#v", channel)
	}
	if channel.ObservedFailures != 1 || channel.LastError != "classifier endpoint missing" || channel.LastSessionID != "classifier-session" {
		t.Fatalf("classifier diagnostics after re-enable = %#v", channel)
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
	channelA := findChannel(t, got, "model-a", traffic.RequestTypeNormal)
	channelC := findChannel(t, got, "model-c", traffic.RequestTypeNormal)
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
	got := findChannel(t, mustProviderSnapshot(t, store, p), "model", traffic.RequestTypeNormal)
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

func TestRetiredHalfOpenReportReleasesProbeToken(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p1 := testProvider("provider", "generation-1", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p1})
	keyA := testHealthKey(p1, "model-a")
	keyB := testHealthKey(p1, "model-b")
	report(t, store, keyA, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	clock.Advance(time.Minute)
	probe := mustAcquire(t, store, keyA, false)
	if !probe.Lease.ChannelProbe {
		t.Fatalf("half-open lease = %#v", probe.Lease)
	}
	// Keep the retired scope alive after the probe completes so its token can
	// be observed through another old-generation Acquire.
	held := mustAcquire(t, store, keyB, false)

	p2 := testProvider("provider", "generation-2", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p2})
	store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})

	next := store.Acquire(keyA, false)
	if !next.Available || !next.Lease.ChannelProbe || next.ChannelState != scheduler.ChannelHalfOpen {
		t.Fatalf("retired probe token was not released = %#v", next)
	}
	store.Report(next.Lease, scheduler.Outcome{Class: scheduler.FailureClientCanceled, ClientCanceled: true})
	store.Report(held.Lease, scheduler.Outcome{Class: scheduler.FailureClientCanceled, ClientCanceled: true})
	if len(store.retired) != 0 {
		t.Fatalf("retired scope leaked = %#v", store.retired)
	}
	if current := mustProviderSnapshot(t, store, p2); current.Global.State != scheduler.GlobalUnknown {
		t.Fatalf("retired reports changed current generation = %#v", current)
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
		ProviderID:  p.ID,
		Generation:  p.Generation,
		Model:       model,
		RequestType: traffic.RequestTypeNormal,
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

func findChannel(t *testing.T, snapshot ProviderSnapshot, model string, requestType traffic.RequestType) ChannelSnapshot {
	t.Helper()
	for _, channel := range snapshot.Channels {
		if channel.Model == model && channel.RequestType == requestType {
			return channel
		}
	}
	t.Fatalf("missing channel %q/%q in %#v", model, requestType, snapshot.Channels)
	return ChannelSnapshot{}
}
