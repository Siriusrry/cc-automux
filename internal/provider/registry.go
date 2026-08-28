package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	PatchAnyRouter   = "anyrouter"
	PatchCLIProxyAPI = "cliproxyapi"
)

// PatchMetadata describes a project-maintained preset independently of its
// runtime implementation state.
type PatchMetadata struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Implemented bool   `json:"implemented"`
}

var ErrUnknownPatch = errors.New("unknown provider patch")
var ErrPatchNotImplemented = errors.New("provider patch is not implemented")

// Registry is an immutable lookup table for known preset IDs.
type Registry struct {
	entries map[string]PatchMetadata
}

func (r Registry) Empty() bool { return len(r.entries) == 0 }

// DefaultRegistry contains the built-in preset metadata. A preset can be known
// to configuration and discovery while its execution remains unimplemented.
func DefaultRegistry() Registry {
	entries := []PatchMetadata{
		{
			ID:          PatchAnyRouter,
			Name:        "AnyRouter",
			Description: "AnyRouter-specific request compatibility behavior",
			Implemented: false,
		},
		{
			ID:          PatchCLIProxyAPI,
			Name:        "CLIProxyAPI",
			Description: "CLIProxyAPI-specific classifier compatibility behavior",
			Implemented: false,
		},
	}
	registry, err := NewRegistry(entries)
	if err != nil {
		panic(err)
	}
	return registry
}

// NewRegistry makes a defensive copy of metadata and rejects malformed or
// duplicate entries. Registry construction never silently drops presets.
func NewRegistry(entries []PatchMetadata) (Registry, error) {
	registry := Registry{entries: make(map[string]PatchMetadata, len(entries))}
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" || strings.ContainsAny(entry.ID, " \t\r\n") {
			return Registry{}, errors.New("provider patch id must be non-empty and contain no whitespace")
		}
		if _, exists := registry.entries[entry.ID]; exists {
			return Registry{}, fmt.Errorf("duplicate provider patch id %q", entry.ID)
		}
		if strings.TrimSpace(entry.Name) == "" {
			return Registry{}, fmt.Errorf("provider patch %q name must not be empty", entry.ID)
		}
		registry.entries[entry.ID] = entry
	}
	return registry, nil
}

func (r Registry) Lookup(id string) (PatchMetadata, bool) {
	entry, ok := r.entries[id]
	return entry, ok
}

func (r Registry) List() []PatchMetadata {
	// Keep the public order deterministic and independent of map iteration.
	ordered := []string{PatchAnyRouter, PatchCLIProxyAPI}
	result := make([]PatchMetadata, 0, len(r.entries))
	seen := make(map[string]struct{}, len(r.entries))
	for _, id := range ordered {
		if entry, ok := r.entries[id]; ok {
			result = append(result, entry)
			seen[id] = struct{}{}
		}
	}
	custom := make([]PatchMetadata, 0, len(r.entries)-len(result))
	for id, entry := range r.entries {
		if _, ok := seen[id]; !ok {
			custom = append(custom, entry)
		}
	}
	sort.Slice(custom, func(i, j int) bool { return custom[i].ID < custom[j].ID })
	return append(result, custom...)
}

func (r Registry) Validate(ids []string) error {
	for _, id := range ids {
		entry, ok := r.Lookup(id)
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownPatch, id)
		}
		if entry.ID == "" {
			return fmt.Errorf("%w: %q has no metadata", ErrUnknownPatch, id)
		}
	}
	return nil
}
