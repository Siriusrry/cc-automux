package scheduler

import (
	"testing"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestHealthEpochChangeClearsOnlyTargetSchedulingState(t *testing.T) {
	health := newFakeHealth()
	selector := newTestScheduler(t, health, Policy{}, nil)
	a := compileProvider(t, providerA, "a", []string{"a", "pending"}, 0)
	b := compileProvider(t, providerB, "b", []string{"b"}, 0)
	a.HealthEpoch, b.HealthEpoch = 1, 1
	old := &fakeSnapshot{revision: 1, providers: []*provider.CompiledProvider{a, b}}
	selector.Reconcile(old)
	acquire := func(snapshot *fakeSnapshot, key StickyKey) AttemptLease {
		t.Helper()
		lease, err := selector.Acquire(snapshot, key, NewRequestSelection(DefaultAttemptPolicy()))
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	normal := acquire(old, normalKey("a-normal", "a"))
	classifier := acquire(old, StickyKey{SessionID: "a-classifier", Model: "a", RequestType: traffic.RequestTypeClassifier})
	acquire(old, normalKey("b-session", "b"))
	pending := acquire(old, normalKey("a-pending", "pending"))
	health.updates[pending.HealthLease.Key] = HealthUpdate{ChannelEnteredCooldown: true, ChannelState: ChannelCooldown}
	selector.Report(pending, Outcome{Class: FailureChannelImmediate})
	if len(selector.pending) != 1 || len(selector.assignments) != 3 || len(selector.cursors) != 4 {
		t.Fatal("missing initial pending/active bindings or cursors")
	}
	otherBinding := selector.assignments[normalKey("b-session", "b")]
	otherCursorKey := roundRobinKey{Model: "b", RequestType: traffic.RequestTypeNormal}
	otherCursor := selector.cursors[otherCursorKey]
	currentA := a.Clone()
	currentA.HealthEpoch = 3 // The disabled snapshot was never reconciled.
	next := &fakeSnapshot{revision: 3, providers: []*provider.CompiledProvider{currentA, b}}
	selector.Reconcile(next)
	if len(selector.pending) != 0 || len(selector.assignments) != 1 {
		t.Fatal("enabled transition retained target scheduling state")
	}
	for _, cursor := range selector.cursors {
		if cursor.ProviderID == a.ID {
			t.Fatal("enabled transition retained a target cursor")
		}
	}
	if selector.assignments[normalKey("b-session", "b")] != otherBinding || selector.cursors[otherCursorKey] != otherCursor {
		t.Fatal("unrelated provider scheduling state changed")
	}
	selector.Report(normal, Outcome{Class: FailureNone})
	selector.Report(classifier, Outcome{Class: FailureChannelImmediate})
	late := acquire(old, normalKey("late", "a"))
	if late.HealthLease.Key.Epoch != 1 {
		t.Fatal("old request acquired current health epoch")
	}
	selector.Report(late, Outcome{Class: FailureNone})
	if len(selector.assignments) != 1 {
		t.Fatal("old request recreated a binding")
	}
	current := acquire(next, normalKey("new", "a"))
	if current.HealthLease.Key.Epoch != 3 || current.FromSticky {
		t.Fatal("new request did not start a fresh health lifetime")
	}
}
