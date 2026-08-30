package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
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

// Config is the complete v1 persisted configuration. It intentionally has no
// fields from the retired v0 configuration.
type Config struct {
	SchemaVersion int              `json:"schema_version"`
	Service       ServiceConfig    `json:"service"`
	Auth          AuthConfig       `json:"auth"`
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
		Providers: []ProviderConfig{},
	}
}

// Clone returns a deep copy. Config values are passed across the runtime
// boundary and must never share mutable slices with an active snapshot.
func (c Config) Clone() Config {
	out := c
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
	if strings.TrimSpace(raw) == "" {
		return errors.New("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("host is required")
	}
	if u.Hostname() == "" {
		return errors.New("host is required")
	}
	if u.User != nil {
		return errors.New("userinfo is not allowed")
	}
	// net/url represents a bare trailing fragment marker (for example,
	// "https://host/path#") with an empty Fragment. Inspect the original URI
	// as well so every literal fragment delimiter is rejected consistently.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.IndexByte(raw, '#') >= 0 {
		return errors.New("query and fragment are not allowed")
	}
	if u.Opaque != "" {
		return errors.New("opaque URLs are not allowed")
	}
	if strings.ContainsAny(u.Host, " \t\r\n") {
		return errors.New("host contains whitespace")
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("port must not be empty")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || strconv.Itoa(n) != port || n < 1 || n > 65535 {
			return errors.New("port must be a canonical decimal integer between 1 and 65535")
		}
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
