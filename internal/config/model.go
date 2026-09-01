package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/modelname"
)

const (
	// SchemaVersion is the only configuration schema understood by v1.
	SchemaVersion = 1

	DefaultListenAddr  = "127.0.0.1:8765"
	DefaultLogMaxBytes = int64(100 * 1024 * 1024)

	ConfigPathEnv = "CC_AUTOMUX_CONFIG"

	configFileName = "config.json"
	appConfigDir   = "cc-automux"
)

// Auto Mode values are intentionally small, closed vocabulary strings.  They
// are exported so the runtime and flow layers do not duplicate literals.
const (
	AutoModeDisabled      = "disabled"
	AutoModeProviderPool  = "provider_pool"
	AutoModeFixedProvider = "fixed_provider"
)

// Claude Code path modes are intentionally closed schema values. The empty
// path value is meaningful only with PathModeDefault.
const (
	PathModeDefault = "default"
	PathModeCustom  = "custom"
)

// ErrActiveProfileReadOnly identifies an attempt to supply the server-owned
// active profile state through a client configuration update.
var ErrActiveProfileReadOnly = errors.New("active_profile_id is server-managed and read-only")

// ClientConfigUpdate is the client-owned portion of a configuration PUT. The
// server-owned active profile ID is intentionally absent.
type ClientConfigUpdate struct {
	SchemaVersion int              `json:"schema_version"`
	Service       ServiceConfig    `json:"service"`
	Auth          AuthConfig       `json:"auth"`
	AutoMode      AutoModeConfig   `json:"auto_mode"`
	Harnesses     ClientHarnesses  `json:"harnesses"`
	Providers     []ProviderConfig `json:"providers"`
}

// Fixed provider protocol identifiers are schema values, not an indication
// that a protocol adapter is currently available at runtime.  Adapters are
// looked up by the execution layer and a missing implementation fails closed.
const (
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolOpenAICompatible  = "openai_compatible"
)

// Config is the complete persisted configuration and management resource
// representation. It intentionally has no fields from the retired v0
// configuration; server-owned state is visible here and is excluded from
// ClientConfigUpdate requests.
type Config struct {
	SchemaVersion int              `json:"schema_version"`
	Service       ServiceConfig    `json:"service"`
	Auth          AuthConfig       `json:"auth"`
	AutoMode      AutoModeConfig   `json:"auto_mode"`
	Harnesses     HarnessesConfig  `json:"harnesses"`
	Providers     []ProviderConfig `json:"providers"`
}

type ServiceConfig struct {
	ListenAddr  string `json:"listen_addr"`
	LogMaxBytes int64  `json:"log_max_bytes"`
}

type AuthConfig struct {
	GatewayKey    string `json:"gateway_key"`
	ManagementKey string `json:"management_key"`
}

// HarnessesConfig contains the persisted configuration for the supported
// harness adapters. The value form deliberately keeps the Claude Code object
// present in every normalized v1 configuration, even before activation.
type HarnessesConfig struct {
	ClaudeCode ClaudeCodeConfig `json:"claude_code"`
}

// ClientHarnesses and ClientClaudeCodeConfig contain only fields accepted in
// a client update request.
type ClientHarnesses struct {
	ClaudeCode ClientClaudeCodeConfig `json:"claude_code"`
}

type ClientClaudeCodeConfig struct {
	PathMode         string    `json:"path_mode"`
	SettingsPath     string    `json:"settings_path"`
	DisableTelemetry bool      `json:"disable_telemetry"`
	Profiles         []Profile `json:"profiles"`
}

// HarnessMutation is the narrow server-side transaction surface used by the
// harness configuration service. Implementations hold the Runtime mutation
// lock for the lifetime of the callback that receives this value. The methods
// intentionally return only errors so the config package does not depend on
// Runtime's result types.
type HarnessMutation interface {
	Config() Config
	UpdateHarness(func(*HarnessesConfig) error) error
	ClearActiveProfileID() error
	SetActiveProfileID(string) error
}

