package runtime

import (
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
)

// Snapshot is one immutable, fully compiled runtime state.
type Snapshot struct {
	revision uint64
	config   config.Config
	catalog  *provider.Catalog
	created  time.Time
}

func newSnapshot(revision uint64, cfg config.Config, catalog *provider.Catalog, now time.Time) *Snapshot {
	return &Snapshot{
		revision: revision,
		config:   cfg.Clone(),
		catalog:  catalog,
		created:  now,
	}
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

func (s *Snapshot) Catalog() *provider.Catalog {
	if s == nil {
		return &provider.Catalog{}
	}
	return s.catalog.Clone()
}

func (s *Snapshot) CreatedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.created
}
