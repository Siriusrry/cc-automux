package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/gateway"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
	"github.com/Siriusrry/cc-automux/internal/health"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
	"github.com/Siriusrry/cc-automux/internal/management"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/protocol"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
	"github.com/Siriusrry/cc-automux/internal/webui"
)

type Options struct {
	ConfigPath       string
	LogDir           string
	LogOpener        LogOpener
	Registry         *patch.Registry
	Restart          func() error
	Exec             func() error
	RestartDelay     time.Duration
	Preflight        func(current, next config.Config) error
	Now              func() time.Time
	HarnessRegistry  harnessconfig.AdapterRegistry
	HarnessFileStore *harnessconfig.FileStore
	HarnessHomeDir   harnessconfig.HomeResolver
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
	harnesses      *harnessconfig.Manager
	logBroker      *logstore.Broker
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
	// The composition root creates the process-local runtime resources exactly
	// once. Every Runtime Snapshot, Provider compilation and Patch Instance
	// receives the same registry/service handles; lower layers never allocate a
	// replacement AliasStore during hot updates or request execution.
	var registry patch.Registry
	if options.Registry != nil && !options.Registry.Empty() {
		// An injected registry is already a caller-owned runtime resource. It must
		// carry its own shared services; do not supplement it with another store.
		registry = *options.Registry
	} else {
		aliasStore := patch.NewAliasStore()
		registry, err = patch.NewDefaultRegistry(patch.Services{AliasStore: aliasStore})
		if err != nil {
			return nil, err
		}
	}
	runtimeContext, contextErr := provider.NewRuntimeContext(registry)
	if contextErr != nil {
		return nil, contextErr
	}
	classifierDetector := automode.NewClassifierDetector()
	detectorRegistry, detectorErr := traffic.NewRegistry(classifierDetector)
	if detectorErr != nil {
		return nil, fmt.Errorf("initialize detector registry: %w", detectorErr)
	}
	scanRequirements, scanErr := runtime.NewScanRequirements(detectorRegistry.RequiredPaths(), detectorRegistry.RequiredRawMarkers())
	if scanErr != nil {
		return nil, fmt.Errorf("initialize request scan requirements: %w", scanErr)
	}
	startup, err := runtime.LoadStartup(store, runtimeContext)
	if err != nil {
		return nil, err
	}

	logDir := options.LogDir
	if logDir == "" {
		logDir, err = config.LogDir()
		if err != nil {
			return nil, err
		}
	}
	logOpener := options.LogOpener
	if logOpener == nil {
		logOpener = logOpenerForDir(logDir)
	}
	logBroker := logstore.NewBroker()

	// Acquire the complete candidate before promotion. If pending resources fail,
	// rollback selects the old active configuration and resources are acquired
	// again from that immutable candidate.
	cfg := startup.Config()
	logs, listener, resourceErr := acquireResources(logOpener, logBroker, cfg)
	if resourceErr != nil && startup.FromPending() {
		_ = startup.Rollback(resourceErr)
		cfg = startup.Config()
		logs, listener, resourceErr = acquireResources(logOpener, logBroker, cfg)
	}
	if resourceErr != nil {
		return nil, resourceErr
	}

	policy := scheduler.DefaultPolicy()
	managerOptions := runtime.Options{
		RuntimeContext:          runtimeContext,
		ScanRequirements:        scanRequirements,
		AttemptPolicy:           scheduler.DefaultAttemptPolicy(),
		ClassifierAttemptPolicy: scheduler.DefaultClassifierAttemptPolicy(),
		RestartDelay:            options.RestartDelay,
		Now:                     options.Now,
		Preflight:               options.Preflight,
	}
	if managerOptions.RestartDelay == 0 {
		managerOptions.RestartDelay = 250 * time.Millisecond
	}
	app := &App{
		configPath: path,
		listener:   listener,
		logs:       logs,
		logOpener:  logOpener,
		logBroker:  logBroker,
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
		logs, listener, resourceErr = acquireResources(logOpener, logBroker, cfg)
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
			logs, listener, resourceErr = acquireResources(logOpener, logBroker, cfg)
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
		app.logServiceEvent(slog.LevelWarn, "pending_rejected", slog.String("error", startup.Warning().Error()))
	}
	app.manager = manager
	var harnessRegistry harnessconfig.AdapterRegistry = options.HarnessRegistry
	if harnessRegistry == nil {
		adapter := harnessconfig.NewClaudeCodeAdapter(harnessconfig.ClaudeCodeAdapterOptions{HomeDir: options.HarnessHomeDir})
		harnessRegistry, err = harnessconfig.NewRegistry(adapter)
		if err != nil {
			closeResources(app.logs, app.listener)
			return nil, fmt.Errorf("initialize harness registry: %w", err)
		}
	}
	harnessManager, harnessErr := harnessconfig.NewManager(manager, harnessRegistry, harnessconfig.ManagerOptions{
		Registry:       harnessRegistry,
		FileStore:      options.HarnessFileStore,
		ProtectedPaths: []string{path, config.PendingPath(path)},
	})
	if harnessErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("initialize harness manager: %w", harnessErr)
	}
	// Reconcile before constructing the HTTP server. A persistence failure is
	// represented as state_error by the harness manager and does not stop the
	// loopback data plane; an unexpected manager error is an initialization
	// failure.
	if _, harnessErr = harnessManager.ReconcileAll(); harnessErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("reconcile harness configuration: %w", harnessErr)
	}
	app.harnesses = harnessManager
	fixedDiagnostics := automode.NewDiagnostics()
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
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
	flowRegistry, flowErr := flow.NewRegistry(flow.NewNormalPlanner(), automode.NewClassifierPlanner())
	if flowErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("initialize flow registry: %w", flowErr)
	}
	app.gateway = gateway.NewWithOptions(app.snapshotForGateway, selector, gateway.Options{
		Recorder:         gateway.EventRecorderFunc(app.recordGatewayEvent),
		DetectorRegistry: detectorRegistry,
		FlowDispatcher:   flow.NewDispatcher(flowRegistry),
		ProtocolAdapters: protocol.EmptyRegistry(),
		FixedDiagnostics: fixedDiagnostics,
	})
	logSource := app.logs.source
	if logSource == nil {
		logSource, err = logstore.NewDirectorySource(logDir)
		if err != nil {
			closeResources(app.logs, app.listener)
			return nil, fmt.Errorf("initialize log reader source: %w", err)
		}
	}
	logReader, logReaderErr := logstore.NewReader(logSource)
	if logReaderErr != nil {
		closeResources(app.logs, app.listener)
		return nil, fmt.Errorf("initialize log reader: %w", logReaderErr)
	}
	app.management = management.NewWithOptions(manager, management.Options{
		Health:              healthStore,
		Selector:            selector,
		Sync:                app.syncRuntime,
		ActiveRequests:      app.activeDataRequests,
		AutoModeDiagnostics: fixedDiagnostics,
		Harnesses:           harnessManager,
		Logs:                logReader,
		LogStream:           logBroker,
		// Read through the App so a restart that swaps the logger keeps the
		// reported health pointing at the live one.
		LogHealth: app.loggingHealth,
	})
	app.syncRuntime()
	app.server = newHTTPServer(app.rootHandler())
	app.logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", cfg.Service.ListenAddr))
	return app, nil
}

