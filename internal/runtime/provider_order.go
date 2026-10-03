package runtime

import (
	"errors"

	"github.com/Siriusrry/cc-automux/internal/config"
)

var ErrPreconditionRequired = errors.New("configuration validator is required")

// ReorderProviders requires the exact strong validator from the displayed
// snapshot. It never reconstructs a Provider from a client JSON number.
func (m *Manager) ReorderProviders(ids []string, condition string) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restartStatus.InProgress {
		return ApplyResult{}, ErrRestartInProgress
	}
	if condition == "" {
		return ApplyResult{}, ErrPreconditionRequired
	}
	current := m.current.Load()
	if condition != current.ConfigETag() {
		return ApplyResult{}, ErrConfigChanged
	}
	next := current.Config()
	providers, err := config.ReorderProviders(next.Providers, ids)
	if err != nil {
		return ApplyResult{}, err
	}
	next.Providers = providers
	return m.applyLocked(next)
}
