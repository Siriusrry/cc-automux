package harnessconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/runtime"
)

// RuntimeBoundary is the only persistent-runtime surface required by Manager.
// Runtime implementations must keep the mutation lock held for the complete
// callback, including the external target-file operation performed by Manager.
type RuntimeBoundary interface {
	Config() config.Config
	WithHarnessMutation(func(config.HarnessMutation) error) error
}

// ProtectedPathProvider lets a Runtime expose its active and pending
// configuration paths without making path metadata part of the generic
// mutation interface.
type ProtectedPathProvider interface {
	ProtectedConfigPaths() []string
}

var (
	ErrHarnessNotFound          = errors.New("harnessconfig: harness not found")
	ErrProfileNotFound          = errors.New("harnessconfig: profile not found")
	ErrProfileIDImmutable       = errors.New("harnessconfig: profile id is immutable")
	ErrActiveProfile            = errors.New("harnessconfig: profile is active")
	ErrHarnessConfigConflict    = errors.New("harnessconfig: target configuration conflict")
	ErrHarnessConfigIOFailed    = errors.New("harnessconfig: target configuration I/O failed")
	ErrActiveProfileStateFailed = errors.New("harnessconfig: active profile state persistence failed")
	ErrManagerNotInitialized    = errors.New("harnessconfig: manager is not initialized")
)

// HarnessState is the closed set returned by status reads. Only in_sync may
// expose a non-empty verified active profile ID.
type HarnessState string

const (
	StateInactive    HarnessState = "inactive"
	StateInSync      HarnessState = "in_sync"
	StateMissing     HarnessState = "missing"
	StateInvalid     HarnessState = "invalid"
	StateUnreadable  HarnessState = "unreadable"
	StateOutOfSync   HarnessState = "out_of_sync"
	StateError       HarnessState = "state_error"
	stateReasonEmpty              = ""
)

// State is a short alias for callers that prefer the generic name.
type State = HarnessState

const (
	Inactive   = StateInactive
	InSync     = StateInSync
	Missing    = StateMissing
	Invalid    = StateInvalid
	Unreadable = StateUnreadable
	OutOfSync  = StateOutOfSync
	StateErr   = StateError
)

const (
	reasonActiveProfileMissing = "active_profile_missing"
	reasonTargetMissing        = "target_missing"
	reasonTargetInvalid        = "target_invalid"
	reasonTargetUnreadable     = "target_unreadable"
	reasonProjectionMismatch   = "projection_mismatch"
	reasonStatePersistence     = "active_profile_state_failed"
	reasonPathConflict         = "protected_path_conflict"
	reasonPathInvalid          = "path_invalid"
	reasonGatewayMissing       = "gateway_not_configured"
	reasonConfigurationChanged = "configuration_changed"
)

// HarnessStatus is the safe, target-content-free status projection used by
// later management/API layers. It never contains unknown target fields or
// credentials read from the target document.
type HarnessStatus struct {
	ID                     string       `json:"id"`
	PathMode               string       `json:"path_mode"`
	SettingsPath           string       `json:"settings_path"`
	ResolvedSettingsPath   string       `json:"resolved_settings_path"`
	DisableTelemetry       bool         `json:"disable_telemetry"`
	ActiveProfileID        string       `json:"active_profile_id"`
	State                  HarnessState `json:"state"`
	ProfileCount           int          `json:"profile_count"`
	LastInvalidationReason string       `json:"last_invalidation_reason"`
}

// Status is an alias retained for generic callers of Manager.Status.
type Status = HarnessStatus

// ProfileView adds the derived active bit to a persisted profile. The bit is
// never persisted and is true only when the manager has verified the complete
// external projection.
type ProfileView struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	HaikuModel           string `json:"haiku_model"`
	SonnetModel          string `json:"sonnet_model"`
	OpusModel            string `json:"opus_model"`
	FableModel           string `json:"fable_model"`
	SubagentModel        string `json:"subagent_model"`
	TeammateDefaultModel string `json:"teammate_default_model"`
	Active               bool   `json:"active"`
}

