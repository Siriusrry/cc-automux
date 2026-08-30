package runtime

import (
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

// Snapshot is one immutable, fully compiled runtime state.
type Snapshot struct {
	revision           uint64
	config             config.Config
	catalog            *provider.Catalog
	attempts           scheduler.AttemptPolicy
	classifierAttempts scheduler.AttemptPolicy
	created            time.Time
}

func newSnapshot(revision uint64, cfg config.Config, catalog *provider.Catalog, attempts, classifierAttempts scheduler.AttemptPolicy, now time.Time) *Snapshot {
	return &Snapshot{
		revision:           revision,
		config:             cfg.Clone(),
		catalog:            catalog,
		attempts:           attempts,
		classifierAttempts: classifierAttempts,
		created:            now,
	}
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

func (s *Snapshot) AutoMode() flow.AutoModeSnapshot { return flow.AutoModeSnapshot{} }

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
