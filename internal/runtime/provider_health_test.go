package runtime

import (
	"reflect"
	"testing"
)

func TestProviderHealthEpochFollowsAppliedEnabledTransitions(t *testing.T) {
	m, _, cfg := newRuntimeManager(t, Options{})
	initial := m.Snapshot()
	original := initial.Providers()[0]
	if original.HealthEpoch == 0 {
		t.Fatal("missing initial health epoch")
	}
	for _, enabled := range []bool{false, true, false, true} {
		previous := m.Snapshot()
		cfg.Providers[0].Enabled = enabled
		if _, err := m.Apply(cfg); err != nil {
			t.Fatal(err)
		}
		snapshot := m.Snapshot()
		p := snapshot.Providers()[0]
		if p.HealthEpoch <= previous.Providers()[0].HealthEpoch || p.Generation != original.Generation {
			t.Fatal("enabled transition did not isolate health while preserving target identity")
		}
		if p.HealthEpoch != snapshot.Catalog().Providers()[0].HealthEpoch {
			t.Fatal("catalog epoch differs from providers")
		}
		if enabled && p.HealthEpoch != snapshot.Candidates("model")[0].HealthEpoch {
			t.Fatal("candidate epoch differs from providers")
		}
		if !reflect.DeepEqual(initial.Providers()[0], original) {
			t.Fatal("old snapshot mutated")
		}
		if _, err := m.Apply(cfg); err != nil || m.Snapshot() != snapshot {
			t.Fatal("same enabled value was not a no-op", err)
		}
	}
	before := m.Snapshot().Providers()[0]
	cfg.Providers[0].Name = "renamed"
	cfg.Providers[0].Priority++
	cfg.Providers[0].Models = append(cfg.Providers[0].Models, "another")
	cfg.Providers[0].DisableHealth = true
	cfg.Auth.GatewayKey = "gateway-key"
	if _, err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Providers()[0].HealthEpoch != before.HealthEpoch {
		t.Fatal("unrelated fields reset the enabled-state epoch")
	}
	second := cfg.Providers[0]
	second.ID, second.Name = "22222222-2222-4222-8222-222222222222", "second"
	cfg.Providers = append(cfg.Providers, second)
	if _, err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	beforeOrder := m.Snapshot().Providers()
	if _, err := m.ReorderProviders([]string{second.ID, before.ID}, m.Snapshot().ConfigETag()); err != nil {
		t.Fatal(err)
	}
	afterOrder := m.Snapshot().Providers()
	if afterOrder[0].HealthEpoch != beforeOrder[1].HealthEpoch || afterOrder[1].HealthEpoch != beforeOrder[0].HealthEpoch {
		t.Fatal("reorder changed health epochs")
	}
	cfg.Providers = cfg.Providers[1:]
	if _, err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = append(cfg.Providers, before.Config())
	if _, err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Providers()[1].HealthEpoch <= before.HealthEpoch {
		t.Fatal("recreated provider reused a retired health epoch")
	}
}

func TestProviderHealthEpochDoesNotPublishFailedOrPendingChanges(t *testing.T) {
	m, store, cfg := newRuntimeManager(t, Options{})
	failing := &failingConfigStore{Store: store, failSave: true}
	m.store = failing
	before := m.Snapshot()
	cfg.Providers[0].Enabled = false
	if _, err := m.Apply(cfg); err == nil || m.Snapshot() != before {
		t.Fatal("failed save published a health reset")
	}
	failing.failSave = false
	invalid := cfg.Clone()
	invalid.Providers[0].Models = []string{"model", "model"}
	if _, err := m.Apply(invalid); err == nil || m.Snapshot() != before {
		t.Fatal("invalid candidate published a health reset")
	}
	cfg.Service.LogMaxBytes++
	result, err := m.Apply(cfg)
	if err != nil || !result.RestartRequired || m.Snapshot() != before {
		t.Fatal("pending candidate published a health reset", err)
	}
	if err := m.RestartSucceeded(); err != nil {
		t.Fatal(err)
	}
	if p := m.Snapshot().Providers()[0]; p.Enabled || p.HealthEpoch <= before.Providers()[0].HealthEpoch {
		t.Fatal("committed pending change did not reset health")
	}
}
