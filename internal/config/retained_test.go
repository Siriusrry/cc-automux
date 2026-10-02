package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTLSStrictInputAndCanonicalOutput(t *testing.T) {
	for _, tc := range []struct{ input, mode, ca string }{
		{`{}`, TLSSystem, ""},
		{`{"ca_file":"ca.pem","insecure_skip_verify":false}`, TLSCustom, "ca.pem"},
		{`{"insecure_skip_verify":true}`, TLSSkip, ""},
		{`{"mode":"system","ca_file":"retained.pem"}`, TLSSystem, "retained.pem"},
		{`{"mode":"skip","ca_file":"retained.pem"}`, TLSSkip, "retained.pem"},
	} {
		p, err := DecodeProvider([]byte(`{"tls":` + tc.input + `}`))
		if err != nil || p.TLS.Mode != tc.mode || p.TLS.CAFile != tc.ca {
			t.Fatalf("%s: %+v, %v", tc.input, p.TLS, err)
		}
		data, err := json.Marshal(p.TLS)
		if err != nil || strings.Contains(string(data), "insecure_skip_verify") {
			t.Fatalf("noncanonical output: %s, %v", data, err)
		}
		var again TLSConfig
		if err := json.Unmarshal(data, &again); err != nil || again != p.TLS {
			t.Fatalf("roundtrip: %+v, %v", again, err)
		}
	}
	for _, input := range []string{
		`null`, `[]`, `{"mode":null}`, `{"mode":""}`, `{"mode":true}`, `{"mode":"system","mode":"skip"}`,
		`{"mode":"system","insecure_skip_verify":false}`, `{"ca_file":"ca.pem","insecure_skip_verify":true}`,
		`{"ca_file":null}`, `{"unknown":1}`, `{"insecure_skip_verify":"true"}`,
	} {
		if _, err := DecodeProvider([]byte(`{"tls":` + input + `}`)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	for _, tls := range []TLSConfig{{Mode: "invalid"}, {Mode: TLSCustom}} {
		if err := tls.Validate("tls"); err == nil {
			t.Fatalf("accepted invalid TLS: %+v", tls)
		}
	}
}

func TestRetainedConfigRoundTripAndActivePathInputs(t *testing.T) {
	cfg := configWithProfileForTest()
	cfg.AutoMode = validAutoModeFixed()
	cfg.AutoMode.Mode = AutoModeDisabled
	cfg.AutoMode.FixedProvider.TLS = TLSConfig{Mode: TLSSystem, CAFile: filepath.Join(t.TempDir(), "missing.pem")}
	cfg.Harnesses.ClaudeCode.SettingsPath = filepath.Join(t.TempDir(), "remembered.json")
	cfg.Harnesses.ClaudeCode.ActiveProfileID = cfg.Harnesses.ClaudeCode.Profiles[0].ID
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil || !reflect.DeepEqual(decoded, cfg.Normalize()) {
		t.Fatalf("retained roundtrip: %v", err)
	}
	next := cfg.Clone()
	next.Harnesses.ClaudeCode.SettingsPath = filepath.Join(t.TempDir(), "other.json")
	if !cfg.ActiveProfileInputsEqual(next) {
		t.Fatal("dormant path invalidated active profile")
	}
	next.Harnesses.ClaudeCode.PathMode = PathModeCustom
	if cfg.ActiveProfileInputsEqual(next) {
		t.Fatal("effective path change kept active")
	}
	cfg.AutoMode.FixedProvider.APIKey = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("incomplete dormant target was accepted")
	}
}
