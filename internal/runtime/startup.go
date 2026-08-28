package runtime

import (
	"errors"
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
)

// StartupCandidate selects a fully compiled pending configuration when one is
// valid, otherwise the active configuration. The application promotes a pending
// candidate only after logging and the listener are ready.
type StartupCandidate struct {
	store          *config.Store
	activeConfig   config.Config
	activeCatalog  *provider.Catalog
	config         config.Config
	catalog        *provider.Catalog
	fromPending    bool
	pendingBlocked bool
	warning        error
}

func LoadStartup(store *config.Store, registry provider.Registry) (*StartupCandidate, error) {
	if store == nil {
		return nil, errors.New("configuration store is required")
	}
	if registry.Empty() {
		registry = provider.DefaultRegistry()
	}
	active, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load active configuration: %w", err)
	}
	activeCatalog, err := provider.CompileCatalogWithRegistry(active.Providers, registry)
	if err != nil {
		return nil, fmt.Errorf("compile active configuration: %w", err)
	}
	candidate := &StartupCandidate{
		store:         store,
		activeConfig:  active,
		activeCatalog: activeCatalog,
		config:        active,
		catalog:       activeCatalog,
	}
	exists, err := store.PendingExists()
	if err != nil {
		return nil, fmt.Errorf("inspect pending configuration: %w", err)
	}
	if !exists {
		return candidate, nil
	}
	pending, err := store.LoadPending()
	if err != nil {
		candidate.warning = fmt.Errorf("discard invalid pending configuration: %w", err)
		if removeErr := store.RemovePending(); removeErr != nil {
			candidate.warning = fmt.Errorf("%v; remove pending: %w", candidate.warning, removeErr)
			candidate.pendingBlocked = true
		}
		return candidate, nil
	}
	pendingCatalog, err := provider.CompileCatalogWithRegistry(pending.Providers, registry)
	if err != nil {
		candidate.warning = fmt.Errorf("discard uncompilable pending configuration: %w", err)
		if removeErr := store.RemovePending(); removeErr != nil {
			candidate.warning = fmt.Errorf("%v; remove pending: %w", candidate.warning, removeErr)
			candidate.pendingBlocked = true
		}
		return candidate, nil
	}
	candidate.config = pending
	candidate.catalog = pendingCatalog
	candidate.fromPending = true
	return candidate, nil
}

func (c *StartupCandidate) Config() config.Config { return c.config.Clone() }

func (c *StartupCandidate) Catalog() *provider.Catalog { return c.catalog.Clone() }

func (c *StartupCandidate) FromPending() bool { return c != nil && c.fromPending }

func (c *StartupCandidate) PendingBlocked() bool { return c != nil && c.pendingBlocked }

func (c *StartupCandidate) Warning() error {
	if c == nil {
		return nil
	}
	return c.warning
}

// Promote commits a pending candidate after app resources are ready.
func (c *StartupCandidate) Promote() error {
	if c == nil || !c.fromPending {
		return nil
	}
	if err := c.store.PromotePending(); err != nil {
		return fmt.Errorf("promote pending configuration: %w", err)
	}
	c.activeConfig = c.config.Clone()
	c.activeCatalog = c.catalog
	c.fromPending = false
	c.pendingBlocked = false
	return nil
}

// Rollback removes a pending candidate and selects the old active state.
func (c *StartupCandidate) Rollback(cause error) error {
	if c == nil || !c.fromPending {
		return nil
	}
	removeErr := c.store.RemovePending()
	c.config = c.activeConfig.Clone()
	c.catalog = c.activeCatalog
	c.fromPending = false
	c.warning = cause
	if removeErr != nil {
		c.pendingBlocked = true
		if c.warning != nil {
			c.warning = fmt.Errorf("%v; remove pending: %w", c.warning, removeErr)
		} else {
			c.warning = fmt.Errorf("remove pending: %w", removeErr)
		}
	}
	return removeErr
}
