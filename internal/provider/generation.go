package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Siriusrry/cc-automux/internal/config"
)

// ProviderGeneration identifies the request-affecting runtime identity of a
// provider without exposing any credential that participates in that identity.
type ProviderGeneration string

func (g ProviderGeneration) String() string { return string(g) }

func generationFor(input config.ProviderConfig) ProviderGeneration {
	patches := make([]string, len(input.Patches))
	copy(patches, input.Patches)
	identity := struct {
		BaseURL    string           `json:"base_url"`
		APIKey     string           `json:"api_key"`
		UseXAPIKey bool             `json:"use_x_api_key"`
		TLS        config.TLSConfig `json:"tls"`
		Patches    []string         `json:"patches"`
	}{
		BaseURL:    input.BaseURL,
		APIKey:     input.APIKey,
		UseXAPIKey: input.UseXAPIKey,
		TLS:        input.TLS,
		Patches:    patches,
	}
	return hashGeneration(identity)
}

// fixedGenerationFor includes the shared classifier model and protocol ID in
// addition to the request-affecting endpoint/auth/TLS/patch fields.  Changing
// either value therefore receives a fresh target identity while the process
// shared AliasStore remains safe to reuse.
func fixedGenerationFor(input config.FixedProviderConfig, classifierModel string) ProviderGeneration {
	patches := make([]string, len(input.Patches))
	copy(patches, input.Patches)
	identity := struct {
		BaseURL         string           `json:"base_url"`
		APIKey          string           `json:"api_key"`
		UseXAPIKey      bool             `json:"use_x_api_key"`
		TLS             config.TLSConfig `json:"tls"`
		Patches         []string         `json:"patches"`
		ClassifierModel string           `json:"classifier_model"`
		Protocol        string           `json:"protocol"`
	}{
		BaseURL:         input.BaseURL,
		APIKey:          input.APIKey,
		UseXAPIKey:      input.UseXAPIKey,
		TLS:             input.TLS,
		Patches:         patches,
		ClassifierModel: classifierModel,
		Protocol:        input.Protocol,
	}
	return hashGeneration(identity)
}

// FixedGenerationFor exposes the deterministic fixed-target identity helper
// for diagnostics and tests without exposing any credential material.
func FixedGenerationFor(input config.FixedProviderConfig, classifierModel string) ProviderGeneration {
	return fixedGenerationFor(input, classifierModel)
}

func hashGeneration(identity any) ProviderGeneration {
	encoded, err := json.Marshal(identity)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return ProviderGeneration(hex.EncodeToString(sum[:]))
}
