package config

import (
	"encoding/json"
	"errors"
	"fmt"
)

type ProviderOrderUpdate struct {
	ProviderIDs []string `json:"provider_ids"`
}

func DecodeProviderOrder(data []byte) (ProviderOrderUpdate, error) {
	var raw map[string]json.RawMessage
	if err := decodeObject(data, &raw); err != nil {
		return ProviderOrderUpdate{}, err
	}
	if raw == nil {
		return ProviderOrderUpdate{}, &SyntaxError{Err: errors.New("order must be an object")}
	}
	if err := rejectUnknownKeys(raw, map[string]struct{}{"provider_ids": {}}); err != nil {
		return ProviderOrderUpdate{}, &SyntaxError{Err: err}
	}
	if _, ok := raw["provider_ids"]; !ok {
		return ProviderOrderUpdate{}, validation("provider_ids", "is required")
	}
	var ids []json.RawMessage
	if err := json.Unmarshal(raw["provider_ids"], &ids); err != nil {
		return ProviderOrderUpdate{}, &SyntaxError{Err: err}
	}
	for _, id := range ids {
		if isJSONNull(id) {
			return ProviderOrderUpdate{}, &SyntaxError{Err: errors.New("provider_ids must not contain null")}
		}
	}
	var update ProviderOrderUpdate
	if err := decodeObject(data, &update); err != nil {
		return update, err
	}
	return update, nil
}

// ReorderProviders replaces only the slots occupied by a complete priority
// group. Objects and all other priorities retain their exact stored values.
func ReorderProviders(providers []ProviderConfig, ids []string) ([]ProviderConfig, error) {
	if len(ids) == 0 {
		return nil, validation("provider_ids", "must contain a complete priority group")
	}
	byID := make(map[string]ProviderConfig, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}
	selected := make([]ProviderConfig, len(ids))
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		field := fmt.Sprintf("provider_ids[%d]", i)
		if !IsUUID(id) {
			return nil, validation(field, "must be a canonical UUID")
		}
		if seen[id] {
			return nil, validation(field, "duplicates an earlier provider")
		}
		seen[id] = true
		p, ok := byID[id]
		if !ok {
			return nil, validation(field, "provider does not exist")
		}
		if i > 0 && p.Priority != selected[0].Priority {
			return nil, validation(field, "all providers must share the same priority")
		}
		selected[i] = p
	}
	count := 0
	for _, p := range providers {
		if p.Priority == selected[0].Priority {
			count++
		}
	}
	if count != len(ids) {
		return nil, validation("provider_ids", "must include every provider in the priority group")
	}
	result := append([]ProviderConfig(nil), providers...)
	index := 0
	for i, p := range result {
		if p.Priority == selected[0].Priority {
			result[i] = selected[index]
			index++
		}
	}
	return result, nil
}
