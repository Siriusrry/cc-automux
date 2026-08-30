package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/modelname"
)

func validConfig() Config {
	cfg := Default()
	cfg.Auth.ManagementKey = "management-test-key"
	cfg.Providers = []ProviderConfig{{
		ID:       "11111111-1111-4111-8111-111111111111",
		Name:     "provider-a",
		BaseURL:  "https://example.test/anthropic",
		APIKey:   "provider-key",
		Models:   []string{"model-a"},
		Enabled:  true,
		Priority: 0,
	}}
	return cfg
}

func TestDecodeAppliesOnlyDocumentedDefaults(t *testing.T) {
	cfg, err := Decode([]byte(`{"schema_version":1,"auth":{"management_key":"m"}}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.Service.ListenAddr != DefaultListenAddr || cfg.Service.LogMaxBytes != DefaultLogMaxBytes {
		t.Fatalf("defaults = %#v", cfg.Service)
	}
	if cfg.Auth.ManagementKey != "m" || cfg.Providers == nil {
		t.Fatalf("decoded config = %#v", cfg)
	}
}

func TestDecodeRejectsSchemaUnknownTrailingAndInvalidTypes(t *testing.T) {
	cases := []string{
		`{"auth":{"management_key":"m"}}`,
		`{"schema_version":2,"auth":{"management_key":"m"}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"unknown":true}`,
		`{"schema_version":1,"auth":{"management_key":"m"}} {}`,
		`{"schema_version":"1","auth":{"management_key":"m"}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"service":null}`,
		`{"schema_version":1,"auth":{"management_key":null}}`,
		`{"schema_version":1,"service":{"listen_addr":"","log_max_bytes":104857600},"auth":{"management_key":"m"}}`,
		`{"schema_version":1,"service":{"listen_addr":"127.0.0.1:8765","log_max_bytes":0},"auth":{"management_key":"m"}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"providers":[{"id":"11111111-1111-4111-8111-111111111111","name":"p","base_url":"https://p.test","enabled":null}]}`,
		`{"schema_version":1,"auth":{"management_key":"m","management_key":"m2"}}`,
	}
	for _, raw := range cases {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("Decode(%s) succeeded, want error", raw)
		}
	}
	var syntax *SyntaxError
	if _, err := Decode([]byte(`{"schema_version":1,"auth":{"management_key":"m"}} {}`)); !errors.As(err, &syntax) {
		t.Fatalf("trailing error = %T %v, want SyntaxError", err, err)
	}
}

func TestProviderPriorityModelsAndKeyRules(t *testing.T) {
	decoded, err := DecodeProvider([]byte(`{"name":"p","base_url":"https://p.test","models":[]}`))
	if err != nil || decoded.Priority != 0 {
		t.Fatalf("missing priority = %d, err %v", decoded.Priority, err)
	}
	for _, raw := range []string{
		`{"name":"p","base_url":"https://p.test","models":[],"priority":9223372036854775807}`,
		`{"name":"p","base_url":"https://p.test","models":[],"priority":-9223372036854775808}`,
	} {
		if _, err := DecodeProvider([]byte(raw)); err != nil {
			t.Fatalf("DecodeProvider(%s) error = %v", raw, err)
		}
	}
	cfg := validConfig()
	cfg.Providers[0].Priority = -1
	cfg.Providers[0].Models = []string{"Model", "model"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("case-sensitive model names rejected: %v", err)
	}

	cfg.Providers[0].Models = []string{"Model", "Model"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "models") {
		t.Fatalf("exact duplicate accepted/error = %v", err)
	}

	cfg.Providers[0].Models = []string{}
	cfg.Providers[0].APIKey = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("model-less provider should be valid without key: %v", err)
	}

	cfg.Providers[0].Models = []string{"model-a"}
	cfg.Providers[0].Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("enabled provider with models and no key was accepted")
	}
	cfg.Providers[0].Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled provider without key rejected: %v", err)
	}
}

func TestProviderModelNamesUseDecodedUTF8ByteLimit(t *testing.T) {
	cfg := validConfig()
	for _, model := range []string{
		strings.Repeat("a", modelname.MaxBytes),
		strings.Repeat("界", 85) + "a",
	} {
		cfg.Providers[0].Models = []string{model}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%d-byte model rejected: %v", len(model), err)
		}
	}
	for _, model := range []string{
		strings.Repeat("a", modelname.MaxBytes+1),
		strings.Repeat("界", 86),
	} {
		cfg.Providers[0].Models = []string{model}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "256 UTF-8 bytes") {
			t.Fatalf("%d-byte model error = %v", len(model), err)
		}
	}
}

func TestProviderDisableHealthStrictDecoding(t *testing.T) {
	missing, err := DecodeProvider([]byte(`{"name":"p","base_url":"https://p.test","models":[]}`))
	if err != nil || missing.DisableHealth {
		t.Fatalf("missing disable_health = %v, err %v", missing.DisableHealth, err)
	}

	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{raw: `{"name":"p","base_url":"https://p.test","models":[],"disable_health":false}`, want: false},
		{raw: `{"name":"p","base_url":"https://p.test","models":[],"disable_health":true}`, want: true},
	} {
		got, err := DecodeProvider([]byte(tc.raw))
		if err != nil || got.DisableHealth != tc.want {
			t.Fatalf("DecodeProvider(%s) disable_health = %v, err %v", tc.raw, got.DisableHealth, err)
		}
	}

	for _, raw := range []string{
		`{"name":"p","base_url":"https://p.test","models":[],"disable_health":null}`,
		`{"name":"p","base_url":"https://p.test","models":[],"disable_health":"true"}`,
		`{"name":"p","base_url":"https://p.test","models":[],"disable_health":1}`,
	} {
		if _, err := DecodeProvider([]byte(raw)); err == nil {
			t.Fatalf("DecodeProvider(%s) succeeded, want error", raw)
		}
	}

	cfg := validConfig()
	cfg.Providers[0].DisableHealth = true
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Decode(data)
	if err != nil || !roundTrip.Providers[0].DisableHealth {
		t.Fatalf("round trip disable_health = %v, err %v", roundTrip.Providers[0].DisableHealth, err)
	}
}

func TestProviderPatchOrderRoundTripAndExactDuplicateValidation(t *testing.T) {
	cfg := validConfig()
	want := []string{"patch-z", "patch-a", "Patch-A"}
	cfg.Providers[0].Patches = append([]string(nil), want...)
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	got := roundTrip.Providers[0].Patches
	if len(got) != len(want) {
		t.Fatalf("round-trip patches = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-trip patches = %#v, want ordered %#v", got, want)
		}
	}

	cfg.Providers[0].Patches = []string{"same", "same"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "patches[1]") {
		t.Fatalf("exact duplicate patch validation error = %v", err)
	}
	cfg.Providers[0].Patches = []string{"same", "Same"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("case-distinct patch IDs rejected at schema layer: %v", err)
	}
}

func TestValidateRejectsConflictingKeysAndListenAddresses(t *testing.T) {
	cfg := validConfig()
	cfg.Auth.GatewayKey = cfg.Auth.ManagementKey
	if err := cfg.Validate(); err == nil {
		t.Fatal("identical gateway and management keys accepted")
	}
	for _, addr := range []string{"0.0.0.0:1", "localhost:1", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:+1", "127.0.0.1:01"} {
		cfg = validConfig()
		cfg.Service.ListenAddr = addr
		if err := cfg.Validate(); err == nil {
			t.Errorf("listen address %q accepted", addr)
		}
	}
}

func TestValidateRejectsWhitespaceCredentialsAndInvalidBaseURLPorts(t *testing.T) {
	cfg := validConfig()
	cfg.Auth.GatewayKey = "   "
	if err := cfg.Validate(); err == nil {
		t.Fatal("whitespace-only gateway key was accepted")
	}
	for _, raw := range []string{"https://example.test:", "https://example.test:0", "https://example.test:65536", "https://example.test:01", "https://example.test/path#"} {
		cfg = validConfig()
		cfg.Providers[0].BaseURL = raw
		if err := cfg.Validate(); err == nil {
			t.Errorf("base URL %q was accepted", raw)
		}
	}
	// An escaped hash is path data, not a fragment delimiter, and remains valid.
	cfg = validConfig()
	cfg.Providers[0].BaseURL = "https://example.test/path%23segment"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("escaped hash in base URL was rejected: %v", err)
	}
}

func TestInitializeAndAtomicStorePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg, created, err := Initialize(path, "management-key")
	if err != nil || !created {
		t.Fatalf("Initialize() = created %v, err %v", created, err)
	}
	if cfg.Auth.ManagementKey != "management-key" {
		t.Fatalf("management key was not initialized")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	store, _ := NewStore(path)
	if _, created, err := Initialize(path, "replacement-key"); err != nil || created {
		t.Fatalf("reinitialize = created %v, err %v", created, err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Auth.ManagementKey != "management-key" {
		t.Fatalf("existing config changed: %#v, %v", loaded, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("insecure mode error = %v", err)
	}
}

func TestConfigPathOverrideMustBeAbsolute(t *testing.T) {
	t.Setenv(ConfigPathEnv, "relative/config.json")
	if _, err := Path(); err == nil {
		t.Fatal("relative configuration override was accepted")
	}
	absolute := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(ConfigPathEnv, "  "+absolute+"  ")
	if got, err := Path(); err != nil || got != absolute {
		t.Fatalf("Path() = %q, %v", got, err)
	}
}

func TestPendingFileUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(validConfig()); err != nil {
		t.Fatal(err)
	}
	pending := validConfig()
	pending.Service.LogMaxBytes++
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.PendingPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pending mode = %v", info.Mode().Perm())
	}
}

func TestGenerateUUID(t *testing.T) {
	id, err := GenerateUUID()
	if err != nil || !IsUUID(id) {
		t.Fatalf("GenerateUUID() = %q, %v", id, err)
	}
}

func TestGenerateManagementKeyUses256BitsOfRandomness(t *testing.T) {
	first, err := GenerateManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("generated key decodes to %d bytes, err %v", len(decoded), err)
	}
	if first == second {
		t.Fatal("two generated management keys were identical")
	}
}

func TestStoreSaveReplacesAtomicallyWithoutTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := validConfig()
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.GatewayKey = "gateway-key"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Auth.GatewayKey != "gateway-key" {
		t.Fatalf("saved config = %#v, %v", loaded, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config.json.tmp-") {
			t.Fatalf("temporary file remains after save: %s", entry.Name())
		}
	}
}
