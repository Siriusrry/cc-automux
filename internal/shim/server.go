package shim

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type proxyServer struct {
	configMu sync.Mutex
	state    atomic.Pointer[proxyState]
	// boundListenAddr is the listen address this process actually bound in Run.
	// restart_required is computed against this (not the last-applied config), so
	// a listen_addr change correctly reports restart-pending until the process is
	// actually restarted, even across later unrelated saves.
	boundListenAddr string
	// boundLogMaxBytes is the per-file log cap (bytes) this process actually bound
	// its log writers to in Run (configureLogging). restart_required is computed
	// against this exactly like boundListenAddr: configureLogging runs only at
	// startup (Main/Run), never on the hot-swap path, so a log_max_bytes change
	// stays restart-pending until the process restarts and rebinds the writers.
	// It is the normalized cap (always positive); it is 0 only when newProxyServer
	// got a nil runtime, which swapStateLocked treats as unbound (no restart claim).
	boundLogMaxBytes int64
	// startTime is when this proxyServer was constructed (newProxyServer). It is
	// the uptime origin reported by GET /admin/status.
	startTime time.Time
	// rewriteCount is the cumulative number of CLASSIFIER rewrites proxied with an
	// upstream response since start — auto-mode classifier fixes only (AnyRouter
	// cloak, CPA session isolation, gpt reassembly; every ctx.classifier request
	// that reaches writeTargetResponse). The non-classifier thinking.disabled→adaptive
	// promotion is logged but deliberately NOT counted. Surfaced by GET /admin/status.
	// Atomic: it is incremented from concurrent request goroutines and read by the
	// admin handler.
	rewriteCount atomic.Int64
}

type proxyState struct {
	configPath string
	runtime    *runtimeConfig
	table      *routingTable
}

func newProxyServer(cfg appConfig) *proxyServer {
	server := &proxyServer{startTime: time.Now()}
	if cfg.runtime != nil {
		// Record the address and log cap Run binds; restart_required is measured
		// against these (boundListenAddr / boundLogMaxBytes).
		server.boundListenAddr = cfg.runtime.ListenAddr
		server.boundLogMaxBytes = cfg.runtime.LogMaxBytes
	}
	server.state.Store(&proxyState{
		configPath: cfg.configPath,
		runtime:    cloneRuntimeConfig(cfg.runtime),
		table:      cfg.table,
	})
	return server
}
func (p *proxyServer) applyRuntimeConfig(next *runtimeConfig) (bool, error) {
	p.configMu.Lock()
	defer p.configMu.Unlock()

	compiled, err := compileRuntimeConfig(next)
	if err != nil {
		return false, err
	}
	return p.swapStateLocked(compiled)
}

// errListenAddrUnbindable marks a saveAndApplyRuntimeConfig failure raised by the
// pre-persist bind probe below: the requested listen_addr could not be bound
// (typically an occupied port). It is a CLIENT error — the user picked a bad port —
// so the POST /admin/config handler maps it to 400 (not 500), with nothing
// persisted, swapped, or restarted. It is wrapped with %w so errors.Is finds it.
var errListenAddrUnbindable = errors.New("cannot bind listen address")

func (p *proxyServer) saveAndApplyRuntimeConfig(next *runtimeConfig) (bool, error) {
	p.configMu.Lock()
	defer p.configMu.Unlock()

	state := p.currentState()
	if state.configPath == "" {
		return false, fmt.Errorf("runtime config path is empty")
	}
	compiled, err := compileRuntimeConfig(next)
	if err != nil {
		return false, err
	}
	// Before persisting a listen_addr CHANGE, prove the new address is bindable.
	// Without this, a Save onto an occupied/unbindable port would persist, hot-swap,
	// and re-exec (scheduleSelfRestart) into a process whose ListenAndServe fails —
	// Run returns, Main does os.Exit(1) — leaving the service dead on BOTH the old
	// port (its listener is already gone) and the new one, and crash-looping under
	// launchd KeepAlive on the same bad config. Probe only an ACTUAL change versus
	// the address THIS process bound (boundListenAddr): re-binding our own live port
	// would always fail with "address already in use", and an unbound server (tests
	// that never set boundListenAddr) has nothing to compare — so skip both, matching
	// how swapStateLocked computes listenChanged. A tiny TOCTOU window remains between
	// this probe and the re-exec's bind (the port could be grabbed in between); that
	// is acceptable — this closes the common case where the user typed a port already
	// in use. On failure return errListenAddrUnbindable so the POST handler answers
	// 400 with nothing persisted; on success close the listener immediately so the
	// subsequent re-exec can bind it.
	if p.boundListenAddr != "" && compiled.runtime.ListenAddr != p.boundListenAddr {
		ln, err := net.Listen("tcp", compiled.runtime.ListenAddr)
		if err != nil {
			return false, fmt.Errorf("%w %s: %v", errListenAddrUnbindable, compiled.runtime.ListenAddr, err)
		}
		_ = ln.Close()
	}
	// Persist before swapping so a write failure leaves the live config and the
	// on-disk config consistent (the old one).
	if err := writeRuntimeConfigFile(state.configPath, compiled.runtime); err != nil {
		return false, err
	}
	return p.swapStateLocked(compiled)
}