func profileView(profile config.Profile, activeID string) ProfileView {
	return ProfileView{
		ID:                   profile.ID,
		Name:                 profile.Name,
		HaikuModel:           profile.HaikuModel,
		SonnetModel:          profile.SonnetModel,
		OpusModel:            profile.OpusModel,
		FableModel:           profile.FableModel,
		SubagentModel:        profile.SubagentModel,
		TeammateDefaultModel: profile.TeammateDefaultModel,
		Active:               activeID != "" && profile.ID == activeID,
	}
}

// AdapterInfo is the discovery projection for one registered adapter.
type AdapterInfo struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ProfileSupport  bool     `json:"profile_support"`
	PathModes       []string `json:"path_modes"`
	DefaultPathMode string   `json:"default_path_mode"`
}

// HarnessUpdate is the complete trusted harness-level input used by the
// compatibility-style Update method. active_profile_id and profiles are
// intentionally absent; server-side profile transitions have dedicated
// Manager methods. HTTP callers use HarnessUpdatePatch for partial updates.
type HarnessUpdate struct {
	PathMode         string `json:"path_mode"`
	SettingsPath     string `json:"settings_path"`
	DisableTelemetry bool   `json:"disable_telemetry"`
}

// HarnessUpdatePatch is the transaction-safe partial form used by HTTP PUT.
// A nil field means that the currently persisted value is retained.
type HarnessUpdatePatch struct {
	PathMode         *string
	SettingsPath     *string
	DisableTelemetry *bool
}

// ManagerOptions supplies construction-time seams for Manager.
type ManagerOptions struct {
	Registry       AdapterRegistry
	FileStore      *FileStore
	ProtectedPaths []string
}

// Options is a concise alias for ManagerOptions.
type Options = ManagerOptions

// Manager owns harness semantics and active reconciliation. It does not own a
// second runtime snapshot or any data-plane state.
type Manager struct {
	runtime    RuntimeBoundary
	registry   AdapterRegistry
	files      *FileStore
	protected  []string
	mu         sync.Mutex
	lastReason map[string]string
}

