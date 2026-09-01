package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validProfileForTest() Profile {
	return Profile{
		ID:                   "22222222-2222-4222-8222-222222222222",
		Name:                 "Daily",
		HaikuModel:           "model-haiku",
		SonnetModel:          "model-sonnet",
		OpusModel:            "model-opus",
		FableModel:           "model-fable",
		SubagentModel:        "model-subagent",
		TeammateDefaultModel: "model-teammate",
	}
}

func configWithProfileForTest() Config {
	cfg := validConfig()
	profile := validProfileForTest()
	cfg.Harnesses.ClaudeCode.Profiles = []Profile{profile}
	return cfg
}

func TestDefaultHarnessValuesAndNormalize(t *testing.T) {
	defaults := Default()
	if defaults.Harnesses.ClaudeCode.PathMode != PathModeDefault {
		t.Fatalf("default path mode = %q", defaults.Harnesses.ClaudeCode.PathMode)
	}
	if defaults.Harnesses.ClaudeCode.SettingsPath != "" {
		t.Fatalf("default settings path = %q", defaults.Harnesses.ClaudeCode.SettingsPath)
	}
	if !defaults.Harnesses.ClaudeCode.DisableTelemetry {
		t.Fatal("telemetry is not disabled by default")
	}
	if defaults.Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatalf("default active profile = %q", defaults.Harnesses.ClaudeCode.ActiveProfileID)
	}
	if defaults.Harnesses.ClaudeCode.Profiles == nil {
		t.Fatal("default profiles is nil")
	}

	normalized := (Config{}).Normalize()
	if normalized.Harnesses.ClaudeCode.PathMode != PathModeDefault || normalized.Harnesses.ClaudeCode.Profiles == nil {
		t.Fatalf("normalized zero harness = %#v", normalized.Harnesses.ClaudeCode)
	}
	if normalized.Providers == nil {
		t.Fatal("normalized providers is nil")
	}
}

func TestHarnessRoundTripAndDeepClone(t *testing.T) {
	cfg := configWithProfileForTest()
	cfg.Harnesses.ClaudeCode.PathMode = PathModeCustom
	cfg.Harnesses.ClaudeCode.SettingsPath = filepath.Join(t.TempDir(), ".claude", "settings.json")
	cfg.Harnesses.ClaudeCode.DisableTelemetry = false
	cfg.Harnesses.ClaudeCode.ActiveProfileID = cfg.Harnesses.ClaudeCode.Profiles[0].ID

	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, cfg.Normalize()) {
		t.Fatalf("round-trip config differs:\n got %#v\nwant %#v", decoded, cfg.Normalize())
	}
	if !strings.HasSuffix(string(data), "\n") || !strings.Contains(string(data), `"harnesses"`) {
		t.Fatalf("canonical config encoding = %q", data)
	}

	clone := cfg.Clone()
	clone.Harnesses.ClaudeCode.Profiles[0].Name = "Changed"
	clone.Harnesses.ClaudeCode.Profiles = append(clone.Harnesses.ClaudeCode.Profiles, validProfileForTest())
	clone.Harnesses.ClaudeCode.Profiles[1].ID = "33333333-3333-4333-8333-333333333333"
	if cfg.Harnesses.ClaudeCode.Profiles[0].Name != "Daily" || len(cfg.Harnesses.ClaudeCode.Profiles) != 1 {
		t.Fatalf("Config.Clone shares profile state: %#v", cfg.Harnesses.ClaudeCode)
	}
	if &clone.Harnesses.ClaudeCode.Profiles[0] == &cfg.Harnesses.ClaudeCode.Profiles[0] {
		t.Fatal("Config.Clone shares profile backing array")
	}
}

