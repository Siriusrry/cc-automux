package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Decode strictly parses a complete v1 configuration and applies documented
// defaults. It rejects unknown fields and any non-whitespace trailing data.
func Decode(data []byte) (Config, error) {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return Config{}, err
	}
	if raw == nil {
		return Config{}, &SyntaxError{Err: errors.New("configuration must be a JSON object")}
	}
	if err := checkConfigKeys(raw); err != nil {
		return Config{}, &SyntaxError{Err: err}
	}
	if _, ok := raw["schema_version"]; !ok {
		return Config{}, validation("schema_version", "is required and must be 1")
	}
	// Auto Mode is part of the v1 root contract even when disabled.  Keeping
	// this presence check in the strict decoder (rather than the Go value
	// validator) lets programmatic Config values use the documented default
	// while rejecting an omitted persisted object.
	if _, ok := raw["auto_mode"]; !ok {
		return Config{}, validation("auto_mode", "is required and must be a JSON object")
	}
	if _, ok := raw["harnesses"]; !ok {
		return Config{}, validation("harnesses", "is required and must be a JSON object")
	}
	for _, requiredObject := range []string{"service", "auth", "auto_mode", "harnesses", "providers"} {
		if value, ok := raw[requiredObject]; ok && isJSONNull(value) {
			return Config{}, &SyntaxError{Err: fmt.Errorf("%s must not be null", requiredObject)}
		}
	}

	// Seed documented service defaults before decoding so omission gets a
	// default while an explicitly supplied empty/zero value remains visible to
	// validation and is rejected.
	cfg := Default()
	if err := decodeObject(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DecodeClientUpdate parses the client-owned portion of a configuration PUT.
// A resource response from GET is deliberately a different object: it
// includes active_profile_id, while this request decoder rejects that field by
// presence (including an explicit empty value).
func DecodeClientUpdate(data []byte) (ClientConfigUpdate, error) {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return ClientConfigUpdate{}, err
	}
	if raw == nil {
		return ClientConfigUpdate{}, &SyntaxError{Err: errors.New("configuration must be a JSON object")}
	}
	if hasActiveProfileField(raw) {
		return ClientConfigUpdate{}, ErrActiveProfileReadOnly
	}
	if err := checkConfigKeys(raw); err != nil {
		return ClientConfigUpdate{}, &SyntaxError{Err: err}
	}
	// Reuse the persisted schema decoder for defaults and semantic validation,
	// then copy only client-owned fields into the request value.
	cfg, err := Decode(data)
	if err != nil {
		return ClientConfigUpdate{}, err
	}
	return clientUpdateFromConfig(cfg), nil
}

// hasActiveProfileField performs the read-only presence check before regular
// schema validation. This keeps an explicitly supplied null or empty value in
// the server-owned field from being mistaken for an ordinary client field
// error, while malformed surrounding objects still flow through syntax
// validation below.
func hasActiveProfileField(raw map[string]json.RawMessage) bool {
	harnesses, ok := raw["harnesses"]
	if !ok || isJSONNull(harnesses) {
		return false
	}
	var harnessObject map[string]json.RawMessage
	if err := json.Unmarshal(harnesses, &harnessObject); err != nil || harnessObject == nil {
		return false
	}
	claudeCode, ok := harnessObject["claude_code"]
	if !ok || isJSONNull(claudeCode) {
		return false
	}
	var claudeObject map[string]json.RawMessage
	if err := json.Unmarshal(claudeCode, &claudeObject); err != nil || claudeObject == nil {
		return false
	}
	_, present := claudeObject["active_profile_id"]
	return present
}