// NewManager constructs a harness manager. The registry is normally supplied
// by the application composition root; a nil registry uses the default
// Claude Code-only registry for focused callers.
func NewManager(runtimeBoundary RuntimeBoundary, registry AdapterRegistry, options ...ManagerOptions) (*Manager, error) {
	if isNilRuntime(runtimeBoundary) {
		return nil, ErrManagerNotInitialized
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	if _, ok := registry.Lookup(ClaudeCodeAdapterID); !ok {
		return nil, fmt.Errorf("%w: %q", ErrHarnessNotFound, ClaudeCodeAdapterID)
	}
	var option ManagerOptions
	if len(options) > 0 {
		option = options[0]
	}
	files := option.FileStore
	if files == nil {
		files = NewFileStore()
	}
	protected := append([]string(nil), option.ProtectedPaths...)
	if provider, ok := runtimeBoundary.(ProtectedPathProvider); ok {
		protected = append(protected, provider.ProtectedConfigPaths()...)
	}
	protected = normalizeProtectedPaths(protected)
	return &Manager{
		runtime:    runtimeBoundary,
		registry:   registry,
		files:      files,
		protected:  protected,
		lastReason: make(map[string]string),
	}, nil
}

// NewManagerWithOptions is the options-first constructor used by composition
// roots that want the registry inside one options value.
func NewManagerWithOptions(runtimeBoundary RuntimeBoundary, options ManagerOptions) (*Manager, error) {
	return NewManager(runtimeBoundary, options.Registry, options)
}

// New is a short constructor alias.
func New(runtimeBoundary RuntimeBoundary, registry AdapterRegistry, options ...ManagerOptions) (*Manager, error) {
	return NewManager(runtimeBoundary, registry, options...)
}

func isNilRuntime(value RuntimeBoundary) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
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

// Discover returns adapter capabilities in deterministic ID order.
func (m *Manager) Discover() []AdapterInfo {
	if m == nil || m.registry == nil {
		return []AdapterInfo{}
	}
	ids := m.registry.List()
	result := make([]AdapterInfo, 0, len(ids))
	for _, id := range ids {
		adapter, ok := m.registry.Lookup(id)
		if !ok || adapter == nil {
			continue
		}
		info := AdapterInfo{
			ID:              id,
			Name:            id,
			PathModes:       []string{},
			DefaultPathMode: PathModeDefault,
		}
		if id == ClaudeCodeAdapterID {
			info.Name = "Claude Code"
			info.ProfileSupport = true
			info.PathModes = []string{PathModeDefault, PathModeCustom}
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// ListAdapters is a discovery alias.
func (m *Manager) ListAdapters() []AdapterInfo { return m.Discover() }

func (m *Manager) lookup(id string) (Adapter, error) {
	if m == nil || m.registry == nil {
		return nil, ErrManagerNotInitialized
	}
	adapter, ok := m.registry.Lookup(id)
	if !ok || adapter == nil {
		return nil, fmt.Errorf("%w: %q", ErrHarnessNotFound, id)
	}
	if id != ClaudeCodeAdapterID {
		return nil, fmt.Errorf("%w: %q", ErrHarnessNotFound, id)
	}
	return adapter, nil
}

func (m *Manager) protectedPathConflict(target string) bool {
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

func (m *Manager) pathFor(cfg config.Config, adapter Adapter) (string, error) {
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

func baseStatus(id string, cfg config.Config, resolved string) HarnessStatus {
	cc := cfg.Harnesses.ClaudeCode.Normalize()
	return HarnessStatus{
		ID:                     id,
		PathMode:               cc.PathMode,
		SettingsPath:           cc.SettingsPath,
		ResolvedSettingsPath:   resolved,
		DisableTelemetry:       cc.DisableTelemetry,
		ActiveProfileID:        "",
		State:                  StateInactive,
		ProfileCount:           len(cc.Profiles),
		LastInvalidationReason: stateReasonEmpty,
	}
}

func (m *Manager) profileFor(cfg config.Config, id string) (config.Profile, bool) {
	for _, profile := range cfg.Harnesses.ClaudeCode.Profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return config.Profile{}, false
}

func activationInput(cfg config.Config, profile config.Profile) ActivationInput {
	return ActivationInput{
		ListenAddr:       cfg.Service.ListenAddr,
		GatewayKey:       cfg.Auth.GatewayKey,
		Profile:          ProfileFromConfig(profile),
		DisableTelemetry: cfg.Harnesses.ClaudeCode.DisableTelemetry,
	}
}

// ProfileFromConfig converts the persistent schema profile into the adapter
// input without sharing mutable state or introducing a second model schema.
func ProfileFromConfig(profile config.Profile) Profile {
	return Profile{
		ID:                   profile.ID,
		Name:                 profile.Name,
		HaikuModel:           profile.HaikuModel,
		SonnetModel:          profile.SonnetModel,
		OpusModel:            profile.OpusModel,
		FableModel:           profile.FableModel,
		SubagentModel:        profile.SubagentModel,
		TeammateDefaultModel: profile.TeammateDefaultModel,
	}
}

func (m *Manager) recordReason(id, reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		delete(m.lastReason, id)
		return
	}
	m.lastReason[id] = reason
}

func (m *Manager) rememberedReason(id string) string {
	if m == nil {
		return ""
	}
	return m.lastReason[id]
}

// Status reads and reconciles one harness. Reconciliation is performed inside
// the Runtime mutation transaction so a stale active ID cannot race with a
// concurrent configuration write. A failed attempt to persist the clear is
// represented as state_error rather than being exposed as active.
func (m *Manager) Status(id string) (HarnessStatus, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return HarnessStatus{}, err
	}
	if m == nil {
		return HarnessStatus{}, ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var status HarnessStatus
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status = m.reconcileInMutation(tx, id, adapter, tx.Config())
		return nil
	})
	if err != nil {
		// A restart blocks writes, but a status read remains useful. It must not
		// claim a verified active profile because the reconciliation could not
		// complete under the mutation boundary.
		if errors.Is(err, runtime.ErrRestartInProgress) {
			cfg := m.runtime.Config()
			status = m.statusWithoutReconcile(id, adapter, cfg)
			status.State = StateError
			status.ActiveProfileID = ""
			status.LastInvalidationReason = runtime.ErrRestartInProgress.Error()
			return status, nil
		}
		return HarnessStatus{}, err
	}
	return status, nil
}

// GetStatus is an explicit alias for callers that prefer resource-oriented
// naming.
func (m *Manager) GetStatus(id string) (HarnessStatus, error) { return m.Status(id) }

// Reconcile is the lifecycle-oriented alias used by the application startup
// hook and tests.
func (m *Manager) Reconcile(id string) (HarnessStatus, error) { return m.Status(id) }

// ReconcileAll reconciles all registered adapters. v1 has one stateful
// adapter, but the result shape keeps the operation generic for future ones.
func (m *Manager) ReconcileAll() ([]HarnessStatus, error) {
	if m == nil || m.registry == nil {
		return nil, ErrManagerNotInitialized
	}
	ids := m.registry.List()
	result := make([]HarnessStatus, 0, len(ids))
	for _, id := range ids {
		status, err := m.Status(id)
		if err != nil {
			return nil, err
		}
		result = append(result, status)
	}
	return result, nil
}

func (m *Manager) statusWithoutReconcile(id string, adapter Adapter, cfg config.Config) HarnessStatus {
	resolved, err := adapter.ResolvePath(PathConfig{
		PathMode:     cfg.Harnesses.ClaudeCode.PathMode,
		SettingsPath: cfg.Harnesses.ClaudeCode.SettingsPath,
	})
	if err != nil {
		resolved = ""
	}
	return baseStatus(id, cfg, resolved)
}

func (m *Manager) reconcileInMutation(tx config.HarnessMutation, id string, adapter Adapter, cfg config.Config) HarnessStatus {
	cfg = cfg.Normalize()
	resolved, pathErr := m.pathFor(cfg, adapter)
	status := baseStatus(id, cfg, resolved)
	activeID := cfg.Harnesses.ClaudeCode.ActiveProfileID
	status.ActiveProfileID = activeID
	if pathErr != nil {
		state, reason := classifyPathError(pathErr)
		status.State = state
		status.LastInvalidationReason = reason
		return m.invalidateInMutation(tx, status, reason)
	}

	if activeID == "" {
		status.State = StateInactive
		status.LastInvalidationReason = m.rememberedReason(id)
		return status
	}
	status.ActiveProfileID = activeID
	profile, ok := m.profileFor(cfg, activeID)
	if !ok {
		status.State = StateInvalid
		return m.invalidateInMutation(tx, status, reasonActiveProfileMissing)
	}

	projection, projectionErr := adapter.BuildManagedProjection(activationInput(cfg, profile))
	if projectionErr != nil {
		state, reason := classifyProjectionError(projectionErr)
		status.State = state
		return m.invalidateInMutation(tx, status, reason)
	}
	target, readErr := m.files.Read(resolved)
	if readErr != nil {
		state, reason := classifyReadError(readErr)
		status.State = state
		return m.invalidateInMutation(tx, status, reason)
	}
	if verifyErr := adapter.Verify(target, projection); verifyErr != nil {
		state, reason := classifyVerifyError(verifyErr)
		status.State = state
		return m.invalidateInMutation(tx, status, reason)
	}

	status.State = StateInSync
	status.LastInvalidationReason = ""
	m.recordReason(id, "")
	return status
}

func (m *Manager) invalidateInMutation(tx config.HarnessMutation, status HarnessStatus, reason string) HarnessStatus {
	status.LastInvalidationReason = reason
	m.recordReason(status.ID, reason)
	if status.ActiveProfileID == "" {
		return status
	}
	status.ActiveProfileID = ""
	if tx == nil {
		status.State = StateError
		status.LastInvalidationReason = reasonStatePersistence
		return status
	}
	if err := tx.ClearActiveProfileID(); err != nil {
		status.State = StateError
		status.LastInvalidationReason = reasonStatePersistence
		return status
	}
	return status
}

func classifyPathError(err error) (HarnessState, string) {
	switch {
	case errors.Is(err, ErrPathConflict):
		return StateInvalid, reasonPathConflict
	case errors.Is(err, ErrInvalidPathConfig), errors.Is(err, ErrInvalidTargetPath):
		return StateInvalid, reasonPathInvalid
	default:
		return StateInvalid, reasonPathInvalid
	}
}

func classifyProjectionError(err error) (HarnessState, string) {
	if errors.Is(err, ErrGatewayKeyRequired) {
		return StateOutOfSync, reasonGatewayMissing
	}
	return StateInvalid, reasonPathInvalid
}

func classifyReadError(err error) (HarnessState, string) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StateMissing, reasonTargetMissing
	case errors.Is(err, ErrTargetSymlink), errors.Is(err, ErrTargetNotRegular),
		errors.Is(err, ErrTargetTooLarge), errors.Is(err, ErrInvalidTargetPath),
		errors.Is(err, ErrPathConflict):
		return StateInvalid, reasonTargetInvalid
	default:
		return StateUnreadable, reasonTargetUnreadable
	}
}

func classifyVerifyError(err error) (HarnessState, string) {
	switch {
	case errors.Is(err, ErrProjectionMismatch):
		return StateOutOfSync, reasonProjectionMismatch
	case errors.Is(err, ErrInvalidJSON), errors.Is(err, ErrDuplicateJSONKey),
		errors.Is(err, ErrTrailingJSON), errors.Is(err, ErrTopLevelNotObject),
		errors.Is(err, ErrEnvNotObject):
		return StateInvalid, reasonTargetInvalid
	default:
		return StateInvalid, reasonTargetInvalid
	}
}

// ActivationResult describes one successful activation. Idempotent is true
// when the requested profile was already fully verified and no target write or
// active-ID mutation was necessary.
type ActivationResult struct {
	Harness    HarnessStatus `json:"harness"`
	Profile    ProfileView   `json:"profile"`
	Active     bool          `json:"active"`
	Idempotent bool          `json:"idempotent"`
}

// Activate performs the complete profile activation state machine while the
// Runtime mutation lock is held. In particular, a failed target write never
// restores the old active ID, and a failed final state write never reports the
// target as active.
func (m *Manager) Activate(id, profileID string) (ActivationResult, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return ActivationResult{}, err
	}
	if m == nil {
		return ActivationResult{}, ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var result ActivationResult
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		cfg := tx.Config().Normalize()
		selected, ok := m.profileFor(cfg, profileID)
		if !ok {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		// Validate all inputs before clearing an existing active ID. This is
		// important for the empty-gateway-key contract and for protected paths.
		path, pathErr := m.pathFor(cfg, adapter)
		if pathErr != nil {
			return activationPathError(pathErr)
		}
		projection, projectionErr := adapter.BuildManagedProjection(activationInput(cfg, selected))
		if projectionErr != nil {
			return activationInputError(projectionErr)
		}

		currentStatus := m.reconcileInMutation(tx, id, adapter, cfg)
		if currentStatus.State == StateError {
			return fmt.Errorf("%w: %s", ErrActiveProfileStateFailed, currentStatus.LastInvalidationReason)
		}
		if currentStatus.State == StateInSync && currentStatus.ActiveProfileID == profileID {
			result = ActivationResult{
				Harness:    currentStatus,
				Profile:    profileView(selected, profileID),
				Active:     true,
				Idempotent: true,
			}
			m.recordReason(id, "")
			return nil
		}

		// Reconciliation clears a stale current ID. A valid current ID for a
		// different profile still has to be cleared before the new target write.
		cfg = tx.Config().Normalize()
		if cfg.Harnesses.ClaudeCode.ActiveProfileID != "" {
			if err := tx.ClearActiveProfileID(); err != nil {
				return fmt.Errorf("%w: clear active profile: %w", ErrActiveProfileStateFailed, err)
			}
			cfg = tx.Config().Normalize()
		}
		// Re-read the selected profile from the post-clear snapshot. The runtime
		// lock prevents another internal update, and this keeps the input tied to
		// the exact snapshot used for the target operation.
		selected, ok = m.profileFor(cfg, profileID)
		if !ok {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		path, pathErr = m.pathFor(cfg, adapter)
		if pathErr != nil {
			return activationPathError(pathErr)
		}
		projection, projectionErr = adapter.BuildManagedProjection(activationInput(cfg, selected))
		if projectionErr != nil {
			return activationInputError(projectionErr)
		}
		if _, writeErr := m.files.Apply(path, adapter, projection); writeErr != nil {
			return activationFileError(writeErr)
		}
		if err := tx.SetActiveProfileID(profileID); err != nil {
			// SetActiveProfileID publishes nothing when persistence fails; the
			// runtime therefore remains inactive even though the target changed.
			return fmt.Errorf("%w: record active profile: %w", ErrActiveProfileStateFailed, err)
		}

		finalCfg := tx.Config().Normalize()
		finalStatus := baseStatus(id, finalCfg, path)
		finalStatus.ActiveProfileID = profileID
		finalStatus.State = StateInSync
		result = ActivationResult{
			Harness:    finalStatus,
			Profile:    profileView(selected, profileID),
			Active:     true,
			Idempotent: false,
		}
		m.recordReason(id, "")
		return nil
	})
	if err != nil {
		return ActivationResult{}, err
	}
	return result, nil
}

// ActivateProfile is the descriptive activation alias.
func (m *Manager) ActivateProfile(id, profileID string) (ActivationResult, error) {
	return m.Activate(id, profileID)
}

func activationPathError(err error) error {
	if errors.Is(err, ErrInvalidPathConfig) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrHarnessConfigConflict, err)
}

