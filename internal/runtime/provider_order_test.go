package runtime

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
)

func TestProviderOrderPersistsAcrossLoadAndFailureDoesNotPublish(t *testing.T) {
	m, store, _ := newRuntimeManager(t, Options{})
	if _, err := m.Update(func(c *config.Config) error {
		p := c.Providers[0]
		p.ID = "22222222-2222-4222-8222-222222222222"
		p.Name = "second"
		c.Providers = append(c.Providers, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{m.Config().Providers[1].ID, m.Config().Providers[0].ID}
	if _, err := m.ReorderProviders(ids, m.Snapshot().ConfigETag()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !reflect.DeepEqual(loaded, m.Config()) {
		t.Fatal("persistent order", err)
	}
	failing := &failingConfigStore{Store: store, failSave: true}
	restart, err := NewManager(failing, loaded, Options{RuntimeContext: m.RuntimeContext()})
	if err != nil {
		t.Fatal(err)
	}
	before := restart.Snapshot()
	ids[0], ids[1] = ids[1], ids[0]
	if _, err := restart.ReorderProviders(ids, before.ConfigETag()); err == nil || restart.Snapshot() != before {
		t.Fatal("failed order published")
	}
	restart.restartStatus.InProgress = true
	if _, err := restart.ReorderProviders(ids, before.ConfigETag()); !errors.Is(err, ErrRestartInProgress) {
		t.Fatal(err)
	}
}
