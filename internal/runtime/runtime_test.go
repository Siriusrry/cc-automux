package runtime

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
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
	if options.RuntimeContext.Registry.Empty() {
		options.RuntimeContext = testRuntimeContext(t)
	}
	manager, err := NewManager(store, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, cfg
}

func testRuntimeContext(t *testing.T) provider.RuntimeContext {
	t.Helper()
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	return context
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

func TestSnapshotCarriesAttemptPolicyAcrossRevisions(t *testing.T) {
	attempts := scheduler.AttemptPolicy{MaxAttempts: 2}
	classifierAttempts := scheduler.AttemptPolicy{MaxAttempts: 1}
	manager, _, cfg := newRuntimeManager(t, Options{AttemptPolicy: attempts, ClassifierAttemptPolicy: classifierAttempts})
	if got := manager.Snapshot().AttemptPolicy(); got != attempts {
		t.Fatalf("initial attempt policy = %#v", got)
	}
	if got := manager.Snapshot().ClassifierAttemptPolicy(); got != classifierAttempts {
		t.Fatalf("initial classifier attempt policy = %#v", got)
	}
	next := cfg.Clone()
	next.Auth.GatewayKey = "gateway-key"
	if _, err := manager.Apply(next); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().AttemptPolicy(); got != attempts {
		t.Fatalf("hot-applied attempt policy = %#v", got)
	}
	if got := manager.Snapshot().ClassifierAttemptPolicy(); got != classifierAttempts {
		t.Fatalf("hot-applied classifier attempt policy = %#v", got)
	}

	defaultManager, _, _ := newRuntimeManager(t, Options{})
	if got := defaultManager.Snapshot().AttemptPolicy(); got != scheduler.DefaultAttemptPolicy() {
		t.Fatalf("default attempt policy = %#v", got)
	}
	if got := defaultManager.Snapshot().ClassifierAttemptPolicy(); got != scheduler.DefaultClassifierAttemptPolicy() {
		t.Fatalf("default classifier attempt policy = %#v", got)
	}
}

func TestNewManagerRejectsInvalidAttemptPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(store, runtimeConfig(), Options{
		AttemptPolicy:  scheduler.AttemptPolicy{MaxAttempts: -1},
		RuntimeContext: testRuntimeContext(t),
	}); err == nil || !strings.Contains(err.Error(), "attempt policy") {
		t.Fatalf("invalid attempt policy error = %v", err)
	}
	if _, err := NewManager(store, runtimeConfig(), Options{
		ClassifierAttemptPolicy: scheduler.AttemptPolicy{MaxAttempts: -1},
		RuntimeContext:          testRuntimeContext(t),
	}); err == nil || !strings.Contains(err.Error(), "classifier attempt policy") {
		t.Fatalf("invalid classifier attempt policy error = %v", err)
	}
}