func activationInputError(err error) error {
	if errors.Is(err, ErrGatewayKeyRequired) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrHarnessConfigConflict, err)
}

func activationFileError(err error) error {
	if isFileConflict(err) {
		return fmt.Errorf("%w: %w", ErrHarnessConfigConflict, err)
	}
	return fmt.Errorf("%w: %w", ErrHarnessConfigIOFailed, err)
}

func isFileConflict(err error) bool {
	for _, candidate := range []error{
		ErrInvalidJSON, ErrDuplicateJSONKey, ErrTrailingJSON, ErrTopLevelNotObject,
		ErrEnvNotObject, ErrProjectionMismatch, ErrTargetSymlink, ErrBackupSymlink,
		ErrTargetNotRegular, ErrBackupNotRegular, ErrTempNotRegular, ErrTargetTooLarge,
		ErrPathConflict, ErrTargetChanged, ErrInvalidTargetPath,
	} {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

func (m *Manager) withClaudeMutation(id string, fn func(config.HarnessMutation, Adapter, config.Config) error) error {
	adapter, err := m.lookup(id)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrManagerNotInitialized
	}
	if fn == nil {
		return ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		return fn(tx, adapter, tx.Config().Normalize())
	})
}

func (m *Manager) reconciledOperationState(tx config.HarnessMutation, id string, adapter Adapter, cfg config.Config) (HarnessStatus, config.Config, error) {
	status := m.reconcileInMutation(tx, id, adapter, cfg)
	if status.State == StateError {
		return status, tx.Config(), fmt.Errorf("%w: %s", ErrActiveProfileStateFailed, status.LastInvalidationReason)
	}
	return status, tx.Config().Normalize(), nil
}

