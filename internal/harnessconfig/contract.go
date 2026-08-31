// Package harnessconfig contains the harness-facing configuration-file
// contracts. It deliberately has no dependency on the persistent runtime
// configuration or on the management HTTP layer.
package harnessconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

const (
	// ClaudeCodeAdapterID is the public harness identifier used by the generic
	// harness API. The persistent configuration key is kept separately because
	// URL identifiers and schema keys are different namespaces.
	ClaudeCodeAdapterID = "claude-code"
	ClaudeCodeSchemaKey = "claude_code"

	PathModeDefault = "default"
	PathModeCustom  = "custom"

	BackupFileName = ".cc-automux.bak"

	// MaxExistingTargetBytes is the fixed limit for an existing Claude Code
	// settings file. It does not apply to the Messages data plane.
	MaxExistingTargetBytes int64 = 8 * 1024 * 1024
)

// Names of the Claude Code settings managed by a profile activation.
const (
	EnvAnthropicBaseURL            = "ANTHROPIC_BASE_URL"
	EnvAnthropicAuthToken          = "ANTHROPIC_AUTH_TOKEN"
	EnvAnthropicDefaultHaikuModel  = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
	EnvAnthropicDefaultSonnetModel = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	EnvAnthropicDefaultOpusModel   = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	EnvAnthropicDefaultFableModel  = "ANTHROPIC_DEFAULT_FABLE_MODEL"
	EnvClaudeCodeSubagentModel     = "CLAUDE_CODE_SUBAGENT_MODEL"
	EnvClaudeCodeAttributionHeader = "CLAUDE_CODE_ATTRIBUTION_HEADER"
	EnvDisableFeedbackCommand      = "DISABLE_FEEDBACK_COMMAND"
	EnvDisableErrorReporting       = "DISABLE_ERROR_REPORTING"
	EnvDisableTelemetry            = "DISABLE_TELEMETRY"

	TopLevelTeammateDefaultModel = "teammateDefaultModel"
)

// Short aliases make the managed-field names convenient at integration
// boundaries without creating a second set of field semantics.
const (
	ManagedEnvBaseURL           = EnvAnthropicBaseURL
	ManagedEnvAuthToken         = EnvAnthropicAuthToken
	ManagedEnvHaikuModel        = EnvAnthropicDefaultHaikuModel
	ManagedEnvSonnetModel       = EnvAnthropicDefaultSonnetModel
	ManagedEnvOpusModel         = EnvAnthropicDefaultOpusModel
	ManagedEnvFableModel        = EnvAnthropicDefaultFableModel
	ManagedEnvSubagentModel     = EnvClaudeCodeSubagentModel
	ManagedEnvAttributionHeader = EnvClaudeCodeAttributionHeader
	ManagedEnvFeedbackCommand   = EnvDisableFeedbackCommand
	ManagedEnvErrorReporting    = EnvDisableErrorReporting
	ManagedEnvTelemetry         = EnvDisableTelemetry
	ManagedTeammateDefaultModel = TopLevelTeammateDefaultModel
)

var (
	ErrNilAdapter         = errors.New("harnessconfig: nil adapter")
	ErrDuplicateAdapter   = errors.New("harnessconfig: duplicate adapter")
	ErrInvalidAdapter     = errors.New("harnessconfig: invalid adapter")
	ErrInvalidPathConfig  = errors.New("harnessconfig: invalid path configuration")
	ErrInvalidActivation  = errors.New("harnessconfig: invalid activation input")
	ErrGatewayKeyRequired = errors.New("harnessconfig: gateway key is required")
	ErrInvalidListenAddr  = errors.New("harnessconfig: invalid listen address")
	ErrInvalidModel       = errors.New("harnessconfig: invalid model")
	ErrInvalidProjection  = errors.New("harnessconfig: invalid managed projection")
	ErrProjectionMismatch = errors.New("harnessconfig: managed projection mismatch")
	ErrInvalidJSON        = errors.New("harnessconfig: invalid JSON")
	ErrDuplicateJSONKey   = errors.New("harnessconfig: duplicate JSON key")
	ErrTrailingJSON       = errors.New("harnessconfig: trailing JSON")
	ErrTopLevelNotObject  = errors.New("harnessconfig: top-level JSON value must be an object")
	ErrEnvNotObject       = errors.New("harnessconfig: env must be an object")
	ErrSymlink            = errors.New("harnessconfig: path must not be a symbolic link")
	ErrNotRegular         = errors.New("harnessconfig: path must be a regular file")
	ErrTooLarge           = errors.New("harnessconfig: target file exceeds the read limit")
	ErrTargetTooLarge     = fmt.Errorf("%w: target", ErrTooLarge)
	ErrTargetSymlink      = fmt.Errorf("%w: target", ErrSymlink)
	ErrBackupSymlink      = fmt.Errorf("%w: backup", ErrSymlink)
	ErrTargetNotRegular   = fmt.Errorf("%w: target", ErrNotRegular)
	ErrBackupNotRegular   = fmt.Errorf("%w: backup", ErrNotRegular)
	ErrTempNotRegular     = errors.New("harnessconfig: temporary file must be regular")
	ErrTargetChanged      = errors.New("harnessconfig: target changed during write")
	ErrPathConflict       = errors.New("harnessconfig: target and backup paths conflict")
	ErrBackupIO           = errors.New("harnessconfig: backup I/O failed")
	ErrTargetIO           = errors.New("harnessconfig: target I/O failed")
	ErrTemporaryIO        = errors.New("harnessconfig: temporary-file I/O failed")
	ErrAtomicReplace      = errors.New("harnessconfig: atomic replacement failed")
	ErrVerification       = errors.New("harnessconfig: post-write verification failed")
	ErrInvalidTargetPath  = errors.New("harnessconfig: target path must be absolute")
	ErrNilFileSystem      = errors.New("harnessconfig: file system is required")
)

