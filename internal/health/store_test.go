package health

import (
	"sync"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestReconcileCreatesUnknownTwoLayerSnapshot(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)}
	store, err := New(scheduler.DefaultPolicy(), clock)
	if err != nil {
		t.Fatal(err)
	}
	p := testProvider("provider-a", "generation-a", false, "model-a", "model-b")
	store.Reconcile([]*provider.CompiledProvider{p})

	snapshot := store.Snapshot()
	if !snapshot.GeneratedAt.Equal(clock.Now()) || len(snapshot.Providers) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	got := snapshot.Providers[0]
	if got.Global.State != scheduler.GlobalUnknown || len(got.Channels) != 2 {
		t.Fatalf("provider snapshot = %#v", got)
	}
	for _, channel := range got.Channels {
		if channel.State != scheduler.ChannelUnknown || channel.RequestType != traffic.RequestTypeNormal {
			t.Fatalf("channel = %#v", channel)
		}
	}
}

func testProvider(id, generation string, disableHealth bool, models ...string) *provider.CompiledProvider {
	return &provider.CompiledProvider{
		CompiledTarget: provider.CompiledTarget{ID: id, Generation: provider.ProviderGeneration(generation)},
		DisableHealth:  disableHealth,
		Models:         append([]string(nil), models...),
		Enabled:        true,
	}
}
