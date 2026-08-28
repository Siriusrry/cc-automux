package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
)

func runtimeConfig() config.Config {
	cfg := config.Default()
	cfg.Auth.ManagementKey = "management-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider",
		BaseURL: "https://provider.example/anthropic",
		APIKey:  "provider-key",
		Models:  []string{"model"},
		Enabled: true,
	}}
	return cfg
}

func newRuntimeManager(t *testing.T, options Options) (*Manager, *config.Store, config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if options.Preflight == nil {
		options.Preflight = func(config.Config, config.Config) error { return nil }
	}
	manager, err := NewManager(store, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, cfg
}

func TestHotApplyPublishesOnlyAfterPersist(t *testing.T) {
	manager, store, cfg := newRuntimeManager(t, Options{})
	before := manager.Snapshot()
	next := cfg.Clone()
	next.Auth.GatewayKey = "gateway-key"
	next.Providers[0].Priority = -1
	result, err := manager.Apply(next)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !result.Applied || result.RestartRequired || result.Revision != before.Revision()+1 {
		t.Fatalf("Apply() result = %#v", result)
	}
	if got := manager.Snapshot().Config().Providers[0].Priority; got != -1 {
		t.Fatalf("snapshot priority = %d", got)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Auth.GatewayKey != "gateway-key" {
		t.Fatalf("persisted config = %#v, err %v", loaded, err)
	}

	bad := next.Clone()
	bad.Providers[0].Models = []string{"model", "model"}
	oldRevision := manager.Snapshot().Revision()
	if _, err := manager.Apply(bad); err == nil {
		t.Fatal("invalid hot candidate was accepted")
	}
	if manager.Snapshot().Revision() != oldRevision {
		t.Fatal("invalid candidate changed revision")
	}
	loaded, err = store.Load()
	if err != nil || loaded.Providers[0].Priority != -1 || len(loaded.Providers[0].Models) != 1 {
		t.Fatalf("invalid candidate changed disk: %#v, %v", loaded, err)
	}
}

type failingConfigStore struct {
	*config.Store
	failSave        bool
	failSavePending bool
}

func (s *failingConfigStore) Save(cfg config.Config) error {
	if s.failSave {
		return errors.New("disk save failed")
	}
	return s.Store.Save(cfg)
}

func (s *failingConfigStore) SavePending(cfg config.Config) error {
	if s.failSavePending {
		return errors.New("pending save failed")
	}
	return s.Store.SavePending(cfg)
}

func TestPersistenceFailureLeavesDiskAndSnapshotUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	failing := &failingConfigStore{Store: store, failSave: true}
	manager, err := NewManager(failing, cfg, Options{Preflight: func(config.Config, config.Config) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision := manager.Snapshot().Revision()
	next := cfg.Clone()
	next.Auth.GatewayKey = "gateway-key"
	if _, err := manager.Apply(next); err == nil || !strings.Contains(err.Error(), "disk save failed") {
		t.Fatalf("hot Apply() error = %v", err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Auth.GatewayKey != "" || manager.Snapshot().Revision() != beforeRevision {
		t.Fatalf("state changed after hot persistence failure: disk %#v, revision %d, err %v", loaded, manager.Snapshot().Revision(), err)
	}

	failing.failSave = false
	failing.failSavePending = true
	next = cfg.Clone()
	next.Service.LogMaxBytes++
	if _, err := manager.Apply(next); err == nil || !strings.Contains(err.Error(), "pending save failed") {
		t.Fatalf("restart Apply() error = %v", err)
	}
	if exists, err := store.PendingExists(); err != nil || exists {
		t.Fatalf("pending after persistence failure = %v, %v", exists, err)
	}
	loaded, err = store.Load()
	if err != nil || loaded.Service.LogMaxBytes != cfg.Service.LogMaxBytes || manager.Snapshot().Revision() != beforeRevision {
		t.Fatalf("state changed after pending persistence failure: disk %#v, revision %d, err %v", loaded, manager.Snapshot().Revision(), err)
	}
}

func TestRestartApplyUsesPendingAndRollsBackOnFailure(t *testing.T) {
	manager, store, cfg := newRuntimeManager(t, Options{})
	activeBefore, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	next := cfg.Clone()
	next.Service.LogMaxBytes += 1
	result, err := manager.Apply(next)
	if err != nil {
		t.Fatalf("restart Apply() error = %v", err)
	}
	if !result.RestartRequired || !result.Restarting {
		t.Fatalf("restart result = %#v", result)
	}
	if _, err := store.LoadPending(); err != nil {
		t.Fatalf("pending not written: %v", err)
	}
	if got := manager.Snapshot().Config().Service.LogMaxBytes; got != activeBefore.Service.LogMaxBytes {
		t.Fatalf("active snapshot changed before restart: %d", got)
	}
	activeDisk, _ := store.Load()
	if activeDisk.Service.LogMaxBytes != activeBefore.Service.LogMaxBytes {
		t.Fatal("active disk changed before restart")
	}
	if _, err := manager.Apply(cfg); !errors.Is(err, ErrRestartInProgress) {
		t.Fatalf("second apply error = %v, want ErrRestartInProgress", err)
	}
	if err := manager.RestartFailed(errors.New("exec failed")); err != nil {
		t.Fatalf("RestartFailed() error = %v", err)
	}
	if exists, err := store.PendingExists(); err != nil || exists {
		t.Fatalf("pending after rollback = %v, %v", exists, err)
	}
	status := manager.RestartStatus()
	if status.InProgress || status.State != "failed" || !strings.Contains(status.LastError, "exec failed") {
		t.Fatalf("restart status = %#v", status)
	}
	if got := manager.Snapshot().Config().Service.LogMaxBytes; got != activeBefore.Service.LogMaxBytes {
		t.Fatal("rollback changed snapshot")
	}
}

func TestRestartSuccessPromotesPendingAndPublishesNewRevision(t *testing.T) {
	var manager *Manager
	manager, store, cfg := newRuntimeManager(t, Options{
		Restart:      func() error { return nil },
		RestartDelay: -1,
	})
	next := cfg.Clone()
	next.Auth.GatewayKey = "new-gateway"
	next.Service.LogMaxBytes++
	before := manager.Snapshot().Revision()
	result, err := manager.Apply(next)
	if err != nil || !result.RestartRequired {
		t.Fatalf("Apply() = %#v, %v", result, err)
	}
	if err := manager.TriggerRestart(); err != nil {
		t.Fatalf("TriggerRestart() = %v", err)
	}
	// The negative delay runs the simulated callback immediately in a goroutine;
	// wait until the state transition is observable.
	for i := 0; i < 100; i++ {
		if !manager.RestartStatus().InProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := manager.RestartStatus(); status.InProgress || status.State != "idle" {
		t.Fatalf("success status = %#v", status)
	}
	if got := manager.Snapshot().Config().Auth.GatewayKey; got != "new-gateway" {
		t.Fatalf("snapshot gateway key = %q", got)
	}
	if manager.Snapshot().Revision() != before+1 {
		t.Fatalf("revision = %d, want %d", manager.Snapshot().Revision(), before+1)
	}
	if _, err := store.LoadPending(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending after success = %v", err)
	}
}

func TestPreflightFailureDoesNotWritePending(t *testing.T) {
	manager, store, cfg := newRuntimeManager(t, Options{
		Preflight: func(config.Config, config.Config) error { return errors.New("port occupied") },
	})
	next := cfg.Clone()
	next.Service.ListenAddr = "127.0.0.1:54321"
	if _, err := manager.Apply(next); err == nil {
		t.Fatal("preflight failure was accepted")
	} else {
		var preflight *PreflightError
		if !errors.As(err, &preflight) {
			t.Fatalf("error = %T %v, want PreflightError", err, err)
		}
	}
	if _, err := store.LoadPending(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending after preflight failure = %v", err)
	}
}

func TestLoadStartupPromotesOnlyExplicitly(t *testing.T) {
	_, store, cfg := newRuntimeManager(t, Options{})
	next := cfg.Clone()
	next.Auth.GatewayKey = "new-gateway"
	if err := store.SavePending(next); err != nil {
		t.Fatal(err)
	}
	startup, err := LoadStartup(store, providerRegistry())
	if err != nil {
		t.Fatalf("LoadStartup() error = %v", err)
	}
	if !startup.FromPending() || startup.Config().Auth.GatewayKey != "new-gateway" {
		t.Fatalf("startup candidate = pending %v, config %#v", startup.FromPending(), startup.Config())
	}
	active, _ := store.Load()
	if active.Auth.GatewayKey != "" {
		t.Fatal("active config changed before Promote")
	}
	if err := startup.Promote(); err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
	active, err = store.Load()
	if err != nil || active.Auth.GatewayKey != "new-gateway" {
		t.Fatalf("promoted active = %#v, %v", active, err)
	}
}

func TestNewManagerBlocksSubmissionsWhenPendingCleanupSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePending(cfg); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, cfg, Options{InitialPending: true})
	if err != nil {
		t.Fatal(err)
	}
	status := manager.RestartStatus()
	if !status.InProgress || !status.Pending || status.State != "failed" {
		t.Fatalf("restart status = %#v", status)
	}
	if _, err := manager.Apply(cfg); !errors.Is(err, ErrRestartInProgress) {
		t.Fatalf("Apply() = %v, want ErrRestartInProgress", err)
	}
}

func providerRegistry() provider.Registry { return provider.DefaultRegistry() }