// Profiles returns all persisted profiles with the active bit derived from a
// fresh reconciliation. The returned slice is always non-nil.
func (m *Manager) Profiles(id string) ([]ProfileView, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []ProfileView
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status, cfg, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		activeID := ""
		if status.State == StateInSync {
			activeID = status.ActiveProfileID
		}
		profiles := cfg.Harnesses.ClaudeCode.Profiles
		result = make([]ProfileView, 0, len(profiles))
		for _, profile := range profiles {
			result = append(result, profileView(profile, activeID))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListProfiles is the collection-oriented alias.
func (m *Manager) ListProfiles(id string) ([]ProfileView, error) { return m.Profiles(id) }

// GetProfile returns one profile after the required status reconciliation.
func (m *Manager) GetProfile(id, profileID string) (ProfileView, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return ProfileView{}, err
	}
	if m == nil {
		return ProfileView{}, ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result ProfileView
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status, cfg, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		profile, ok := m.profileFor(cfg, profileID)
		if !ok {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		activeID := ""
		if status.State == StateInSync {
			activeID = status.ActiveProfileID
		}
		result = profileView(profile, activeID)
		return nil
	})
	if err != nil {
		return ProfileView{}, err
	}
	return result, nil
}

// CreateProfile persists a new profile. An omitted ID is filled with a random
// v4 UUID before the schema validation transaction.
func (m *Manager) CreateProfile(id string, profile config.Profile) (ProfileView, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return ProfileView{}, err
	}
	if m == nil {
		return ProfileView{}, ErrManagerNotInitialized
	}
	if profile.ID == "" {
		profile.ID, err = config.GenerateUUID()
		if err != nil {
			return ProfileView{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result ProfileView
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status, cfg, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		beforeActiveID := cfg.Harnesses.ClaudeCode.ActiveProfileID
		for _, existing := range cfg.Harnesses.ClaudeCode.Profiles {
			if existing.ID == profile.ID {
				return &config.ConflictError{Field: "id", Message: "profile id already exists"}
			}
			if existing.Name == profile.Name {
				return &config.ConflictError{Field: "name", Message: "profile name already exists"}
			}
		}
		if err := tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			h.ClaudeCode.Profiles = append(h.ClaudeCode.Profiles, profile)
			return nil
		}); err != nil {
			return err
		}
		if beforeActiveID != "" && tx.Config().Harnesses.ClaudeCode.ActiveProfileID == "" {
			m.recordReason(id, reasonConfigurationChanged)
		}
		status, cfg, stateErr = m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		activeID := ""
		if status.State == StateInSync {
			activeID = status.ActiveProfileID
		}
		created, ok := m.profileFor(cfg, profile.ID)
		if !ok {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profile.ID)
		}
		result = profileView(created, activeID)
		return nil
	})
	if err != nil {
		return ProfileView{}, err
	}
	return result, nil
}

