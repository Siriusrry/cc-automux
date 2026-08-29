package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/gateway"
	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/management"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

type Options struct {
	ConfigPath   string
	Stdout       io.Writer
	Stderr       io.Writer
	LogOpener    LogOpener
	Registry     *provider.Registry
	Restart      func() error
	Exec         func() error
	RestartDelay time.Duration
	Preflight    func(current, next config.Config) error
	Now          func() time.Time
}

type App struct {
	manager        *runtime.Manager
	configPath     string
	server         *http.Server
	listener       net.Listener
	logs           *logger
	logOpener      LogOpener
	exec           func() error
	health         *health.Store
	selector       *scheduler.Scheduler
	gateway        *gateway.Handler
	management     *management.Handler
	runtimeMu      sync.Mutex
	syncedRevision uint64

	lifecycleMu sync.Mutex
	resume      chan struct{}
	done        chan struct{}
	closed      bool
	restarting  bool
}

func New(options Options) (*App, error) {
	path := options.ConfigPath
	var err error
	if path == "" {
		path, err = config.Path()
		if err != nil {
			return nil, err
		}
	}
	store, err := config.NewStore(path)
	if err != nil {
		return nil, err
	}
	registry := provider.DefaultRegistry()
	if options.Registry != nil && !options.Registry.Empty() {
		registry = *options.Registry
	}
	startup, err := runtime.LoadStartup(store, registry)
	if err != nil {
		return nil, err
	}

	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	logOpener := options.LogOpener
	if logOpener == nil {
		logOpener = logOpenerFor(stdout, stderr)
	}

	// Acquire the complete candidate before promotion. If pending resources fail,
	// rollback selects the old active configuration and resources are acquired
	// again from that immutable candidate.
	cfg := startup.Config()
	logs, listener, resourceErr := acquireResources(logOpener, cfg)
	if resourceErr != nil && startup.FromPending() {
		_ = startup.Rollback(resourceErr)
		cfg = startup.Config()
		logs, listener, resourceErr = acquireResources(logOpener, cfg)
	}
	if resourceErr != nil {
		return nil, resourceErr
	}

	managerOptions := runtime.Options{
		Registry:     &registry,
		RestartDelay: options.RestartDelay,
		Now:          options.Now,
		Preflight:    options.Preflight,
	}
	if managerOptions.RestartDelay == 0 {
		managerOptions.RestartDelay = 250 * time.Millisecond
	}
	app := &App{
		configPath: path,
		listener:   listener,
		logs:       logs,
		logOpener:  logOpener,
		resume:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if managerOptions.Preflight == nil {
		managerOptions.Preflight = app.preflight
	}
	if options.Restart != nil {
		managerOptions.Restart = options.Restart
	} else {
		if options.Exec != nil {
			app.exec = options.Exec
		}
		managerOptions.Restart = app.restartProcess
	}
	buildManager := func() (*runtime.Manager, error) {
		candidateOptions := managerOptions
		candidateOptions.InitialRestartError = startup.Warning()
		candidateOptions.InitialPending = startup.PendingBlocked()
		return runtime.NewManager(store, cfg, candidateOptions)
	}
	manager, managerErr := buildManager()
	if managerErr != nil && startup.FromPending() {
		closeResources(logs, listener)
		_ = startup.Rollback(managerErr)
		cfg = startup.Config()
		logs, listener, resourceErr = acquireResources(logOpener, cfg)
		if resourceErr == nil {
			app.logs = logs
			app.listener = listener
			manager, managerErr = buildManager()
		}
	}
	if resourceErr != nil || managerErr != nil {
		closeResources(logs, listener)
		if resourceErr != nil {
			return nil, resourceErr
		}
		return nil, managerErr
	}
	if startup.FromPending() {
		if promoteErr := startup.Promote(); promoteErr != nil {
			closeResources(logs, listener)
			_ = startup.Rollback(promoteErr)
			cfg = startup.Config()
			logs, listener, resourceErr = acquireResources(logOpener, cfg)
			if resourceErr != nil {
				return nil, resourceErr
			}
			app.logs = logs
			app.listener = listener
			manager, managerErr = buildManager()
			if managerErr != nil {
				closeResources(logs, listener)
				return nil, managerErr
			}
		}
	}
	if startup.Warning() != nil && logs != nil {
		logs.error.Printf("pending configuration was not activated: %v", startup.Warning())
	}
	app.manager = manager
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	policy := scheduler.DefaultPolicy()
	healthStore, healthErr := health.New(policy, health.ClockFunc(clock))
	if healthErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("initialize health store: %w", healthErr)
	}
	selector, selectorErr := scheduler.NewSelector(healthStore, scheduler.Options{Policy: policy, Now: clock})
	if selectorErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("initialize scheduler: %w", selectorErr)
	}
	app.health = healthStore
	app.selector = selector
	app.gateway = gateway.NewWithOptions(app.snapshotForGateway, selector, gateway.Options{
		Recorder: gateway.EventRecorderFunc(app.recordGatewayEvent),
	})
	app.management = management.NewWithOptions(manager, management.Options{
		Health:         healthStore,
		Selector:       selector,
		Sync:           app.syncRuntime,
		ActiveRequests: app.activeDataRequests,
	})
	app.syncRuntime()
	app.server = newHTTPServer(app.rootHandler())
	app.logs.info.Printf("listening on %s", cfg.Service.ListenAddr)
	return app, nil
}

