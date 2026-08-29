package provider

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
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
	catalog, err := CompileCatalog(inputs)
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

func TestApplyAuthHeadersReplacesClientCredentials(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "provider-key", 0)
	compiled, err := Compile(p)
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
	compiled, err = Compile(p)
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

func TestTLSDefaultsAndFailClosedPatchRegistry(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	compiled, err := Compile(p)
	if err != nil || compiled.TLS != nil {
		t.Fatalf("default TLS = %#v, err %v; want system-root nil", compiled.TLS, err)
	}
	p.TLS.InsecureSkipVerify = true
	compiled, err = Compile(p)
	if err != nil || compiled.TLS == nil || !compiled.TLS.InsecureSkipVerify {
		t.Fatalf("insecure TLS = %#v, err %v", compiled.TLS, err)
	}
	p.TLS.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p.TLS.CAFile, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(p); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("CA/insecure conflict error = %v", err)
	}

	p = providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	p.Patches = []string{"not-known"}
	if _, err := Compile(p); err == nil || !strings.Contains(err.Error(), "unknown provider patch") {
		t.Fatalf("unknown patch error = %v", err)
	}
	p.Patches = []string{PatchAnyRouter}
	compiled, err = Compile(p)
	if err != nil {
		t.Fatalf("known unimplemented patch compile error = %v", err)
	}
	if err := compiled.ApplyPatches(); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("unimplemented patch application error = %v", err)
	}
}

func TestCustomCACompilesIntoUsableHTTPClient(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	certificate := upstream.Certificate()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	p.BaseURL = upstream.URL
	p.TLS.CAFile = caPath
	compiled, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	response, err := compiled.NewHTTPClient().Get(upstream.URL)
	if err != nil {
		t.Fatalf("custom-CA request failed: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("custom-CA status = %d", response.StatusCode)
	}
}

func TestCompiledProviderCloneDoesNotShareSlices(t *testing.T) {
	p := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	compiled, err := Compile(p)
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
}

func TestProviderGenerationTracksOnlyRuntimeIdentity(t *testing.T) {
	base := providerConfig("11111111-1111-4111-8111-111111111111", "p", "m", "k", 0)
	compiled, err := Compile(base)
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
		got, err := Compile(candidate)
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
		func(p *config.ProviderConfig) { p.Patches = []string{PatchAnyRouter} },
	} {
		candidate := base
		candidate.Models = append([]string(nil), base.Models...)
		mutate(&candidate)
		got, err := Compile(candidate)
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

func TestRegistryRejectsMalformedAndDuplicateMetadata(t *testing.T) {
	for _, entries := range [][]PatchMetadata{
		{{ID: "", Name: "empty"}},
		{{ID: "with space", Name: "bad"}},
		{{ID: "same", Name: "one"}, {ID: "same", Name: "two"}},
		{{ID: "missing-name", Name: ""}},
	} {
		if _, err := NewRegistry(entries); err == nil {
			t.Fatalf("NewRegistry(%#v) succeeded", entries)
		}
	}
	registry, err := NewRegistry([]PatchMetadata{{ID: "z", Name: "Z"}, {ID: "a", Name: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	items := registry.List()
	if len(items) != 2 || items[0].ID != "a" || items[1].ID != "z" {
		t.Fatalf("custom registry order = %#v", items)
	}
}