// UpdateProfile replaces a profile while preserving its immutable ID.
func (m *Manager) UpdateProfile(id, profileID string, profile config.Profile) (ProfileView, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return ProfileView{}, err
	}
	if m == nil {
		return ProfileView{}, ErrManagerNotInitialized
	}
	if profile.ID != "" && profile.ID != profileID {
		return ProfileView{}, ErrProfileIDImmutable
	}
	profile.ID = profileID
	m.mu.Lock()
	defer m.mu.Unlock()
	var result ProfileView
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status, cfg, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		beforeActiveID := cfg.Harnesses.ClaudeCode.ActiveProfileID
		found := false
		for _, existing := range cfg.Harnesses.ClaudeCode.Profiles {
			if existing.ID == profileID {
				found = true
				continue
			}
			if existing.Name == profile.Name {
				return &config.ConflictError{Field: "name", Message: "profile name already exists"}
			}
		}
		if !found {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		if err := tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			for index := range h.ClaudeCode.Profiles {
				if h.ClaudeCode.Profiles[index].ID == profileID {
					h.ClaudeCode.Profiles[index] = profile
					return nil
				}
			}
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}); err != nil {
			return err
		}
		if beforeActiveID != "" && tx.Config().Harnesses.ClaudeCode.ActiveProfileID == "" {
			m.recordReason(id, reasonConfigurationChanged)
		}
		status, cfg, stateErr = m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		activeID := ""
		if status.State == StateInSync {
			activeID = status.ActiveProfileID
		}
		updated, ok := m.profileFor(cfg, profileID)
		if !ok {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		result = profileView(updated, activeID)
		return nil
	})
	if err != nil {
		return ProfileView{}, err
	}
	return result, nil
}