// ClaudeCodeConfig is the persistent, file-independent portion of the Claude
// Code harness configuration. External settings.json semantics belong to the
// harnessconfig package; this type only describes durable CC AutoMux state.
type ClaudeCodeConfig struct {
	PathMode         string    `json:"path_mode"`
	SettingsPath     string    `json:"settings_path"`
	DisableTelemetry bool      `json:"disable_telemetry"`
	ActiveProfileID  string    `json:"active_profile_id"`
	Profiles         []Profile `json:"profiles"`
}

// Profile stores one immutable-by-ID model mapping. Empty optional model
// fields mean that the corresponding Claude Code setting is unset.
type Profile struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	HaikuModel           string `json:"haiku_model"`
	SonnetModel          string `json:"sonnet_model"`
	OpusModel            string `json:"opus_model"`
	FableModel           string `json:"fable_model"`
	SubagentModel        string `json:"subagent_model"`
	TeammateDefaultModel string `json:"teammate_default_model"`
}

func DefaultClaudeCodeConfig() ClaudeCodeConfig {
	return ClaudeCodeConfig{
		PathMode:         PathModeDefault,
		SettingsPath:     "",
		DisableTelemetry: true,
		ActiveProfileID:  "",
		Profiles:         []Profile{},
	}
}

func DefaultHarnesses() HarnessesConfig {
	return HarnessesConfig{ClaudeCode: DefaultClaudeCodeConfig()}
}

func (p Profile) Clone() Profile { return p }

// Normalize is intentionally a no-op for Profile. It is present so callers
// can normalize a complete schema recursively without special cases.
func (p Profile) Normalize() Profile { return p.Clone() }

func (p Profile) Validate() error { return validateProfile("profile", p.Normalize()) }

func (c ClaudeCodeConfig) Clone() ClaudeCodeConfig {
	out := c
	out.Profiles = cloneProfiles(c.Profiles)
	return out
}

func (c ClaudeCodeConfig) Normalize() ClaudeCodeConfig {
	out := c.Clone()
	if out.PathMode == "" {
		out.PathMode = PathModeDefault
	}
	if out.Profiles == nil {
		out.Profiles = []Profile{}
	}
	for i := range out.Profiles {
		out.Profiles[i] = out.Profiles[i].Normalize()
	}
	return out
}

func (c ClaudeCodeConfig) Validate() error {
	return validateClaudeCode("harnesses.claude_code", c.Normalize())
}

func (h HarnessesConfig) Clone() HarnessesConfig {
	out := h
	out.ClaudeCode = h.ClaudeCode.Clone()
	return out
}

func (h HarnessesConfig) Normalize() HarnessesConfig {
	out := h.Clone()
	out.ClaudeCode = out.ClaudeCode.Normalize()
	return out
}

func (h HarnessesConfig) Validate() error {
	return h.ClaudeCode.Validate()
}

// AutoModeConfig is the persisted classifier routing configuration.  The
// classifier model is deliberately shared by provider-pool and fixed-provider
// modes; FixedProvider never carries a second model field.
type AutoModeConfig struct {
	Mode          string               `json:"mode"`
	Model         string               `json:"model"`
	FixedProvider *FixedProviderConfig `json:"fixed_provider,omitempty"`
}

// FixedProviderConfig describes the pool-external classifier target.  It is a
// target description only: scheduling, health, and protocol conversion remain
// runtime concerns outside the config package.
type FixedProviderConfig struct {
	BaseURL    string    `json:"base_url"`
	APIKey     string    `json:"api_key"`
	UseXAPIKey bool      `json:"use_x_api_key"`
	Protocol   string    `json:"protocol"`
	TLS        TLSConfig `json:"tls"`
	Patches    []string  `json:"patches"`
}

func (f FixedProviderConfig) Clone() FixedProviderConfig {
	out := f
	out.Patches = cloneStrings(f.Patches)
	return out
}

func (f FixedProviderConfig) Normalize() FixedProviderConfig {
	out := f.Clone()
	if out.Patches == nil {
		out.Patches = []string{}
	}
	return out
}

func (f FixedProviderConfig) Validate() error {
	return validateFixedProvider("fixed_provider", f.Normalize())
}