// swapStateLocked rebuilds the routing table from a compiled config and
// atomically publishes a new proxyState, preserving the resolved config path.
// It reports whether a restart is still pending — true when either the listen
// address or the log cap now differs from the value this process actually bound
// (boundListenAddr / boundLogMaxBytes). Both are startup-only bindings (the
// listener address; the log writers, set in Run via configureLogging), and the
// hot-swap path changes neither, so a change to either stays restart-pending
// until the process restarts. Measuring against the bound values (not the
// last-applied config) keeps the flag accurate across later unrelated saves.
// configMu must be held by the caller.
func (p *proxyServer) swapStateLocked(compiled *compiledRuntimeConfig) (bool, error) {
	state := p.currentState()
	table, err := buildRoutingTable(compiled)
	if err != nil {
		return false, err
	}
	listenChanged := p.boundListenAddr != "" && p.boundListenAddr != compiled.runtime.ListenAddr
	logCapChanged := p.boundLogMaxBytes != 0 && p.boundLogMaxBytes != compiled.runtime.LogMaxBytes
	restartRequired := listenChanged || logCapChanged
	p.state.Store(&proxyState{
		configPath: state.configPath,
		runtime:    cloneRuntimeConfig(compiled.runtime),
		table:      table,
	})
	return restartRequired, nil
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.handleLocalRequest(w, r) {
		return
	}
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
		return
	}

	state := p.currentState()
	route, strippedPath, ok := state.table.matchRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	rawBody, err := readRequestBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Global feature switch (runtimeConfig.enabled): when off, degrade to a plain
	// prefix-routed reverse proxy. This single early return runs before
	// resolveTarget/dispatchClassifier and the account-selection block below, so
	// none of account rotation, classifier rewrite, response reassembly, or
	// shim-owned auth applies — only prefix routing + verbatim forwarding + the
	// /any double-entrance failover (preserved inside attemptTarget).
	if !state.runtime.isEnabled() {
		p.servePassthrough(w, r, route, strippedPath, rawBody)
		return
	}

	target := p.resolveTarget(state, route, strippedPath, rawBody)

	// Shim-owned credential selection: pick the upstream key for this request from
	// the route's credentialProvider — an AnyRouter rotation account (sticky per
	// session) or CPA's single static key — and let the per-attempt header override
	// below swap it in. The credential is chosen once here; entrance failover replays
	// the same key across entrances, and AnyRouter account switching happens only
	// across requests via eviction + reassignment. Rotation only fills an UNSET key,
	// so a classifier global target's key (set by dispatchClassifier) is left
	// untouched. A request redirected to the global target (target.globalTarget)
	// never draws a provider key at all — with an empty target_key it forwards the
	// client's auth (applyAccountAuth no-op) and accountIdx stays -1, so its host is
	// never charged to an account. assign returning ok=false (an empty AnyRouter
	// pool) likewise leaves the client's own auth in place.
	accountIdx := -1
	if !target.globalTarget && target.key == "" && route.credentials != nil {
		if key, idx, ok := route.credentials.assign(sessionIDForRequest(r, rawBody)); ok {
			target.key = key
			accountIdx = idx
		}
	}

	prepared, err := p.prepareProxyRequest(target.profile, r, strippedPath, rawBody)
	if err != nil {
		http.Error(w, "failed to rewrite auto-mode classifier request", http.StatusBadRequest)
		errorf("%s rewrite error for %s %s: %v", target.routeName, r.Method, r.URL.RequestURI(), err)
		logRequestDiagnostic(target.routeName, r, strippedPath, rawBody, "classifier request rewrite failed", err)
		return
	}

	resp, servedIdx, ok, upstreamExhausted := p.attemptTarget(w, r, target, strippedPath, prepared.body, prepared.ctx)
	if !ok {
		// Penalize the credential only when every entrance was genuinely exhausted at
		// the transport/upstream level. A request-build error is the shim's fault,
		// not the account's, and a client cancel is not an entrance failure, so
		// neither counts against the account.
		if accountIdx >= 0 && upstreamExhausted && r.Context().Err() == nil {
			route.credentials.reportResult(accountIdx, 0, true)
		}
		return
	}
	defer resp.Body.Close()

	if accountIdx >= 0 {
		route.credentials.reportResult(accountIdx, resp.StatusCode, false)
	}

	p.recordServedEntrance(resp, target, servedIdx)
	p.writeTargetResponse(w, r, target, strippedPath, rawBody, resp, servedIdx, prepared.ctx)
}

