package harnessconfig

import (
	"bytes"
	"os"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	runtimeconfig "github.com/Siriusrry/cc-automux/internal/runtime"
)

func TestProfileBudgetPublicationAndExternalProjection(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	status, err := f.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.NormalMaxAttempts != 3 || status.AttemptPolicySource != "default" {
		t.Fatalf("inactive: %#v %v", status, err)
	}
	p := f.profiles[0]
	p.MaxAttempts = 8
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 {
		t.Fatal("inactive edit changed effective budget")
	}
	result, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID)
	if err != nil || result.Harness.NormalMaxAttempts != 8 || result.Harness.AttemptPolicySource != "profile" {
		t.Fatalf("activation: %#v %v", result, err)
	}
	before, _ := os.Stat(f.target)
	data, _ := os.ReadFile(f.target)
	old := f.runtime.Snapshot()
	p.MaxAttempts = 2
	saved, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p)
	if err != nil || !saved.Active || saved.MaxAttempts != 2 {
		t.Fatalf("budget update: %#v %v", saved, err)
	}
	after, _ := os.Stat(f.target)
	nextData, _ := os.ReadFile(f.target)
	if !os.SameFile(before, after) || !bytes.Equal(data, nextData) {
		t.Fatal("budget edit rewrote external settings")
	}
	if old.NormalAttemptPolicy().MaxAttempts != 8 || f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 {
		t.Fatal("budget snapshot was not replaced atomically")
	}
	revision := f.runtime.Snapshot().Revision()
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().Revision() != revision {
		t.Fatal("idempotent edit published revision")
	}
	other := f.profiles[1]
	other.MaxAttempts = 5
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, other.ID, other); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 {
		t.Fatal("non-active edit changed budget")
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, other.ID); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 5 {
		t.Fatal("switch did not publish")
	}
	other.SonnetModel = "changed"
	other.MaxAttempts = 7
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, other.ID, other); err != nil {
		t.Fatal(err)
	}
	status, err = f.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.ActiveProfileID != "" || status.NormalMaxAttempts != 3 || status.AttemptPolicySource != "default" {
		t.Fatalf("model edit: %#v %v", status, err)
	}
}

func TestFullConfigBudgetUpdateChecksExternalDrift(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	p := f.profiles[0]
	p.MaxAttempts = 8
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
		t.Fatal(err)
	}
	cfg := f.runtime.Config()
	cfg.Harnesses.ClaudeCode.ActiveProfileID = ""
	cfg.Harnesses.ClaudeCode.Profiles[0].MaxAttempts = 2
	if _, err := f.runtime.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 {
		t.Fatal("full config budget not applied")
	}
	if err := os.WriteFile(f.target, []byte(`{"env":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = f.runtime.Config()
	cfg.Harnesses.ClaudeCode.ActiveProfileID = ""
	cfg.Harnesses.ClaudeCode.Profiles[0].MaxAttempts = 9
	if _, err := f.runtime.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 || f.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatal("full config edit ignored drift")
	}
}

func TestStartupVerifiesSavedBudgetWithoutRewritingConfig(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "drifted"}[valid], func(t *testing.T) {
			f := newHarnessFixture(t, "gateway-key")
			p := f.profiles[0]
			p.MaxAttempts = 8
			if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
				t.Fatal(err)
			}
			if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
				t.Fatal(err)
			}
			if !valid {
				if err := os.Remove(f.target); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := f.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(f.store.Path())
			manager, err := runtimeconfig.NewManager(f.store, cfg, runtimeconfig.Options{RuntimeContext: f.runtime.RuntimeContext()})
			if err != nil {
				t.Fatal(err)
			}
			if manager.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 {
				t.Fatal("unverified startup budget active")
			}
			registry, _ := NewRegistry(f.adapter)
			harness, err := NewManager(manager, registry)
			if err != nil {
				t.Fatal(err)
			}
			status, err := harness.Status(ClaudeCodeAdapterID)
			if err != nil {
				t.Fatal(err)
			}
			want := config.DefaultNormalMaxAttempts
			if valid {
				want = 8
			}
			if status.NormalMaxAttempts != want || manager.Snapshot().NormalAttemptPolicy().MaxAttempts != want {
				t.Fatalf("startup state: %#v", status)
			}
			if valid {
				after, _ := os.ReadFile(f.store.Path())
				if !bytes.Equal(raw, after) {
					t.Fatal("verification rewrote config")
				}
			}
		})
	}
}
