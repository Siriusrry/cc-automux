package shim

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed admin_assets/*
var adminAssets embed.FS

// maxAdminConfigBytes caps the POST /admin/config body. The runtime config is
// tiny; this only guards against a runaway client on the loopback desk.
const maxAdminConfigBytes int64 = 1 << 20

// handleAdminRequest serves the 127.0.0.1 web config desk. It follows
// handleLocalRequest's contract: it returns true once it has fully handled the
// request, so the caller skips the proxy path entirely. It is mounted before
// matchRoute, so the /admin namespace never reaches the AnyRouter/CPA routes.
// No auth: the listener is loopback-only (validateListenAddr keeps 127.0.0.1).
func (p *proxyServer) handleAdminRequest(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/admin/config":
		p.handleAdminConfig(w, r)
		return true
	case "/admin/status":
		p.handleAdminStatus(w, r)
		return true
	case "/admin":
		serveAdminAsset(w, r, "admin_assets/admin.html", "text/html; charset=utf-8")
		return true
	case "/admin/logs":
		serveAdminAsset(w, r, "admin_assets/logs.html", "text/html; charset=utf-8")
		return true
	case "/admin/logs/tail":
		p.handleAdminLogsTail(w, r)
		return true
	case "/admin/assets/product-mascot.png":
		serveAdminAsset(w, r, "admin_assets/product-mascot.png", "image/png")
		return true
	case "/admin/assets/shared.css":
		serveAdminAsset(w, r, "admin_assets/shared.css", "text/css; charset=utf-8")
		return true
	case "/admin/assets/admin.css":
		serveAdminAsset(w, r, "admin_assets/admin.css", "text/css; charset=utf-8")
		return true
	case "/admin/assets/logs.css":
		serveAdminAsset(w, r, "admin_assets/logs.css", "text/css; charset=utf-8")
		return true
	case "/admin/assets/admin.js":
		serveAdminAsset(w, r, "admin_assets/admin.js", "text/javascript; charset=utf-8")
		return true
	case "/admin/assets/logs.js":
		serveAdminAsset(w, r, "admin_assets/logs.js", "text/javascript; charset=utf-8")
		return true
	}
	return false
}

func serveAdminAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := adminAssets.ReadFile(name)
	if err != nil {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// handleAdminConfig exposes the live runtime config for the desk. GET returns it
// verbatim (no key masking because the desk is loopback-only). POST validates,
// then persists and swaps atomically; an invalid body is rejected without
// touching the live or on-disk config.
func (p *proxyServer) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminJSON(w, http.StatusOK, p.currentState().runtime)
	case http.MethodPost:
		var cfg runtimeConfig
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminConfigBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Reject trailing content after the JSON object ({…}{junk}), matching
		// readRuntimeConfig, so a malformed POST never applies partially.
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeAdminError(w, http.StatusBadRequest, "unexpected trailing JSON content")
			return
		}
		if err := validateRuntimeConfig(&cfg); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		restartRequired, err := p.saveAndApplyRuntimeConfig(&cfg)
		if err != nil {
			// A failed bind probe (the requested listen_addr is occupied/unbindable)
			// is the user's fault, so surface it as 400 — nothing was persisted,
			// swapped, or restarted. Any other save failure (e.g. a config-file write
			// error) is the shim's fault and stays a 500.
			status := http.StatusInternalServerError
			if errors.Is(err, errListenAddrUnbindable) {
				status = http.StatusBadRequest
			}
			writeAdminError(w, status, err.Error())
			return
		}
		// Echo the just-applied listen address (read from the swapped-in live config)
		// so the desk can tell a same-port restart (a log-size change) from a port
		// move, and report restarting == restart_required: when a restart is needed
		// the shim now re-execs itself (scheduleSelfRestart below) instead of leaving
		// the new listen/log values pending a manual stop/start.
		writeAdminJSON(w, http.StatusOK, adminConfigPostResponse{
			RestartRequired: restartRequired,
			Restarting:      restartRequired,
			ListenAddr:      p.currentState().runtime.ListenAddr,
		})
		if restartRequired {
			// The 200 must reach the browser before the process image is replaced, so
			// flush what we just wrote and hand off to scheduleSelfRestart, whose
			// re-exec runs only after this handler returns (net/http finalizes the
			// response on return) — see its comment.
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			p.scheduleSelfRestart()
		}
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminConfigPostResponse is the POST /admin/config success body. restart_required
// stays for back-compat (a listen_addr or log_max_bytes change still cannot be
// hot-swapped — both bind only at startup); restarting tells the desk the shim is
// re-exec'ing itself to apply that change (today always == restart_required);
// listen_addr echoes the just-applied address so the desk can distinguish a
// same-port restart from a port move (only the latter needs the user to reopen the
// desk on the new port, since a browser cannot follow a port change).
type adminConfigPostResponse struct {
	RestartRequired bool   `json:"restart_required"`
	Restarting      bool   `json:"restarting"`
	ListenAddr      string `json:"listen_addr"`
}

// adminStatus is the read-only runtime contract consumed by the dynamic admin UI.
// Field names are stable; runtime configuration remains on GET /admin/config.
type adminStatus struct {
	// Product and Version identify the running CC AutoMux build. They come from
	// the same embedded source used by the CLI and all current UI surfaces.
	Product string `json:"product"`
	Version string `json:"version"`
	// UptimeSeconds is whole seconds since this process built its proxyServer.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// StartTime is the proxyServer construction time as RFC3339 UTC, for an
	// absolute "running since" display.
	StartTime string `json:"start_time"`
	// Rewrites is the cumulative count of CLASSIFIER rewrites proxied with an
	// upstream response since start: auto-mode classifier fixes only (cloak / CPA
	// session isolation / gpt reassembly). The non-classifier thinking.disabled→
	// adaptive promotion is NOT counted here.
	Rewrites int64 `json:"rewrites"`
	// Entrances is the AnyRouter entrance pool — each entrance with its host/URL
	// and whether it is the current (active) one; the others are standby. Empty
	// only if the AnyRouter route has no entrances.
	Entrances []entranceStatus `json:"entrances"`
	// Accounts is the AnyRouter rotation pool's relative-health snapshot, empty when no
	// rotation accounts are configured.
	Accounts []accountStatus `json:"accounts"`
}

// handleAdminStatus serves the read-only runtime snapshot (GET only). Like the
// rest of the desk it is loopback-only with no auth. It never mutates state, so
// it has no POST form.
func (p *proxyServer) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeAdminJSON(w, http.StatusOK, p.statusSnapshot())
}

// adminLogsTail is the GET /admin/logs/tail response: the buffered log lines newer
// than the requested cursor (Entries, oldest-first) and Head, the largest seq
// currently buffered. A poller re-requests with after=Head to receive only newer
// lines. Entries always serializes as a JSON array, never null.
type adminLogsTail struct {
	Entries []logEntry `json:"entries"`
	Head    int64      `json:"head"`
}

// handleAdminLogsTail serves the in-process log ring as an incremental tail. Like
// the rest of the desk it is GET-only, loopback, no auth. The ?after=<seq> query
// is the cursor: it returns only entries whose seq exceeds it (after omitted or
// empty ⇒ 0 ⇒ everything currently buffered). It reads already-emitted log lines
// from memory and never touches request/response bodies, so it adds no body dump.
func (p *proxyServer) handleAdminLogsTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var after int64
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "after must be an integer")
			return
		}
		after = parsed
	}
	entries, head := logBuffer.after(after)
	writeAdminJSON(w, http.StatusOK, adminLogsTail{Entries: entries, Head: head})
}

func writeAdminJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeAdminJSON(w, status, map[string]string{"error": message})
}