type ProviderConfig struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	BaseURL       string    `json:"base_url"`
	APIKey        string    `json:"api_key"`
	Models        []string  `json:"models"`
	Priority      int64     `json:"priority"`
	Enabled       bool      `json:"enabled"`
	UseXAPIKey    bool      `json:"use_x_api_key"`
	TLS           TLSConfig `json:"tls"`
	Patches       []string  `json:"patches"`
	DisableHealth bool      `json:"disable_health"`
}

type TLSConfig struct {
	CAFile             string `json:"ca_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

// Default returns a valid, intentionally data-plane-inactive configuration
// except for the required management credential. Callers must provide the key
// before persisting it.
func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Service: ServiceConfig{
			ListenAddr:  DefaultListenAddr,
			LogMaxBytes: DefaultLogMaxBytes,
		},
		AutoMode: AutoModeConfig{
			Mode:          AutoModeDisabled,
			Model:         "",
			FixedProvider: nil,
		},
		Harnesses: DefaultHarnesses(),
		Providers: []ProviderConfig{},
	}
}

// Clone returns a deep copy. Config values are passed across the runtime
// boundary and must never share mutable slices with an active snapshot.
func (c Config) Clone() Config {
	out := c
	out.Harnesses = c.Harnesses.Clone()
	if c.AutoMode.FixedProvider != nil {
		fixed := *c.AutoMode.FixedProvider
		fixed.Patches = cloneStrings(c.AutoMode.FixedProvider.Patches)
		out.AutoMode.FixedProvider = &fixed
	}
	out.Providers = make([]ProviderConfig, len(c.Providers))
	for i := range c.Providers {
		out.Providers[i] = c.Providers[i]
		out.Providers[i].Models = cloneStrings(c.Providers[i].Models)
		out.Providers[i].Patches = cloneStrings(c.Providers[i].Patches)
	}
	return out
}

// Normalize applies only the documented defaults. It does not turn a missing
// schema version into v1 and never trims or rewrites credentials or model
// names.
func (c Config) Normalize() Config {
	out := c.Clone()
	if out.AutoMode.Mode == "" {
		out.AutoMode.Mode = AutoModeDisabled
	}
	if out.AutoMode.FixedProvider != nil && out.AutoMode.FixedProvider.Patches == nil {
		out.AutoMode.FixedProvider.Patches = []string{}
	}
	out.Harnesses = out.Harnesses.Normalize()
	if out.Providers == nil {
		out.Providers = []ProviderConfig{}
	}
	for i := range out.Providers {
		if out.Providers[i].Models == nil {
			out.Providers[i].Models = []string{}
		}
		if out.Providers[i].Patches == nil {
			out.Providers[i].Patches = []string{}
		}
	}
	return out
}

// Validate validates the complete persisted v1 contract. The returned error
// is a *ValidationError so HTTP callers can distinguish semantic 422 errors
// from malformed JSON.
func (c Config) Validate() error {
	c = c.Normalize()
	if c.SchemaVersion != SchemaVersion {
		if c.SchemaVersion == 0 {
			return validation("schema_version", "is required and must be 1")
		}
		return validation("schema_version", fmt.Sprintf("unsupported version %d", c.SchemaVersion))
	}
	if err := validateListenAddr(c.Service.ListenAddr); err != nil {
		return validation("service.listen_addr", err.Error())
	}
	if c.Service.LogMaxBytes <= 0 {
		return validation("service.log_max_bytes", "must be a positive integer")
	}
	if err := validateCredential("auth.management_key", c.Auth.ManagementKey, true); err != nil {
		return err
	}
	if c.Auth.GatewayKey != "" {
		if err := validateCredential("auth.gateway_key", c.Auth.GatewayKey, false); err != nil {
			return err
		}
		if c.Auth.GatewayKey == c.Auth.ManagementKey {
			return validation("auth", "gateway_key and management_key must be different")
		}
	}
	if err := validateAutoMode(c.AutoMode); err != nil {
		return err
	}
	if err := c.Harnesses.Validate(); err != nil {
		return err
	}

	seenIDs := make(map[string]int, len(c.Providers))
	seenNames := make(map[string]int, len(c.Providers))
	for i, p := range c.Providers {
		prefix := fmt.Sprintf("providers[%d]", i)
		if !isUUID(p.ID) {
			return validation(prefix+".id", "must be a canonical UUID")
		}
		if p.Name == "" || strings.TrimSpace(p.Name) == "" {
			return validation(prefix+".name", "must not be empty")
		}
		if containsControl(p.Name) {
			return validation(prefix+".name", "must not contain control characters")
		}
		if previous, ok := seenIDs[p.ID]; ok {
			return conflict(prefix+".id", fmt.Sprintf("duplicates providers[%d].id", previous))
		}
		seenIDs[p.ID] = i
		if previous, ok := seenNames[p.Name]; ok {
			return conflict(prefix+".name", fmt.Sprintf("duplicates providers[%d].name", previous))
		}
		seenNames[p.Name] = i

		if err := validateBaseURL(p.BaseURL); err != nil {
			return validation(prefix+".base_url", err.Error())
		}
		if err := validateCredential(prefix+".api_key", p.APIKey, p.Enabled && len(p.Models) > 0); err != nil {
			return err
		}
		seenModels := make(map[string]struct{}, len(p.Models))
		for j, model := range p.Models {
			if model == "" {
				return validation(fmt.Sprintf("%s.models[%d]", prefix, j), "must not be empty")
			}
			if len(model) > modelname.MaxBytes {
				return validation(fmt.Sprintf("%s.models[%d]", prefix, j), fmt.Sprintf("must not exceed %d UTF-8 bytes", modelname.MaxBytes))
			}
			if containsControl(model) {
				return validation(fmt.Sprintf("%s.models[%d]", prefix, j), "must not contain control characters")
			}
			if _, exists := seenModels[model]; exists {
				return validation(fmt.Sprintf("%s.models[%d]", prefix, j), "duplicates an earlier model using exact case-sensitive matching")
			}
			seenModels[model] = struct{}{}
		}
		if p.TLS.CAFile != "" && p.TLS.InsecureSkipVerify {
			return validation(prefix+".tls", "ca_file and insecure_skip_verify are mutually exclusive")
		}
		seenPatches := make(map[string]struct{}, len(p.Patches))
		for j, patch := range p.Patches {
			if patch == "" || strings.TrimSpace(patch) == "" {
				return validation(fmt.Sprintf("%s.patches[%d]", prefix, j), "must not be empty")
			}
			if containsControl(patch) {
				return validation(fmt.Sprintf("%s.patches[%d]", prefix, j), "must not contain control characters")
			}
			if _, exists := seenPatches[patch]; exists {
				return validation(fmt.Sprintf("%s.patches[%d]", prefix, j), "duplicates an earlier patch id")
			}
			seenPatches[patch] = struct{}{}
		}
	}
	return nil
}

// ApplyClientUpdate prepares a complete in-process candidate. The HTTP layer
// should use ApplyClientRequest with ClientConfigUpdate so the request shape
// cannot contain server-owned fields.
func (current Config) ApplyClientUpdate(next Config) (Config, error) {
	current = current.Normalize()
	next = next.Normalize()
	if next.Harnesses.ClaudeCode.ActiveProfileID != "" {
		return Config{}, ErrActiveProfileReadOnly
	}
	next = current.prepareServerUpdate(next)
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	return next, nil
}

// ApplyClientRequest applies a client-owned request to the current resource.
// The active profile ID is inherited or invalidated according to the normal
// active-input rules; it is never read from the request object.
func (current Config) ApplyClientRequest(update ClientConfigUpdate) (Config, error) {
	return current.ApplyClientUpdate(configFromClientUpdate(update))
}

// ValidateClientUpdate checks an in-process candidate's server-owned-state
// rules without returning the normalized candidate. HTTP callers should use
// DecodeClientUpdate and ClientConfigUpdate instead.
func ValidateClientUpdate(current, next Config) error {
	_, err := current.ApplyClientUpdate(next)
	return err
}

// ApplyServerUpdate prepares a trusted read-modify-write candidate. A server
// mutator may omit the active ID or leave its current value in place; it may
// not replace a non-empty ID with another profile. Relevant active inputs are
// reconciled before the candidate is validated.
func (current Config) ApplyServerUpdate(next Config) (Config, error) {
	current = current.Normalize()
	next = next.Normalize()
	currentID := current.Harnesses.ClaudeCode.ActiveProfileID
	nextID := next.Harnesses.ClaudeCode.ActiveProfileID
	if nextID != "" && nextID != currentID {
		return Config{}, ErrActiveProfileReadOnly
	}
	next = current.prepareServerUpdate(next)
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	return next, nil
}

// ActiveProfileInputsEqual reports whether the inputs that can make a
// previously verified active profile stale are unchanged. Unrelated Provider
// and non-active Profile changes do not invalidate the active record.
func (current Config) ActiveProfileInputsEqual(next Config) bool {
	current = current.Normalize()
	next = next.Normalize()
	activeID := current.Harnesses.ClaudeCode.ActiveProfileID
	if activeID == "" || next.Harnesses.ClaudeCode.ActiveProfileID != "" && next.Harnesses.ClaudeCode.ActiveProfileID != activeID {
		return false
	}
	if current.Service.ListenAddr != next.Service.ListenAddr || current.Auth.GatewayKey != next.Auth.GatewayKey {
		return false
	}
	left := current.Harnesses.ClaudeCode
	right := next.Harnesses.ClaudeCode
	if left.PathMode != right.PathMode || left.SettingsPath != right.SettingsPath || left.DisableTelemetry != right.DisableTelemetry {
		return false
	}
	leftProfile, leftOK := findProfile(left.Profiles, activeID)
	rightProfile, rightOK := findProfile(right.Profiles, activeID)
	return leftOK && rightOK && leftProfile == rightProfile
}

func (current Config) prepareServerUpdate(next Config) Config {
	current = current.Normalize()
	next = next.Normalize()
	activeID := current.Harnesses.ClaudeCode.ActiveProfileID
	if activeID == "" || !current.ActiveProfileInputsEqual(next) {
		next.Harnesses.ClaudeCode.ActiveProfileID = ""
	} else {
		next.Harnesses.ClaudeCode.ActiveProfileID = activeID
	}
	return next
}

// WithActiveProfileID creates a server-side candidate for the active profile
// transition. It is intentionally separate from client update handling so only
// trusted runtime code can record a selected profile.
func (c Config) WithActiveProfileID(id string) (Config, error) {
	out := c.Normalize()
	if id != "" {
		if !isUUID(id) {
			return Config{}, validation("harnesses.claude_code.active_profile_id", "must be a canonical UUID")
		}
		if _, ok := findProfile(out.Harnesses.ClaudeCode.Profiles, id); !ok {
			return Config{}, validation("harnesses.claude_code.active_profile_id", "must reference an existing profile")
		}
	}
	out.Harnesses.ClaudeCode.ActiveProfileID = id
	if err := out.Validate(); err != nil {
		return Config{}, err
	}
	return out, nil
}

func findProfile(profiles []Profile, id string) (Profile, bool) {
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

// ValidateAutoMode validates an Auto Mode object independently of the root
// configuration.  Runtime compilation performs the additional registry-aware
// patch applicability check for fixed targets.
func (a AutoModeConfig) Validate() error {
	return validateAutoMode(a.Normalize())
}

// Normalize applies the documented Auto Mode defaults and deep-copies the
// optional fixed target.  It does not trim or rewrite user values.
func (a AutoModeConfig) Normalize() AutoModeConfig {
	out := a
	if out.Mode == "" {
		out.Mode = AutoModeDisabled
	}
	if out.FixedProvider != nil {
		fixed := out.FixedProvider.Normalize()
		out.FixedProvider = &fixed
	}
	return out
}

// Clone deep-copies the optional fixed target without applying defaults.  Use
// Normalize when the documented disabled-mode default is desired.
func (a AutoModeConfig) Clone() AutoModeConfig {
	out := a
	if a.FixedProvider != nil {
		fixed := *a.FixedProvider
		fixed.Patches = cloneStrings(a.FixedProvider.Patches)
		out.FixedProvider = &fixed
	}
	return out
}

func validateAutoMode(a AutoModeConfig) error {
	a = a.Normalize()
	switch a.Mode {
	case AutoModeDisabled:
		if a.Model != "" {
			return validation("auto_mode.model", "must be empty when mode is disabled")
		}
		if a.FixedProvider != nil {
			return validation("auto_mode.fixed_provider", "must be omitted when mode is disabled")
		}
	case AutoModeProviderPool:
		if err := validateClassifierModel("auto_mode.model", a.Model); err != nil {
			return err
		}
		if a.FixedProvider != nil {
			return validation("auto_mode.fixed_provider", "must be omitted in provider_pool mode")
		}
	case AutoModeFixedProvider:
		if err := validateClassifierModel("auto_mode.model", a.Model); err != nil {
			return err
		}
		if a.FixedProvider == nil {
			return validation("auto_mode.fixed_provider", "is required in fixed_provider mode")
		}
		if err := validateFixedProvider("auto_mode.fixed_provider", *a.FixedProvider); err != nil {
			return err
		}
	default:
		return validation("auto_mode.mode", fmt.Sprintf("unsupported mode %q", a.Mode))
	}
	return nil
}

func validateClassifierModel(field, model string) error {
	if model == "" {
		return validation(field, "must not be empty")
	}
	if !utf8.ValidString(model) {
		return validation(field, "must be valid UTF-8")
	}
	if len(model) > modelname.MaxBytes {
		return validation(field, fmt.Sprintf("must not exceed %d UTF-8 bytes", modelname.MaxBytes))
	}
	if containsControl(model) {
		return validation(field, "must not contain control characters")
	}
	return nil
}

func validateClaudeCode(prefix string, value ClaudeCodeConfig) error {
	switch value.PathMode {
	case PathModeDefault:
		if value.SettingsPath != "" {
			return validation(prefix+".settings_path", "must be empty in default path mode")
		}
	case PathModeCustom:
		if value.SettingsPath == "" {
			return validation(prefix+".settings_path", "must be a non-empty absolute path in custom mode")
		}
		if !filepath.IsAbs(value.SettingsPath) {
			return validation(prefix+".settings_path", "must be an absolute path in custom mode")
		}
		if !utf8.ValidString(value.SettingsPath) {
			return validation(prefix+".settings_path", "must be valid UTF-8")
		}
		if containsControl(value.SettingsPath) {
			return validation(prefix+".settings_path", "must not contain control characters")
		}
	default:
		return validation(prefix+".path_mode", fmt.Sprintf("unsupported mode %q", value.PathMode))
	}

	seenIDs := make(map[string]int, len(value.Profiles))
	seenNames := make(map[string]int, len(value.Profiles))
	for i, profile := range value.Profiles {
		profilePrefix := fmt.Sprintf("%s.profiles[%d]", prefix, i)
		if err := validateProfile(profilePrefix, profile); err != nil {
			return err
		}
		if previous, exists := seenIDs[profile.ID]; exists {
			return conflict(profilePrefix+".id", fmt.Sprintf("duplicates %s.profiles[%d].id", prefix, previous))
		}
		seenIDs[profile.ID] = i
		if previous, exists := seenNames[profile.Name]; exists {
			return conflict(profilePrefix+".name", fmt.Sprintf("duplicates %s.profiles[%d].name", prefix, previous))
		}
		seenNames[profile.Name] = i
	}
	if value.ActiveProfileID != "" {
		if !isUUID(value.ActiveProfileID) {
			return validation(prefix+".active_profile_id", "must be a canonical UUID")
		}
		if _, exists := seenIDs[value.ActiveProfileID]; !exists {
			return validation(prefix+".active_profile_id", "must reference an existing profile")
		}
	}
	return nil
}

func validateProfile(prefix string, profile Profile) error {
	if !isUUID(profile.ID) {
		return validation(prefix+".id", "must be a canonical UUID")
	}
	if profile.Name == "" {
		return validation(prefix+".name", "must not be empty")
	}
	if strings.TrimSpace(profile.Name) != profile.Name {
		return validation(prefix+".name", "must not have leading or trailing whitespace")
	}
	if !utf8.ValidString(profile.Name) {
		return validation(prefix+".name", "must be valid UTF-8")
	}
	if containsControl(profile.Name) {
		return validation(prefix+".name", "must not contain control characters")
	}
	for _, item := range []struct {
		field    string
		model    string
		required bool
	}{
		{field: "haiku_model", model: profile.HaikuModel, required: true},
		{field: "sonnet_model", model: profile.SonnetModel, required: true},
		{field: "opus_model", model: profile.OpusModel, required: true},
		{field: "fable_model", model: profile.FableModel, required: true},
		{field: "subagent_model", model: profile.SubagentModel},
		{field: "teammate_default_model", model: profile.TeammateDefaultModel},
	} {
		if err := validateProfileModel(prefix+"."+item.field, item.model, item.required); err != nil {
			return err
		}
	}
	return nil
}

func validateProfileModel(field, model string, required bool) error {
	if model == "" {
		if required {
			return validation(field, "must not be empty")
		}
		return nil
	}
	if !utf8.ValidString(model) {
		return validation(field, "must be valid UTF-8")
	}
	if len(model) > modelname.MaxBytes {
		return validation(field, fmt.Sprintf("must not exceed %d UTF-8 bytes", modelname.MaxBytes))
	}
	if containsControl(model) {
		return validation(field, "must not contain control characters")
	}
	return nil
}

func validateFixedProvider(prefix string, fixed FixedProviderConfig) error {
	if err := validateBaseURL(fixed.BaseURL); err != nil {
		return validation(prefix+".base_url", err.Error())
	}
	if err := validateCredential(prefix+".api_key", fixed.APIKey, true); err != nil {
		return err
	}
	if fixed.Protocol != ProtocolAnthropicMessages && fixed.Protocol != ProtocolOpenAIResponses && fixed.Protocol != ProtocolOpenAICompatible {
		return validation(prefix+".protocol", fmt.Sprintf("unsupported protocol %q", fixed.Protocol))
	}
	if fixed.TLS.CAFile != "" && fixed.TLS.InsecureSkipVerify {
		return validation(prefix+".tls", "ca_file and insecure_skip_verify are mutually exclusive")
	}
	seen := make(map[string]struct{}, len(fixed.Patches))
	for i, id := range fixed.Patches {
		field := fmt.Sprintf("%s.patches[%d]", prefix, i)
		if id == "" || strings.TrimSpace(id) == "" {
			return validation(field, "must not be empty")
		}
		if containsControl(id) {
			return validation(field, "must not contain control characters")
		}
		if _, ok := seen[id]; ok {
			return validation(field, "duplicates an earlier patch id")
		}
		seen[id] = struct{}{}
	}
	return nil
}

// ValidateProviderForRequest applies the complete schema-level checks that are
// independent of the surrounding configuration. The management API uses it
// after assigning a stable ID to a POST body and after resolving a PUT path.
func ValidateProviderForRequest(p ProviderConfig) error {
	check := Default()
	check.Auth.ManagementKey = "provider-request-validation-key"
	check.Providers = []ProviderConfig{p}
	return check.Validate()
}

func validateCredential(field, value string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return validation(field, "must not be empty")
	}
	if value != "" && strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return validation(field, "must not contain whitespace")
	}
	if value != "" && containsControl(value) {
		return validation(field, "contains invalid control characters")
	}
	return nil
}

func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be 127.0.0.1:<port>: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("host must be 127.0.0.1, got %q", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || strconv.Itoa(n) != port {
		return fmt.Errorf("port must be a canonical decimal integer")
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("must not be empty")
	}
	_, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// IsUUID accepts the canonical 8-4-4-4-12 lowercase or uppercase UUID text.
// UUID version/variant bits are deliberately not constrained; imported stable
// IDs only need to be valid UUIDs, while new IDs are generated as v4.
func IsUUID(value string) bool {
	return isUUID(value)
}

func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneProfiles(in []Profile) []Profile {
	if in == nil {
		return nil
	}
	out := make([]Profile, len(in))
	copy(out, in)
	return out
}

// ValidationError identifies a semantic configuration error.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func validation(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// ConflictError identifies a validly-shaped request that collides with an
// existing resource, such as a duplicate Provider ID or name.
type ConflictError struct {
	Field   string
	Message string
}

func (e *ConflictError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func conflict(field, message string) error {
	return &ConflictError{Field: field, Message: message}
}

// SyntaxError identifies malformed JSON, unknown fields, invalid JSON types,
// or trailing JSON data.
type SyntaxError struct{ Err error }

func (e *SyntaxError) Error() string { return e.Err.Error() }
func (e *SyntaxError) Unwrap() error { return e.Err }

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}
