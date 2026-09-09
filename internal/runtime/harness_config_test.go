package runtime

import (
	"errors"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
)

var _ HarnessConfigRuntime = (*Manager)(nil)

func TestHarnessRuntimePersistsActiveIDAndReconcilesClientUpdates(t *testing.T) {
	manager, store, _ := newRuntimeManager(t, Options{})
	profile := config.Profile{
		ID:          "22222222-2222-4222-8222-222222222222",
		Name:        "Daily",
		HaikuModel:  "haiku",
		SonnetModel: "sonnet",
		OpusModel:   "opus",
		FableModel:  "fable",
	}
	if err := manager.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			h.ClaudeCode.Profiles = []config.Profile{profile}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.SetActiveProfileID(profile.ID)
	}); err != nil {
		t.Fatalf("SetActiveProfileID() error = %v", err)
	}
	if got := manager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != profile.ID {
		t.Fatalf("published active ID = %q", got)
	}
	persisted, err := store.Load()
	if err != nil || persisted.Harnesses.ClaudeCode.ActiveProfileID != profile.ID {
		t.Fatalf("persisted active ID = %q, err %v", persisted.Harnesses.ClaudeCode.ActiveProfileID, err)
	}

	clientUpdate := manager.Config()
	clientUpdate.Harnesses.ClaudeCode.ActiveProfileID = ""
	clientUpdate.Providers[0].Priority = 9
	if _, err := manager.Apply(clientUpdate); err != nil {
		t.Fatalf("unrelated client update error = %v", err)
	}
	if got := manager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != profile.ID {
		t.Fatalf("unrelated update lost active ID: %q", got)
	}

	clientUpdate = manager.Config()
	clientUpdate.Harnesses.ClaudeCode.ActiveProfileID = ""
	clientUpdate.Harnesses.ClaudeCode.Profiles[0].Name = "Renamed"
	if _, err := manager.Apply(clientUpdate); err != nil {
		t.Fatalf("profile rename error = %v", err)
	}
	if got := manager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != profile.ID {
		t.Fatalf("profile rename lost active ID: %q", got)
	}

	clientUpdate = manager.Config()
	clientUpdate.Harnesses.ClaudeCode.ActiveProfileID = ""
	clientUpdate.Harnesses.ClaudeCode.Profiles[0].SonnetModel = "changed"
	if _, err := manager.Apply(clientUpdate); err != nil {
		t.Fatalf("active profile client update error = %v", err)
	}
	if got := manager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
		t.Fatalf("active profile change retained active ID: %q", got)
	}
}

func TestHarnessRuntimeRejectsClientAndServerActiveIDReplacement(t *testing.T) {
	manager, _, _ := newRuntimeManager(t, Options{})
	profile := config.Profile{
		ID:          "22222222-2222-4222-8222-222222222222",
		Name:        "Daily",
		HaikuModel:  "haiku",
		SonnetModel: "sonnet",
		OpusModel:   "opus",
		FableModel:  "fable",
	}
	if err := manager.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			h.ClaudeCode.Profiles = []config.Profile{profile}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.SetActiveProfileID(profile.ID)
	}); err != nil {
		t.Fatal(err)
	}

	client := manager.Config()
	if _, err := manager.Apply(client); !errors.Is(err, config.ErrActiveProfileReadOnly) {
		t.Fatalf("client active ID error = %v", err)
	}
	if err := manager.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			h.ClaudeCode.ActiveProfileID = "33333333-3333-4333-8333-333333333333"
			return nil
		})
	}); !errors.Is(err, config.ErrActiveProfileReadOnly) {
		t.Fatalf("server active ID replacement error = %v", err)
	}
}
