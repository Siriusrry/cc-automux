package harnessconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testActivation() ActivationInput {
	return ActivationInput{
		ListenAddr: "127.0.0.1:8765",
		GatewayKey: "gateway-test-value",
		Profile: Profile{
			ID:                   "00000000-0000-4000-8000-000000000000",
			Name:                 "Test profile",
			HaikuModel:           "model-haiku",
			SonnetModel:          "model-sonnet",
			OpusModel:            "model-opus",
			FableModel:           "model-fable",
			SubagentModel:        "model-subagent",
			TeammateDefaultModel: "model-teammate",
		},
		DisableTelemetry: true,
	}
}

func testAdapter(t *testing.T) *ClaudeCodeAdapter {
	t.Helper()
	return NewClaudeCodeAdapterWithHomeResolver(func() (string, error) {
		return t.TempDir(), nil
	})
}

func testProjection(t *testing.T, disableTelemetry bool, optional bool) ManagedProjection {
	t.Helper()
	input := testActivation()
	input.DisableTelemetry = disableTelemetry
	if !optional {
		input.Profile.SubagentModel = ""
		input.Profile.TeammateDefaultModel = ""
	}
	projection, err := NewClaudeCodeAdapter().BuildManagedProjection(input)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func TestPathResolutionUsesExplicitDefaultAndLiteralCustomModes(t *testing.T) {
	home := t.TempDir()
	adapter := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) {
		return home, nil
	})
	got, err := adapter.ResolvePath(DefaultPathConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", "settings.json")
	if got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}

	custom := filepath.Join(home, "separate", "settings.json")
	got, err = adapter.ResolvePath(CustomPathConfig(custom))
	if err != nil || got != custom {
		t.Fatalf("custom path = %q, err %v", got, err)
	}
	// A custom path is literal. It is not expanded even when it resembles a
	// shell shorthand or an environment-variable reference.
	literal := filepath.Join(home, "~", "$NOT_EXPANDED", "settings.json")
	got, err = adapter.ResolvePath(CustomPathConfig(literal))
	if err != nil || got != literal {
		t.Fatalf("literal custom path = %q, err %v", got, err)
	}
	if _, err := adapter.ResolvePath(PathConfig{PathMode: PathModeDefault, SettingsPath: custom}); !errors.Is(err, ErrInvalidPathConfig) {
		t.Fatalf("default path with value error = %v", err)
	}
	if _, err := adapter.ResolvePath(PathConfig{PathMode: PathModeCustom, SettingsPath: "relative/settings.json"}); !errors.Is(err, ErrInvalidPathConfig) {
		t.Fatalf("relative custom path error = %v", err)
	}
	if _, err := adapter.ResolvePath(PathConfig{PathMode: PathModeCustom, SettingsPath: "~/settings.json"}); !errors.Is(err, ErrInvalidPathConfig) {
		t.Fatalf("tilde custom path error = %v", err)
	}
	if _, err := adapter.ResolvePath(PathConfig{PathMode: "other"}); !errors.Is(err, ErrInvalidPathConfig) {
		t.Fatalf("unknown path mode error = %v", err)
	}
	if err := (PathConfig{PathMode: PathModeDefault}).Validate(); err != nil {
		t.Fatalf("default PathConfig.Validate() error = %v", err)
	}
	if err := (PathConfig{PathMode: PathModeCustom, SettingsPath: custom}).Validate(); err != nil {
		t.Fatalf("custom PathConfig.Validate() error = %v", err)
	}

	resolverErr := errors.New("home unavailable")
	broken := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return "", resolverErr })
	if _, err := broken.ResolvePath(DefaultPathConfig()); !errors.Is(err, ErrInvalidPathConfig) || !strings.Contains(err.Error(), resolverErr.Error()) {
		t.Fatalf("home resolver error = %v", err)
	}
	for _, value := range []string{"", "relative", "\x00absolute"} {
		bad := NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return value, nil })
		if _, err := bad.ResolvePath(DefaultPathConfig()); !errors.Is(err, ErrInvalidPathConfig) {
			t.Errorf("home %q accepted: %v", value, err)
		}
	}
}

type registryAdapter struct{ id string }

func (a *registryAdapter) ID() string { return a.id }

func (*registryAdapter) ResolvePath(PathConfig) (string, error) { return "", nil }

