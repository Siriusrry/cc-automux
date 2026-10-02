package harnessconfig

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	runtimeconfig "github.com/Siriusrry/cc-automux/internal/runtime"
)

func TestTelemetryAppliesAndKeepsSingleVerifiedSnapshot(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{false, true} {
		before := f.runtime.Snapshot()
		status, err := f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled})
		if err != nil {
			t.Fatal(err)
		}
		after := f.runtime.Snapshot()
		if after.Revision() != before.Revision()+1 || status.ActiveProfileID != testProfileOneID || status.State != StateInSync || after.NormalAttemptStatus() != before.NormalAttemptStatus() {
			t.Fatalf("state: %+v", status)
		}
		projection, _ := f.adapter.BuildManagedProjection(activationInput(f.runtime.Config(), f.runtime.Config().Harnesses.ClaudeCode.Profiles[0]))
		data, _ := os.ReadFile(f.target)
		if err := f.adapter.Verify(data, projection); err != nil {
			t.Fatal(err)
		}
		disk, err := f.store.Load()
		if err != nil || disk.Harnesses.ClaudeCode.DisableTelemetry != disabled || disk.Harnesses.ClaudeCode.ActiveProfileID != testProfileOneID {
			t.Fatal("disk mismatch", err)
		}
		if _, err := f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled}); err != nil || f.runtime.Snapshot() != after {
			t.Fatal("non-idempotent update", err)
		}
	}
}

type telemetryReads struct {
	OSFileOps
	calls int
}

func (f *telemetryReads) Lstat(path string) (os.FileInfo, error) {
	f.calls++
	return f.OSFileOps.Lstat(path)
}
func TestTelemetryInactiveNeverTouchesTargetAndDriftIsNotOverwritten(t *testing.T) {
	fs := &telemetryReads{}
	f := newHarnessFixture(t, "gateway-key", fs)
	disabled := false
	fs.calls = 0
	if _, err := f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled}); err != nil {
		t.Fatal(err)
	}
	if fs.calls != 0 {
		t.Fatalf("inactive target I/O: %d", fs.calls)
	}
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	drift := []byte(`{"env":{"DISABLE_TELEMETRY":"external"},"keep":"mine"}`)
	if err := os.WriteFile(f.target, drift, 0600); err != nil {
		t.Fatal(err)
	}
	disabled = true
	status, err := f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled})
	if err != nil || status.ActiveProfileID != "" || !status.DisableTelemetry {
		t.Fatalf("drift: %+v %v", status, err)
	}
	got, _ := os.ReadFile(f.target)
	if !bytes.Equal(got, drift) {
		t.Fatal("overwrote external drift")
	}
}

type telemetryConfigStore struct {
	*config.Store
	fail func(config.Config) error
}

func (s *telemetryConfigStore) Save(c config.Config) error {
	if s.fail != nil {
		if err := s.fail(c); err != nil {
			return err
		}
	}
	return s.Store.Save(c)
}
func TestTelemetrySaveFailureRestoresOwnOriginalOrInvalidates(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "external conflict"}[conflict], func(t *testing.T) {
			f := newHarnessFixture(t, "gateway-key")
			if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(f.target)
			store := &telemetryConfigStore{Store: f.store}
			manager, err := runtimeconfig.NewManager(store, f.runtime.Config(), runtimeconfig.Options{RuntimeContext: f.runtime.RuntimeContext(), HarnessValidator: f.harness.Validator, HarnessUpdater: NewTelemetryUpdater(f.harness.Validator)})
			if err != nil {
				t.Fatal(err)
			}
			h, _ := NewManager(manager, f.harness.Validator)
			if _, err := h.Status(ClaudeCodeAdapterID); err != nil {
				t.Fatal(err)
			}
			before := manager.Snapshot()
			external := []byte(`{"changed":"outside"}`)
			store.fail = func(c config.Config) error {
				if conflict {
					_ = os.WriteFile(f.target, external, 0600)
				}
				return errors.New("save failed")
			}
			disabled := false
			if _, err := h.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled}); err == nil {
				t.Fatal("reported success")
			}
			got, _ := os.ReadFile(f.target)
			if conflict {
				if !bytes.Equal(got, external) || manager.Snapshot().NormalAttemptStatus().Source != "default" {
					t.Fatal("unsafe recovery")
				}
			} else if !bytes.Equal(got, original) || manager.Snapshot() != before {
				t.Fatal("failed to restore original snapshot and bytes")
			}
		})
	}
}

type telemetryFaults struct {
	OSFileOps
	enabled  bool
	stage    string
	target   string
	failRead bool
}

func (f *telemetryFaults) CreateTemp(dir, pattern string) (FileHandle, error) {
	if f.enabled && f.stage == "temporary" {
		return nil, errors.New("temp failed")
	}
	return f.OSFileOps.CreateTemp(dir, pattern)
}
func (f *telemetryFaults) AtomicReplace(from, to string) error {
	if f.enabled && f.stage == "replace" {
		return errors.New("replace failed")
	}
	err := f.OSFileOps.AtomicReplace(from, to)
	if f.enabled && f.stage == "readback" {
		f.failRead = true
		f.enabled = false
	}
	return err
}
func (f *telemetryFaults) Open(path string) (FileHandle, error) {
	if f.failRead && path == f.target {
		f.failRead = false
		return nil, errors.New("readback failed")
	}
	return f.OSFileOps.Open(path)
}
func TestTelemetryFileFaultsKeepOriginalState(t *testing.T) {
	for _, stage := range []string{"temporary", "replace", "readback"} {
		t.Run(stage, func(t *testing.T) {
			fs := &telemetryFaults{stage: stage}
			f := newHarnessFixture(t, "gateway-key", fs)
			fs.target = f.target
			if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			before := f.runtime.Snapshot()
			original, _ := os.ReadFile(f.target)
			fs.enabled = true
			disabled := false
			if _, err := f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled}); err == nil {
				t.Fatal("reported success")
			}
			got, _ := os.ReadFile(f.target)
			if !bytes.Equal(original, got) || f.runtime.Snapshot() != before {
				t.Fatal("file failure changed state")
			}
		})
	}
}

func TestTelemetryConcurrentUpdatesAndActivationRemainCoherent(t *testing.T) {
	f := newHarnessFixture(t, "gateway-key")
	if _, err := f.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%3 == 0 {
				_, err = f.harness.Activate(ClaudeCodeAdapterID, testProfileTwoID)
			} else {
				disabled := i%2 == 0
				_, err = f.harness.UpdatePatch(ClaudeCodeAdapterID, HarnessUpdatePatch{DisableTelemetry: &disabled})
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	status, err := f.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.State != StateInSync {
		t.Fatalf("final: %+v %v", status, err)
	}
}