// DeleteProfile removes a non-active profile. A profile that is stale-active
// is first reconciled; a verified active profile remains protected.
func (m *Manager) DeleteProfile(id, profileID string) error {
	adapter, err := m.lookup(id)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		status, cfg, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		if status.State == StateInSync && status.ActiveProfileID == profileID {
			return ErrActiveProfile
		}
		found := false
		for _, profile := range cfg.Harnesses.ClaudeCode.Profiles {
			if profile.ID == profileID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		}
		return tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			for index := range h.ClaudeCode.Profiles {
				if h.ClaudeCode.Profiles[index].ID == profileID {
					h.ClaudeCode.Profiles = append(h.ClaudeCode.Profiles[:index], h.ClaudeCode.Profiles[index+1:]...)
					return nil
				}
			}
			return fmt.Errorf("%w: %q", ErrProfileNotFound, profileID)
		})
	})
}

// Update applies a path/telemetry harness-level update and immediately runs
// the required post-update reconciliation.
func (m *Manager) Update(id string, update HarnessUpdate) (HarnessStatus, error) {
	pathMode := update.PathMode
	settingsPath := update.SettingsPath
	disableTelemetry := update.DisableTelemetry
	return m.UpdatePatch(id, HarnessUpdatePatch{
		PathMode:         &pathMode,
		SettingsPath:     &settingsPath,
		DisableTelemetry: &disableTelemetry,
	})
}

