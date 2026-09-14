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
	if err != nil || status.NormalMaxAttempts != 3 || status.NormalStickyNoCooldownAttempts != 1 || status.AttemptPolicySource != "default" {
		t.Fatalf("inactive: %#v %v", status, err)
	}
	p := f.profiles[0]
	p.MaxAttempts = 8
	p.StickyNoCooldownAttempts = 5
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 1 {
		t.Fatal("inactive edit changed effective budget")
	}
	result, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID)
	if err != nil || result.Harness.NormalMaxAttempts != 8 || result.Harness.NormalStickyNoCooldownAttempts != 5 || result.Harness.AttemptPolicySource != "profile" {
		t.Fatalf("activation: %#v %v", result, err)
	}
	before, _ := os.Stat(f.target)
	data, _ := os.ReadFile(f.target)
	old := f.runtime.Snapshot()
	p.MaxAttempts = 2
	p.StickyNoCooldownAttempts = 4
	saved, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p)
	if err != nil || !saved.Active || saved.MaxAttempts != 2 || saved.StickyNoCooldownAttempts != 4 {
		t.Fatalf("budget update: %#v %v", saved, err)
	}
	after, _ := os.Stat(f.target)
	nextData, _ := os.ReadFile(f.target)
	if !os.SameFile(before, after) || !bytes.Equal(data, nextData) {
		t.Fatal("budget edit rewrote external settings")
	}
	if old.NormalAttemptPolicy().MaxAttempts != 8 || old.NormalAttemptPolicy().StickyNoCooldownAttempts != 5 || f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 4 {
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
	other.StickyNoCooldownAttempts = 9
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, other.ID, other); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 4 {
		t.Fatal("non-active edit changed budget")
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, other.ID); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 5 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 9 {
		t.Fatal("switch did not publish")
	}
	other.SonnetModel = "changed"
	other.MaxAttempts = 7
	other.StickyNoCooldownAttempts = 6
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, other.ID, other); err != nil {
		t.Fatal(err)
	}
	status, err = f.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.ActiveProfileID != "" || status.NormalMaxAttempts != 3 || status.NormalStickyNoCooldownAttempts != 1 || status.AttemptPolicySource != "default" {
		t.Fatalf("model edit: %#v %v", status, err)
	}
}

func TestFullConfigBudgetUpdateChecksExternalDrift(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	p := f.profiles[0]
	p.MaxAttempts = 8
	p.StickyNoCooldownAttempts = 5
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
		t.Fatal(err)
	}
	cfg := f.runtime.Config()
	cfg.Harnesses.ClaudeCode.ActiveProfileID = ""
	cfg.Harnesses.ClaudeCode.Profiles[0].MaxAttempts = 2
	cfg.Harnesses.ClaudeCode.Profiles[0].StickyNoCooldownAttempts = 4
	if _, err := f.runtime.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 2 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 4 {
		t.Fatal("full config budget not applied")
	}
	if err := os.WriteFile(f.target, []byte(`{"env":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = f.runtime.Config()
	cfg.Harnesses.ClaudeCode.ActiveProfileID = ""
	cfg.Harnesses.ClaudeCode.Profiles[0].MaxAttempts = 9
	cfg.Harnesses.ClaudeCode.Profiles[0].StickyNoCooldownAttempts = 8
	if _, err := f.runtime.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 || f.runtime.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 1 || f.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatal("full config edit ignored drift")
	}
}

func TestStartupVerifiesSavedBudgetWithoutRewritingConfig(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "drifted"}[valid], func(t *testing.T) {
			f := newHarnessFixture(t, "gateway-key")
			p := f.profiles[0]
			p.MaxAttempts = 8
			p.StickyNoCooldownAttempts = 5
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
			manager, err := runtimeconfig.NewManager(f.store, cfg, runtimeconfig.Options{RuntimeContext: f.runtime.RuntimeContext(), HarnessValidator: f.harness.Validator})
			if err != nil {
				t.Fatal(err)
			}
			if manager.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 || manager.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != 1 {
				t.Fatal("unverified startup budget active")
			}
			harness, err := NewManager(manager, f.harness.Validator)
			if err != nil {
				t.Fatal(err)
			}
			status, err := harness.Status(ClaudeCodeAdapterID)
			if err != nil {
				t.Fatal(err)
			}
			want := config.DefaultNormalMaxAttempts
			wantSticky := config.DefaultStickyNoCooldownAttempts
			if valid {
				want = 8
				wantSticky = 5
			}
			if status.NormalMaxAttempts != want || status.NormalStickyNoCooldownAttempts != wantSticky || manager.Snapshot().NormalAttemptPolicy().MaxAttempts != want || manager.Snapshot().NormalAttemptPolicy().StickyNoCooldownAttempts != wantSticky {
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

func TestStickyLimitOnlyPublishesWithoutChangingClientFile(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	p := f.profiles[0]
	p.MaxAttempts = 8
	p.StickyNoCooldownAttempts = 5
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(f.target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	old := f.runtime.Snapshot()
	p.StickyNoCooldownAttempts = 7
	saved, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p)
	if err != nil || !saved.Active || saved.MaxAttempts != 8 || saved.StickyNoCooldownAttempts != 7 {
		t.Fatalf("saved=%#v err=%v", saved, err)
	}
	after, err := os.Stat(f.target)
	if err != nil {
		t.Fatal(err)
	}
	nextData, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !bytes.Equal(data, nextData) {
		t.Fatal("sticky-only edit rewrote settings")
	}
	current := f.runtime.Snapshot().NormalAttemptStatus()
	if current.MaxAttempts != 8 || current.StickyNoCooldownAttempts != 7 || current.Source != "profile" || old.NormalAttemptPolicy().StickyNoCooldownAttempts != 5 {
		t.Fatalf("effective policy=%#v", current)
	}
}