func (*registryAdapter) BuildManagedProjection(ActivationInput) (ManagedProjection, error) {
	return ManagedProjection{}, nil
}

func (*registryAdapter) Merge([]byte, ManagedProjection) ([]byte, error) { return []byte("{}"), nil }

func (*registryAdapter) Verify([]byte, ManagedProjection) error { return nil }

func TestAdapterRegistryIsImmutableAndClaudeCodeIsTheDefaultAdapter(t *testing.T) {
	claude := NewClaudeCodeAdapter()
	registry, err := NewAdapterRegistry(claude)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Lookup(ClaudeCodeAdapterID); !ok || got != claude {
		t.Fatalf("Claude Code lookup = %v, %v", got, ok)
	}
	listed := registry.List()
	if !reflect.DeepEqual(listed, []string{ClaudeCodeAdapterID}) {
		t.Fatalf("registry list = %#v", listed)
	}
	listed[0] = "changed"
	if again := registry.List(); !reflect.DeepEqual(again, []string{ClaudeCodeAdapterID}) {
		t.Fatalf("registry list shares state: %#v", again)
	}
	if _, ok := EmptyRegistry().Lookup(ClaudeCodeAdapterID); ok {
		t.Fatal("empty registry unexpectedly found Claude Code")
	}
	if got := DefaultRegistry().List(); !reflect.DeepEqual(got, []string{ClaudeCodeAdapterID}) {
		t.Fatalf("default registry list = %#v", got)
	}

	var typedNil *registryAdapter
	for name, adapters := range map[string][]Adapter{
		"nil":           {nil},
		"typed nil":     {typedNil},
		"empty id":      {&registryAdapter{}},
		"space id":      {&registryAdapter{id: "bad id"}},
		"unicode space": {&registryAdapter{id: "bad\u2003id"}},
		"duplicate":     {&registryAdapter{id: "same"}, &registryAdapter{id: "same"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry(adapters...); err == nil {
				t.Fatal("invalid registry unexpectedly succeeded")
			}
		})
	}
	customA := &registryAdapter{id: "zeta"}
	customB := &registryAdapter{id: "alpha"}
	custom, err := NewRegistryFromSlice([]Adapter{customA, customB})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := custom.List(), []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted registry list = %#v, want %#v", got, want)
	}
	var nilRegistry *Registry
	if _, ok := nilRegistry.Lookup("anything"); ok || nilRegistry.List() == nil {
		t.Fatal("nil registry contract failed")
	}
}

func TestProjectionContainsAllManagedFieldsAndExactTelemetryTuples(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	if got := len(RequiredManagedEnvKeys()); got != 10 {
		t.Fatalf("required field count = %d, want 10", got)
	}
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("disable=%t", disabled), func(t *testing.T) {
			input := testActivation()
			input.DisableTelemetry = disabled
			projection, err := adapter.BuildManagedProjection(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := projection.Validate(); err != nil {
				t.Fatal(err)
			}
			if len(projection.Env) != 11 || len(projection.TopLevel) != 1 {
				t.Fatalf("projection sizes = env %d, top %d", len(projection.Env), len(projection.TopLevel))
			}
			if projection.Env[EnvAnthropicBaseURL] != "http://127.0.0.1:8765" {
				t.Fatalf("base URL = %q", projection.Env[EnvAnthropicBaseURL])
			}
			if projection.Env[EnvAnthropicAuthToken] != input.GatewayKey {
				t.Fatal("gateway key was not projected")
			}
			if disabled {
				want := map[string]string{
					EnvClaudeCodeAttributionHeader: "0",
					EnvDisableFeedbackCommand:      "1",
					EnvDisableErrorReporting:       "1",
					EnvDisableTelemetry:            "1",
				}
				for key, value := range want {
					if projection.Env[key] != value {
						t.Errorf("%s = %q, want %q", key, projection.Env[key], value)
					}
				}
			} else {
				want := map[string]string{
					EnvClaudeCodeAttributionHeader: "1",
					EnvDisableFeedbackCommand:      "0",
					EnvDisableErrorReporting:       "",
					EnvDisableTelemetry:            "",
				}
				for key, value := range want {
					if projection.Env[key] != value {
						t.Errorf("%s = %q, want %q", key, projection.Env[key], value)
					}
				}
			}
			if _, oldNameWritten := projection.Env["DISABLE_BUG_COMMAND"]; oldNameWritten {
				t.Fatal("legacy telemetry field was projected")
			}
		})
	}

	withoutOption := testActivation()
	withoutOption.Profile.SubagentModel = ""
	withoutOption.Profile.TeammateDefaultModel = ""
	projection, err := adapter.BuildManagedProjection(withoutOption)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := projection.Env[EnvClaudeCodeSubagentModel]; ok {
		t.Fatal("unset subagent model was projected")
	}
	if _, ok := projection.TopLevel[TopLevelTeammateDefaultModel]; ok {
		t.Fatal("unset teammate model was projected")
	}
	clone := projection.Clone()
	clone.Env[EnvAnthropicBaseURL] = "changed"
	if projection.Env[EnvAnthropicBaseURL] == "changed" {
		t.Fatal("projection clone shares env map")
	}
}