// FileError preserves a low-level category and the underlying operating
// system error while adding the operation and affected path. Later layers can
// map errors with errors.Is without parsing error strings.
type FileError struct {
	Operation string
	Path      string
	Err       error
}

func (e *FileError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %v", e.Operation, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Operation, e.Path, e.Err)
}

func (e *FileError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PathConfig is the adapter-independent path selection contract. A default
// path is represented by an empty SettingsPath; custom paths are literal
// absolute paths and are never expanded by the adapter.
type PathConfig struct {
	PathMode     string `json:"path_mode"`
	SettingsPath string `json:"settings_path"`
}

func DefaultPathConfig() PathConfig {
	return PathConfig{PathMode: PathModeDefault}
}

func CustomPathConfig(path string) PathConfig {
	return PathConfig{PathMode: PathModeCustom, SettingsPath: path}
}

// Validate checks the adapter-independent shape of a path selection. The
// default path's concrete home directory is intentionally resolved only by an
// adapter, so this method does not touch the filesystem.
func (p PathConfig) Validate() error {
	switch p.PathMode {
	case PathModeDefault:
		if p.SettingsPath != "" {
			return fmt.Errorf("%w: default mode requires an empty settings path", ErrInvalidPathConfig)
		}
	case PathModeCustom:
		if p.SettingsPath == "" || !isAbsolutePath(p.SettingsPath) || hasControlBytes(p.SettingsPath) {
			return fmt.Errorf("%w: custom settings path must be a literal absolute path", ErrInvalidPathConfig)
		}
	default:
		return fmt.Errorf("%w: unsupported path mode %q", ErrInvalidPathConfig, p.PathMode)
	}
	return nil
}

// Profile contains the model mapping facts needed by a Claude Code
// activation. ID and Name are carried for callers that already have a
// persisted profile, but the adapter only consumes the model fields.
type Profile struct {
	ID                   string `json:"id,omitempty"`
	Name                 string `json:"name,omitempty"`
	HaikuModel           string `json:"haiku_model"`
	SonnetModel          string `json:"sonnet_model"`
	OpusModel            string `json:"opus_model"`
	FableModel           string `json:"fable_model"`
	SubagentModel        string `json:"subagent_model,omitempty"`
	TeammateDefaultModel string `json:"teammate_default_model,omitempty"`
}

// These aliases let the schema package and management layer name the same
// value according to their local responsibility without duplicating it.
type ProfileInput = Profile
type ModelProfile = Profile

// ActivationInput is the explicit, snapshot-derived input to an adapter. It
// contains no Runtime Snapshot, persistence handle, HTTP request, or route
// state, so the adapter remains independently testable and reusable.
type ActivationInput struct {
	ListenAddr       string  `json:"listen_addr"`
	GatewayKey       string  `json:"gateway_key"`
	Profile          Profile `json:"profile"`
	DisableTelemetry bool    `json:"disable_telemetry"`
}

// Validate checks the fields common to every Claude Code projection. It is
// also called by BuildManagedProjection before any map is constructed.
func (i ActivationInput) Validate() error {
	if err := validateLoopbackAddress(i.ListenAddr); err != nil {
		return err
	}
	if err := validateActivationKey(i.GatewayKey); err != nil {
		return err
	}
	for _, item := range []struct {
		field    string
		value    string
		required bool
	}{
		{field: "haiku_model", value: i.Profile.HaikuModel, required: true},
		{field: "sonnet_model", value: i.Profile.SonnetModel, required: true},
		{field: "opus_model", value: i.Profile.OpusModel, required: true},
		{field: "fable_model", value: i.Profile.FableModel, required: true},
		{field: "subagent_model", value: i.Profile.SubagentModel},
		{field: "teammate_default_model", value: i.Profile.TeammateDefaultModel},
	} {
		if err := validateModelValue(item.field, item.value, item.required); err != nil {
			return err
		}
	}
	return nil
}

// ManagedProjection is the complete set of fields an adapter is allowed to
// write. Missing optional entries mean deletion. The maps are copied by
// Clone and by adapter operations; callers should treat a returned projection
// as immutable after construction.
type ManagedProjection struct {
	Env      map[string]string `json:"env"`
	TopLevel map[string]string `json:"top_level"`
}

type Projection = ManagedProjection

func (p ManagedProjection) Clone() ManagedProjection {
	out := p
	if p.Env != nil {
		out.Env = make(map[string]string, len(p.Env))
		for key, value := range p.Env {
			out.Env[key] = value
		}
	}
	if p.TopLevel != nil {
		out.TopLevel = make(map[string]string, len(p.TopLevel))
		for key, value := range p.TopLevel {
			out.TopLevel[key] = value
		}
	}
	return out
}

func (p ManagedProjection) EnvValue(key string) (string, bool) {
	value, ok := p.Env[key]
	return value, ok
}

func (p ManagedProjection) TopLevelValue(key string) (string, bool) {
	value, ok := p.TopLevel[key]
	return value, ok
}

// RequiredManagedEnvKeys returns the ten non-optional fields in the
// documented order. The returned slice is always a fresh copy.
func RequiredManagedEnvKeys() []string {
	return []string{
		EnvAnthropicBaseURL,
		EnvAnthropicAuthToken,
		EnvAnthropicDefaultHaikuModel,
		EnvAnthropicDefaultSonnetModel,
		EnvAnthropicDefaultOpusModel,
		EnvAnthropicDefaultFableModel,
		EnvClaudeCodeAttributionHeader,
		EnvDisableFeedbackCommand,
		EnvDisableErrorReporting,
		EnvDisableTelemetry,
	}
}

func OptionalManagedEnvKeys() []string {
	return []string{EnvClaudeCodeSubagentModel}
}

func ManagedEnvKeys() []string {
	keys := RequiredManagedEnvKeys()
	return append(keys, OptionalManagedEnvKeys()...)
}

func ManagedTopLevelKeys() []string {
	return []string{TopLevelTeammateDefaultModel}
}

// Adapter is the generic harness file-semantics boundary. It does not read
// runtime state, persist root configuration, choose HTTP status codes, or
// perform management routing.
type Adapter interface {
	ID() string
	ResolvePath(PathConfig) (string, error)
	BuildManagedProjection(ActivationInput) (ManagedProjection, error)
	Merge([]byte, ManagedProjection) ([]byte, error)
	Verify([]byte, ManagedProjection) error
}

// AdapterRegistry is the read-only lookup boundary used by later Manager and
// management layers.
type AdapterRegistry interface {
	Lookup(id string) (Adapter, bool)
	List() []string
}

// Registry is immutable after construction. Its map and discovery slice are
// never exposed to callers.
type Registry struct {
	entries map[string]Adapter
	order   []string
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	entries := make(map[string]Adapter, len(adapters))
	order := make([]string, 0, len(adapters))
	for index, adapter := range adapters {
		if isNilAdapter(adapter) {
			return nil, fmt.Errorf("%w at index %d", ErrNilAdapter, index)
		}
		id := adapter.ID()
		if id == "" || id != strings.TrimSpace(id) || containsWhitespace(id) {
			return nil, fmt.Errorf("%w at index %d: adapter id must be non-empty and whitespace-free", ErrInvalidAdapter, index)
		}
		if _, exists := entries[id]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateAdapter, id)
		}
		entries[id] = adapter
		order = append(order, id)
	}
	return &Registry{entries: entries, order: order}, nil
}

