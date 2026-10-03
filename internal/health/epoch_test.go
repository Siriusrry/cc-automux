package health

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestHealthEpochResetsDiagnosticsAndIsolatesLeases(t *testing.T) {
	for _, modes := range [][2]bool{{false, false}, {true, true}, {false, true}, {true, false}} {
		disabledHealth, nextDisabledHealth := modes[0], modes[1]
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("health_off=%t_to_%t/enabled=%t", disabledHealth, nextDisabledHealth, enabled), func(t *testing.T) {
				clock := newFakeClock()
				store := newTestStore(t, clock)
				p := testProvider("target", "generation", disabledHealth, "a", "b")
				p.Enabled, p.HealthEpoch = enabled, 1
				other := testProvider("other", "generation", false, "a")
				store.Reconcile([]*provider.CompiledProvider{p, other})
				report(t, store, testHealthKey(other, "a"), false, scheduler.Outcome{Class: scheduler.FailureChannelTransient, RawError: "keep"})
				otherBefore := mustProviderSnapshot(t, store, other)
				key := testHealthKey(p, "a")
				report(t, store, key, disabledHealth, scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
				lateSuccess := mustAcquire(t, store, key, disabledHealth).Lease
				lateFailure := mustAcquire(t, store, key, disabledHealth).Lease
				global := mustAcquire(t, store, key, disabledHealth).Lease
				for _, model := range p.Models {
					for _, kind := range []traffic.RequestType{traffic.RequestTypeNormal, traffic.RequestTypeClassifier} {
						channelKey := testHealthKey(p, model)
						channelKey.RequestType = kind
						report(t, store, channelKey, disabledHealth, scheduler.Outcome{
							Class: scheduler.FailureChannelImmediate, RawError: "old channel failure", ErrorPending: true,
							UpstreamURL: "https://provider.example/v1/messages", SessionID: "old-session",
						})
					}
				}
				_, observation := store.Report(global, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, RawError: "old global failure", ErrorPending: true})
				before := mustProviderSnapshot(t, store, p)
				if before.Global.ObservedFailures != 1 || before.Global.LastSuccessAt == nil || len(before.Channels) != 4 {
					t.Fatalf("missing initial observations: %#v", before)
				}
				clock.Advance(time.Minute)
				oldProbe := mustAcquire(t, store, key, disabledHealth).Lease
				if !disabledHealth && (!oldProbe.GlobalProbe || !oldProbe.ChannelProbe) {
					t.Fatal("expected an in-flight half-open probe")
				}

				next := p.Clone()
				next.Enabled, next.HealthEpoch = !enabled, 2
				next.DisableHealth = nextDisabledHealth
				store.Reconcile([]*provider.CompiledProvider{next, other})
				fresh := mustProviderSnapshot(t, store, next)
				wantGlobal, wantChannel := scheduler.GlobalUnknown, scheduler.ChannelUnknown
				if nextDisabledHealth {
					wantGlobal, wantChannel = scheduler.GlobalDisabled, scheduler.ChannelDisabled
				}
				if fresh.Global.State != wantGlobal || fresh.Global.Diagnostic != (Diagnostic{}) || len(fresh.Channels) != 2 {
					t.Fatalf("global/retained channels not reset: %#v", fresh)
				}
				for _, channel := range fresh.Channels {
					if channel.State != wantChannel || channel.RequestType != traffic.RequestTypeNormal || channel.Diagnostic != (Diagnostic{}) {
						t.Fatalf("channel not reset: %#v", channel)
					}
				}
				newKey := testHealthKey(next, "a")
				if retry, ok := store.EarliestRetry([]scheduler.HealthKey{newKey}); ok || !retry.IsZero() {
					t.Fatal("new epoch inherited old cooldown")
				}
				store.UpdateError(global, observation, "late body", true, true)
				store.Report(lateSuccess, scheduler.Outcome{Class: scheduler.FailureNone})
				store.Report(lateFailure, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate})
				store.Report(oldProbe, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
				oldRequest := mustAcquire(t, store, key, disabledHealth).Lease
				store.Report(oldRequest, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate})
				if got := mustProviderSnapshot(t, store, next); !reflect.DeepEqual(got, fresh) {
					t.Fatalf("old epoch changed current health: %#v", got)
				}
				if got := mustProviderSnapshot(t, store, other); !reflect.DeepEqual(got, otherBefore) {
					t.Fatalf("unrelated provider changed: %#v", got)
				}
				if len(store.leases) != 0 || len(store.retired) != 0 {
					t.Fatal("retired leases/scopes were not released")
				}
				current := mustAcquire(t, store, newKey, nextDisabledHealth).Lease
				store.Report(current, scheduler.Outcome{Class: scheduler.FailureNone})
				if got := mustProviderSnapshot(t, store, next); got.Global.LastSuccessAt == nil {
					t.Fatal("new epoch does not accept current results")
				}
			})
		}
	}
}

func TestHealthRetryDeadlineDoesNotCrossEpochs(t *testing.T) {
	store := newTestStore(t, newFakeClock())
	p := testProvider("provider", "generation", false, "model")
	p.HealthEpoch = 1
	store.Reconcile([]*provider.CompiledProvider{p})
	oldKey := testHealthKey(p, "model")
	held := mustAcquire(t, store, oldKey, false).Lease
	report(t, store, oldKey, false, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate})
	next := p.Clone()
	next.HealthEpoch = 3
	store.Reconcile([]*provider.CompiledProvider{next})
	if _, ok := store.EarliestRetry([]scheduler.HealthKey{oldKey}); !ok {
		t.Fatal("retired epoch lost its in-flight cooldown")
	}
	if _, ok := store.EarliestRetry([]scheduler.HealthKey{testHealthKey(next, "model")}); ok {
		t.Fatal("current epoch inherited retired retry deadline")
	}
	store.Report(held, scheduler.Outcome{Class: scheduler.FailureClientCanceled})
}
