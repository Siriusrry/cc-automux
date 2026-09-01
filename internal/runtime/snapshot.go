package runtime

import (
	"fmt"
	"time"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func compileAutoMode(auto config.AutoModeConfig, context provider.RuntimeContext) (CompiledAutoMode, error) {
	auto = auto.Normalize()
	if err := auto.Validate(); err != nil {
		return CompiledAutoMode{}, err
	}
	compiled := CompiledAutoMode{Mode: auto.Mode, ClassifierModel: auto.Model}
	if auto.Mode == config.AutoModeFixedProvider {
		target, err := provider.CompileAutoModeTarget(auto, context)
		if err != nil {
			return CompiledAutoMode{}, fmt.Errorf("compile auto_mode.fixed_provider: %w", err)
		}
		compiled.FixedTarget = target
	}
	return compiled, nil
}

// CompileAutoMode validates and compiles the Auto Mode portion of a runtime
// configuration using the supplied process-owned Provider context.
func CompileAutoMode(auto config.AutoModeConfig, context provider.RuntimeContext) (CompiledAutoMode, error) {
	return compileAutoMode(auto, context)
}

// Snapshot is one immutable, fully compiled runtime state.
type Snapshot struct {
	revision           uint64
	config             config.Config
	catalog            *provider.Catalog
	autoMode           CompiledAutoMode
	runtimeContext     provider.RuntimeContext
	attempts           scheduler.AttemptPolicy
	classifierAttempts scheduler.AttemptPolicy
	requestScan        *bodyfile.CompiledScanSpec
	created            time.Time
}

// CompiledAutoMode is the immutable runtime representation of Auto Mode.  It
// carries the one shared classifier model and, only for fixed-provider mode,
// the precompiled pool-external target.  The target is compiled before a
// snapshot is published and is never constructed on a request path.
type CompiledAutoMode struct {
	Mode            string
	ClassifierModel string
	FixedTarget     *provider.CompiledFixedTarget
}

func (a CompiledAutoMode) Clone() CompiledAutoMode {
	out := a
	if a.FixedTarget != nil {
		out.FixedTarget = cloneFixedTarget(a.FixedTarget)
	}
	return out
}

func cloneFixedTarget(target *provider.CompiledFixedTarget) *provider.CompiledFixedTarget {
	if target == nil {
		return nil
	}
	return target.Clone()
}

// newSnapshot is kept as a small compatibility wrapper for package-local
// callers that construct snapshots in tests. Runtime Manager uses
// newSnapshotWithAuto after compiling the candidate so publication cannot
// hide a fixed-target compilation error.
func newSnapshot(revision uint64, cfg config.Config, catalog *provider.Catalog, runtimeContext provider.RuntimeContext, requirements ScanRequirements, attempts, classifierAttempts scheduler.AttemptPolicy, now time.Time) (*Snapshot, error) {
	autoMode, err := compileAutoMode(cfg.AutoMode, runtimeContext)
	if err != nil {
		return nil, err
	}
	return newSnapshotWithAuto(revision, cfg, catalog, autoMode, runtimeContext, requirements, attempts, classifierAttempts, now)
}

func newSnapshotWithAuto(revision uint64, cfg config.Config, catalog *provider.Catalog, autoMode CompiledAutoMode, runtimeContext provider.RuntimeContext, requirements ScanRequirements, attempts, classifierAttempts scheduler.AttemptPolicy, now time.Time) (*Snapshot, error) {
	requestScan, err := compileRequestScan(catalog, autoMode, requirements)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		revision:           revision,
		config:             cfg.Clone(),
		catalog:            catalog,
		autoMode:           autoMode.Clone(),
		runtimeContext:     runtimeContext,
		attempts:           attempts,
		classifierAttempts: classifierAttempts,
		requestScan:        requestScan,
		created:            now,
	}, nil
}

// RuntimeContext returns the shared, process-owned preparation context used to
// compile this snapshot. It is intentionally the same value across hot-applied
// revisions; only immutable configuration-derived catalog data changes.
func (s *Snapshot) RuntimeContext() provider.RuntimeContext {
	if s == nil {
		return provider.RuntimeContext{}
	}
	return s.runtimeContext
}

// AliasStore returns the shared session alias map captured by this snapshot's
// runtime context. Generation remains part of each key, so target identity
// changes cannot reuse an old alias while the map itself remains shared.
func (s *Snapshot) AliasStore() *patch.AliasStore {
	if s == nil {
		return nil
	}
	return s.runtimeContext.Registry.AliasStore()
}

// NormalAttemptPolicy and ClassifierAttemptPolicy implement flow.SnapshotView
// without exposing mutable runtime state to planners.
func (s *Snapshot) NormalAttemptPolicy() scheduler.AttemptPolicy {
	if s == nil {
		return scheduler.AttemptPolicy{}
	}
	return s.attempts
}

func (s *Snapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy {
	if s == nil {
		return scheduler.AttemptPolicy{}
	}
	return s.classifierAttempts
}

func (s *Snapshot) AutoMode() flow.AutoModeSnapshot {
	if s == nil {
		return flow.AutoModeSnapshot{}
	}
	// Snapshot publication has already cloned and validated the target.  The
	// flow boundary is read-only, so return that immutable runtime object
	// directly instead of deep-copying its URL/TLS state on every request.
	return flow.AutoModeSnapshot{
		Mode:            s.autoMode.Mode,
		ClassifierModel: s.autoMode.ClassifierModel,
		FixedTarget:     s.autoMode.FixedTarget,
	}
}

// CompiledAutoMode returns a defensive copy of the precompiled Auto Mode
// state. It is the copy boundary for management or diagnostic callers that
// need to mutate an inspection value; flow planners use AutoMode, which shares
// the immutable published target.
func (s *Snapshot) CompiledAutoMode() CompiledAutoMode {
	if s == nil {
		return CompiledAutoMode{}
	}
	return s.autoMode.Clone()
}

func (s *Snapshot) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision
}

func (s *Snapshot) Config() config.Config {
	if s == nil {
		return config.Config{}
	}
	return s.config.Clone()
}

func (s *Snapshot) ManagementKey() string {
	if s == nil {
		return ""
	}
	return s.config.Auth.ManagementKey
}

// GatewayKey returns the credential used by the Messages data plane. A
// snapshot owns the value for the lifetime of requests that captured it.
func (s *Snapshot) GatewayKey() string {
	if s == nil {
		return ""
	}
	return s.config.Auth.GatewayKey
}

// AttemptPolicy returns the request retry budget captured with this immutable
// runtime revision.
func (s *Snapshot) AttemptPolicy() scheduler.AttemptPolicy {
	if s == nil {
		return scheduler.AttemptPolicy{}
	}
	return s.attempts
}

func (s *Snapshot) Catalog() *provider.Catalog {
	if s == nil {
		return &provider.Catalog{}
	}
	return s.catalog.Clone()
}

// Candidates returns the compiled providers that declare the exact model.
// The catalog supplies defensive copies so callers cannot mutate a snapshot.
func (s *Snapshot) Candidates(model string) []*provider.CompiledProvider {
	if s == nil || s.catalog == nil {
		return []*provider.CompiledProvider{}
	}
	return s.catalog.Match(model)
}

// Providers returns all compiled providers as defensive copies.
func (s *Snapshot) Providers() []*provider.CompiledProvider {
	if s == nil || s.catalog == nil {
		return []*provider.CompiledProvider{}
	}
	return s.catalog.Providers()
}

func (s *Snapshot) CreatedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.created
}