func NewAdapterRegistry(adapters ...Adapter) (*Registry, error) {
	return NewRegistry(adapters...)
}

func NewRegistryFromSlice(adapters []Adapter) (*Registry, error) {
	return NewRegistry(adapters...)
}

func EmptyRegistry() *Registry {
	return &Registry{entries: map[string]Adapter{}}
}

func NewEmptyRegistry() *Registry {
	return EmptyRegistry()
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(NewClaudeCodeAdapter())
	if err != nil {
		panic(err)
	}
	return registry
}

func NewDefaultRegistry() (*Registry, error) {
	return NewRegistry(NewClaudeCodeAdapter())
}

func (r *Registry) Lookup(id string) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	adapter, ok := r.entries[id]
	return adapter, ok
}

func (r *Registry) List() []string {
	if r == nil {
		return []string{}
	}
	result := append([]string(nil), r.order...)
	if len(result) > 1 {
		sort.Strings(result)
	}
	return result
}

func isNilAdapter(adapter Adapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func containsWhitespace(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// These tiny helpers live here so PathConfig and ClaudeCodeAdapter share the
// same literal-path rules without introducing a dependency on the adapter's
// home resolver.
func isAbsolutePath(value string) bool {
	return filepath.IsAbs(value)
}

func hasControlBytes(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

var _ Adapter = (*ClaudeCodeAdapter)(nil)
var _ AdapterRegistry = (*Registry)(nil)