func TestClaudeCodePathModeValidation(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(home, ".claude", "settings.json")
	cases := []struct {
		name      string
		mode      string
		path      string
		wantValid bool
	}{
		{name: "default", mode: PathModeDefault, path: "", wantValid: true},
		{name: "custom", mode: PathModeCustom, path: filepath.Join(t.TempDir(), "settings.json"), wantValid: true},
		{name: "custom equals default", mode: PathModeCustom, path: defaultPath, wantValid: true},
		{name: "default with path", mode: PathModeDefault, path: defaultPath},
		{name: "relative", mode: PathModeCustom, path: "settings.json"},
		{name: "tilde is literal and not absolute", mode: PathModeCustom, path: "~/settings.json"},
		{name: "environment variable is literal and not absolute", mode: PathModeCustom, path: "$HOME/settings.json"},
		{name: "empty custom", mode: PathModeCustom, path: ""},
		{name: "unknown mode", mode: "other", path: ""},
		{name: "control character", mode: PathModeCustom, path: filepath.Join(t.TempDir(), "bad\nsettings.json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configWithProfileForTest()
			cfg.Harnesses.ClaudeCode.PathMode = tc.mode
			cfg.Harnesses.ClaudeCode.SettingsPath = tc.path
			err := cfg.Validate()
			if tc.wantValid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !tc.wantValid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestProfileValidationBoundariesAndUniqueness(t *testing.T) {
	valid := configWithProfileForTest()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"haiku", "sonnet", "opus", "fable"} {
		cfg := valid.Clone()
		switch field {
		case "haiku":
			cfg.Harnesses.ClaudeCode.Profiles[0].HaikuModel = ""
		case "sonnet":
			cfg.Harnesses.ClaudeCode.Profiles[0].SonnetModel = ""
		case "opus":
			cfg.Harnesses.ClaudeCode.Profiles[0].OpusModel = ""
		case "fable":
			cfg.Harnesses.ClaudeCode.Profiles[0].FableModel = ""
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("empty required %s model was accepted", field)
		}
	}

	boundary := valid.Clone()
	boundary.Harnesses.ClaudeCode.Profiles[0].HaikuModel = strings.Repeat("a", 256)
	if err := boundary.Validate(); err != nil {
		t.Fatalf("256-byte model rejected: %v", err)
	}
	for _, model := range []string{strings.Repeat("a", 257), "bad\x00model", string([]byte{0xff})} {
		invalid := valid.Clone()
		invalid.Harnesses.ClaudeCode.Profiles[0].SubagentModel = model
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid optional model %q was accepted", model)
		}
	}

	for _, name := range []string{"", " leading", "trailing ", "bad\nname", string([]byte{0xff})} {
		invalid := valid.Clone()
		invalid.Harnesses.ClaudeCode.Profiles[0].Name = name
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid profile name %q was accepted", name)
		}
	}
	for _, id := range []string{"", "not-a-uuid", "22222222-2222-4222-8222-22222222222z"} {
		invalid := valid.Clone()
		invalid.Harnesses.ClaudeCode.Profiles[0].ID = id
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid profile ID %q was accepted", id)
		}
	}

	duplicateID := valid.Clone()
	duplicateID.Harnesses.ClaudeCode.Profiles = append(duplicateID.Harnesses.ClaudeCode.Profiles, validProfileForTest())
	duplicateID.Harnesses.ClaudeCode.Profiles[1].Name = "Other"
	if err := duplicateID.Validate(); err == nil {
		t.Fatal("duplicate profile ID was accepted")
	}
	duplicateName := valid.Clone()
	other := validProfileForTest()
	other.ID = "33333333-3333-4333-8333-333333333333"
	duplicateName.Harnesses.ClaudeCode.Profiles = append(duplicateName.Harnesses.ClaudeCode.Profiles, other)
	if err := duplicateName.Validate(); err == nil {
		t.Fatal("duplicate profile name was accepted")
	}

	for _, id := range []string{"not-a-uuid", "33333333-3333-4333-8333-333333333333"} {
		invalid := valid.Clone()
		invalid.Harnesses.ClaudeCode.ActiveProfileID = id
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid active profile ID %q was accepted", id)
		}
	}
	valid.Harnesses.ClaudeCode.ActiveProfileID = valid.Harnesses.ClaudeCode.Profiles[0].ID
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid active profile rejected: %v", err)
	}
}