func TestNewManagerAndStartupRejectMissingRuntimeContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(store, cfg, Options{}); err == nil || !strings.Contains(err.Error(), "runtime context") {
		t.Fatalf("NewManager missing context error = %v", err)
	}
	if _, err := LoadStartup(store, provider.RuntimeContext{}); err == nil || !strings.Contains(err.Error(), "runtime context") {
		t.Fatalf("LoadStartup missing context error = %v", err)
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

func TestPatchPlanHotApplyAndAliasGenerationIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	cfg.Providers[0].Patches = []string{patch.CLIProxyAPIClassifierSessionID}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	failing := &failingConfigStore{Store: store}
	manager, err := NewManager(failing, cfg, Options{RuntimeContext: testRuntimeContext(t), Preflight: func(config.Config, config.Config) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}

	oldSnapshot := manager.Snapshot()
	oldProvider := onlyRuntimeProvider(t, oldSnapshot)
	if got := oldProvider.PatchPlan.IDs(); len(got) != 1 || got[0] != patch.CLIProxyAPIClassifierSessionID {
		t.Fatalf("old patch plan = %#v", got)
	}
	oldAlias := runtimeClassifierAlias(t, oldProvider, "session-a")
	if again := runtimeClassifierAlias(t, oldProvider, "session-a"); again != oldAlias {
		t.Fatalf("alias changed within old generation: %q != %q", again, oldAlias)
	}

	next := cfg.Clone()
	next.Providers[0].Patches = []string{
		patch.CLIProxyAPIClassifierSessionID,
		patch.GPTClassifierResponseReassemblyID,
	}
	failing.failSave = true
	if _, err := manager.Apply(next); err == nil || !strings.Contains(err.Error(), "disk save failed") {
		t.Fatalf("failed hot apply error = %v", err)
	}
	failedSnapshot := manager.Snapshot()
	failedProvider := onlyRuntimeProvider(t, failedSnapshot)
	if failedSnapshot.Revision() != oldSnapshot.Revision() || failedProvider.Generation != oldProvider.Generation {
		t.Fatalf("failed apply published revision/generation: revision %d, generation %s", failedSnapshot.Revision(), failedProvider.Generation)
	}
	if got := failedProvider.PatchPlan.IDs(); len(got) != 1 || got[0] != patch.CLIProxyAPIClassifierSessionID {
		t.Fatalf("failed apply published patch plan = %#v", got)
	}

	failing.failSave = false
	result, err := manager.Apply(next)
	if err != nil {
		t.Fatal(err)
	}
	newSnapshot := manager.Snapshot()
	newProvider := onlyRuntimeProvider(t, newSnapshot)
	if !result.Applied || result.Revision != oldSnapshot.Revision()+1 || newSnapshot.Revision() != result.Revision {
		t.Fatalf("successful hot apply = %#v, snapshot revision %d", result, newSnapshot.Revision())
	}
	if got := newProvider.PatchPlan.IDs(); len(got) != 2 || got[0] != patch.CLIProxyAPIClassifierSessionID || got[1] != patch.GPTClassifierResponseReassemblyID {
		t.Fatalf("new patch plan = %#v", got)
	}
	if newProvider.Generation == oldProvider.Generation {
		t.Fatalf("patch plan change retained generation %s", newProvider.Generation)
	}

	oldAfterApply := onlyRuntimeProvider(t, oldSnapshot)
	if oldAfterApply.Generation != oldProvider.Generation {
		t.Fatalf("old snapshot generation changed: %s != %s", oldAfterApply.Generation, oldProvider.Generation)
	}
	if got := oldAfterApply.PatchPlan.IDs(); len(got) != 1 || got[0] != patch.CLIProxyAPIClassifierSessionID {
		t.Fatalf("old snapshot patch plan changed = %#v", got)
	}
	newAlias := runtimeClassifierAlias(t, newProvider, "session-a")
	if newAlias == oldAlias {
		t.Fatalf("alias was reused across generations: %q", newAlias)
	}
	if oldAgain := runtimeClassifierAlias(t, oldAfterApply, "session-a"); oldAgain != oldAlias {
		t.Fatalf("old-generation alias changed after hot apply: %q != %q", oldAgain, oldAlias)
	}
}

// A hot-applied configuration rebuilds the immutable catalog, but the runtime
// manager must keep publishing plans backed by the one process-owned AliasStore.
// This models the normal management update path (name/priority changes do not
// alter target generation) and catches accidental per-compile store creation.
func TestHotApplyRetainsSharedSessionAliasStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig()
	cfg.Providers[0].Patches = []string{patch.CLIProxyAPIClassifierSessionID}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	context := testRuntimeContext(t)
	manager, err := NewManager(store, cfg, Options{
		RuntimeContext: context,
		Preflight:      func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if manager.AliasStore() == nil || manager.AliasStore() != context.Registry.AliasStore() {
		t.Fatal("manager did not retain the injected AliasStore")
	}
	oldProvider := onlyRuntimeProvider(t, manager.Snapshot())
	oldAlias := runtimeClassifierAlias(t, oldProvider, "session-hot-update")

	next := cfg.Clone()
	next.Providers[0].Name = "renamed-provider"
	next.Providers[0].Priority = 42
	if _, err := manager.Apply(next); err != nil {
		t.Fatal(err)
	}
	newProvider := onlyRuntimeProvider(t, manager.Snapshot())
	if newProvider.Generation != oldProvider.Generation {
		t.Fatalf("non-identity hot update changed generation: %s != %s", newProvider.Generation, oldProvider.Generation)
	}
	newAlias := runtimeClassifierAlias(t, newProvider, "session-hot-update")
	if newAlias != oldAlias {
		t.Fatalf("hot update replaced shared alias store: %q != %q", newAlias, oldAlias)
	}
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
	manager, err := NewManager(failing, cfg, Options{RuntimeContext: testRuntimeContext(t), Preflight: func(config.Config, config.Config) error { return nil }})
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
	context := testRuntimeContext(t)
	startup, err := LoadStartup(store, context)
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
	manager, err := NewManager(store, cfg, Options{InitialPending: true, RuntimeContext: testRuntimeContext(t)})
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

func onlyRuntimeProvider(t *testing.T, snapshot *Snapshot) *provider.CompiledProvider {
	t.Helper()
	providers := snapshot.Providers()
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	return providers[0]
}

func runtimeClassifierAlias(t *testing.T, compiled *provider.CompiledProvider, sessionID string) string {
	t.Helper()
	body, err := bodyfile.Capture(bytes.NewBufferString(`{"model":"model"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := compiled.PatchPlan.RequiredPaths(patch.StageRequest, patch.RequestTypeClassifier)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	spec, err := bodyfile.RequestScanSpec(paths...)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	index, err := bodyfile.IndexSelective(body, spec)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	request := patch.NewMutableRequest(body, index, patch.NewHTTPHeaderSet(make(http.Header)))
	context := patch.PatchContext{
		RequestType:       patch.RequestTypeClassifier,
		OriginalModel:     "model",
		EffectiveModel:    "model",
		OriginalSessionID: sessionID,
		TargetID:          compiled.ID,
		Generation:        compiled.Generation.String(),
	}
	execution, err := compiled.PatchPlan.NewInstance(context)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	if err := execution.ApplyRequestOnly(request); err != nil {
		_ = execution.Close()
		_ = request.Body.Close()
		_ = body.Close()
		t.Fatal(err)
	}
	alias, ok := request.Headers.Get("X-Claude-Code-Session-Id")
	if !ok || alias == "" {
		t.Fatalf("isolated session header = %q, present %v", alias, ok)
	}
	if err := execution.Close(); err != nil {
		t.Fatal(err)
	}
	if err := request.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	return alias
}
