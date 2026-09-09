package harnessconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	runtimeconfig "github.com/Siriusrry/cc-automux/internal/runtime"
)

const (
	testProfileOneID = "11111111-1111-4111-8111-111111111111"
	testProfileTwoID = "22222222-2222-4222-8222-222222222222"
)

func managerProfile(id, name string) config.Profile {
	return config.Profile{
		ID:                   id,
		Name:                 name,
		HaikuModel:           "haiku-" + name,
		SonnetModel:          "sonnet-" + name,
		OpusModel:            "opus-" + name,
		FableModel:           "fable-" + name,
		SubagentModel:        "subagent-" + name,
		TeammateDefaultModel: "teammate-" + name,
	}
}

type harnessTestFixture struct {
	harness  *Manager
	runtime  *runtimeconfig.Manager
	store    *config.Store
	config   config.Config
	home     string
	target   string
	adapter  *ClaudeCodeAdapter
	profiles []config.Profile
}

// newHarnessFixture builds a manager over a temporary runtime configuration and
// a temporary home directory. An optional FileOps replaces the target file
// backend so read failures can be injected after a successful activation.
func newHarnessFixture(t *testing.T, gatewayKey string, files ...FileOps) harnessTestFixture {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "runtime", "config.json")
	store, err := config.NewStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Service.ListenAddr = "127.0.0.1:8765"
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = gatewayKey
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimeconfig.NewManager(store, cfg, runtimeconfig.Options{
		RuntimeContext: context,
		Preflight:      func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return home, nil })
	adapterRegistry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	harness, err := NewManager(runtimeManager, adapterRegistry, managerOptionsForFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	profiles := []config.Profile{managerProfile(testProfileOneID, "one"), managerProfile(testProfileTwoID, "two")}
	for _, profile := range profiles {
		if _, err := harness.CreateProfile(ClaudeCodeAdapterID, profile); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(home, ".claude", "settings.json")
	return harnessTestFixture{
		harness:  harness,
		runtime:  runtimeManager,
		store:    store,
		config:   cfg,
		home:     home,
		target:   target,
		adapter:  adapter,
		profiles: profiles,
	}
}

func managerOptionsForFixture(files []FileOps) ManagerOptions {
	if len(files) == 0 || files[0] == nil {
		return ManagerOptions{}
	}
	return ManagerOptions{FileStore: NewFileStore(FileStoreOptions{FS: files[0]})}
}

func writeSettingsFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestManagerActivationReconcileAndIdempotence(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	original := []byte(`{"custom":{"keep":true},"env":{"KEEP_VALUE":42}}`)
	writeSettingsFixture(t, fixture.target, original)

	first, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID)
	if err != nil {
		t.Fatalf("first activation error = %v", err)
	}
	if !first.Active || first.Idempotent || first.Harness.State != StateInSync || first.Harness.ActiveProfileID != testProfileOneID {
		t.Fatalf("first activation result = %#v", first)
	}
	if got, err := fixture.store.Load(); err != nil || got.Harnesses.ClaudeCode.ActiveProfileID != testProfileOneID {
		t.Fatalf("persisted active ID = %q, err %v", got.Harnesses.ClaudeCode.ActiveProfileID, err)
	}
	backup := readFixture(t, BackupPath(fixture.target))
	if !bytes.Equal(backup, original) {
		t.Fatalf("backup = %q, want %q", backup, original)
	}

	before := readFixture(t, fixture.target)
	second, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID)
	if err != nil {
		t.Fatalf("idempotent activation error = %v", err)
	}
	if !second.Active || !second.Idempotent || second.Harness.State != StateInSync {
		t.Fatalf("idempotent result = %#v", second)
	}
	if after := readFixture(t, fixture.target); !bytes.Equal(after, before) {
		t.Fatal("idempotent activation rewrote target")
	}
	if after := readFixture(t, BackupPath(fixture.target)); !bytes.Equal(after, original) {
		t.Fatal("idempotent activation changed one-time backup")
	}

	status, err := fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateInSync || status.ActiveProfileID != testProfileOneID || status.ProfileCount != 2 {
		t.Fatalf("status = %#v", status)
	}
	profiles, err := fixture.harness.Profiles(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || !profiles[0].Active || profiles[1].Active {
		t.Fatalf("profile views = %#v", profiles)
	}
}