func clientUpdateFromConfig(value Config) ClientConfigUpdate {
	value = value.Normalize()
	return ClientConfigUpdate{
		SchemaVersion: value.SchemaVersion,
		Service:       value.Service,
		Auth:          value.Auth,
		AutoMode:      value.AutoMode.Clone(),
		Harnesses: ClientHarnesses{ClaudeCode: ClientClaudeCodeConfig{
			PathMode:         value.Harnesses.ClaudeCode.PathMode,
			SettingsPath:     value.Harnesses.ClaudeCode.SettingsPath,
			DisableTelemetry: value.Harnesses.ClaudeCode.DisableTelemetry,
			Profiles:         cloneProfiles(value.Harnesses.ClaudeCode.Profiles),
		}},
		Providers: value.Clone().Providers,
	}
}

// DecodeClient is retained for older in-process callers.
func DecodeClient(data []byte) (Config, error) {
	update, err := DecodeClientUpdate(data)
	if err != nil {
		return Config{}, err
	}
	return configFromClientUpdate(update), nil
}

func configFromClientUpdate(update ClientConfigUpdate) Config {
	next := Config{
		SchemaVersion: update.SchemaVersion,
		Service:       update.Service,
		Auth:          update.Auth,
		AutoMode:      update.AutoMode.Clone(),
		Harnesses: HarnessesConfig{ClaudeCode: ClaudeCodeConfig{
			PathMode:         update.Harnesses.ClaudeCode.PathMode,
			SettingsPath:     update.Harnesses.ClaudeCode.SettingsPath,
			DisableTelemetry: update.Harnesses.ClaudeCode.DisableTelemetry,
			Profiles:         cloneProfiles(update.Harnesses.ClaudeCode.Profiles),
		}},
		Providers: make([]ProviderConfig, len(update.Providers)),
	}
	for index := range update.Providers {
		next.Providers[index] = update.Providers[index]
		next.Providers[index].Models = cloneStrings(update.Providers[index].Models)
		next.Providers[index].Patches = cloneStrings(update.Providers[index].Patches)
	}
	return next
}

// DecodeAutoMode strictly parses one Auto Mode object, applying the same
// defaults and schema checks as the nested root field. It is used by focused
// management/tests and intentionally does not compile registry-dependent patch
// applicability.
func DecodeAutoMode(data []byte) (AutoModeConfig, error) {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return AutoModeConfig{}, err
	}
	if raw == nil {
		return AutoModeConfig{}, &SyntaxError{Err: errors.New("auto_mode must be a JSON object")}
	}
	if err := checkAutoModeKeys(raw); err != nil {
		return AutoModeConfig{}, &SyntaxError{Err: err}
	}
	var value AutoModeConfig
	if err := decodeObject(data, &value); err != nil {
		return AutoModeConfig{}, err
	}
	value = value.Normalize()
	if err := value.Validate(); err != nil {
		return AutoModeConfig{}, err
	}
	return value, nil
}

// MarshalAutoMode emits the canonical representation of one Auto Mode object.
func MarshalAutoMode(value AutoModeConfig) ([]byte, error) {
	value = value.Normalize()
	if err := value.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal auto_mode: %w", err)
	}
	return append(data, '\n'), nil
}

// DecodeProvider strictly parses a provider object. The ID may be omitted by
// the management POST endpoint and filled by the caller before validation.
func DecodeProvider(data []byte) (ProviderConfig, error) {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return ProviderConfig{}, err
	}
	if raw == nil {
		return ProviderConfig{}, &SyntaxError{Err: errors.New("provider must be a JSON object")}
	}
	if err := checkProviderKeys(raw); err != nil {
		return ProviderConfig{}, &SyntaxError{Err: err}
	}
	for _, field := range []string{"tls", "models", "patches"} {
		if value, ok := raw[field]; ok && isJSONNull(value) {
			return ProviderConfig{}, &SyntaxError{Err: fmt.Errorf("%s must not be null", field)}
		}
	}
	var provider ProviderConfig
	if err := decodeObject(data, &provider); err != nil {
		return ProviderConfig{}, err
	}
	provider.Models = cloneStrings(provider.Models)
	provider.Patches = cloneStrings(provider.Patches)
	if provider.Models == nil {
		provider.Models = []string{}
	}
	if provider.Patches == nil {
		provider.Patches = []string{}
	}
	return provider, nil
}

