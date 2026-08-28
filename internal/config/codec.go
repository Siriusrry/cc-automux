package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	for _, requiredObject := range []string{"service", "auth", "providers"} {
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
		"schema_version": {}, "service": {}, "auth": {}, "providers": {},
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

func checkProviderKeys(raw map[string]json.RawMessage) error {
	if err := rejectUnknownKeys(raw, map[string]struct{}{
		"id": {}, "name": {}, "base_url": {}, "api_key": {}, "models": {},
		"priority": {}, "enabled": {}, "use_x_api_key": {}, "tls": {}, "patches": {},
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