func TestManagerReconcileClearsManualManagedChangesButPreservesUnknownFields(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	writeSettingsFixture(t, fixture.target, []byte(`{"env":{"KEEP":"value"}}`))
	if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(readFixture(t, fixture.target), &document); err != nil {
		t.Fatal(err)
	}
	document["unknown"] = json.RawMessage(`{"preserve":[true,2]}`)
	unknownChanged, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeSettingsFixture(t, fixture.target, append(unknownChanged, '\n'))
	status, err := fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateInSync || status.ActiveProfileID != testProfileOneID {
		t.Fatalf("unknown-field status = %#v", status)
	}
	if !bytes.Contains(readFixture(t, fixture.target), []byte(`"unknown"`)) {
		t.Fatal("unknown field was lost")
	}

	if err := json.Unmarshal(readFixture(t, fixture.target), &document); err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(document["env"], &env); err != nil {
		t.Fatal(err)
	}
	env[EnvAnthropicDefaultOpusModel] = json.RawMessage(`"manually-changed"`)
	document["env"], err = json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	managedChanged, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeSettingsFixture(t, fixture.target, append(managedChanged, '\n'))
	status, err = fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateOutOfSync || status.ActiveProfileID != "" || status.LastInvalidationReason != reasonProjectionMismatch {
		t.Fatalf("managed-field status = %#v", status)
	}
	if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
		t.Fatalf("runtime active ID after invalidation = %q", got)
	}
}

func TestManagerReconcileClearsEveryManagedField(t *testing.T) {
	cases := []struct {
		name       string
		optional   bool
		mutateJSON func(t *testing.T, top map[string]json.RawMessage)
	}{
		{name: EnvAnthropicBaseURL, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicBaseURL, `"http://127.0.0.1:9999"`)
		}},
		{name: EnvAnthropicAuthToken, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicAuthToken, `"manually-changed"`)
		}},
		{name: EnvAnthropicDefaultHaikuModel, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicDefaultHaikuModel, `"manually-changed"`)
		}},
		{name: EnvAnthropicDefaultSonnetModel, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicDefaultSonnetModel, `"manually-changed"`)
		}},
		{name: EnvAnthropicDefaultOpusModel, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicDefaultOpusModel, `"manually-changed"`)
		}},
		{name: EnvAnthropicDefaultFableModel, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvAnthropicDefaultFableModel, `"manually-changed"`)
		}},
		{name: EnvClaudeCodeAttributionHeader, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvClaudeCodeAttributionHeader, `"manually-changed"`)
		}},
		{name: EnvDisableFeedbackCommand, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvDisableFeedbackCommand, `"manually-changed"`)
		}},
		{name: EnvDisableErrorReporting, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvDisableErrorReporting, `"manually-changed"`)
		}},
		{name: EnvDisableTelemetry, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvDisableTelemetry, `"manually-changed"`)
		}},
		{name: "optional subagent", mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvClaudeCodeSubagentModel, `"manually-changed"`)
		}},
		{name: "optional teammate", mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			top[TopLevelTeammateDefaultModel] = json.RawMessage(`"manually-changed"`)
		}},
		{name: "unset optional fields", optional: true, mutateJSON: func(t *testing.T, top map[string]json.RawMessage) {
			mutateManagedEnv(t, top, EnvClaudeCodeSubagentModel, `"stale-subagent"`)
			top[TopLevelTeammateDefaultModel] = json.RawMessage(`"stale-teammate"`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHarnessFixture(t, "gateway-key")
			if tc.optional {
				profile := fixture.profiles[0]
				profile.SubagentModel = ""
				profile.TeammateDefaultModel = ""
				if _, err := fixture.harness.UpdateProfile(ClaudeCodeAdapterID, profile.ID, profile); err != nil {
					t.Fatal(err)
				}
			}
			writeSettingsFixture(t, fixture.target, []byte(`{"preserve":true}`))
			if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(readFixture(t, fixture.target), &top); err != nil {
				t.Fatal(err)
			}
			tc.mutateJSON(t, top)
			data, err := json.Marshal(top)
			if err != nil {
				t.Fatal(err)
			}
			writeSettingsFixture(t, fixture.target, data)

			status, err := fixture.harness.Status(ClaudeCodeAdapterID)
			if err != nil {
				t.Fatal(err)
			}
			if status.State != StateOutOfSync || status.ActiveProfileID != "" || status.LastInvalidationReason != reasonProjectionMismatch {
				t.Fatalf("status after %s mutation = %#v", tc.name, status)
			}
			if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
				t.Fatalf("runtime active ID after %s mutation = %q", tc.name, got)
			}
		})
	}
}

