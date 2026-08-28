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
func (p *CompiledProvider) ApplyAuthHeaders(headers http.Header) error {
	if p == nil || p.APIKey == "" {
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
	if p.UseXAPIKey {
		headers.Set("X-Api-Key", p.APIKey)
		return nil
	}
	headers.Set("Authorization", "Bearer "+p.APIKey)
	return nil
}

// PrepareHeaders clones client headers and applies the provider credential,
// making the credential-boundary operation convenient for forwarding code.
func (p *CompiledProvider) PrepareHeaders(source http.Header) (http.Header, error) {
	destination := source.Clone()
	if destination == nil {
		destination = make(http.Header)
	}
	if err := p.ApplyAuthHeaders(destination); err != nil {
		return nil, err
	}
	return destination, nil
}