func Run() error {
	app, err := New(Options{})
	if err != nil {
		return err
	}
	return app.Serve()
}

// Main is the command entrypoint. Initialization errors are deliberately
// fatal; in particular a missing management key must never start a listener.
func Main() {
	if err := Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "CC AutoMux failed to start: %v\n", err)
		os.Exit(1)
	}
}

func (a *App) Manager() *runtime.Manager {
	if a == nil {
		return nil
	}
	return a.manager
}

func (a *App) Listener() net.Listener {
	if a == nil {
		return nil
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	return a.listener
}

func (a *App) Serve() error {
	if a == nil {
		return errors.New("app is not initialized")
	}
	for {
		a.lifecycleMu.Lock()
		if a.closed {
			a.lifecycleMu.Unlock()
			return nil
		}
		server := a.server
		listener := a.listener
		a.lifecycleMu.Unlock()
		if server == nil || listener == nil {
			return errors.New("app listener is unavailable")
		}
		err := server.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			a.lifecycleMu.Lock()
			recovering := a.restarting && !a.closed
			closed := a.closed
			a.lifecycleMu.Unlock()
			if recovering {
				select {
				case <-a.resume:
				case <-a.done:
					return nil
				}
				continue
			}
			if closed {
				return nil
			}
			select {
			case <-a.resume:
				continue
			case <-a.done:
				return nil
			default:
			}
			return nil
		}
		select {
		case <-a.resume:
			continue
		case <-a.done:
			return nil
		default:
			return err
		}
	}
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.lifecycleMu.Lock()
	if a.closed {
		a.lifecycleMu.Unlock()
		return nil
	}
	a.closed = true
	server := a.server
	listener := a.listener
	logs := a.logs
	if a.done != nil {
		close(a.done)
	}
	a.lifecycleMu.Unlock()
	var result error
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.Shutdown(ctx); err != nil {
			result = err
		}
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	if logs != nil {
		if err := logs.Close(); result == nil {
			result = err
		}
	}
	if appGateway := a.gateway; appGateway != nil {
		if err := appGateway.Close(); result == nil {
			result = err
		}
	}
	a.signalResume()
	return result
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func (a *App) rootHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil || r == nil || r.URL == nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == gateway.MessagesPath:
			a.gateway.ServeHTTP(w, r)
		case r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/"):
			a.management.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *App) snapshotForGateway() scheduler.Snapshot {
	if a == nil || a.manager == nil {
		return nil
	}
	a.syncRuntime()
	return a.manager.Snapshot()
}

