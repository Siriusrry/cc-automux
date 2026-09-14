package harnessconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	runtimeconfig "github.com/Siriusrry/cc-automux/internal/runtime"
)

type countingReadOps struct {
	OSFileOps
	reads int
}

func (o *countingReadOps) Open(path string) (FileHandle, error) {
	o.reads++
	return o.OSFileOps.Open(path)
}

type countingSaveStore struct {
	*config.Store
	saves int
	fail  bool
}

func (s *countingSaveStore) Save(cfg config.Config) error {
	s.saves++
	if s.fail {
		return errors.New("injected persistence failure")
	}
	return s.Store.Save(cfg)
}

func TestCandidateValidationUsesOneReadWriteAndRevision(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "drift"}[drift], func(t *testing.T) {
			base := newHarnessFixture(t, "gateway-key")
			if _, err := base.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			reads := &countingReadOps{}
			store := &countingSaveStore{Store: base.store}
			validator, err := NewValidator(base.harness.registry, NewFileStore(FileStoreOptions{FS: reads}), []string{base.store.Path()})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := runtimeconfig.NewManager(store, base.runtime.Config(), runtimeconfig.Options{RuntimeContext: base.runtime.RuntimeContext(), HarnessValidator: validator})
			if err != nil {
				t.Fatal(err)
			}
			harness, _ := NewManager(manager, validator)
			if _, err := harness.Status(ClaudeCodeAdapterID); err != nil {
				t.Fatal(err)
			}
			if drift {
				if err := os.WriteFile(base.target, []byte(`{"env":{}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reads.reads = 0
			store.saves = 0
			before := manager.Snapshot().Revision()
			p := manager.Config().Harnesses.ClaudeCode.Profiles[0]
			p.MaxAttempts = 8
			updated, err := harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p)
			if err != nil {
				t.Fatal(err)
			}
			if reads.reads != 1 || store.saves != 1 || manager.Snapshot().Revision() != before+1 {
				t.Fatalf("reads=%d writes=%d revisions=%d", reads.reads, store.saves, manager.Snapshot().Revision()-before)
			}
			if updated.Active == drift {
				t.Fatalf("active=%v drift=%v", updated.Active, drift)
			}
			if drift && manager.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 {
				t.Fatal("drift budget survived")
			}
			if !drift {
				reads.reads = 0
				store.saves = 0
				before = manager.Snapshot().Revision()
				if err := harness.DeleteProfile(ClaudeCodeAdapterID, testProfileTwoID); err != nil {
					t.Fatal(err)
				}
				if reads.reads != 1 || store.saves != 1 || manager.Snapshot().Revision() != before+1 {
					t.Fatalf("delete reads=%d writes=%d revisions=%d", reads.reads, store.saves, manager.Snapshot().Revision()-before)
				}
			}

		})
	}
}

func TestValidatorChecksSuppliedCandidateAndPropagatesLookupError(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	candidate := f.runtime.Config()
	candidate.Harnesses.ClaudeCode.Profiles[0].SonnetModel = "changed"
	result, err := f.harness.Validator.Check(candidate)
	if err != nil || result.State != string(StateOutOfSync) {
		t.Fatalf("candidate result=%#v %v", result, err)
	}
	if result, err := (&Validator{registry: EmptyRegistry()}).Check(candidate); !errors.Is(err, ErrHarnessNotFound) {
		t.Fatalf("missing adapter=%#v %v", result, err)
	}
	if f.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID == "" {
		t.Fatal("stateless verification mutated runtime")
	}
}

func TestStartupUnreadableAndInvalidTargetsUseDefaultBudget(t *testing.T) {
	for _, kind := range []string{"invalid", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			f := newHarnessFixture(t, "gateway-key")
			p := f.profiles[0]
			p.MaxAttempts = 8
			if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
				t.Fatal(err)
			}
			if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
				t.Fatal(err)
			}
			files := NewFileStore()
			if kind == "invalid" {
				os.WriteFile(f.target, []byte(`{`), 0600)
			} else {
				files = NewFileStore(FileStoreOptions{FS: &faultOps{failTargetOpen: errors.New("unreadable")}})
			}
			validator, _ := NewValidator(f.harness.registry, files, []string{filepath.Join(f.home, "unused")})
			manager, err := runtimeconfig.NewManager(f.store, f.runtime.Config(), runtimeconfig.Options{RuntimeContext: f.runtime.RuntimeContext(), HarnessValidator: validator})
			if err != nil {
				t.Fatal(err)
			}
			harness, _ := NewManager(manager, validator)
			status, err := harness.Status(ClaudeCodeAdapterID)
			if err != nil || status.NormalMaxAttempts != 3 || status.AttemptPolicySource != "default" || status.ActiveProfileID != "" {
				t.Fatalf("status=%#v %v", status, err)
			}
		})
	}
}

func TestActivationRejectsDriftAtFinalCandidateCheck(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	err := f.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error { return tx.SetActiveProfileID(testProfileOneID) })
	if !errors.Is(err, config.ErrActiveProfileStateFailed) {
		t.Fatalf("unverified activation accepted: %v", err)
	}
	if f.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID != "" || f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 3 {
		t.Fatal("unverified activation published")
	}
}

func TestRestoredFileLiftsFailedClearGuard(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	p := f.profiles[0]
	p.MaxAttempts = 8
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingSaveStore{Store: f.store}
	manager, err := runtimeconfig.NewManager(store, f.runtime.Config(), runtimeconfig.Options{RuntimeContext: f.runtime.RuntimeContext(), HarnessValidator: f.harness.Validator})
	if err != nil {
		t.Fatal(err)
	}
	harness, _ := NewManager(manager, f.harness.Validator)
	if _, err := harness.Status(ClaudeCodeAdapterID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.target, []byte(`{"env":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	status, err := harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.State != StateError || status.NormalMaxAttempts != 3 {
		t.Fatalf("failed clear=%#v %v", status, err)
	}
	if err := os.WriteFile(f.target, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Clearing failed, so the original ID still exists. Restoring the file may
	// reverify it even while persistence is still unavailable.
	status, err = harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.State != StateInSync || status.NormalMaxAttempts != 8 || status.ActiveProfileID != p.ID {
		t.Fatalf("restored=%#v %v", status, err)
	}
}

func TestRenameRetainsActiveBudget(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	p := f.profiles[0]
	p.MaxAttempts = 8
	if _, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, p.ID); err != nil {
		t.Fatal(err)
	}
	p.Name = "Renamed"
	saved, err := f.harness.UpdateProfile(ClaudeCodeAdapterID, p.ID, p)
	if err != nil || !saved.Active || saved.MaxAttempts != 8 || f.runtime.Snapshot().NormalAttemptPolicy().MaxAttempts != 8 {
		t.Fatalf("renamed=%#v %v", saved, err)
	}
}
