package harnessconfig

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HomeResolver is injected into ClaudeCodeAdapter so path resolution tests do
// not depend on the process user's home directory.
type HomeResolver func() (string, error)

// ClaudeCodeAdapterOptions contains construction-time dependencies. The
// adapter is stateless after construction and is safe for concurrent reads.
type ClaudeCodeAdapterOptions struct {
	HomeDir HomeResolver
}

// ClaudeCodeAdapter implements the Claude Code settings.json semantics. It
// never reads the target file during path resolution or projection building.
type ClaudeCodeAdapter struct {
	homeDir HomeResolver
}

// NewClaudeCodeAdapter constructs an adapter using os.UserHomeDir for the
// default path. An optional options value is accepted for test injection.
func NewClaudeCodeAdapter(options ...ClaudeCodeAdapterOptions) *ClaudeCodeAdapter {
	var option ClaudeCodeAdapterOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.HomeDir == nil {
		option.HomeDir = os.UserHomeDir
	}
	return &ClaudeCodeAdapter{homeDir: option.HomeDir}
}

func NewClaudeCodeAdapterWithHomeResolver(resolve HomeResolver) *ClaudeCodeAdapter {
	return NewClaudeCodeAdapter(ClaudeCodeAdapterOptions{HomeDir: resolve})
}

func NewClaudeCodeAdapterWithOptions(options ClaudeCodeAdapterOptions) *ClaudeCodeAdapter {
	return NewClaudeCodeAdapter(options)
}

func (a *ClaudeCodeAdapter) ID() string {
	return ClaudeCodeAdapterID
}

// ResolvePath resolves the explicit default/custom path contract. Custom
// values are passed through literally; no shell, environment, or tilde
// expansion is performed.
func (a *ClaudeCodeAdapter) ResolvePath(config PathConfig) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: Claude Code adapter is nil", ErrInvalidAdapter)
	}
	if err := config.Validate(); err != nil {
		return "", err
	}
	switch config.PathMode {
	case PathModeDefault:
		if a.homeDir == nil {
			return "", fmt.Errorf("%w: home resolver is nil", ErrInvalidPathConfig)
		}
		home, err := a.homeDir()
		if err != nil {
			return "", fmt.Errorf("%w: resolve user home: %v", ErrInvalidPathConfig, err)
		}
		if home == "" || !filepath.IsAbs(home) || hasControl(home) {
			return "", fmt.Errorf("%w: resolved home must be an absolute path", ErrInvalidPathConfig)
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	case PathModeCustom:
		return config.SettingsPath, nil
	}
	return "", fmt.Errorf("%w: unsupported path mode %q", ErrInvalidPathConfig, config.PathMode)
}

// BuildManagedProjection converts the current listener, gateway credential,
// profile and telemetry preference into the complete managed field set.
func (a *ClaudeCodeAdapter) BuildManagedProjection(input ActivationInput) (ManagedProjection, error) {
	if a == nil {
		return ManagedProjection{}, fmt.Errorf("%w: Claude Code adapter is nil", ErrInvalidAdapter)
	}
	if err := input.Validate(); err != nil {
		return ManagedProjection{}, err
	}
	profile := input.Profile

	baseURL := "http://" + input.ListenAddr
	projection := ManagedProjection{
		Env: map[string]string{
			EnvAnthropicBaseURL:            baseURL,
			EnvAnthropicAuthToken:          input.GatewayKey,
			EnvAnthropicDefaultHaikuModel:  profile.HaikuModel,
			EnvAnthropicDefaultSonnetModel: profile.SonnetModel,
			EnvAnthropicDefaultOpusModel:   profile.OpusModel,
			EnvAnthropicDefaultFableModel:  profile.FableModel,
		},
		TopLevel: map[string]string{},
	}
	if input.DisableTelemetry {
		projection.Env[EnvClaudeCodeAttributionHeader] = "0"
		projection.Env[EnvDisableFeedbackCommand] = "1"
		projection.Env[EnvDisableErrorReporting] = "1"
		projection.Env[EnvDisableTelemetry] = "1"
	} else {
		projection.Env[EnvClaudeCodeAttributionHeader] = "1"
		projection.Env[EnvDisableFeedbackCommand] = "0"
		projection.Env[EnvDisableErrorReporting] = ""
		projection.Env[EnvDisableTelemetry] = ""
	}
	if profile.SubagentModel != "" {
		projection.Env[EnvClaudeCodeSubagentModel] = profile.SubagentModel
	}
	if profile.TeammateDefaultModel != "" {
		projection.TopLevel[TopLevelTeammateDefaultModel] = profile.TeammateDefaultModel
	}
	if _, err := canonicalProjection(projection); err != nil {
		return ManagedProjection{}, err
	}
	return projection, nil
}

