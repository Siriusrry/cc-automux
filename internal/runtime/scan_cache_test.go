package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type scanCacheRequestPatch struct{}

func (scanCacheRequestPatch) ApplyRequest(patch.PatchContext, *patch.MutableRequest) error {
	return nil
}

type scanCacheResponsePatch struct{}

func (scanCacheResponsePatch) ApplyResponse(patch.PatchContext, *patch.MutableResponse) error {
	return nil
}

func TestSnapshotCompilesRequestScanByReachableFlow(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		want      []string
		doNotWant []string
	}{
		{
			name:      "disabled skips every classifier patch",
			mode:      config.AutoModeDisabled,
			want:      []string{"/detector", "/normal_matching", "/normal_other"},
			doNotWant: []string{"/normal_disabled", "/normal_empty", "/classifier_matching", "/classifier_other", "/classifier_disabled", "/fixed_classifier"},
		},
		{
			name:      "provider pool includes only providers for classifier model",
			mode:      config.AutoModeProviderPool,
			want:      []string{"/detector", "/normal_matching", "/normal_other", "/classifier_matching"},
			doNotWant: []string{"/normal_disabled", "/normal_empty", "/classifier_other", "/classifier_disabled", "/fixed_classifier"},
		},
		{
			name:      "fixed provider includes only fixed classifier patches",
			mode:      config.AutoModeFixedProvider,
			want:      []string{"/detector", "/normal_matching", "/normal_other", "/fixed_classifier"},
			doNotWant: []string{"/normal_disabled", "/normal_empty", "/classifier_matching", "/classifier_other", "/classifier_disabled"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := newScanCacheSnapshot(t, test.mode)
			if snapshot.RequestScanSpec() == nil || snapshot.RequestScanSpec() != snapshot.RequestScanSpec() {
				t.Fatal("request scan contract was not cached on the snapshot")
			}
			body, index, err := bodyfile.CaptureAndScanCompiled(strings.NewReader(scanCacheRequestBody()), snapshot.RequestScanSpec(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			for _, path := range test.want {
				if _, ok := index.Lookup(path); !ok {
					t.Errorf("required path %q was not retained", path)
				}
			}
			for _, path := range test.doNotWant {
				if _, ok := index.Lookup(path); ok {
					t.Errorf("unreachable path %q was retained", path)
				}
			}
			if found, tracked := index.RawMarkerStatus("detector-marker"); !tracked || !found {
				t.Fatalf("detector marker found=%v tracked=%v", found, tracked)
			}
		})
	}
}

func TestPatchPlanCachesResponseScansIncludingEmptyPathHooks(t *testing.T) {
	snapshot := newScanCacheSnapshot(t, config.AutoModeProviderPool)
	var matching *provider.CompiledProvider
	for _, item := range snapshot.Providers() {
		if item.Name == "matching" {
			matching = item
			break
		}
	}
	if matching == nil {
		t.Fatal("matching provider missing")
	}
	for _, test := range []struct {
		requestType traffic.RequestType
		path        string
	}{
		{traffic.RequestTypeNormal, "/normal_matching_response"},
		{traffic.RequestTypeClassifier, "/classifier_matching_response"},
	} {
		spec, ok := matching.PatchPlan.ResponseScanSpec(test.requestType)
		if !ok || spec == nil {
			t.Fatalf("response scan missing for %q", test.requestType)
		}
		body, index, err := bodyfile.CaptureAndScanCompiled(strings.NewReader(`{"normal_matching_response":true,"classifier_matching_response":true}`), spec, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := index.Lookup(test.path); !ok {
			_ = body.Close()
			t.Fatalf("response path %q was not retained", test.path)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	headerOnly, ok := matching.PatchPlan.ResponseScanSpec(traffic.RequestTypeClassifier)
	if !ok || headerOnly == nil {
		t.Fatal("classifier response scan missing for header-only hook")
	}
}

func newScanCacheSnapshot(t *testing.T, mode string) *Snapshot {
	t.Helper()
	definitions := []patch.PatchDefinition{
		scanCacheDefinition("normal-matching", traffic.RequestTypeNormal, "/normal_matching", "/normal_matching_response"),
		scanCacheDefinition("normal-other", traffic.RequestTypeNormal, "/normal_other", "/normal_other_response"),
		scanCacheDefinition("normal-disabled", traffic.RequestTypeNormal, "/normal_disabled", "/normal_disabled_response"),
		scanCacheDefinition("normal-empty", traffic.RequestTypeNormal, "/normal_empty", "/normal_empty_response"),
		scanCacheDefinition("classifier-matching", traffic.RequestTypeClassifier, "/classifier_matching", "/classifier_matching_response"),
		scanCacheDefinition("classifier-other", traffic.RequestTypeClassifier, "/classifier_other", "/classifier_other_response"),
		scanCacheDefinition("classifier-disabled", traffic.RequestTypeClassifier, "/classifier_disabled", "/classifier_disabled_response"),
		scanCacheDefinition("fixed-classifier", traffic.RequestTypeClassifier, "/fixed_classifier", "/fixed_classifier_response"),
		scanCacheDefinition("classifier-header-only", traffic.RequestTypeClassifier, "", ""),
	}
	registry, err := patch.NewRegistry(definitions)
	if err != nil {
		t.Fatal(err)
	}
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := NewScanRequirements([]string{"/detector"}, []string{"detector-marker"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.ManagementKey = "management-key"
	cfg.Providers = []config.ProviderConfig{
		scanCacheProvider("11111111-1111-4111-8111-111111111111", "matching", true, []string{"classifier-model"}, "normal-matching", "classifier-matching", "classifier-header-only"),
		scanCacheProvider("22222222-2222-4222-8222-222222222222", "other", true, []string{"other-model"}, "normal-other", "classifier-other"),
		scanCacheProvider("33333333-3333-4333-8333-333333333333", "disabled", false, []string{"classifier-model"}, "normal-disabled", "classifier-disabled"),
		scanCacheProvider("44444444-4444-4444-8444-444444444444", "empty", true, []string{}, "normal-empty"),
	}
	switch mode {
	case config.AutoModeDisabled:
		cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeDisabled}
	case config.AutoModeProviderPool:
		cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeProviderPool, Model: "classifier-model"}
	case config.AutoModeFixedProvider:
		cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeFixedProvider, Model: "classifier-model", FixedProvider: &config.FixedProviderConfig{
			BaseURL:  "https://fixed.example",
			APIKey:   "fixed-key",
			Protocol: config.ProtocolAnthropicMessages,
			Patches:  []string{"fixed-classifier", "classifier-header-only"},
		}}
	default:
		t.Fatalf("unsupported test mode %q", mode)
	}
	store, err := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, cfg, Options{
		RuntimeContext:   context,
		ScanRequirements: requirements,
		Preflight:        func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager.Snapshot()
}

func scanCacheDefinition(id string, requestType traffic.RequestType, requestPath, responsePath string) patch.PatchDefinition {
	stages := make([]patch.Stage, 0, 2)
	var requestPaths, responsePaths []string
	hooks := patch.Hooks{}
	if requestPath != "" {
		stages = append(stages, patch.StageRequest)
		requestPaths = []string{requestPath}
		hooks.Request = scanCacheRequestPatch{}
	}
	stages = append(stages, patch.StageResponse)
	if responsePath != "" {
		responsePaths = []string{responsePath}
	}
	hooks.Response = scanCacheResponsePatch{}
	return patch.PatchDefinition{
		ID:            id,
		Name:          id,
		Description:   id,
		RequestTypes:  []patch.RequestType{requestType},
		Stages:        stages,
		RequestPaths:  requestPaths,
		ResponsePaths: responsePaths,
		Idempotence:   patch.Idempotent,
		Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
			return patch.NewHooksInstance(hooks), nil
		},
	}
}

func scanCacheProvider(id, name string, enabled bool, models []string, patches ...string) config.ProviderConfig {
	return config.ProviderConfig{
		ID:      id,
		Name:    name,
		BaseURL: "https://" + name + ".example",
		APIKey:  name + "-key",
		Models:  models,
		Enabled: enabled,
		Patches: patches,
	}
}

func scanCacheRequestBody() string {
	return `{"model":"classifier-model","detector":true,"normal_matching":true,"normal_other":true,"normal_disabled":true,"normal_empty":true,"classifier_matching":true,"classifier_other":true,"classifier_disabled":true,"fixed_classifier":true,"marker":"detector-marker"}`
}

var _ patch.RequestPatch = scanCacheRequestPatch{}
var _ patch.ResponsePatch = scanCacheResponsePatch{}
