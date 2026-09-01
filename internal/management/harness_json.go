package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
)

// decodeStrictJSONObject performs the HTTP-side schema shape checks shared by
// harness and profile requests. It rejects duplicate keys at every nesting
// level, unknown top-level fields, null values where a typed field is expected,
// and any trailing JSON value.
func decodeStrictJSONObject(data []byte, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: request JSON is not valid UTF-8", harnessconfig.ErrInvalidJSON)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanStrictJSONValue(decoder); err != nil {
		return nil, fmt.Errorf("%w: %v", harnessconfig.ErrInvalidJSON, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing JSON content", harnessconfig.ErrInvalidJSON)
		}
		return nil, fmt.Errorf("%w: trailing JSON content", harnessconfig.ErrInvalidJSON)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(data), &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("request body must be a JSON object")
		}
		return nil, fmt.Errorf("%w: %v", harnessconfig.ErrInvalidJSON, err)
	}
	for key := range object {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("%w: unknown field %q", harnessconfig.ErrInvalidJSON, key)
		}
	}
	return object, nil
}

func scanStrictJSONValue(decoder *json.Decoder) error {
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
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanStrictJSONValue(decoder); err != nil {
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
			if err := scanStrictJSONValue(decoder); err != nil {
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

func jsonNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func requestString(object map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := object[key]
	if !ok {
		return "", false, nil
	}
	if jsonNull(raw) {
		return "", true, fmt.Errorf("%w: %s must not be null", harnessconfig.ErrInvalidJSON, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, fmt.Errorf("%w: %s must be a string", harnessconfig.ErrInvalidJSON, key)
	}
	return value, true, nil
}

func requestBool(object map[string]json.RawMessage, key string) (bool, bool, error) {
	raw, ok := object[key]
	if !ok {
		return false, false, nil
	}
	if jsonNull(raw) {
		return false, true, fmt.Errorf("%w: %s must not be null", harnessconfig.ErrInvalidJSON, key)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, fmt.Errorf("%w: %s must be a boolean", harnessconfig.ErrInvalidJSON, key)
	}
	return value, true, nil
}

var harnessUpdateAllowed = map[string]struct{}{
	"path_mode": {}, "settings_path": {}, "disable_telemetry": {}, "active_profile_id": {},
}

func decodeHarnessUpdate(data []byte) (harnessconfig.HarnessUpdatePatch, error) {
	object, err := decodeStrictJSONObject(data, harnessUpdateAllowed)
	if err != nil {
		return harnessconfig.HarnessUpdatePatch{}, err
	}
	if _, present := object["active_profile_id"]; present {
		// Presence itself is forbidden: this is server-owned state, so even a
		// null or incorrectly typed client value must not be interpreted as an
		// ordinary validation failure.
		return harnessconfig.HarnessUpdatePatch{}, config.ErrActiveProfileReadOnly
	}
	var patch harnessconfig.HarnessUpdatePatch
	if value, present, err := requestString(object, "path_mode"); err != nil {
		return patch, err
	} else if present {
		patch.PathMode = &value
	}
	if value, present, err := requestString(object, "settings_path"); err != nil {
		return patch, err
	} else if present {
		patch.SettingsPath = &value
	}
	if value, present, err := requestBool(object, "disable_telemetry"); err != nil {
		return patch, err
	} else if present {
		patch.DisableTelemetry = &value
	}
	return patch, nil
}

var profileAllowed = map[string]struct{}{
	"id": {}, "name": {}, "haiku_model": {}, "sonnet_model": {},
	"opus_model": {}, "fable_model": {}, "subagent_model": {},
	"teammate_default_model": {}, "active": {},
}

func decodeProfile(data []byte) (config.Profile, error) {
	object, err := decodeStrictJSONObject(data, profileAllowed)
	if err != nil {
		return config.Profile{}, err
	}
	if _, present := object["active"]; present {
		// The active bit is derived response state and is never accepted in a
		// client profile representation, regardless of the supplied JSON type.
		return config.Profile{}, config.ErrActiveProfileReadOnly
	}
	var profile config.Profile
	for key, destination := range map[string]*string{
		"id": &profile.ID, "name": &profile.Name,
		"haiku_model": &profile.HaikuModel, "sonnet_model": &profile.SonnetModel,
		"opus_model": &profile.OpusModel, "fable_model": &profile.FableModel,
		"subagent_model":         &profile.SubagentModel,
		"teammate_default_model": &profile.TeammateDefaultModel,
	} {
		if raw, present := object[key]; present {
			if jsonNull(raw) {
				return config.Profile{}, fmt.Errorf("%w: %s must not be null", harnessconfig.ErrInvalidJSON, key)
			}
			if err := json.Unmarshal(raw, destination); err != nil {
				return config.Profile{}, fmt.Errorf("%w: %s must be a string", harnessconfig.ErrInvalidJSON, key)
			}
		}
	}
	return profile, nil
}

func validateActivationBody(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	object, err := decodeStrictJSONObject(data, map[string]struct{}{})
	if err != nil {
		return err
	}
	if len(object) != 0 {
		return fmt.Errorf("%w: activation body must be empty", harnessconfig.ErrInvalidJSON)
	}
	return nil
}
