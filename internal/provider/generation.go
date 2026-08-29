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
	encoded, err := json.Marshal(identity)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return ProviderGeneration(hex.EncodeToString(sum[:]))
}