func mutateManagedEnv(t *testing.T, top map[string]json.RawMessage, key, value string) {
	t.Helper()
	var env map[string]json.RawMessage
	if raw, ok := top["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
	} else {
		env = map[string]json.RawMessage{}
	}
	env[key] = json.RawMessage([]byte(value))
	encoded, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	top["env"] = encoded
}

func TestManagerErrorClassificationKeepsProjectionFailuresOutOfPathCategory(t *testing.T) {
	if state, reason := classifyPathError(ErrPathConflict); state != StateInvalid || reason != reasonPathConflict {
		t.Fatalf("path conflict classification = %v/%q", state, reason)
	}
	if state, reason := classifyPathError(ErrInvalidPathConfig); state != StateInvalid || reason != reasonPathInvalid {
		t.Fatalf("path validation classification = %v/%q", state, reason)
	}
	if state, reason := classifyProjectionError(ErrGatewayKeyRequired); state != StateOutOfSync || reason != reasonGatewayMissing {
		t.Fatalf("gateway classification = %v/%q", state, reason)
	}
	for _, err := range []error{ErrInvalidListenAddr, ErrInvalidActivation, ErrInvalidModel, ErrInvalidProjection, errors.New("adapter failure")} {
		if state, reason := classifyProjectionError(err); state != StateInvalid || reason != reasonProjectionInvalid {
			t.Fatalf("projection error %v classification = %v/%q", err, state, reason)
		}
	}
}

func TestManagerHomeResolutionFailureClearsActiveState(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	writeSettingsFixture(t, fixture.target, []byte(`{"env":{}}`))
	if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	brokenAdapter := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) {
		return "", errors.New("user home is unavailable")
	})
	brokenRegistry, err := NewRegistry(brokenAdapter)
	if err != nil {
		t.Fatal(err)
	}
	brokenManager, err := NewManager(fixture.runtime, brokenRegistry)
	if err != nil {
		t.Fatal(err)
	}
	status, err := brokenManager.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateInvalid || status.ActiveProfileID != "" || status.LastInvalidationReason != reasonPathInvalid {
		t.Fatalf("home resolution failure status = %#v", status)
	}
	if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
		t.Fatalf("home resolution failure left active ID %q", got)
	}
}

func TestManagerSwitchClearsOldActiveBeforeTargetFailure(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	writeSettingsFixture(t, fixture.target, []byte(`{"env":{}}`))
	if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	// Make the current target invalid. Reconciliation must clear the old ID,
	// then the attempted activation must fail without restoring it.
	writeSettingsFixture(t, fixture.target, []byte(`{"env":`))
	before := readFixture(t, fixture.target)
	_, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileTwoID)
	if !errors.Is(err, ErrHarnessConfigConflict) {
		t.Fatalf("activation error = %v, want target conflict", err)
	}
	if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
		t.Fatalf("old active ID was restored: %q", got)
	}
	if after := readFixture(t, fixture.target); !bytes.Equal(after, before) {
		t.Fatal("failed activation changed invalid target")
	}
}

type failingConfigStore struct {
	*config.Store
	mu       sync.Mutex
	failNext bool
	failure  error
}

func (s *failingConfigStore) Save(value config.Config) error {
	s.mu.Lock()
	fail := s.failNext
	if fail {
		s.failNext = false
	}
	failure := s.failure
	s.mu.Unlock()
	if fail {
		return failure
	}
	return s.Store.Save(value)
}

