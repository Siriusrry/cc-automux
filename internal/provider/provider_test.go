package provider

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
)

func providerConfig(id, name, model, key string, priority int64) config.ProviderConfig {
	return config.ProviderConfig{
		ID:       id,
		Name:     name,
		BaseURL:  "https://example.test/anthropic",
		APIKey:   key,
		Models:   []string{model},
		Priority: priority,
		Enabled:  true,
	}
}

func testCompileContext(t *testing.T) RuntimeContext {
	t.Helper()
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func TestCompileCatalogExactModelIndexAndPriority(t *testing.T) {
	inputs := []config.ProviderConfig{
		providerConfig("11111111-1111-4111-8111-111111111111", "low", "Model", "key-low", -1),
		providerConfig("22222222-2222-4222-8222-222222222222", "high", "Model", "key-high", 5),
		providerConfig("33333333-3333-4333-8333-333333333333", "case", "model", "key-case", 99),
		providerConfig("44444444-4444-4444-8444-444444444444", "disabled", "Model", "", 100),
		{
			ID:      "55555555-5555-4555-8555-555555555555",
			Name:    "empty",
			BaseURL: "https://empty.example",
			Enabled: true,
			APIKey:  "",
			Models:  []string{},
		},
	}
	inputs[3].Enabled = false
	catalog, err := CompileCatalog(inputs, testCompileContext(t))
	if err != nil {
		t.Fatalf("CompileCatalog() error = %v", err)
	}
	got := catalog.Match("Model")
	if len(got) != 2 || got[0].Name != "high" || got[1].Name != "low" {
		t.Fatalf("Model candidates = %#v, want high then low", got)
	}
	if got := catalog.Match("model"); len(got) != 1 || got[0].Name != "case" {
		t.Fatalf("case-sensitive candidates = %#v", got)
	}
	if catalog.HasModel("MODEL") {
		t.Fatal("model lookup unexpectedly folded case")
	}
	if len(catalog.Match("missing")) != 0 {
		t.Fatal("missing model returned candidates")
	}
}

func TestCompileRejectsMissingRuntimeContext(t *testing.T) {
	input := providerConfig("11111111-1111-4111-8111-111111111111", "provider", "model", "key", 0)
	if _, err := Compile(input, RuntimeContext{}); err == nil || !strings.Contains(err.Error(), "patch registry is required") {
		t.Fatalf("Compile missing context error = %v", err)
	}
	if _, err := CompileCatalog([]config.ProviderConfig{input}, RuntimeContext{}); err == nil || !strings.Contains(err.Error(), "patch registry is required") {
		t.Fatalf("CompileCatalog missing context error = %v", err)
	}
}

func TestApplyAuthHeadersReplacesClientCredentials(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "provider-key", 0)
	compiled, err := Compile(p, testCompileContext(t))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	headers := http.Header{
		"Authorization": []string{"Bearer client-key"},
		"X-Api-Key":     []string{"client-key-2"},
		"X-Other":       []string{"keep"},
	}
	if err := compiled.ApplyAuthHeaders(headers); err != nil {
		t.Fatalf("ApplyAuthHeaders() error = %v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer provider-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key = %q, want absent", got)
	}
	if headers.Get("X-Other") != "keep" {
		t.Fatal("unrelated header was removed")
	}

	p.UseXAPIKey = true
	compiled, err = Compile(p, testCompileContext(t))
	if err != nil {
		t.Fatalf("Compile(x-api-key) error = %v", err)
	}
	headers = http.Header{"Authorization": []string{"Bearer client"}}
	if err := compiled.ApplyAuthHeaders(headers); err != nil {
		t.Fatalf("ApplyAuthHeaders(x-api-key) error = %v", err)
	}
	if headers.Get("Authorization") != "" || headers.Get("X-Api-Key") != "provider-key" {
		t.Fatalf("x-api-key headers = %#v", headers)
	}
}

func TestTLSDefaultsAndExecutablePatchRegistry(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	compiled, err := Compile(p, testCompileContext(t))
	if err != nil || compiled.TLS != nil {
		t.Fatalf("default TLS = %#v, err %v; want system-root nil", compiled.TLS, err)
	}
	p.TLS.InsecureSkipVerify = true
	compiled, err = Compile(p, testCompileContext(t))
	if err != nil || compiled.TLS == nil || !compiled.TLS.InsecureSkipVerify {
		t.Fatalf("insecure TLS = %#v, err %v", compiled.TLS, err)
	}
	p.TLS.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p.TLS.CAFile, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(p, testCompileContext(t)); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("CA/insecure conflict error = %v", err)
	}

	p = providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	p.Patches = []string{"not-known"}
	if _, err := Compile(p, testCompileContext(t)); err == nil || !strings.Contains(err.Error(), "unknown provider patch") {
		t.Fatalf("unknown patch error = %v", err)
	}
	p.Patches = []string{patch.AnyRouterSubagentThinkingID}
	compiled, err = Compile(p, testCompileContext(t))
	if err != nil {
		t.Fatalf("known executable patch compile error = %v", err)
	}
	if got := compiled.PatchPlan.IDs(); len(got) != 1 || got[0] != patch.AnyRouterSubagentThinkingID {
		t.Fatalf("compiled patch plan = %#v", got)
	}
}

