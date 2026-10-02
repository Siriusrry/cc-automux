package config

import (
	"encoding/json"
	"errors"
)

const (
	TLSSystem = "system"
	TLSCustom = "custom"
	TLSSkip   = "skip"
)

// TLSConfig keeps the selected trust mode and the last applied custom CA path.
type TLSConfig struct {
	Mode   string `json:"mode"`
	CAFile string `json:"ca_file"`
}

func (t TLSConfig) Normalize() TLSConfig {
	if t.Mode == "" {
		t.Mode = TLSSystem
	}
	return t
}

func (t TLSConfig) MarshalJSON() ([]byte, error) {
	type wire TLSConfig
	return json.Marshal(wire(t.Normalize()))
}

func (t TLSConfig) Validate(prefix string) error {
	switch t.Normalize().Mode {
	case TLSSystem, TLSSkip:
	case TLSCustom:
		if t.CAFile == "" {
			return validation(prefix+".ca_file", "is required in custom mode")
		}
	default:
		return validation(prefix+".mode", "must be system, custom, or skip")
	}
	if containsControl(t.CAFile) {
		return validation(prefix+".ca_file", "must not contain control characters")
	}
	return nil
}

// EffectiveTLS contains only transport-affecting inputs. Retained CA paths
// never affect a target's identity or filesystem access outside custom mode.
type EffectiveTLS struct {
	CAFile             string
	InsecureSkipVerify bool
}

func (t TLSConfig) Effective() EffectiveTLS {
	t = t.Normalize()
	if t.Mode == TLSCustom {
		return EffectiveTLS{CAFile: t.CAFile}
	}
	return EffectiveTLS{InsecureSkipVerify: t.Mode == TLSSkip}
}

// UnmarshalJSON accepts the published selector once at the input boundary.
// All in-process values and subsequent output use the explicit mode.
func (t *TLSConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("tls must be an object")
	}
	if err := rejectUnknownKeys(raw, map[string]struct{}{"mode": {}, "ca_file": {}, "insecure_skip_verify": {}}); err != nil {
		return err
	}
	var wire struct {
		Mode   string `json:"mode"`
		CAFile string `json:"ca_file"`
		Skip   bool   `json:"insecure_skip_verify"`
	}
	if err := decodeObject(data, &wire); err != nil {
		return err
	}
	if _, explicit := raw["mode"]; explicit {
		if _, legacy := raw["insecure_skip_verify"]; legacy {
			return errors.New("tls.mode cannot be combined with insecure_skip_verify")
		}
		if wire.Mode == "" {
			return errors.New("tls.mode must not be empty")
		}
	} else {
		wire.Mode = TLSSystem
		if wire.CAFile != "" && wire.Skip {
			return errors.New("ca_file and insecure_skip_verify are mutually exclusive")
		}
		if wire.CAFile != "" {
			wire.Mode = TLSCustom
		}
		if wire.Skip {
			wire.Mode = TLSSkip
		}
	}
	*t = TLSConfig{Mode: wire.Mode, CAFile: wire.CAFile}
	return nil
}