func TestActivationValidationRejectsInvalidBoundaryValues(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	cases := []struct {
		name string
		edit func(*ActivationInput)
		want error
	}{
		{name: "empty key", edit: func(v *ActivationInput) { v.GatewayKey = "" }, want: ErrGatewayKeyRequired},
		{name: "key whitespace", edit: func(v *ActivationInput) { v.GatewayKey = "key value" }, want: ErrInvalidActivation},
		{name: "listen host", edit: func(v *ActivationInput) { v.ListenAddr = "0.0.0.0:8765" }, want: ErrInvalidListenAddr},
		{name: "listen port spelling", edit: func(v *ActivationInput) { v.ListenAddr = "127.0.0.1:08765" }, want: ErrInvalidListenAddr},
		{name: "listen port range", edit: func(v *ActivationInput) { v.ListenAddr = "127.0.0.1:65536" }, want: ErrInvalidListenAddr},
		{name: "empty haiku", edit: func(v *ActivationInput) { v.Profile.HaikuModel = "" }, want: ErrInvalidModel},
		{name: "long model", edit: func(v *ActivationInput) { v.Profile.OpusModel = strings.Repeat("x", 257) }, want: ErrInvalidModel},
		{name: "control model", edit: func(v *ActivationInput) { v.Profile.FableModel = "bad\nmodel" }, want: ErrInvalidModel},
		{name: "long optional", edit: func(v *ActivationInput) { v.Profile.SubagentModel = strings.Repeat("x", 257) }, want: ErrInvalidModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := testActivation()
			tc.edit(&input)
			if _, err := adapter.BuildManagedProjection(input); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
	invalidUTF8 := testActivation()
	invalidUTF8.Profile.HaikuModel = string([]byte{0xff})
	if _, err := adapter.BuildManagedProjection(invalidUTF8); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("invalid UTF-8 model error = %v", err)
	}
}

func TestMergePreservesUnknownFieldsAndManagesOptionalFields(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	projection := testProjection(t, true, false)
	original := []byte(`{
  "customTop": {"keep": true},
  "env": {
    "KEEP_STRING": "value",
    "KEEP_NUMBER": 42,
    "KEEP_OBJECT": {"nested": [true, false]},
    "CLAUDE_CODE_SUBAGENT_MODEL": "old-subagent",
    "DISABLE_BUG_COMMAND": "legacy"
  },
  "teammateDefaultModel": "old-teammate"
}`)
	merged, err := adapter.Merge(original, projection)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(merged, []byte{'\n'}) {
		t.Fatal("merged document has no final newline")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(merged, &top); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(top["customTop"], map[string]any{"keep": true}) {
		t.Fatalf("unknown top-level field changed: %#v", top["customTop"])
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(got["env"], &env); err != nil {
		t.Fatal(err)
	}
	if string(env["KEEP_NUMBER"]) != "42" || string(env["DISABLE_BUG_COMMAND"]) != `"legacy"` {
		t.Fatalf("unknown env fields changed: number=%s legacy=%s", env["KEEP_NUMBER"], env["DISABLE_BUG_COMMAND"])
	}
	if _, ok := env[EnvClaudeCodeSubagentModel]; ok {
		t.Fatal("unset optional env field was not deleted")
	}
	if _, ok := got[TopLevelTeammateDefaultModel]; ok {
		t.Fatal("unset optional top-level field was not deleted")
	}
	if err := adapter.Verify(merged, projection); err != nil {
		t.Fatalf("Verify(merged) error = %v", err)
	}

	withOptions := testProjection(t, false, true)
	withOptionsMerged, err := adapter.Merge(merged, withOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Verify(withOptionsMerged, withOptions); err != nil {
		t.Fatal(err)
	}
	var withOptionsTop map[string]json.RawMessage
	if err := json.Unmarshal(withOptionsMerged, &withOptionsTop); err != nil {
		t.Fatal(err)
	}
	var withOptionsEnv map[string]json.RawMessage
	if err := json.Unmarshal(withOptionsTop["env"], &withOptionsEnv); err != nil {
		t.Fatal(err)
	}
	var subagent, errorReporting string
	if err := json.Unmarshal(withOptionsEnv[EnvClaudeCodeSubagentModel], &subagent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(withOptionsEnv[EnvDisableErrorReporting], &errorReporting); err != nil {
		t.Fatal(err)
	}
	if subagent != "model-subagent" || errorReporting != "" {
		t.Fatalf("optional/telemetry fields = %#v", withOptionsEnv)
	}
	if string(withOptionsTop[TopLevelTeammateDefaultModel]) != `"model-teammate"` {
		t.Fatalf("teammate field = %s", withOptionsTop[TopLevelTeammateDefaultModel])
	}
	if _, err := adapter.Merge(merged, withOptions); err != nil {
		t.Fatal(err)
	}
	if first, _ := adapter.Merge(original, projection); !bytes.Equal(first, mustMerge(t, adapter, original, projection)) {
		t.Fatal("merge output is not deterministic")
	}
}

func mustMerge(t *testing.T, adapter *ClaudeCodeAdapter, original []byte, projection ManagedProjection) []byte {
	t.Helper()
	data, err := adapter.Merge(original, projection)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMergeAcceptsMissingDocumentAndCreatesEnv(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	projection := testProjection(t, true, false)
	for _, original := range [][]byte{nil, {}} {
		merged, err := adapter.Merge(original, projection)
		if err != nil {
			t.Fatalf("Merge(%q) error = %v", original, err)
		}
		if err := adapter.Verify(merged, projection); err != nil {
			t.Fatalf("Verify(Merge(%q)) error = %v", original, err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(merged, &top); err != nil {
			t.Fatal(err)
		}
		if _, ok := top["env"]; !ok {
			t.Fatal("missing env object")
		}
	}
}

func TestMergeRejectsMalformedDuplicateTrailingAndWrongShapes(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	projection := testProjection(t, true, false)
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{name: "malformed", raw: []byte(`{"env":`), want: ErrInvalidJSON},
		{name: "trailing", raw: []byte(`{} {}`), want: ErrTrailingJSON},
		{name: "duplicate top", raw: []byte(`{"env":{},"env":{}}`), want: ErrDuplicateJSONKey},
		{name: "duplicate unknown nested", raw: []byte(`{"custom":{"x":1,"x":2}}`), want: ErrDuplicateJSONKey},
		{name: "duplicate env", raw: []byte(`{"env":{"x":1,"x":2}}`), want: ErrDuplicateJSONKey},
		{name: "array top", raw: []byte(`[]`), want: ErrTopLevelNotObject},
		{name: "null top", raw: []byte(`null`), want: ErrTopLevelNotObject},
		{name: "env null", raw: []byte(`{"env":null}`), want: ErrEnvNotObject},
		{name: "env array", raw: []byte(`{"env":[]}`), want: ErrEnvNotObject},
		{name: "env string", raw: []byte(`{"env":"not-an-object"}`), want: ErrEnvNotObject},
		{name: "invalid UTF-8", raw: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, want: ErrInvalidJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Merge(tc.raw, projection); !errors.Is(err, tc.want) {
				t.Fatalf("Merge error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsMissingWrongAndNonStringManagedValues(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	projection := testProjection(t, true, false)
	valid, err := adapter.Merge([]byte(`{}`), projection)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(valid, &top); err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(top["env"], &env); err != nil {
		t.Fatal(err)
	}
	delete(env, EnvAnthropicDefaultOpusModel)
	top["env"], _ = json.Marshal(env)
	missing, _ := json.Marshal(top)
	if err := adapter.Verify(missing, projection); !errors.Is(err, ErrProjectionMismatch) {
		t.Fatalf("missing field Verify error = %v", err)
	}

	env[EnvAnthropicDefaultOpusModel] = []byte(`{"nested":true}`)
	top["env"], _ = json.Marshal(env)
	nonString, _ := json.Marshal(top)
	if err := adapter.Verify(nonString, projection); !errors.Is(err, ErrProjectionMismatch) {
		t.Fatalf("non-string Verify error = %v", err)
	}

	wrong := projection.Clone()
	wrong.Env[EnvDisableTelemetry] = "0"
	if err := wrong.Validate(); !errors.Is(err, ErrInvalidProjection) {
		t.Fatalf("invalid telemetry projection error = %v", err)
	}
	if err := adapter.Verify(valid, wrong); !errors.Is(err, ErrInvalidProjection) {
		t.Fatalf("invalid projection Verify error = %v", err)
	}
	if err := adapter.Verify([]byte(`{"env":{}}`), projection); !errors.Is(err, ErrProjectionMismatch) {
		t.Fatalf("empty document Verify error = %v", err)
	}
}

func TestFileStoreExistingTargetCreatesOneTimeBackupAndUsesPrivateFiles(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "nested", "settings")
	target := filepath.Join(targetDir, "settings.json")
	original := []byte(`{"custom":true,"env":{"KEEP":123}}`)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	projection := testProjection(t, true, false)
	store := NewFileStore(FileStoreOptions{Clock: ClockFunc(func() time.Time { return time.Unix(123, 0) })})
	final, err := store.Apply(target, NewClaudeCodeAdapter(), projection)
	if err != nil {
		t.Fatal(err)
	}
	backup := BackupPath(target)
	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupData, original) {
		t.Fatalf("backup = %q, want original %q", backupData, original)
	}
	if !bytes.Equal(final, mustReadFile(t, target)) {
		t.Fatal("Apply result differs from committed target")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %v, err %v; want 0600", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, err %v; want 0600", info.Mode().Perm(), err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(targetDir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("target directory mode = %v, err %v; want 0700", info.Mode().Perm(), err)
		}
	}

	secondProjection := testProjection(t, false, true)
	if err := store.Write(target, NewClaudeCodeAdapter(), secondProjection); err != nil {
		t.Fatal(err)
	}
	if got := mustReadFile(t, backup); !bytes.Equal(got, original) {
		t.Fatalf("backup was overwritten: %q", got)
	}
	if err := NewClaudeCodeAdapter().Verify(mustReadFile(t, target), secondProjection); err != nil {
		t.Fatalf("second committed target Verify error = %v", err)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(target) && entry.Name() != BackupFileName {
			t.Errorf("temporary entry remains: %s", entry.Name())
		}
	}
}

func TestFileStoreMissingTargetCreatesNoBackup(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "settings.json")
	projection := testProjection(t, true, false)
	if _, err := NewFileStore().Apply(target, NewClaudeCodeAdapter(), projection); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(BackupPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup stat error = %v, want not exist", err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new target mode = %v, err %v; want 0600", info.Mode().Perm(), err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFileStoreChecksTargetAndBackupTypes(t *testing.T) {
	root := t.TempDir()
	projection := testProjection(t, true, false)
	adapter := NewClaudeCodeAdapter()

	regular := filepath.Join(root, "regular.json")
	if err := os.WriteFile(regular, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "target-link.json")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewFileStore().Apply(symlink, adapter, projection); !errors.Is(err, ErrTargetSymlink) {
		t.Fatalf("target symlink error = %v", err)
	}
	directory := filepath.Join(root, "target-dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore().Apply(directory, adapter, projection); !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("target directory error = %v", err)
	}

	target := filepath.Join(root, "with-backup.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := BackupPath(target)
	backupLink := filepath.Join(root, "backup-link-target")
	if err := os.WriteFile(backupLink, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backupLink, backup); err != nil {
		t.Skipf("backup symlinks unavailable: %v", err)
	}
	if _, err := NewFileStore().Apply(target, adapter, projection); !errors.Is(err, ErrBackupSymlink) {
		t.Fatalf("backup symlink error = %v", err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore().Apply(target, adapter, projection); !errors.Is(err, ErrBackupNotRegular) {
		t.Fatalf("backup directory error = %v", err)
	}
	if !errors.Is(ErrTargetSymlink, ErrSymlink) || !errors.Is(ErrTargetNotRegular, ErrNotRegular) {
		t.Fatal("low-level error aliases are not usable with errors.Is")
	}
}

func TestFileStoreEnforcesExistingTargetReadLimit(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "large.json")
	exact := make([]byte, MaxExistingTargetBytes)
	exact[0] = '{'
	exact[len(exact)-1] = '}'
	for i := 1; i < len(exact)-1; i++ {
		exact[i] = ' '
	}
	if err := os.WriteFile(target, exact, 0o600); err != nil {
		t.Fatal(err)
	}
	projection := testProjection(t, true, false)
	if _, err := NewFileStore().Apply(target, NewClaudeCodeAdapter(), projection); err != nil {
		t.Fatalf("exact-limit target rejected: %v", err)
	}
	if _, err := os.Stat(BackupPath(target)); err != nil {
		t.Fatalf("exact-limit backup missing: %v", err)
	}

	oversizedRoot := t.TempDir()
	oversized := filepath.Join(oversizedRoot, "oversized.json")
	tooLarge := make([]byte, MaxExistingTargetBytes+1)
	tooLarge[0] = '{'
	tooLarge[len(tooLarge)-1] = '}'
	if err := os.WriteFile(oversized, tooLarge, 0o600); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, oversized)
	if _, err := NewFileStore().Apply(oversized, NewClaudeCodeAdapter(), projection); !errors.Is(err, ErrTargetTooLarge) {
		t.Fatalf("oversized target error = %v", err)
	}
	if after := mustReadFile(t, oversized); !bytes.Equal(after, before) {
		t.Fatal("oversized target changed")
	}
	if _, err := os.Stat(BackupPath(oversized)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized target backup stat = %v", err)
	}
	if got, err := NewFileStore().Read(oversized); !errors.Is(err, ErrTargetTooLarge) || got != nil {
		t.Fatalf("Read oversized = %d bytes, err %v", len(got), err)
	}
	if _, err := NewFileStore().Read(filepath.Join(root, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read missing error = %v", err)
	}
}

type faultOps struct {
	base              OSFileOps
	failBackupOpen    error
	failBackupWrite   error
	failBackupSync    error
	failTempCreate    error
	failTempSync      error
	failRename        error
	failDirectorySync error
}

func (o *faultOps) Lstat(path string) (fs.FileInfo, error) { return o.base.Lstat(path) }

func (o *faultOps) Open(path string) (FileHandle, error) { return o.base.Open(path) }

func (o *faultOps) OpenFile(path string, flag int, perm fs.FileMode) (FileHandle, error) {
	if filepath.Base(path) == BackupFileName && o.failBackupOpen != nil {
		return nil, o.failBackupOpen
	}
	file, err := o.base.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultHandle{
		FileHandle: file,
		failWrite:  filepath.Base(path) == BackupFileName && o.failBackupWrite != nil,
		writeErr:   o.failBackupWrite,
		failSync:   filepath.Base(path) == BackupFileName && o.failBackupSync != nil,
		syncErr:    o.failBackupSync,
	}, nil
}

func (o *faultOps) CreateTemp(directory, pattern string) (FileHandle, error) {
	if o.failTempCreate != nil {
		return nil, o.failTempCreate
	}
	file, err := o.base.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	return &faultHandle{FileHandle: file, failSync: o.failTempSync != nil, syncErr: o.failTempSync}, nil
}

func (o *faultOps) MkdirAll(path string, perm fs.FileMode) error { return o.base.MkdirAll(path, perm) }

func (o *faultOps) Rename(oldPath, newPath string) error {
	if o.failRename != nil {
		return o.failRename
	}
	return o.base.Rename(oldPath, newPath)
}

func (o *faultOps) Remove(path string) error { return o.base.Remove(path) }

func (o *faultOps) SyncDirectory(path string) error { return o.failDirectorySync }

type faultHandle struct {
	FileHandle
	failWrite bool
	writeErr  error
	failSync  bool
	syncErr   error
}

func (f *faultHandle) Write(data []byte) (int, error) {
	if f.failWrite {
		return 0, f.writeErr
	}
	return f.FileHandle.Write(data)
}

func (f *faultHandle) Sync() error {
	if f.failSync {
		return f.syncErr
	}
	return f.FileHandle.Sync()
}

func (f *faultHandle) WriteData(data []byte) (int, error) { return f.FileHandle.Write(data) }

func TestFileStoreFailurePathsPreserveTargetAndCleanTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "settings.json")
	original := []byte(`{"keep":"original","env":{"KEEP":true}}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	projection := testProjection(t, true, false)

	cases := []struct {
		name   string
		ops    func() *faultOps
		want   error
		backup bool
	}{
		{name: "backup open", ops: func() *faultOps { return &faultOps{failBackupOpen: errors.New("backup open failure")} }, want: ErrBackupIO},
		{name: "backup write", ops: func() *faultOps { return &faultOps{failBackupWrite: errors.New("backup write failure")} }, want: ErrBackupIO},
		{name: "backup sync", ops: func() *faultOps { return &faultOps{failBackupSync: errors.New("backup sync failure")} }, want: ErrBackupIO},
		{name: "temporary create", ops: func() *faultOps { return &faultOps{failTempCreate: errors.New("temporary create failure")} }, want: ErrTemporaryIO, backup: true},
		{name: "temporary sync", ops: func() *faultOps { return &faultOps{failTempSync: errors.New("temporary sync failure")} }, want: ErrTemporaryIO, backup: true},
		{name: "rename", ops: func() *faultOps { return &faultOps{failRename: errors.New("rename failure")} }, want: ErrAtomicReplace, backup: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case gets its own target and directory, all rooted in t.TempDir.
			caseDir := t.TempDir()
			caseTarget := filepath.Join(caseDir, "settings.json")
			if err := os.WriteFile(caseTarget, original, 0o600); err != nil {
				t.Fatal(err)
			}
			ops := tc.ops()
			store := NewFileStore(FileStoreOptions{FS: ops})
			before := mustReadFile(t, caseTarget)
			if _, err := store.Apply(caseTarget, NewClaudeCodeAdapter(), projection); !errors.Is(err, tc.want) {
				t.Fatalf("Apply error = %v, want errors.Is %v", err, tc.want)
			}
			if after := mustReadFile(t, caseTarget); !bytes.Equal(after, before) {
				t.Fatal("target changed after failed Apply")
			}
			backupExists := false
			if _, err := os.Stat(BackupPath(caseTarget)); err == nil {
				backupExists = true
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if backupExists != tc.backup {
				t.Fatalf("backup exists = %t, want %t", backupExists, tc.backup)
			}
			entries, err := os.ReadDir(caseDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != filepath.Base(caseTarget) && entry.Name() != BackupFileName {
					t.Errorf("temporary file remains: %s", entry.Name())
				}
			}
		})
	}

	_ = target // keep the initial fixture path visibly scoped to this temp tree.
}

func TestFileStoreIgnoresPostCommitDirectorySyncFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "settings.json")
	ops := &faultOps{failDirectorySync: errors.New("directory sync unsupported")}
	store := NewFileStore(FileStoreOptions{FS: ops})
	if _, err := store.Apply(target, NewClaudeCodeAdapter(), testProjection(t, true, false)); err != nil {
		t.Fatalf("directory sync failure was surfaced: %v", err)
	}
}

func TestFileStoreRejectsInvalidTargetAndBackupPath(t *testing.T) {
	adapter := NewClaudeCodeAdapter()
	projection := testProjection(t, true, false)
	for _, path := range []string{"", "relative/settings.json"} {
		if _, err := NewFileStore().Apply(path, adapter, projection); !errors.Is(err, ErrInvalidTargetPath) {
			t.Fatalf("path %q error = %v", path, err)
		}
	}
	path := filepath.Join(t.TempDir(), BackupFileName)
	if _, err := NewFileStore().Apply(path, adapter, projection); !errors.Is(err, ErrPathConflict) {
		t.Fatalf("backup-named target error = %v", err)
	}
	if _, err := NewFileStore().Apply(filepath.Join(t.TempDir(), "settings.json"), nil, projection); !errors.Is(err, ErrNilAdapter) {
		t.Fatalf("nil adapter error = %v", err)
	}
	var nilStore *FileStore
	if _, err := nilStore.Apply(filepath.Join(t.TempDir(), "settings.json"), adapter, projection); !errors.Is(err, ErrNilFileSystem) {
		t.Fatalf("nil store error = %v", err)
	}
}