// servePassthrough handles a request when the global feature switch is off. The
// shim becomes a plain reverse proxy: it forwards the body verbatim (no rewrite),
// the client's own credentials (target.key is empty, so applyAccountAuth is a
// no-op — no account rotation, no shim-owned key), and streams the response back
// without reassembly. It deliberately never touches the prefix's account pool nor
// runs dispatchClassifier. The /any double-entrance sticky failover still applies
// (attemptTarget), the listener stays loopback-only, and an exhausted target
// fails closed — attemptTarget passes the last real error/response back.
func (p *proxyServer) servePassthrough(w http.ResponseWriter, r *http.Request, route *proxyRoute, strippedPath string, rawBody []byte) {
	target := resolvedTarget{
		routeName: route.name,
		baseURLs:  route.upstreams,
		client:    route.client,
		profile:   passthroughStrategy{},
	}
	resp, servedIdx, ok, _ := p.attemptTarget(w, r, target, strippedPath, rawBody, requestContext{})
	if !ok {
		return
	}
	defer resp.Body.Close()
	p.recordServedEntrance(resp, target, servedIdx)
	p.writeTargetResponse(w, r, target, strippedPath, rawBody, resp, servedIdx, requestContext{})
}

func (p *proxyServer) handleLocalRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return true
	}
	if p.handleAdminRequest(w, r) {
		return true
	}
	return false
}

func (p *proxyServer) currentState() *proxyState {
	if state := p.state.Load(); state != nil {
		return state
	}
	return &proxyState{runtime: normalizeRuntimeConfig(nil), table: &routingTable{}}
}

func (p *proxyServer) currentRoutingTable() *routingTable {
	return p.currentState().table
}

// statusSnapshot assembles the GET /admin/status runtime view: process uptime,
// the cumulative rewrite count, and the AnyRouter route's live entrance + account
// health. The entrance and account pools are read through their own snapshot
// methods, each taking that pool's lock for a consistent point-in-time view and
// never holding it across I/O. Entrances/Accounts reflect the AnyRouter route
// specifically — the only route with sticky failover entrances and a rotation
// pool (CPA is a single static upstream with one key) — matching the /admin
// schematic's entrance cards and account pills.
func (p *proxyServer) statusSnapshot() adminStatus {
	status := adminStatus{
		UptimeSeconds: int64(time.Since(p.startTime) / time.Second),
		StartTime:     p.startTime.UTC().Format(time.RFC3339),
		Rewrites:      p.rewriteCount.Load(),
		Entrances:     []entranceStatus{},
		Accounts:      []accountStatus{},
	}
	table := p.currentRoutingTable()
	if table == nil {
		return status
	}
	for i := range table.routes {
		route := &table.routes[i]
		if route.prefix != anyRouterPrefix {
			continue
		}
		if route.upstreams != nil {
			status.Entrances = route.upstreams.snapshot()
		}
		// The AnyRouter route's credentialProvider is always the rotation accountPool;
		// only it has per-account health to report (a static CPA key has none).
		if pool, ok := route.credentials.(*accountPool); ok {
			if accts := pool.snapshot(); accts != nil {
				status.Accounts = accts
			}
		}
		break
	}
	return status
}