func validateLoopbackAddress(value string) error {
	if value == "" || hasControl(value) {
		return fmt.Errorf("%w: address must be 127.0.0.1:<port>", ErrInvalidListenAddr)
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("%w: address must be 127.0.0.1:<port>", ErrInvalidListenAddr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || strconv.Itoa(port) != portText || port < 1 || port > 65535 {
		return fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidListenAddr)
	}
	return nil
}

func validateActivationKey(value string) error {
	if value == "" {
		return ErrGatewayKeyRequired
	}
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsSpace) >= 0 || hasControl(value) {
		return fmt.Errorf("%w: gateway key contains whitespace, control characters, or invalid UTF-8", ErrInvalidActivation)
	}
	return nil
}

func validateModelValue(field, value string, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%w: %s must not be empty", ErrInvalidModel, field)
	}
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalidModel, field)
	}
	if len(value) > 256 {
		return fmt.Errorf("%w: %s exceeds 256 UTF-8 bytes", ErrInvalidModel, field)
	}
	if hasControl(value) {
		return fmt.Errorf("%w: %s contains control characters", ErrInvalidModel, field)
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validateGatewayURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: gateway URL must be a plain http://127.0.0.1:<port> URL", ErrInvalidProjection)
	}
	if parsed.Port() == "" {
		return fmt.Errorf("%w: gateway URL must include a port", ErrInvalidProjection)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w: gateway URL has an invalid port", ErrInvalidProjection)
	}
	return nil
}

func canonicalProjection(input ManagedProjection) (ManagedProjection, error) {
	if input.Env == nil {
		return ManagedProjection{}, fmt.Errorf("%w: env projection is nil", ErrInvalidProjection)
	}
	projection := input.Clone()
	allowedEnv := make(map[string]struct{}, len(ManagedEnvKeys()))
	for _, key := range ManagedEnvKeys() {
		allowedEnv[key] = struct{}{}
	}
	for key := range projection.Env {
		if _, ok := allowedEnv[key]; !ok {
			return ManagedProjection{}, fmt.Errorf("%w: unknown env field %q", ErrInvalidProjection, key)
		}
	}
	for key := range projection.TopLevel {
		if key != TopLevelTeammateDefaultModel {
			return ManagedProjection{}, fmt.Errorf("%w: unknown top-level field %q", ErrInvalidProjection, key)
		}
	}
	for _, key := range RequiredManagedEnvKeys() {
		if _, ok := projection.Env[key]; !ok {
			return ManagedProjection{}, fmt.Errorf("%w: missing required field %q", ErrInvalidProjection, key)
		}
	}
	if err := validateGatewayURL(projection.Env[EnvAnthropicBaseURL]); err != nil {
		return ManagedProjection{}, err
	}
	if err := validateActivationKey(projection.Env[EnvAnthropicAuthToken]); err != nil {
		return ManagedProjection{}, fmt.Errorf("%w: %w", ErrInvalidProjection, err)
	}
	for _, key := range []string{
		EnvAnthropicDefaultHaikuModel,
		EnvAnthropicDefaultSonnetModel,
		EnvAnthropicDefaultOpusModel,
		EnvAnthropicDefaultFableModel,
	} {
		if err := validateModelValue(key, projection.Env[key], true); err != nil {
			return ManagedProjection{}, fmt.Errorf("%w: %w", ErrInvalidProjection, err)
		}
	}
	if projection.Env[EnvClaudeCodeAttributionHeader] == "0" && projection.Env[EnvDisableFeedbackCommand] == "1" && projection.Env[EnvDisableErrorReporting] == "1" && projection.Env[EnvDisableTelemetry] == "1" {
		// The disabled tuple is complete and exact.
	} else if projection.Env[EnvClaudeCodeAttributionHeader] == "1" && projection.Env[EnvDisableFeedbackCommand] == "0" && projection.Env[EnvDisableErrorReporting] == "" && projection.Env[EnvDisableTelemetry] == "" {
		// The enabled tuple is complete and exact.
	} else {
		return ManagedProjection{}, fmt.Errorf("%w: telemetry fields do not form a supported tuple", ErrInvalidProjection)
	}
	if value, ok := projection.Env[EnvClaudeCodeSubagentModel]; ok {
		if value == "" {
			delete(projection.Env, EnvClaudeCodeSubagentModel)
		} else if err := validateModelValue(EnvClaudeCodeSubagentModel, value, false); err != nil {
			return ManagedProjection{}, fmt.Errorf("%w: %w", ErrInvalidProjection, err)
		}
	}
	if value, ok := projection.TopLevel[TopLevelTeammateDefaultModel]; ok {
		if value == "" {
			delete(projection.TopLevel, TopLevelTeammateDefaultModel)
		} else if err := validateModelValue(TopLevelTeammateDefaultModel, value, false); err != nil {
			return ManagedProjection{}, fmt.Errorf("%w: %w", ErrInvalidProjection, err)
		}
	}
	return projection, nil
}

func (p ManagedProjection) Validate() error {
	_, err := canonicalProjection(p)
	return err
}
