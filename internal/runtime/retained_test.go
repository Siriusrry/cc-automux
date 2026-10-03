package runtime

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
)

func TestRetainedSettingsPersistWithoutDormantRuntimeEffects(t *testing.T) {
	manager, store, cfg := newRuntimeManager(t, Options{})
	missing := filepath.Join(t.TempDir(), "missing.pem")
	cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeFixedProvider, Model: "classifier", FixedProvider: &config.FixedProviderConfig{
		BaseURL: "https://classifier.example", APIKey: "test-key", Protocol: config.ProtocolAnthropicMessages,
		TLS: config.TLSConfig{Mode: config.TLSSkip, CAFile: missing}, Patches: []string{patch.AnyRouterClassifierRequestID},
	}}
	if _, err := manager.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	original := manager.Snapshot().CompiledAutoMode().FixedTarget.Generation
	cfg.AutoMode.Mode = config.AutoModeProviderPool
	cfg.AutoMode.FixedProvider.TLS.Mode = config.TLSCustom
	cfg.Providers[0].TLS = config.TLSConfig{Mode: config.TLSSystem, CAFile: missing}
	generation := manager.Snapshot().Providers()[0].Generation
	if _, err := manager.Apply(cfg); err != nil {
		t.Fatalf("dormant CA read: %v", err)
	}
	if manager.Snapshot().CompiledAutoMode().FixedTarget != nil {
		t.Fatal("dormant fixed target compiled")
	}
	if manager.Snapshot().Providers()[0].Generation != generation {
		t.Fatal("dormant CA changed generation")
	}
	if manager.Snapshot().Providers()[0].Config().TLS.CAFile != missing {
		t.Fatal("compiled config lost retained CA")
	}
	cfg.AutoMode.Mode = config.AutoModeDisabled
	if _, err := manager.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().AutoMode().EffectiveClassifierModel != "" {
		t.Fatal("disabled model remained effective")
	}
	disk, err := store.Load()
	if err != nil || !reflect.DeepEqual(disk, manager.Config()) {
		t.Fatalf("disk mismatch: %v", err)
	}
	restarted, err := NewManager(store, disk, Options{RuntimeContext: testRuntimeContext(t), HarnessValidator: testHarnessValidator{}, HarnessUpdater: testHarnessValidator{}})
	if err != nil || restarted.Config().AutoMode.Model != "classifier" {
		t.Fatalf("reload: %v", err)
	}
	cfg.AutoMode.Mode = config.AutoModeFixedProvider
	before := manager.Snapshot()
	if _, err := manager.Apply(cfg); err == nil || manager.Snapshot() != before {
		t.Fatal("invalid active CA published")
	}
	cfg.AutoMode.FixedProvider.TLS.Mode = config.TLSSkip
	if _, err := manager.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().CompiledAutoMode().FixedTarget.Generation != original {
		t.Fatal("restored target generation changed")
	}
	cfg.AutoMode.Mode = config.AutoModeDisabled
	cfg.AutoMode.FixedProvider.Patches = []string{patch.AnyRouterSubagentThinkingID}
	if _, err := manager.Apply(cfg); err == nil {
		t.Fatal("inapplicable dormant patch accepted")
	}
}