func (s *failingConfigStore) setFailNext(failure error) {
	s.mu.Lock()
	s.failNext = failure != nil
	s.failure = failure
	s.mu.Unlock()
}

func TestManagerFinalActivePersistenceFailureStaysInactiveAndCanRetry(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	baseStore, err := config.NewStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Service.ListenAddr = "127.0.0.1:8765"
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	if err := baseStore.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store := &failingConfigStore{Store: baseStore, failure: errors.New("injected active state failure")}
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimeconfig.NewManager(store, cfg, runtimeconfig.Options{
		RuntimeContext: context,
		Preflight:      func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	adapter := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return home, nil })
	adapterRegistry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	harness, err := NewManager(runtimeManager, adapterRegistry)
	if err != nil {
		t.Fatal(err)
	}
	profile := managerProfile(testProfileOneID, "one")
	if _, err := harness.CreateProfile(ClaudeCodeAdapterID, profile); err != nil {
		t.Fatal(err)
	}
	store.setFailNext(errors.New("injected active state failure"))
	if _, err := harness.Activate(ClaudeCodeAdapterID, profile.ID); !errors.Is(err, ErrActiveProfileStateFailed) {
		t.Fatalf("activation error = %v, want active state failure", err)
	}
	if got := runtimeManager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
		t.Fatalf("failed final state recorded active ID %q", got)
	}
	target := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	store.setFailNext(nil)
	result, err := harness.Activate(ClaudeCodeAdapterID, profile.ID)
	if err != nil || !result.Active {
		t.Fatalf("retry activation = %#v, err %v", result, err)
	}
	// A failed reconciliation clear must leave the persisted active ID intact
	// but report a fail-closed state; the next read retries the clear.
	target = filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(target, []byte(`{"env":{"MANUALLY_CHANGED":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.setFailNext(errors.New("injected clear failure"))
	state, err := harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateError || state.ActiveProfileID != "" || state.LastInvalidationReason != reasonStatePersistence {
		t.Fatalf("failed reconciliation clear status = %#v", state)
	}
	if got := runtimeManager.Config().Harnesses.ClaudeCode.ActiveProfileID; got != profile.ID {
		t.Fatalf("failed reconciliation clear changed runtime active ID to %q", got)
	}
	store.setFailNext(nil)
	state, err = harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateOutOfSync || state.ActiveProfileID != "" {
		t.Fatalf("retry reconciliation clear status = %#v", state)
	}
}

func TestManagerRejectsGatewayMissingProtectedPathAndRestartPending(t *testing.T) {
	missingGateway := newHarnessFixture(t, "")
	_, err := missingGateway.harness.Activate(ClaudeCodeAdapterID, testProfileOneID)
	if !errors.Is(err, ErrGatewayKeyRequired) {
		t.Fatalf("empty gateway activation error = %v", err)
	}
	if _, statErr := os.Stat(missingGateway.target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty gateway target stat = %v", statErr)
	}

	protected := newHarnessFixture(t, "gateway-key")
	if err := protected.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			h.ClaudeCode.PathMode = config.PathModeCustom
			h.ClaudeCode.SettingsPath = protected.runtime.ConfigPath()
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := protected.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); !errors.Is(err, ErrHarnessConfigConflict) {
		t.Fatalf("protected path activation error = %v", err)
	}

	pending := newHarnessFixture(t, "gateway-key")
	if _, err := pending.runtime.Apply(func() config.Config {
		cfg := pending.runtime.Config()
		cfg.Service.LogMaxBytes++
		return cfg
	}()); err != nil {
		t.Fatal(err)
	}
	// The restart is pending immediately after Apply; no target operation may
	// begin until the pending transaction is resolved.
	if _, err := pending.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); !errors.Is(err, runtimeconfig.ErrRestartInProgress) {
		t.Fatalf("pending restart activation error = %v", err)
	}
}

func TestManagerConcurrentActivationsLeaveOneCoherentVerifiedState(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	writeSettingsFixture(t, fixture.target, []byte(`{"preserve":true}`))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{testProfileOneID, testProfileTwoID} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fixture.harness.Activate(ClaudeCodeAdapterID, id)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent activation error = %v", err)
		}
	}
	status, err := fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateInSync || (status.ActiveProfileID != testProfileOneID && status.ActiveProfileID != testProfileTwoID) {
		t.Fatalf("concurrent final status = %#v", status)
	}
	cfg := fixture.runtime.Config()
	profile, ok := (&Manager{}).profileFor(cfg, status.ActiveProfileID)
	if !ok {
		t.Fatal("final active profile is absent")
	}
	projection, err := fixture.adapter.BuildManagedProjection(activationInput(cfg, profile))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.adapter.Verify(readFixture(t, fixture.target), projection); err != nil {
		t.Fatalf("final target does not match active profile: %v", err)
	}
}

func TestManagerStateClassificationAndDiscovery(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	status, err := fixture.harness.Reconcile(ClaudeCodeAdapterID)
	if err != nil || status.State != StateInactive || status.ActiveProfileID != "" {
		t.Fatalf("initial status = %#v, err %v", status, err)
	}
	items := fixture.harness.Discover()
	if len(items) != 1 || items[0].ID != ClaudeCodeAdapterID || !items[0].ProfileSupport || len(items[0].PathModes) != 2 {
		t.Fatalf("discovery = %#v", items)
	}
	writeSettingsFixture(t, fixture.target, []byte(`{"env":{}}`))
	if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.target); err != nil {
		t.Fatal(err)
	}
	status, err = fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.State != StateMissing || status.ActiveProfileID != "" {
		t.Fatalf("missing status = %#v, err %v", status, err)
	}
	if _, err := fixture.harness.GetProfile(ClaudeCodeAdapterID, "missing"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
}

// A target that is still present but unusable must clear the active record and
// report the specific reason. Type, size and path rejections are structural and
// stay invalid; every other read failure is transient and stays unreadable.
func TestManagerClassifiesUnusableTargetStates(t *testing.T) {
	structural := []struct {
		name    string
		prepare func(t *testing.T, target string)
	}{
		{name: "symlink", prepare: func(t *testing.T, target string) {
			elsewhere := filepath.Join(filepath.Dir(target), "elsewhere.json")
			if err := os.WriteFile(elsewhere, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, target); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
		{name: "directory", prepare: func(t *testing.T, target string) {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized", prepare: func(t *testing.T, target string) {
			oversized := make([]byte, MaxExistingTargetBytes+1)
			oversized[0] = '{'
			oversized[len(oversized)-1] = '}'
			if err := os.WriteFile(target, oversized, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range structural {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHarnessFixture(t, "gateway-key")
			if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, fixture.target)
			status, err := fixture.harness.Status(ClaudeCodeAdapterID)
			if err != nil {
				t.Fatal(err)
			}
			if status.State != StateInvalid || status.ActiveProfileID != "" || status.LastInvalidationReason != reasonTargetInvalid {
				t.Fatalf("status for %s target = %#v", tc.name, status)
			}
			if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
				t.Fatalf("runtime active ID for %s target = %q", tc.name, got)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		ops := &faultOps{}
		fixture := newHarnessFixture(t, "gateway-key", ops)
		if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
			t.Fatal(err)
		}
		ops.failTargetOpen = errors.New("injected target open failure")
		status, err := fixture.harness.Status(ClaudeCodeAdapterID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != StateUnreadable || status.ActiveProfileID != "" || status.LastInvalidationReason != reasonTargetUnreadable {
			t.Fatalf("unreadable target status = %#v", status)
		}
		if got := fixture.runtime.Config().Harnesses.ClaudeCode.ActiveProfileID; got != "" {
			t.Fatalf("runtime active ID for unreadable target = %q", got)
		}
		// The target itself is untouched, so restoring readability leaves the
		// file matching the projection while the cleared record stays cleared.
		ops.failTargetOpen = nil
		status, err = fixture.harness.Status(ClaudeCodeAdapterID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != StateInactive || status.ActiveProfileID != "" {
			t.Fatalf("recovered target status = %#v", status)
		}
	})
}

func TestManagerProfileCRUDPreservesActiveNonTargetProfile(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	// The fixture already contains two profiles; remove the second and recreate
	// it through the public Manager CRUD seam to exercise the full transaction.
	if err := fixture.harness.DeleteProfile(ClaudeCodeAdapterID, testProfileTwoID); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.harness.CreateProfile(ClaudeCodeAdapterID, managerProfile(testProfileTwoID, "two-recreated"))
	if err != nil || created.ID != testProfileTwoID {
		t.Fatalf("recreated profile = %#v, err %v", created, err)
	}
	writeSettingsFixture(t, fixture.target, []byte(`{"env":{}}`))
	if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
		t.Fatal(err)
	}
	updated := managerProfile(testProfileTwoID, "two-updated")
	if _, err := fixture.harness.UpdateProfile(ClaudeCodeAdapterID, testProfileTwoID, updated); err != nil {
		t.Fatal(err)
	}
	status, err := fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil || status.State != StateInSync || status.ActiveProfileID != testProfileOneID {
		t.Fatalf("non-active profile update invalidated active = %#v, err %v", status, err)
	}
	if err := fixture.harness.DeleteProfile(ClaudeCodeAdapterID, testProfileOneID); !errors.Is(err, ErrActiveProfile) {
		t.Fatalf("delete active profile error = %v", err)
	}
	if _, err := fixture.harness.UpdateProfile(ClaudeCodeAdapterID, testProfileTwoID, config.Profile{ID: "33333333-3333-4333-8333-333333333333", Name: "bad", HaikuModel: "h", SonnetModel: "s", OpusModel: "o", FableModel: "f"}); !errors.Is(err, ErrProfileIDImmutable) {
		t.Fatalf("profile ID mutation error = %v", err)
	}
}

func TestManagerRenamePreservesOnlyVerifiedActiveProfile(t *testing.T) {
	for _, test := range []struct {
		name         string
		changeModel  bool
		changeTarget bool
		wantActive   bool
	}{
		{name: "rename only", wantActive: true},
		{name: "rename and model change", changeModel: true},
		{name: "rename after external change", changeTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarnessFixture(t, "gateway-key")
			if _, err := fixture.harness.Activate(ClaudeCodeAdapterID, testProfileOneID); err != nil {
				t.Fatal(err)
			}
			if test.changeTarget {
				writeSettingsFixture(t, fixture.target, []byte(`{"env":{}}`))
			}
			before := readFixture(t, fixture.target)
			profile := fixture.profiles[0]
			profile.Name = "Renamed"
			if test.changeModel {
				profile.SonnetModel = "changed-model"
			}
			updated, err := fixture.harness.UpdateProfile(ClaudeCodeAdapterID, profile.ID, profile)
			if err != nil || updated.Name != "Renamed" || updated.Active != test.wantActive {
				t.Fatalf("renamed profile = %#v, err %v", updated, err)
			}
			if !bytes.Equal(before, readFixture(t, fixture.target)) {
				t.Fatal("profile edit rewrote the settings file")
			}
			wantID := ""
			if test.wantActive {
				wantID = profile.ID
			}
			status, err := fixture.harness.Status(ClaudeCodeAdapterID)
			if err != nil || status.ActiveProfileID != wantID || (status.State == StateInSync) != test.wantActive {
				t.Fatalf("status after rename = %#v, err %v", status, err)
			}
			persisted, err := fixture.store.Load()
			if err != nil || persisted.Harnesses.ClaudeCode.ActiveProfileID != wantID || persisted.Harnesses.ClaudeCode.Profiles[0].Name != "Renamed" {
				t.Fatalf("persisted harness = %#v, err %v", persisted.Harnesses.ClaudeCode, err)
			}
		})
	}
}

func TestManagerErrorMessagesDoNotIncludeTargetContents(t *testing.T) {
	fixture := newHarnessFixture(t, "gateway-key")
	secret := "target-secret-that-must-not-be-returned"
	writeSettingsFixture(t, fixture.target, []byte(fmt.Sprintf(`{"env":%q}`, secret)))
	status, err := fixture.harness.Status(ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if strings := status.LastInvalidationReason; strings == secret {
		t.Fatal("status exposed target content")
	}
}