func TestDecodeRejectsStrictHarnessSchemaViolations(t *testing.T) {
	valid := `{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[]}}}`
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatalf("minimal harness config rejected: %v", err)
	}
	for _, raw := range []string{
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":null}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":[]}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"unknown":{}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"unknown":true}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"path_mode":1}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"settings_path":false}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"disable_telemetry":null}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":{}}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[null]}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[{"id":"22222222-2222-4222-8222-222222222222","name":1}]}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[{"id":"22222222-2222-4222-8222-222222222222","name":"Daily","unknown":true}]}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[]}}} {}`,
		`{"schema_version":1,"auth":{"management_key":"m","management_key":"other"},"auto_mode":{},"harnesses":{"claude_code":{"profiles":[]}}}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("Decode(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestClientActiveProfileIsReadOnlyAndServerUpdatesReconcile(t *testing.T) {
	current := configWithProfileForTest()
	profileID := current.Harnesses.ClaudeCode.Profiles[0].ID
	current.Harnesses.ClaudeCode.ActiveProfileID = profileID
	if err := current.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"active_profile_id":""}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"active_profile_id":"22222222-2222-4222-8222-222222222222"}}}`,
		`{"schema_version":1,"auth":{"management_key":"m"},"auto_mode":{},"harnesses":{"claude_code":{"active_profile_id":null}}}`,
	} {
		if _, err := DecodeClient([]byte(raw)); !errors.Is(err, ErrActiveProfileReadOnly) {
			t.Fatalf("DecodeClient(%s) error = %v, want read-only error", raw, err)
		}
	}

	next := current.Clone()
	next.Harnesses.ClaudeCode.ActiveProfileID = ""
	next.Providers[0].Priority = 10
	prepared, err := current.ApplyClientUpdate(next)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Harnesses.ClaudeCode.ActiveProfileID != profileID {
		t.Fatalf("unrelated client update lost active ID: %q", prepared.Harnesses.ClaudeCode.ActiveProfileID)
	}

	next = current.Clone()
	next.Harnesses.ClaudeCode.ActiveProfileID = ""
	next.Harnesses.ClaudeCode.Profiles[0].SonnetModel = "changed-model"
	prepared, err = current.ApplyClientUpdate(next)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatalf("active profile survived an active profile change: %q", prepared.Harnesses.ClaudeCode.ActiveProfileID)
	}

	if _, err := current.ApplyServerUpdate(func() Config {
		candidate := current.Clone()
		candidate.Harnesses.ClaudeCode.ActiveProfileID = "33333333-3333-4333-8333-333333333333"
		return candidate
	}()); !errors.Is(err, ErrActiveProfileReadOnly) {
		t.Fatalf("server active ID replacement error = %v", err)
	}
	cleared, err := current.WithActiveProfileID("")
	if err != nil || cleared.Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatalf("WithActiveProfileID(clear) = %#v, %v", cleared.Harnesses.ClaudeCode, err)
	}
	set, err := cleared.WithActiveProfileID(profileID)
	if err != nil || set.Harnesses.ClaudeCode.ActiveProfileID != profileID {
		t.Fatalf("WithActiveProfileID(set) = %#v, %v", set.Harnesses.ClaudeCode, err)
	}
}

func TestClientConfigUpdateHasNoServerOwnedFields(t *testing.T) {
	value := configWithProfileForTest()
	update, err := DecodeClientUpdate(func() []byte {
		data, marshalErr := Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var root map[string]json.RawMessage
		if unmarshalErr := json.Unmarshal(data, &root); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		var harnesses map[string]json.RawMessage
		if unmarshalErr := json.Unmarshal(root["harnesses"], &harnesses); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		var claude map[string]json.RawMessage
		if unmarshalErr := json.Unmarshal(harnesses["claude_code"], &claude); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		delete(claude, "active_profile_id")
		harnesses["claude_code"], _ = json.Marshal(claude)
		root["harnesses"], _ = json.Marshal(harnesses)
		result, _ := json.Marshal(root)
		return result
	}())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"active_profile_id"`)) {
		t.Fatalf("client update encoded a server-owned field: %s", encoded)
	}
	decoded, err := DecodeClientUpdate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configFromClientUpdate(decoded), value.Normalize()) {
		t.Fatalf("decoded client update = %#v, want %#v", configFromClientUpdate(decoded), value.Normalize())
	}
	value.Harnesses.ClaudeCode.ActiveProfileID = value.Harnesses.ClaudeCode.Profiles[0].ID
	prepared, err := value.ApplyClientRequest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Harnesses.ClaudeCode.ActiveProfileID != value.Harnesses.ClaudeCode.ActiveProfileID {
		t.Fatalf("client request did not preserve active resource state: %q", prepared.Harnesses.ClaudeCode.ActiveProfileID)
	}

}