// Marshal validates and emits the canonical indented v1 JSON representation.
func Marshal(cfg Config) ([]byte, error) {
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal configuration: %w", err)
	}
	return append(data, '\n'), nil
}

func decodeObject(data []byte, target any) error {
	if !utf8.Valid(data) {
		return &SyntaxError{Err: errors.New("JSON must be valid UTF-8")}
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return &SyntaxError{Err: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &SyntaxError{Err: err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return &SyntaxError{Err: errors.New("unexpected trailing JSON content")}
		}
		return &SyntaxError{Err: errors.New("unexpected trailing JSON content")}
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON content")
		}
		return errors.New("unexpected trailing JSON content")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func checkConfigKeys(raw map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(raw, map[string]struct{}{
		"schema_version": {}, "service": {}, "auth": {}, "auto_mode": {}, "harnesses": {}, "providers": {},
	}); err != nil {
		return err
	}
	if value, ok := raw["service"]; ok {
		object, err := rawObject(value, "service")
		if err != nil {
			return err
		}
		if err := rejectUnknownKeys(object, map[string]struct{}{"listen_addr": {}, "log_max_bytes": {}}); err != nil {
			return err
		}
	}
	if value, ok := raw["auth"]; ok {
		object, err := rawObject(value, "auth")
		if err != nil {
			return err
		}
		if err := rejectUnknownKeys(object, map[string]struct{}{"gateway_key": {}, "management_key": {}}); err != nil {
			return err
		}
	}
	if value, ok := raw["auto_mode"]; ok {
		object, err := rawObject(value, "auto_mode")
		if err != nil {
			return err
		}
		if err := checkAutoModeKeys(object); err != nil {
			return err
		}
	}
	if value, ok := raw["harnesses"]; ok {
		object, err := rawObject(value, "harnesses")
		if err != nil {
			return err
		}
		if err := checkHarnessesKeys(object); err != nil {
			return err
		}
	}
	if value, ok := raw["providers"]; ok {
		var providers []json.RawMessage
		if err := json.Unmarshal(value, &providers); err != nil {
			return fmt.Errorf("providers must be an array: %w", err)
		}
		for i, item := range providers {
			object, err := rawObject(item, fmt.Sprintf("providers[%d]", i))
			if err != nil {
				return err
			}
			if err := checkProviderKeys(object); err != nil {
				return fmt.Errorf("providers[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func checkHarnessesKeys(object map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(object, map[string]struct{}{"claude_code": {}}); err != nil {
		return err
	}
	if value, ok := object["claude_code"]; ok {
		claudeCode, err := rawObject(value, "harnesses.claude_code")
		if err != nil {
			return err
		}
		return checkClaudeCodeKeys(claudeCode)
	}
	return nil
}

func checkClaudeCodeKeys(object map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(object, map[string]struct{}{
		"path_mode": {}, "settings_path": {}, "disable_telemetry": {},
		"active_profile_id": {}, "profiles": {},
	}); err != nil {
		return err
	}
	if value, ok := object["path_mode"]; ok {
		var mode string
		if err := json.Unmarshal(value, &mode); err != nil {
			return fmt.Errorf("harnesses.claude_code.path_mode must be a string: %w", err)
		}
		if mode == "" {
			return errors.New("harnesses.claude_code.path_mode must not be empty")
		}
	}
	for _, field := range []string{"settings_path", "active_profile_id"} {
		if value, ok := object[field]; ok {
			var decoded string
			if err := json.Unmarshal(value, &decoded); err != nil {
				return fmt.Errorf("harnesses.claude_code.%s must be a string: %w", field, err)
			}
		}
	}
	if value, ok := object["disable_telemetry"]; ok {
		var decoded bool
		if err := json.Unmarshal(value, &decoded); err != nil {
			return fmt.Errorf("harnesses.claude_code.disable_telemetry must be a boolean: %w", err)
		}
	}
	if value, ok := object["profiles"]; ok {
		var profiles []json.RawMessage
		if err := json.Unmarshal(value, &profiles); err != nil {
			return fmt.Errorf("harnesses.claude_code.profiles must be an array: %w", err)
		}
		for i, profile := range profiles {
			profileObject, err := rawObject(profile, fmt.Sprintf("harnesses.claude_code.profiles[%d]", i))
			if err != nil {
				return err
			}
			if err := checkProfileKeys(profileObject); err != nil {
				return fmt.Errorf("harnesses.claude_code.profiles[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func checkProfileKeys(object map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(object, map[string]struct{}{
		"id": {}, "name": {}, "haiku_model": {}, "sonnet_model": {},
		"opus_model": {}, "fable_model": {}, "subagent_model": {},
		"teammate_default_model": {},
	}); err != nil {
		return err
	}
	for key, value := range object {
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return fmt.Errorf("profile.%s must be a string: %w", key, err)
		}
	}
	return nil
}

func checkAutoModeKeys(object map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(object, map[string]struct{}{"mode": {}, "model": {}, "fixed_provider": {}}); err != nil {
		return err
	}
	if mode, present := object["mode"]; present {
		var decoded string
		if err := json.Unmarshal(mode, &decoded); err != nil {
			return fmt.Errorf("auto_mode.mode must be a string: %w", err)
		}
		if decoded == "" {
			return errors.New("auto_mode.mode must not be empty")
		}
	}
	if model, present := object["model"]; present {
		var decoded string
		if err := json.Unmarshal(model, &decoded); err != nil {
			return fmt.Errorf("auto_mode.model must be a string: %w", err)
		}
	}
	if fixed, present := object["fixed_provider"]; present {
		fixedObject, err := rawObject(fixed, "auto_mode.fixed_provider")
		if err != nil {
			return err
		}
		if err := rejectUnknownKeys(fixedObject, map[string]struct{}{
			"base_url": {}, "api_key": {}, "use_x_api_key": {}, "protocol": {}, "tls": {}, "patches": {},
		}); err != nil {
			return err
		}
		for _, required := range []string{"base_url", "api_key", "protocol", "tls", "patches"} {
			if _, ok := fixedObject[required]; !ok {
				return fmt.Errorf("auto_mode.fixed_provider.%s is required", required)
			}
		}
		if tls, present := fixedObject["tls"]; present {
			tlsObject, err := rawObject(tls, "auto_mode.fixed_provider.tls")
			if err != nil {
				return err
			}
			if err := rejectUnknownKeys(tlsObject, map[string]struct{}{"ca_file": {}, "insecure_skip_verify": {}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkProviderKeys(raw map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(raw, map[string]struct{}{
		"id": {}, "name": {}, "base_url": {}, "api_key": {}, "models": {},
		"priority": {}, "enabled": {}, "use_x_api_key": {}, "tls": {}, "patches": {},
		"disable_health": {},
	}); err != nil {
		return err
	}
	if value, ok := raw["tls"]; ok {
		object, err := rawObject(value, "tls")
		if err != nil {
			return err
		}
		if err := rejectUnknownKeys(object, map[string]struct{}{"ca_file": {}, "insecure_skip_verify": {}}); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnknownKeys(raw map[string]json.RawMessage, allowed map[string]struct{}) error {
	for key := range raw {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if isJSONNull(raw[key]) {
			return fmt.Errorf("field %q must not be null", key)
		}
	}
	return nil
}

func rawObject(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	if isJSONNull(raw) {
		return nil, fmt.Errorf("%s must not be null", name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("must be a JSON object")
		}
		return nil, fmt.Errorf("%s %w", name, err)
	}
	return object, nil
}
