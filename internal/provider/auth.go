package provider

import (
	"errors"
	"net/http"
	"strings"
)

var ErrProviderKeyEmpty = errors.New("provider api key is empty")

// ApplyAuthHeaders removes client credentials before installing the selected
// provider credential. It never forwards gateway/client Authorization or
// x-api-key values. An empty key fails closed even for a provider that was
// compiled while disabled or model-less.
func (t *CompiledTarget) ApplyAuthHeaders(headers http.Header) error {
	if t == nil || t.APIKey == "" {
		return ErrProviderKeyEmpty
	}
	if headers == nil {
		return errors.New("request headers are nil")
	}
	for key := range headers {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Api-Key") {
			delete(headers, key)
		}
	}
	if t.UseXAPIKey {
		headers.Set("X-Api-Key", t.APIKey)
		return nil
	}
	headers.Set("Authorization", "Bearer "+t.APIKey)
	return nil
}