func TestCompileWithRuntimeContextRejectsUnknownAndConflictingPatchesAndFiltersByRequestType(t *testing.T) {
	registry, err := patch.NewRegistry([]patch.PatchDefinition{
		providerTestRequestDefinition("classifier-only", []patch.RequestType{patch.RequestTypeClassifier}, nil),
		providerTestRequestDefinition("normal-only", []patch.RequestType{patch.RequestTypeNormal}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	input.Patches = []string{"classifier-only", "normal-only"}
	context, err := NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(input, context)
	if err != nil {
		t.Fatal(err)
	}
	if got := compiled.PatchPlan.IDs(); len(got) != 2 || got[0] != "classifier-only" || got[1] != "normal-only" {
		t.Fatalf("compiled patch order = %#v", got)
	}
	normalPlan, err := compiled.PatchPlan.ForRequestType(patch.RequestTypeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalPlan.IDs(); len(got) != 1 || got[0] != "normal-only" {
		t.Fatalf("normal patch plan = %#v", got)
	}
	classifierPlan, err := compiled.PatchPlan.ForRequestType(patch.RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifierPlan.IDs(); len(got) != 1 || got[0] != "classifier-only" {
		t.Fatalf("classifier patch plan = %#v", got)
	}

	input.Patches = []string{"missing"}
	if _, err := Compile(input, context); !errors.Is(err, patch.ErrUnknownPatch) {
		t.Fatalf("unknown patch error = %v", err)
	}

	conflicts := []string{"conflict-b"}
	conflictRegistry, err := patch.NewRegistry([]patch.PatchDefinition{
		providerTestRequestDefinition("conflict-a", []patch.RequestType{patch.RequestTypeNormal}, conflicts),
		providerTestRequestDefinition("conflict-b", []patch.RequestType{patch.RequestTypeNormal}, []string{"conflict-a"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	input.Patches = []string{"conflict-a", "conflict-b"}
	conflictContext, err := NewRuntimeContext(conflictRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(input, conflictContext); !errors.Is(err, patch.ErrPatchConflict) {
		t.Fatalf("conflicting patches error = %v", err)
	}
}

func TestCompileFixedTargetUsesSharedContextAndClassifierPatchFilter(t *testing.T) {
	context := testCompileContext(t)
	input := config.FixedProviderConfig{
		BaseURL:  "https://fixed.example/openai",
		APIKey:   "fixed-key",
		Protocol: config.ProtocolOpenAICompatible,
		Patches:  []string{patch.CLIProxyAPIClassifierSessionID, patch.AnyRouterSubagentThinkingID},
	}
	// The normal-only patch is rejected instead of silently disappearing from
	// the fixed classifier target.
	if _, err := CompileFixedTarget(input, "classifier-model", context); err == nil || !strings.Contains(err.Error(), "not applicable") {
		t.Fatalf("normal-only fixed patch error = %v", err)
	}
	input.Patches = []string{patch.CLIProxyAPIClassifierSessionID}
	target, err := CompileFixedTarget(input, "classifier-model", context)
	if err != nil {
		t.Fatalf("CompileFixedTarget() error = %v", err)
	}
	if target.ID != FixedTargetID || target.Protocol != config.ProtocolOpenAICompatible || target.URLString() != input.BaseURL {
		t.Fatalf("compiled fixed target = %#v", target)
	}
	if got := target.PatchPlan.IDs(); len(got) != 1 || got[0] != patch.CLIProxyAPIClassifierSessionID {
		t.Fatalf("fixed patch plan = %#v", got)
	}
	if target.Generation == "" || target.Generation != FixedGenerationFor(input, "classifier-model") {
		t.Fatalf("fixed generation = %q", target.Generation)
	}
	changed := input
	changed.Protocol = config.ProtocolOpenAIResponses
	other, err := CompileFixedTarget(changed, "classifier-model", context)
	if err != nil {
		t.Fatal(err)
	}
	if other.Generation == target.Generation {
		t.Fatal("protocol change did not isolate fixed generation")
	}
}

func TestCompileFixedTargetRejectsMissingContextAndInvalidModel(t *testing.T) {
	input := config.FixedProviderConfig{BaseURL: "https://fixed.example", APIKey: "k", Protocol: config.ProtocolOpenAIResponses}
	if _, err := CompileFixedTarget(input, "model", RuntimeContext{}); err == nil || !strings.Contains(err.Error(), "patch registry is required") {
		t.Fatalf("missing context error = %v", err)
	}
	if _, err := CompileFixedTarget(input, "", testCompileContext(t)); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty model error = %v", err)
	}
}

func TestCompiledProviderCloneDoesNotShareSlices(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	p.Patches = []string{
		patch.CLIProxyAPIClassifierSessionID,
		patch.GPTClassifierResponseReassemblyID,
	}
	compiled, err := Compile(p, testCompileContext(t))
	if err != nil {
		t.Fatal(err)
	}
	clone := compiled.Clone()
	clone.Models[0] = "changed"
	if compiled.Models[0] == "changed" {
		t.Fatal("clone shares model slice")
	}
	cloneCfg := clone.Config()
	cloneCfg.Models[0] = "changed-again"
	if compiled.Models[0] == "changed-again" {
		t.Fatal("Config() shares model slice")
	}
	clone.Patches[0].RequestTypes[0] = patch.RequestTypeNormal
	clone.Patches[0].Stages[0] = patch.StageResponse
	clone.Patches[0].Conflicts = append(clone.Patches[0].Conflicts, "mutated")
	originalMetadata := compiled.PatchPlan.List()[0]
	if originalMetadata.RequestTypes[0] != patch.RequestTypeClassifier || originalMetadata.Stages[0] != patch.StageRequest || len(originalMetadata.Conflicts) != 0 {
		t.Fatalf("clone mutation changed immutable plan metadata: %#v", originalMetadata)
	}
	if compiled.Patches[0].RequestTypes[0] != patch.RequestTypeClassifier || compiled.Patches[0].Stages[0] != patch.StageRequest || len(compiled.Patches[0].Conflicts) != 0 {
		t.Fatalf("clone shares visible patch metadata: %#v", compiled.Patches[0])
	}

	firstAlias := providerClassifierAlias(t, compiled, "session-a")
	secondAlias := providerClassifierAlias(t, clone, "session-a")
	if firstAlias != secondAlias {
		t.Fatalf("clone did not retain shared alias service: %q != %q", firstAlias, secondAlias)
	}
}

func TestProviderGenerationTracksOnlyRuntimeIdentity(t *testing.T) {
	base := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	compiled, err := Compile(base, testCompileContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Generation == "" {
		t.Fatal("compiled provider has an empty generation")
	}

	for _, mutate := range []func(*config.ProviderConfig){
		func(p *config.ProviderConfig) { p.Name = "renamed" },
		func(p *config.ProviderConfig) { p.Priority = -99 },
		func(p *config.ProviderConfig) { p.Models = []string{"other"} },
		func(p *config.ProviderConfig) { p.DisableHealth = true },
	} {
		candidate := base
		candidate.Models = append([]string(nil), base.Models...)
		mutate(&candidate)
		got, err := Compile(candidate, testCompileContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Generation != compiled.Generation {
			t.Fatalf("non-identity change altered generation: %s != %s", got.Generation, compiled.Generation)
		}
	}

	for _, mutate := range []func(*config.ProviderConfig){
		func(p *config.ProviderConfig) { p.BaseURL = "https://other.test" },
		func(p *config.ProviderConfig) { p.APIKey = "other-key" },
		func(p *config.ProviderConfig) { p.UseXAPIKey = true },
		func(p *config.ProviderConfig) { p.TLS.InsecureSkipVerify = true },
		func(p *config.ProviderConfig) { p.Patches = []string{patch.AnyRouterSubagentThinkingID} },
	} {
		candidate := base
		candidate.Models = append([]string(nil), base.Models...)
		mutate(&candidate)
		got, err := Compile(candidate, testCompileContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Generation == compiled.Generation {
			t.Fatalf("runtime identity change kept generation %s", got.Generation)
		}
	}

	roundTrip := compiled.Config()
	if roundTrip.DisableHealth != base.DisableHealth {
		t.Fatalf("Config() disable_health = %v", roundTrip.DisableHealth)
	}
}

type providerTestRequestPatch struct{}

func (providerTestRequestPatch) ApplyRequest(patch.PatchContext, *patch.MutableRequest) error {
	return nil
}

func providerTestRequestDefinition(id string, requestTypes []patch.RequestType, conflicts []string) patch.PatchDefinition {
	return patch.PatchDefinition{
		ID:           id,
		Name:         id,
		Description:  id,
		RequestTypes: append([]patch.RequestType(nil), requestTypes...),
		Stages:       []patch.Stage{patch.StageRequest},
		Conflicts:    append([]string(nil), conflicts...),
		Idempotence:  patch.Idempotent,
		Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
			return patch.NewHooksInstance(patch.Hooks{Request: providerTestRequestPatch{}}), nil
		},
	}
}

func providerClassifierAlias(t *testing.T, compiled *CompiledProvider, sessionID string) string {
	t.Helper()
	body, err := bodyfile.Capture(bytes.NewBufferString(`{"model":"m"}`), t.TempDir())
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
		OriginalModel:     "m",
		EffectiveModel:    "m",
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