// UpdatePatch applies only the supplied harness-level fields while retaining
// all omitted values from the same transaction snapshot.
func (m *Manager) UpdatePatch(id string, update HarnessUpdatePatch) (HarnessStatus, error) {
	adapter, err := m.lookup(id)
	if err != nil {
		return HarnessStatus{}, err
	}
	if m == nil {
		return HarnessStatus{}, ErrManagerNotInitialized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result HarnessStatus
	err = m.runtime.WithHarnessMutation(func(tx config.HarnessMutation) error {
		_, _, stateErr := m.reconciledOperationState(tx, id, adapter, tx.Config())
		if stateErr != nil {
			return stateErr
		}
		beforeActiveID := tx.Config().Harnesses.ClaudeCode.ActiveProfileID
		if err := tx.UpdateHarness(func(h *config.HarnessesConfig) error {
			if update.PathMode != nil {
				h.ClaudeCode.PathMode = *update.PathMode
			}
			if update.SettingsPath != nil {
				h.ClaudeCode.SettingsPath = *update.SettingsPath
			}
			if update.DisableTelemetry != nil {
				h.ClaudeCode.DisableTelemetry = *update.DisableTelemetry
			}
			return nil
		}); err != nil {
			return err
		}
		if beforeActiveID != "" && tx.Config().Harnesses.ClaudeCode.ActiveProfileID == "" {
			m.recordReason(id, reasonConfigurationChanged)
		}
		result = m.reconcileInMutation(tx, id, adapter, tx.Config())
		return nil
	})
	if err != nil {
		return HarnessStatus{}, err
	}
	return result, nil
}

// UpdateConfig and UpdateHarnessConfig are explicit aliases for the generic
// harness-level update operation.
func (m *Manager) UpdateConfig(id string, update HarnessUpdate) (HarnessStatus, error) {
	return m.Update(id, update)
}

func (m *Manager) UpdateHarnessConfig(id string, update HarnessUpdate) (HarnessStatus, error) {
	return m.Update(id, update)
}
