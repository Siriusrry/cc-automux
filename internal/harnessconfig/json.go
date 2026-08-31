package harnessconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

type settingsDocument struct {
	top map[string]json.RawMessage
	env map[string]json.RawMessage
}

var errDuplicateKeyScan = errors.New("duplicate key while scanning JSON")

// Merge parses one complete settings document, changes only the adapter's
// managed fields, and emits deterministic two-space JSON with a final newline.
func (a *ClaudeCodeAdapter) Merge(original []byte, input ManagedProjection) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: Claude Code adapter is nil", ErrInvalidAdapter)
	}
	projection, err := canonicalProjection(input)
	if err != nil {
		return nil, err
	}
	if len(original) == 0 {
		original = []byte("{}")
	}
	document, err := parseSettingsDocument(original)
	if err != nil {
		return nil, err
	}
	if document.env == nil {
		document.env = make(map[string]json.RawMessage)
	}
	// Optional fields are managed by presence: an omitted projection entry
	// removes a stale value from the target.
	delete(document.env, EnvClaudeCodeSubagentModel)
	delete(document.top, TopLevelTeammateDefaultModel)
	for key, value := range projection.Env {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode env.%s: %v", ErrInvalidProjection, key, marshalErr)
		}
		document.env[key] = encoded
	}
	for key, value := range projection.TopLevel {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode %s: %v", ErrInvalidProjection, key, marshalErr)
		}
		document.top[key] = encoded
	}
	env, err := json.Marshal(document.env)
	if err != nil {
		return nil, fmt.Errorf("%w: encode env: %v", ErrInvalidProjection, err)
	}
	document.top["env"] = env
	result, err := json.MarshalIndent(document.top, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: encode settings: %v", ErrInvalidProjection, err)
	}
	result = append(result, '\n')
	if err := a.Verify(result, projection); err != nil {
		return nil, fmt.Errorf("%w: merged output: %w", ErrVerification, err)
	}
	return result, nil
}

// Verify re-parses the complete document and checks every managed field,
// including optional presence/deletion semantics. Unknown fields are not
// inspected and therefore remain outside the adapter's ownership.
func (a *ClaudeCodeAdapter) Verify(data []byte, input ManagedProjection) error {
	if a == nil {
		return fmt.Errorf("%w: Claude Code adapter is nil", ErrInvalidAdapter)
	}
	projection, err := canonicalProjection(input)
	if err != nil {
		return err
	}
	document, err := parseSettingsDocument(data)
	if err != nil {
		return err
	}
	for _, key := range RequiredManagedEnvKeys() {
		if err := verifyStringField(document.env, key, projection.Env[key]); err != nil {
			return err
		}
	}
	if expected, ok := projection.Env[EnvClaudeCodeSubagentModel]; ok {
		if err := verifyStringField(document.env, EnvClaudeCodeSubagentModel, expected); err != nil {
			return err
		}
	} else if _, exists := document.env[EnvClaudeCodeSubagentModel]; exists {
		return fmt.Errorf("%w: env.%s must be absent", ErrProjectionMismatch, EnvClaudeCodeSubagentModel)
	}
	if expected, ok := projection.TopLevel[TopLevelTeammateDefaultModel]; ok {
		if err := verifyStringField(document.top, TopLevelTeammateDefaultModel, expected); err != nil {
			return err
		}
	} else if _, exists := document.top[TopLevelTeammateDefaultModel]; exists {
		return fmt.Errorf("%w: %s must be absent", ErrProjectionMismatch, TopLevelTeammateDefaultModel)
	}
	return nil
}

func verifyStringField(fields map[string]json.RawMessage, key, expected string) error {
	raw, ok := fields[key]
	if !ok {
		return fmt.Errorf("%w: missing %q", ErrProjectionMismatch, key)
	}
	var actual string
	if err := json.Unmarshal(raw, &actual); err != nil {
		return fmt.Errorf("%w: %q is not a string", ErrProjectionMismatch, key)
	}
	if actual != expected {
		return fmt.Errorf("%w: %q differs", ErrProjectionMismatch, key)
	}
	return nil
}

func parseSettingsDocument(data []byte) (settingsDocument, error) {
	if !utf8.Valid(data) {
		return settingsDocument{}, fmt.Errorf("%w: document is not valid UTF-8", ErrInvalidJSON)
	}
	if err := scanJSONDocument(data); err != nil {
		return settingsDocument{}, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return settingsDocument{}, fmt.Errorf("%w: %w", ErrInvalidJSON, ErrTopLevelNotObject)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil || top == nil {
		if err == nil {
			err = ErrTopLevelNotObject
		}
		return settingsDocument{}, fmt.Errorf("%w: %w: %v", ErrInvalidJSON, ErrTopLevelNotObject, err)
	}
	env := make(map[string]json.RawMessage)
	if raw, ok := top["env"]; ok {
		envTrimmed := bytes.TrimSpace(raw)
		if len(envTrimmed) == 0 || envTrimmed[0] != '{' {
			return settingsDocument{}, fmt.Errorf("%w: %w", ErrInvalidJSON, ErrEnvNotObject)
		}
		if err := json.Unmarshal(envTrimmed, &env); err != nil || env == nil {
			if err == nil {
				err = ErrEnvNotObject
			}
			return settingsDocument{}, fmt.Errorf("%w: %w: %v", ErrInvalidJSON, ErrEnvNotObject, err)
		}
	}
	return settingsDocument{top: top, env: env}, nil
}

func scanJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		if errors.Is(err, errDuplicateKeyScan) {
			return fmt.Errorf("%w: %w: %v", ErrInvalidJSON, ErrDuplicateJSONKey, err)
		}
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: %w", ErrInvalidJSON, ErrTrailingJSON)
		}
		return fmt.Errorf("%w: %w: %v", ErrInvalidJSON, ErrTrailingJSON, err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
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
				return fmt.Errorf("%w: %q", errDuplicateKeyScan, key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("object did not close with }")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("array did not close with ]")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
