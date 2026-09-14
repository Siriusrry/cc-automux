package harnessconfig

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/config"
)

// Validator is immutable and stateless. It verifies a candidate's external
// managed fields and never reads Runtime or publishes configuration.
type Validator struct {
	registry  AdapterRegistry
	files     *FileStore
	protected []string
}

func NewValidator(registry AdapterRegistry, files *FileStore, protected []string) (*Validator, error) {
	if registry == nil {
		return nil, ErrNilAdapter
	}
	if _, ok := registry.Lookup(ClaudeCodeAdapterID); !ok {
		return nil, ErrHarnessNotFound
	}
	if files == nil {
		files = NewFileStore()
	}
	return &Validator{registry: registry, files: files, protected: normalizeProtectedPaths(protected)}, nil
}

func (v *Validator) Check(cfg config.Config) (config.HarnessValidation, error) {
	adapter, ok := v.registry.Lookup(ClaudeCodeAdapterID)
	if !ok || adapter == nil {
		return config.HarnessValidation{}, fmt.Errorf("%w: %s", ErrHarnessNotFound, ClaudeCodeAdapterID)
	}
	path, err := v.pathFor(cfg, adapter)
	result := config.HarnessValidation{State: string(StateInactive), ResolvedPath: path}
	fail := func(state HarnessState, reason string) (config.HarnessValidation, error) {
		result.State = string(state)
		result.Reason = reason
		return result, nil
	}
	if err != nil {
		state, reason := classifyPathError(err)
		return fail(state, reason)
	}
	activeID := cfg.Harnesses.ClaudeCode.ActiveProfileID
	if activeID == "" {
		return result, nil
	}
	profile, ok := v.profileFor(cfg, activeID)
	if !ok {
		return fail(StateInvalid, reasonActiveProfileMissing)
	}
	projection, err := adapter.BuildManagedProjection(activationInput(cfg, profile))
	if err != nil {
		state, reason := classifyProjectionError(err)
		return fail(state, reason)
	}
	target, err := v.files.Read(path)
	if err != nil {
		state, reason := classifyReadError(err)
		return fail(state, reason)
	}
	if err := adapter.Verify(target, projection); err != nil {
		state, reason := classifyVerifyError(err)
		return fail(state, reason)
	}
	result.State = string(StateInSync)
	return result, nil
}

func normalizeProtectedPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		key := path
		if filepath.Separator == '\\' {
			key = strings.ToLower(key)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	return result
}

func (m *Validator) protectedPathConflict(target string) bool {
	if m == nil {
		return false
	}
	backup := BackupPath(target)
	for _, protected := range m.protected {
		if samePath(target, protected) || samePath(backup, protected) {
			return true
		}
	}
	return samePath(target, backup)
}

func (m *Validator) pathFor(cfg config.Config, adapter Adapter) (string, error) {
	cc := cfg.Harnesses.ClaudeCode.Normalize()
	path, err := adapter.ResolvePath(PathConfig{PathMode: cc.PathMode, SettingsPath: cc.SettingsPath})
	if err != nil {
		return "", err
	}
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: resolved path must be absolute", ErrInvalidTargetPath)
	}
	if m.protectedPathConflict(path) {
		return "", ErrPathConflict
	}
	return path, nil
}

func (m *Validator) profileFor(cfg config.Config, id string) (config.Profile, bool) {
	for _, profile := range cfg.Harnesses.ClaudeCode.Profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return config.Profile{}, false
}
