package runtime

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

var (
	ErrRestartInProgress  = errors.New("restart_in_progress")
	ErrRestartUnavailable = errors.New("restart is not configured")
	ErrRestartNotPending  = errors.New("restart is not pending")
)

// PreflightError means a restart-bound resource (normally a new listener or
// log target) could not be acquired before the pending transaction was written.
type PreflightError struct{ Err error }

func (e *PreflightError) Error() string { return e.Err.Error() }
func (e *PreflightError) Unwrap() error { return e.Err }

type ApplyResult struct {
	Revision        uint64 `json:"revision"`
	Applied         bool   `json:"applied"`
	RestartRequired bool   `json:"restart_required"`
	Restarting      bool   `json:"restarting"`
	ListenAddr      string `json:"listen_addr"`
}

type RestartStatus struct {
	State       string     `json:"state"`
	InProgress  bool       `json:"in_progress"`
	Pending     bool       `json:"pending"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type Options struct {
	// RuntimeContext is created once by the application composition root and
	// carries the sole Patch Registry/shared AliasStore for this process.
	// Manager requires it explicitly and reuses it for every published
	// snapshot; it never constructs replacement runtime state.
	RuntimeContext          provider.RuntimeContext
	AttemptPolicy           scheduler.AttemptPolicy
	ClassifierAttemptPolicy scheduler.AttemptPolicy
	// Preflight runs after full schema/provider compilation but before the
	// pending file is written. Nil uses a loopback listener probe when the
	// address changes and relies on config validation for the log limit.
	Preflight func(current, next config.Config) error
	// Restart replaces the process image. It should return only on failure.
	Restart             func() error
	RestartDelay        time.Duration
	Now                 func() time.Time
	InitialRestartError error
	InitialPending      bool
}

// ConfigStore is the persistence boundary required by the runtime manager. The
// concrete config.Store provides atomic files; the narrow interface also makes
// persistence failures directly testable without weakening production paths.
type ConfigStore interface {
	Save(config.Config) error
	SavePending(config.Config) error
	LoadPending() (config.Config, error)
	PromotePending() error
	RemovePending() error
}

type Manager struct {
	mu                 sync.Mutex
	store              ConfigStore
	runtimeContext     provider.RuntimeContext
	current            atomic.Pointer[Snapshot]
	revision           uint64
	attempts           scheduler.AttemptPolicy
	classifierAttempts scheduler.AttemptPolicy

	preflight    func(current, next config.Config) error
	restart      func() error
	restartDelay time.Duration
	now          func() time.Time
	startedAt    time.Time

	restartStatus    RestartStatus
	restartTriggered bool
}

func NewManager(store ConfigStore, initial config.Config, options Options) (*Manager, error) {
	if store == nil {
		return nil, errors.New("configuration store is required")
	}
	initial = initial.Normalize()
	if err := initial.Validate(); err != nil {
		return nil, err
	}
	context := options.RuntimeContext
	if err := context.Validate(); err != nil {
		return nil, fmt.Errorf("runtime context: %w", err)
	}
	catalog, err := provider.CompileCatalog(initial.Providers, context)
	if err != nil {
		return nil, err
	}
	attempts := options.AttemptPolicy
	if attempts == (scheduler.AttemptPolicy{}) {
		attempts = scheduler.DefaultAttemptPolicy()
	}
	if err := attempts.Validate(); err != nil {
		return nil, fmt.Errorf("attempt policy: %w", err)
	}
	classifierAttempts := options.ClassifierAttemptPolicy
	if classifierAttempts == (scheduler.AttemptPolicy{}) {
		classifierAttempts = scheduler.DefaultClassifierAttemptPolicy()
	}
	if err := classifierAttempts.Validate(); err != nil {
		return nil, fmt.Errorf("classifier attempt policy: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	preflight := options.Preflight
	if preflight == nil {
		preflight = defaultPreflight
	}
	startedAt := now()
	m := &Manager{
		store:              store,
		runtimeContext:     context,
		revision:           1,
		attempts:           attempts,
		classifierAttempts: classifierAttempts,
		preflight:          preflight,
		restart:            options.Restart,
		restartDelay:       options.RestartDelay,
		now:                now,
		startedAt:          startedAt,
		restartStatus:      RestartStatus{State: "idle"},
	}
	if options.InitialRestartError != nil {
		m.restartStatus.State = "failed"
		m.restartStatus.LastError = options.InitialRestartError.Error()
	}
	if options.InitialPending {
		// A pending file that survived startup cleanup is unsafe to overwrite:
		// another process could otherwise promote it over a newer active file on
		// the next launch. Keep serving the active snapshot but block submissions
		// until the pending transaction can be cleaned up.
		m.restartStatus.Pending = true
		m.restartStatus.InProgress = true
		m.restartStatus.State = "failed"
		if m.restartStatus.LastError == "" {
			m.restartStatus.LastError = "pending configuration requires cleanup"
		}
	}
	m.current.Store(newSnapshot(m.revision, initial, catalog, m.runtimeContext, m.attempts, m.classifierAttempts, startedAt))
	return m, nil
}

func (m *Manager) Snapshot() *Snapshot {
	current := m.current.Load()
	if current == nil {
		return &Snapshot{}
	}
	// Snapshot itself is immutable and its getters are defensive, so sharing the
	// pointer is safe and avoids rebuilding a catalog on every request.
	return current
}

func (m *Manager) Registry() patch.Registry {
	if m == nil {
		return patch.Registry{}
	}
	return m.runtimeContext.Registry
}

// RuntimeContext returns the process-owned compilation context.  It is a
// value containing immutable registry metadata and shared service handles;
// callers must not construct a second context for a live application.
func (m *Manager) RuntimeContext() provider.RuntimeContext {
	if m == nil {
		return provider.RuntimeContext{}
	}
	return m.runtimeContext
}

// AliasStore returns the one process-local session map owned by the runtime
// manager.  Hot-applied snapshots keep this exact handle even when target
// generations change (the generation remains part of each alias key).
func (m *Manager) AliasStore() *patch.AliasStore {
	if m == nil {
		return nil
	}
	return m.runtimeContext.Registry.AliasStore()
}

func (m *Manager) StartedAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	return m.startedAt
}

func (m *Manager) RestartStatus() RestartStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartStatus
}

// Apply replaces the complete configuration transactionally.
func (m *Manager) Apply(next config.Config) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(next)
}

// Update serializes read-modify-write operations such as Provider CRUD so two
// concurrent requests cannot overwrite each other's changes.
func (m *Manager) Update(mutator func(*config.Config) error) (ApplyResult, error) {
	if mutator == nil {
		return ApplyResult{}, errors.New("configuration mutator is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restartStatus.InProgress {
		return ApplyResult{}, ErrRestartInProgress
	}
	next := m.current.Load().Config()
	if err := mutator(&next); err != nil {
		return ApplyResult{}, err
	}
	return m.applyLocked(next)
}

func (m *Manager) applyLocked(next config.Config) (ApplyResult, error) {
	if m.restartStatus.InProgress {
		return ApplyResult{}, ErrRestartInProgress
	}
	next = next.Normalize()
	if err := next.Validate(); err != nil {
		return ApplyResult{}, err
	}
	catalog, err := provider.CompileCatalog(next.Providers, m.runtimeContext)
	if err != nil {
		return ApplyResult{}, err
	}
	currentSnapshot := m.current.Load()
	currentConfig := currentSnapshot.Config()
	if reflect.DeepEqual(currentConfig, next) {
		return ApplyResult{
			Revision:   currentSnapshot.Revision(),
			Applied:    false,
			ListenAddr: currentConfig.Service.ListenAddr,
		}, nil
	}

	restartRequired := serviceRestartRequired(currentConfig.Service, next.Service)
	if !restartRequired {
		if err := m.store.Save(next); err != nil {
			return ApplyResult{}, fmt.Errorf("persist active configuration: %w", err)
		}
		m.revision++
		m.current.Store(newSnapshot(m.revision, next, catalog, m.runtimeContext, m.attempts, m.classifierAttempts, m.now()))
		m.restartStatus = RestartStatus{State: "idle"}
		return ApplyResult{
			Revision:   m.revision,
			Applied:    true,
			ListenAddr: next.Service.ListenAddr,
		}, nil
	}

	if err := m.preflight(currentConfig, next); err != nil {
		return ApplyResult{}, &PreflightError{Err: err}
	}
	if err := m.store.SavePending(next); err != nil {
		return ApplyResult{}, fmt.Errorf("persist pending configuration: %w", err)
	}
	m.restartTriggered = false
	requestedAt := m.now().UTC()
	m.restartStatus = RestartStatus{
		State:       "pending",
		InProgress:  true,
		Pending:     true,
		RequestedAt: &requestedAt,
	}
	return ApplyResult{
		Revision:        currentSnapshot.Revision(),
		Applied:         false,
		RestartRequired: true,
		Restarting:      true,
		ListenAddr:      next.Service.ListenAddr,
	}, nil
}

// PrepareRestart reserves the pending transaction and returns a starter that
// callers can invoke after an HTTP 202 response has been written. Reserving
// first makes concurrent submissions fail with 409 while keeping process
// replacement after the response write.
func (m *Manager) PrepareRestart() (func(), error) {
	if m == nil {
		return nil, ErrRestartUnavailable
	}
	m.mu.Lock()
	if !m.restartStatus.InProgress || !m.restartStatus.Pending {
		m.mu.Unlock()
		return nil, ErrRestartNotPending
	}
	if m.restartTriggered {
		m.mu.Unlock()
		return func() {}, nil
	}
	if m.restart == nil {
		m.mu.Unlock()
		_ = m.RestartFailed(ErrRestartUnavailable)
		return nil, ErrRestartUnavailable
	}
	m.restartTriggered = true
	m.restartStatus.State = "restarting"
	delay := m.restartDelay
	restart := m.restart
	m.mu.Unlock()
	return func() { m.startRestart(delay, restart) }, nil
}

// TriggerRestart schedules the configured self-reexec. It remains available
// for non-HTTP callers; HTTP handlers should use PrepareRestart so the starter
// runs only after the response has been written.
func (m *Manager) TriggerRestart() error {
	starter, err := m.PrepareRestart()
	if err != nil {
		return err
	}
	starter()
	return nil
}

func (m *Manager) startRestart(delay time.Duration, restart func() error) {
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		if err := restart(); err != nil {
			_ = m.RestartFailed(err)
			return
		}
		// A real syscall.Exec never returns on success. A nil return from an
		// injected restart function is treated as a successful simulated restart
		// and commits the already validated pending state, which keeps the
		// transaction testable without replacing the test process.
		_ = m.RestartSucceeded()
	}()
}

// RestartSucceeded commits a pending restart candidate after the restart
// callback has established that the new process/resources are ready. It is
// primarily useful for a controlled restart implementation or tests; the real
// self-reexec path normally never returns to call it.
func (m *Manager) RestartSucceeded() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.restartStatus.InProgress || !m.restartStatus.Pending {
		return ErrRestartNotPending
	}
	pending, err := m.store.LoadPending()
	if err != nil {
		_ = m.restartFailedLocked(err)
		return err
	}
	catalog, err := provider.CompileCatalog(pending.Providers, m.runtimeContext)
	if err != nil {
		_ = m.restartFailedLocked(err)
		return err
	}
	if err := m.store.PromotePending(); err != nil {
		_ = m.restartFailedLocked(err)
		return err
	}
	m.revision++
	m.current.Store(newSnapshot(m.revision, pending, catalog, m.runtimeContext, m.attempts, m.classifierAttempts, m.now()))
	m.restartTriggered = false
	m.restartStatus = RestartStatus{State: "idle"}
	return nil
}

// RestartFailed rolls back only the pending file and leaves both the active
// file and active snapshot unchanged.
func (m *Manager) RestartFailed(cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartFailedLocked(cause)
}

func (m *Manager) restartFailedLocked(cause error) error {
	removeErr := m.store.RemovePending()
	message := "restart failed"
	if cause != nil {
		message = cause.Error()
	}
	if removeErr != nil {
		message += "; remove pending: " + removeErr.Error()
	}
	m.restartTriggered = false
	m.restartStatus = RestartStatus{
		State:      "failed",
		InProgress: removeErr != nil,
		Pending:    removeErr != nil,
		LastError:  message,
	}
	return removeErr
}

func serviceRestartRequired(current, next config.ServiceConfig) bool {
	return current.ListenAddr != next.ListenAddr || current.LogMaxBytes != next.LogMaxBytes
}

func defaultPreflight(current, next config.Config) error {
	if current.Service.ListenAddr == next.Service.ListenAddr {
		return nil
	}
	listener, err := net.Listen("tcp", next.Service.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen_addr %s is unavailable: %w", next.Service.ListenAddr, err)
	}
	return listener.Close()
}
