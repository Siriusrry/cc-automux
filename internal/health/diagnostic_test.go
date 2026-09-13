package health

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestDiagnosticFillIsBoundedAndRejectsStaleObservations(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock)
	p := testProvider("p", "g", false, "m")
	store.Reconcile([]*provider.CompiledProvider{p})
	key := testHealthKey(p, "m")
	lease := mustAcquire(t, store, key, false).Lease
	_, id := store.Report(lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient, HTTPStatus: 429, RawError: "HTTP 429 Too Many Requests", ErrorPending: true})
	pending := mustProviderSnapshot(t, store, p).Channels[0]
	if !pending.LastErrorPending || id == 0 {
		t.Fatalf("pending=%#v id=%d", pending, id)
	}
	store.UpdateError(lease, id, strings.Repeat("界", 40000), false, true)
	got := mustProviderSnapshot(t, store, p).Channels[0]
	if got.LastErrorPending || !got.LastErrorTruncated || len(got.LastError) > 8192 || !utf8.ValidString(got.LastError) || got.ConsecutiveFailures != pending.ConsecutiveFailures {
		t.Fatalf("filled=%#v", got)
	}
	lease = mustAcquire(t, store, key, false).Lease
	_, old := store.Report(lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient, HTTPStatus: 503, RawError: "pending", ErrorPending: true})
	next := mustAcquire(t, store, key, false).Lease
	_, newer := store.Report(next, scheduler.Outcome{Class: scheduler.FailureNone, HTTPStatus: 200})
	store.UpdateError(lease, old, "old body", true, false)
	got = mustProviderSnapshot(t, store, p).Channels[0]
	if newer <= old || got.LastError != "" || got.LastErrorPending || got.LastErrorIncomplete || got.LastErrorTruncated {
		t.Fatalf("stale fill=%#v", got)
	}
	lease = mustAcquire(t, store, key, false).Lease
	_, id = store.Report(lease, scheduler.Outcome{Class: scheduler.FailureGlobalTransient, RawError: strings.Repeat("界", 4000), ErrorPending: true})
	store.UpdateError(lease, id, "", true, false)
	global := mustProviderSnapshot(t, store, p).Global
	if !global.LastErrorTruncated || !global.LastErrorIncomplete || global.LastErrorPending || len(global.LastError) > 8192 {
		t.Fatalf("global=%#v", global)
	}
}