func Run() error {
	if err := waitForRestartParent(); err != nil {
		return err
	}
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

// HarnessManager returns the process-owned harness configuration service.
func (a *App) HarnessManager() *harnessconfig.Manager {
	if a == nil {
		return nil
	}
	return a.harnesses
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
	logBroker := a.logBroker
	if a.done != nil {
		close(a.done)
	}
	a.lifecycleMu.Unlock()
	var result error
	if logBroker != nil {
		logBroker.Close()
	}
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
	console := webui.New()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil || r == nil || r.URL == nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/" || r.URL.Path == webui.Path || strings.HasPrefix(r.URL.Path, webui.Path+"/"):
			console.ServeHTTP(w, r)
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
	// Capture and reconcile under one lock. A second Manager read (or another
	// request racing a newer revision) must not leave Gateway using a snapshot
	// that the Scheduler/Health pair has not seen yet.
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	snapshot := a.manager.Snapshot()
	a.syncRuntimeSnapshotLocked(snapshot)
	return snapshot
}

func (a *App) syncRuntime() {
	if a == nil || a.manager == nil || a.selector == nil {
		return
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.syncRuntimeSnapshotLocked(a.manager.Snapshot())
}

func (a *App) syncRuntimeSnapshot(snapshot scheduler.Snapshot) {
	if a == nil || a.selector == nil {
		return
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.syncRuntimeSnapshotLocked(snapshot)
}

func (a *App) syncRuntimeSnapshotLocked(snapshot scheduler.Snapshot) {
	if a == nil || a.selector == nil {
		return
	}
	if snapshot == nil {
		return
	}
	if snapshot.Revision() <= a.syncedRevision {
		return
	}
	a.selector.Reconcile(snapshot)
	a.syncedRevision = snapshot.Revision()
	if a.gateway != nil {
		a.gateway.SetRuntimeSnapshot(snapshot)
	}
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
	a.lifecycleMu.Unlock()
	a.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", cause.Error()))
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

func acquireResources(opener LogOpener, broker *logstore.Broker, cfg config.Config) (*logger, net.Listener, error) {
	logs, logErr := openLoggerWithBroker(opener, cfg.Service.LogMaxBytes, broker)
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
