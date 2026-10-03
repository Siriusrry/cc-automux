package health

import (
	"reflect"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestGenerationEditPreservesHealthAndIsolatesOldResults(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "before", false, "model", "removed")
	p.HealthEpoch = 1
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	late := mustAcquire(t, store, key, false).Lease
	report(t, store, key, false, scheduler.Outcome{Class: scheduler.FailureNone})
	global := mustAcquire(t, store, key, false).Lease
	channel := mustAcquire(t, store, key, false).Lease
	classifierKey := key
	classifierKey.RequestType = traffic.RequestTypeClassifier
	report(t, store, classifierKey, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate, RawError: "classifier failure"})
	store.Report(channel, scheduler.Outcome{Class: scheduler.FailureChannelImmediate, RawError: "model failure"})
	_, observation := store.Report(global, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, RawError: "auth failure", ErrorPending: true})
	before := mustProviderSnapshot(t, store, p)
	next := testProvider(p.ID, "after", false, "model", "added")
	next.HealthEpoch = p.HealthEpoch
	store.Reconcile([]*provider.CompiledProvider{next})
	got := mustProviderSnapshot(t, store, next)
	wantGlobal := before.Global
	wantGlobal.LastErrorPending, wantGlobal.LastErrorIncomplete = false, true
	if !reflect.DeepEqual(got.Global, wantGlobal) {
		t.Fatalf("edit reset global health: %#v", got.Global)
	}
	for _, kind := range []traffic.RequestType{traffic.RequestTypeNormal, traffic.RequestTypeClassifier} {
		if !reflect.DeepEqual(findChannel(t, got, "model", kind), findChannel(t, before, "model", kind)) {
			t.Fatalf("edit reset retained channel %s", kind)
		}
	}
	if added := findChannel(t, got, "added", traffic.RequestTypeNormal); added.State != scheduler.ChannelUnknown || added.Diagnostic != (Diagnostic{}) || len(got.Channels) != 3 {
		t.Fatalf("incorrect channel membership: %#v", got.Channels)
	}
	store.UpdateError(global, observation, "late body", false, false)
	store.Report(late, scheduler.Outcome{Class: scheduler.FailureNone})
	if after := mustProviderSnapshot(t, store, next); !reflect.DeepEqual(after, got) {
		t.Fatalf("old results changed inherited health: %#v", after)
	}
	newKey := testHealthKey(next, "model")
	if decision := store.Acquire(newKey, false); decision.Available || decision.RetryAt == nil {
		t.Fatalf("edit bypassed cooldown: %#v", decision)
	}
	clock.Advance(time.Minute)
	probe := mustAcquire(t, store, newKey, false).Lease
	if !probe.GlobalProbe || !probe.ChannelProbe {
		t.Fatal("new generation lost half-open recovery")
	}
	store.Report(probe, scheduler.Outcome{Class: scheduler.FailureNone})
	if after := mustProviderSnapshot(t, store, next); after.Global.State != scheduler.GlobalHealthy || findChannel(t, after, "model", traffic.RequestTypeNormal).State != scheduler.ChannelHealthy {
		t.Fatalf("current probe failed to recover health: %#v", after)
	}
}

func TestGenerationEditDetachesInFlightProbe(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("provider", "before", false, "model")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "model")
	report(t, store, key, false, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	clock.Advance(time.Minute)
	old := mustAcquire(t, store, key, false).Lease
	next := testProvider(p.ID, "after", false, "model")
	store.Reconcile([]*provider.CompiledProvider{next})
	got := mustProviderSnapshot(t, store, next).Channels[0]
	if got.State != scheduler.ChannelHalfOpen || got.ProbeInFlight || got.ObservedFailures != 1 {
		t.Fatalf("edit reset health or inherited old probe: %#v", got)
	}
	current := mustAcquire(t, store, testHealthKey(next, "model"), false).Lease
	store.Report(old, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	if got = mustProviderSnapshot(t, store, next).Channels[0]; !got.ProbeInFlight || got.State != scheduler.ChannelHalfOpen || got.ObservedFailures != 1 {
		t.Fatalf("old probe changed current state: %#v", got)
	}
	store.Report(current, scheduler.Outcome{Class: scheduler.FailureNone})
	if len(store.retired) != 0 || len(store.leases) != 0 {
		t.Fatal("probe leases leaked")
	}
}