func (a *App) syncRuntime() {
	if a == nil || a.manager == nil || a.selector == nil {
		return
	}
	snapshot := a.manager.Snapshot()
	if snapshot == nil {
		return
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if snapshot.Revision() <= a.syncedRevision {
		return
	}
	a.selector.Reconcile(snapshot)
	a.syncedRevision = snapshot.Revision()
}

func (a *App) activeDataRequests() int64 {
	if a == nil || a.gateway == nil {
		return 0
	}
	return a.gateway.ActiveRequests()
}

func (a *App) signalResume() {
	if a == nil || a.resume == nil {
		return
	}
	select {
	case a.resume <- struct{}{}:
	default:
	}
}

func processEnvironment(configPath string) []string {
	environ := os.Environ()
	if configPath == "" {
		return environ
	}
	entry := config.ConfigPathEnv + "=" + configPath
	for i, value := range environ {
		name, _, ok := strings.Cut(value, "=")
		if ok && strings.EqualFold(name, config.ConfigPathEnv) {
			environ[i] = entry
			return environ
		}
	}
	return append(environ, entry)
}

func (a *App) beginRestart() error {
	if a == nil {
		return errors.New("self-restart aborted: app is not initialized")
	}
	a.lifecycleMu.Lock()
	if a.closed {
		a.lifecycleMu.Unlock()
		return errors.New("self-restart aborted: app is closed")
	}
	if a.restarting {
		a.lifecycleMu.Unlock()
		return errors.New("self-restart is already in progress")
	}
	server := a.server
	listener := a.listener
	a.restarting = true
	a.lifecycleMu.Unlock()
	if server != nil {
		// Restart-required changes take effect immediately. Close interrupts
		// in-flight data requests instead of waiting for a graceful drain.
		_ = server.Close()
	}
	if listener != nil {
		_ = listener.Close()
	}
	return nil
}

func (a *App) recoverRestartFailure(cause error) error {
	if cause == nil {
		cause = errors.New("self-restart failed")
	}
	active := a.manager.Snapshot().Config()
	newListener, bindErr := bind(active.Service.ListenAddr)
	if bindErr != nil {
		a.lifecycleMu.Lock()
		a.restarting = false
		a.listener = nil
		a.server = nil
		a.lifecycleMu.Unlock()
		a.signalResume()
		return fmt.Errorf("self-restart failed: %w; rebind old listener: %v", cause, bindErr)
	}
	a.lifecycleMu.Lock()
	if a.closed {
		a.restarting = false
		a.lifecycleMu.Unlock()
		_ = newListener.Close()
		a.signalResume()
		return fmt.Errorf("self-restart failed after app closed: %w", cause)
	}
	a.listener = newListener
	a.server = newHTTPServer(a.rootHandler())
	a.restarting = false
	logs := a.logs
	a.lifecycleMu.Unlock()
	if logs != nil {
		logs.error.Printf("self-restart failed; kept active configuration: %v", cause)
	}
	a.signalResume()
	return cause
}

func (a *App) preflight(current, next config.Config) error {
	if current.Service.ListenAddr != next.Service.ListenAddr {
		listener, err := bind(next.Service.ListenAddr)
		if err != nil {
			return err
		}
		if err := listener.Close(); err != nil {
			return err
		}
	}
	// The log limit itself is validated by config; opening a candidate logger is
	// the resource check for custom LogOpener implementations.
	if a.logs != nil {
		candidate, err := openLogger(a.logOpener, next.Service.LogMaxBytes)
		if candidate != nil {
			_ = candidate.Close()
		}
		return err
	}
	return nil
}

func bind(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

func acquireResources(opener LogOpener, cfg config.Config) (*logger, net.Listener, error) {
	logs, logErr := openLogger(opener, cfg.Service.LogMaxBytes)
	listener, listenErr := bind(cfg.Service.ListenAddr)
	if logErr == nil && listenErr == nil {
		return logs, listener, nil
	}
	closeResources(logs, listener)
	if logErr != nil {
		return nil, nil, fmt.Errorf("initialize logs: %w", logErr)
	}
	return nil, nil, fmt.Errorf("bind %s: %w", cfg.Service.ListenAddr, listenErr)
}

func closeResources(logs *logger, listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
	if logs != nil {
		_ = logs.Close()
	}
}
