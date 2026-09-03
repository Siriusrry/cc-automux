package management

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
	"github.com/Siriusrry/cc-automux/internal/health"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	productversion "github.com/Siriusrry/cc-automux/internal/version"
)

const (
	apiPrefix          = "/api/v1"
	defaultMaxBodySize = int64(1 << 20)
)

var errProviderNotFound = errors.New("provider not found")

type Options struct {
	MaxBodyBytes        int64
	Version             string
	Health              *health.Store
	Selector            scheduler.Selector
	Sync                func()
	ActiveRequests      func() int64
	AutoModeDiagnostics *automode.Diagnostics
	Harnesses           *harnessconfig.Manager
	Logs                *logstore.Reader
	LogStream           *logstore.Broker
}

type Handler struct {
	manager             *runtime.Manager
	maxBodyBytes        int64
	version             string
	registry            patch.Registry
	health              *health.Store
	selector            scheduler.Selector
	syncRuntime         func()
	activeRequests      func() int64
	autoModeDiagnostics *automode.Diagnostics
	harnesses           *harnessconfig.Manager
	logs                *logstore.Reader
	logStream           *logstore.Broker
}

func New(manager *runtime.Manager) *Handler {
	return NewWithOptions(manager, Options{})
}

func NewWithOptions(manager *runtime.Manager, options Options) *Handler {
	var registry patch.Registry
	if manager != nil {
		registry = manager.Registry()
	}
	maxBodyBytes := options.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodySize
	}
	version := options.Version
	if version == "" {
		version = productversion.Current()
	}
	return &Handler{
		manager:             manager,
		maxBodyBytes:        maxBodyBytes,
		version:             version,
		registry:            registry,
		health:              options.Health,
		selector:            options.Selector,
		syncRuntime:         options.Sync,
		activeRequests:      options.ActiveRequests,
		autoModeDiagnostics: options.AutoModeDiagnostics,
		harnesses:           options.Harnesses,
		logs:                options.Logs,
		logStream:           options.LogStream,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.manager == nil || (r.URL.Path != apiPrefix && !strings.HasPrefix(r.URL.Path, apiPrefix+"/")) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !h.authorized(r) {
		writeUnauthorized(w)
		return
	}

	switch r.URL.Path {
	case apiPrefix + "/config":
		h.handleConfig(w, r)
	case apiPrefix + "/providers":
		h.handleProviders(w, r)
	case apiPrefix + "/provider-patches":
		h.handlePatches(w, r)
	case apiPrefix + "/status":
		h.handleStatus(w, r)
	case apiPrefix + "/provider-health":
		h.handleProviderHealth(w, r)
	case apiPrefix + "/logs":
		h.handleLogs(w, r)
	case apiPrefix + "/logs/stream":
		h.handleLogStream(w, r)
	case apiPrefix + "/logs/record":
		h.handleLogRecord(w, r)
	case apiPrefix + "/harnesses":
		h.handleHarnessCollection(w, r)
	default:
		const providerPrefix = apiPrefix + "/providers/"
		if strings.HasPrefix(r.URL.Path, providerPrefix) && strings.Count(strings.TrimPrefix(r.URL.Path, providerPrefix), "/") == 0 && strings.TrimPrefix(r.URL.Path, providerPrefix) != "" {
			h.handleProvider(w, r, strings.TrimPrefix(r.URL.Path, providerPrefix))
			return
		}
		const harnessPrefix = apiPrefix + "/harnesses/"
		if strings.HasPrefix(r.URL.Path, harnessPrefix) {
			h.handleHarnessPath(w, r, strings.TrimPrefix(r.URL.Path, harnessPrefix))
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	token, ok := parseBearerAuthorization(values[0])
	if !ok {
		return false
	}
	key := h.manager.Snapshot().ManagementKey()
	// Hashing both values makes the constant-time comparison independent of
	// their original lengths while preserving exact key comparison.
	want := sha256.Sum256([]byte(key))
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1 && token == key
}

// parseBearerAuthorization accepts the RFC 7235 scheme SP token shape without
// treating tabs, repeated spaces, or surrounding whitespace as equivalent to
// the required single ASCII SP separator. The Bearer scheme name itself is
// case-insensitive, as required by the HTTP authentication grammar.
func parseBearerAuthorization(value string) (string, bool) {
	const scheme = "Bearer"
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) || value[len(scheme)] != ' ' {
		return "", false
	}
	token := value[len(scheme)+1:]
	if token == "" || strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", false
	}
	return token, true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// A GET is a complete resource response. It intentionally includes
		// server-owned state such as active_profile_id for display.
		writeJSON(w, http.StatusOK, h.manager.Snapshot().Config())
	case http.MethodPut:
		update, err := h.decodeClientConfigUpdate(w, r)
		if err != nil {
			return
		}
		result, err := h.manager.ApplyClientUpdate(update)
		if err != nil {
			h.writeApplyError(w, err)
			return
		}
		h.writeApplyResult(w, result)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (h *Handler) handleProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := h.manager.Snapshot().Config()
		if cfg.Providers == nil {
			cfg.Providers = []config.ProviderConfig{}
		}
		writeJSON(w, http.StatusOK, cfg.Providers)
	case http.MethodPost:
		input, err := h.decodeProvider(w, r)
		if err != nil {
			return
		}
		if input.ID == "" {
			input.ID, err = config.GenerateUUID()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "could not generate provider id")
				return
			}
		}
		if err := config.ValidateProviderForRequest(input); err != nil {
			h.writeSemanticError(w, err)
			return
		}
		_, err = h.manager.Update(func(cfg *config.Config) error {
			for _, existing := range cfg.Providers {
				if existing.ID == input.ID {
					return &config.ConflictError{Field: "id", Message: "provider id already exists"}
				}
				if existing.Name == input.Name {
					return &config.ConflictError{Field: "name", Message: "provider name already exists"}
				}
			}
			cfg.Providers = append(cfg.Providers, input)
			return nil
		})
		if err != nil {
			h.writeApplyError(w, err)
			return
		}
		w.Header().Set("Location", apiPrefix+"/providers/"+input.ID)
		writeJSON(w, http.StatusCreated, input)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (h *Handler) handleProvider(w http.ResponseWriter, r *http.Request, id string) {
	cfg := h.manager.Snapshot().Config()
	index := -1
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			index = i
			break
		}
	}

	switch r.Method {
	case http.MethodGet:
		if index < 0 {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		writeJSON(w, http.StatusOK, cfg.Providers[index])
	case http.MethodPut:
		if index < 0 {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		input, err := h.decodeProvider(w, r)
		if err != nil {
			return
		}
		if input.ID == "" {
			input.ID = id
		} else if input.ID != id {
			writeError(w, http.StatusConflict, "id_immutable", "provider id cannot be changed")
			return
		}
		if err := config.ValidateProviderForRequest(input); err != nil {
			h.writeSemanticError(w, err)
			return
		}
		_, err = h.manager.Update(func(next *config.Config) error {
			for _, existing := range next.Providers {
				if existing.ID != id && existing.ID == input.ID {
					return &config.ConflictError{Field: "id", Message: "provider id already exists"}
				}
				if existing.ID != id && existing.Name == input.Name {
					return &config.ConflictError{Field: "name", Message: "provider name already exists"}
				}
			}
			// Re-resolve the index under the serialized update in case a previous
			// request changed the provider set after the initial read.
			for i := range next.Providers {
				if next.Providers[i].ID == id {
					next.Providers[i] = input
					return nil
				}
			}
			return errProviderNotFound
		})
		if err != nil {
			h.writeApplyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, input)
	case http.MethodDelete:
		if index < 0 {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		_, err := h.manager.Update(func(next *config.Config) error {
			for i := range next.Providers {
				if next.Providers[i].ID == id {
					next.Providers = append(next.Providers[:i], next.Providers[i+1:]...)
					return nil
				}
			}
			return errProviderNotFound
		})
		if err != nil {
			h.writeApplyError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (h *Handler) handlePatches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	items := h.registry.List()
	if items == nil {
		items = []patch.PatchMetadata{}
	}
	writeJSON(w, http.StatusOK, items)
}

type statusResponse struct {
	Product                     string                 `json:"product"`
	Version                     string                 `json:"version"`
	Revision                    uint64                 `json:"revision"`
	ListenAddr                  string                 `json:"listen_addr"`
	LogMaxBytes                 int64                  `json:"log_max_bytes"`
	GatewayConfigured           bool                   `json:"gateway_configured"`
	ProviderCount               int                    `json:"provider_count"`
	EnabledProviderCount        int                    `json:"enabled_provider_count"`
	ActiveProviderCount         int                    `json:"active_provider_count"`
	InactiveProviderCount       int                    `json:"inactive_provider_count"`
	GlobalHealth                health.StateCounts     `json:"global_health"`
	ChannelHealth               health.StateCounts     `json:"channel_health"`
	HealthDisabledProviderCount int                    `json:"health_disabled_provider_count"`
	ActiveDataRequests          int64                  `json:"active_data_requests"`
	StickyAssignmentCount       int                    `json:"sticky_assignment_count"`
	UptimeSeconds               int64                  `json:"uptime_seconds"`
	StartTime                   string                 `json:"start_time"`
	Restart                     runtime.RestartStatus  `json:"restart"`
	RestartInProgress           bool                   `json:"restart_in_progress"`
	Pending                     bool                   `json:"pending"`
	AutoMode                    autoModeStatusResponse `json:"auto_mode"`
}

type autoModeStatusResponse struct {
	Mode                    string                    `json:"mode"`
	Model                   string                    `json:"model"`
	FixedProviderConfigured bool                      `json:"fixed_provider_configured"`
	FixedProviderProtocol   string                    `json:"fixed_provider_protocol"`
	FixedTargetLastCall     *automode.FixedTargetCall `json:"fixed_target_last_call"`
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	snapshot := h.manager.Snapshot()
	if h.syncRuntime != nil {
		h.syncRuntime()
		snapshot = h.manager.Snapshot()
	}
	cfg := snapshot.Config()
	enabled := 0
	active := 0
	for _, item := range cfg.Providers {
		if item.Enabled && len(item.Models) > 0 {
			enabled++
		}
	}
	for _, item := range snapshot.Providers() {
		if scheduler.StaticAvailabilityOf(item) == scheduler.StaticActive {
			active++
		}
	}
	var aggregate health.Aggregate
	if h.health != nil {
		aggregate = h.health.Aggregate()
	}
	activeRequests := int64(0)
	if h.activeRequests != nil {
		activeRequests = h.activeRequests()
	}
	stickyCount := 0
	if h.selector != nil {
		stickyCount = h.selector.ActiveAssignmentCount()
	}
	started := h.manager.StartedAt()
	uptime := int64(0)
	if !started.IsZero() {
		uptime = int64(time.Since(started) / time.Second)
		if uptime < 0 {
			uptime = 0
		}
	}
	restart := h.manager.RestartStatus()
	autoStatus := autoModeStatusResponse{
		Mode:                    cfg.AutoMode.Mode,
		Model:                   cfg.AutoMode.Model,
		FixedProviderConfigured: cfg.AutoMode.FixedProvider != nil,
	}
	if cfg.AutoMode.FixedProvider != nil {
		autoStatus.FixedProviderProtocol = cfg.AutoMode.FixedProvider.Protocol
	}
	if h.autoModeDiagnostics != nil {
		autoStatus.FixedTargetLastCall = h.autoModeDiagnostics.Snapshot()
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Product:                     productversion.ProductName,
		Version:                     h.version,
		Revision:                    snapshot.Revision(),
		ListenAddr:                  cfg.Service.ListenAddr,
		LogMaxBytes:                 cfg.Service.LogMaxBytes,
		GatewayConfigured:           cfg.Auth.GatewayKey != "",
		ProviderCount:               len(cfg.Providers),
		EnabledProviderCount:        enabled,
		ActiveProviderCount:         active,
		InactiveProviderCount:       len(cfg.Providers) - active,
		GlobalHealth:                aggregate.Global,
		ChannelHealth:               aggregate.Channels,
		HealthDisabledProviderCount: aggregate.HealthDisabledProviderCount,
		ActiveDataRequests:          activeRequests,
		StickyAssignmentCount:       stickyCount,
		UptimeSeconds:               uptime,
		StartTime:                   started.UTC().Format(time.RFC3339),
		Restart:                     restart,
		RestartInProgress:           restart.InProgress,
		Pending:                     restart.Pending,
		AutoMode:                    autoStatus,
	})
}

type healthDiagnosticResponse struct {
	State               string     `json:"state"`
	BackoffLevel        int        `json:"backoff_level"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	ObservedFailures    uint64     `json:"observed_failures"`
	CooldownUntil       *time.Time `json:"cooldown_until"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	LastFailureAt       *time.Time `json:"last_failure_at"`
	ProbeInFlight       bool       `json:"probe_in_flight"`
	LastUpstreamURL     string     `json:"last_upstream_url"`
	LastError           string     `json:"last_error"`
	LastSessionID       string     `json:"last_session_id"`
}

type channelHealthResponse struct {
	Model               string     `json:"model"`
	RequestType         string     `json:"request_type"`
	State               string     `json:"state"`
	BackoffLevel        int        `json:"backoff_level"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	ObservedFailures    uint64     `json:"observed_failures"`
	CooldownUntil       *time.Time `json:"cooldown_until"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	LastFailureAt       *time.Time `json:"last_failure_at"`
	ProbeInFlight       bool       `json:"probe_in_flight"`
	LastUpstreamURL     string     `json:"last_upstream_url"`
	LastError           string     `json:"last_error"`
	LastSessionID       string     `json:"last_session_id"`
}

type sessionHealthResponse struct {
	SessionID   string    `json:"session_id"`
	Model       string    `json:"model"`
	RequestType string    `json:"request_type"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
}

type providerHealthResponse struct {
	ID                 string                       `json:"id"`
	Name               string                       `json:"name"`
	BaseURL            string                       `json:"base_url"`
	APIKey             string                       `json:"api_key"`
	Priority           int64                        `json:"priority"`
	Enabled            bool                         `json:"enabled"`
	Models             []string                     `json:"models"`
	UseXAPIKey         bool                         `json:"use_x_api_key"`
	TLS                config.TLSConfig             `json:"tls"`
	Patches            []string                     `json:"patches"`
	DisableHealth      bool                         `json:"disable_health"`
	StaticAvailability scheduler.StaticAvailability `json:"static_availability"`
	GlobalHealth       healthDiagnosticResponse     `json:"global_health"`
	Channels           []channelHealthResponse      `json:"channels"`
	Sessions           []sessionHealthResponse      `json:"sessions"`
	ActiveSessionCount int                          `json:"active_session_count"`
}

type providerHealthListResponse struct {
	Revision    uint64                   `json:"revision"`
	GeneratedAt time.Time                `json:"generated_at"`
	Providers   []providerHealthResponse `json:"providers"`
}

func (h *Handler) handleProviderHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if h.syncRuntime != nil {
		h.syncRuntime()
	}
	snapshot := h.manager.Snapshot()
	providers := snapshot.Providers()
	generatedAt := time.Now().UTC()
	healthByProvider := make(map[string]health.ProviderSnapshot, len(providers))
	if h.health != nil {
		healthSnapshot := h.health.Snapshot()
		generatedAt = healthSnapshot.GeneratedAt.UTC()
		for _, item := range healthSnapshot.Providers {
			healthByProvider[item.ProviderID] = item
		}
	}
	result := providerHealthListResponse{
		Revision:    snapshot.Revision(),
		GeneratedAt: generatedAt,
		Providers:   make([]providerHealthResponse, 0, len(providers)),
	}
	for _, item := range providers {
		cfg := item.Config()
		providerHealth := providerHealthResponse{
			ID:                 cfg.ID,
			Name:               cfg.Name,
			BaseURL:            cfg.BaseURL,
			APIKey:             cfg.APIKey,
			Priority:           cfg.Priority,
			Enabled:            cfg.Enabled,
			Models:             append([]string(nil), cfg.Models...),
			UseXAPIKey:         cfg.UseXAPIKey,
			TLS:                cfg.TLS,
			Patches:            append([]string(nil), cfg.Patches...),
			DisableHealth:      cfg.DisableHealth,
			StaticAvailability: scheduler.StaticAvailabilityOf(item),
			Channels:           []channelHealthResponse{},
			Sessions:           []sessionHealthResponse{},
		}
		healthSnapshot, ok := healthByProvider[item.ID]
		if ok {
			providerHealth.GlobalHealth = globalHealthResponse(healthSnapshot.Global)
			providerHealth.Channels = make([]channelHealthResponse, 0, len(healthSnapshot.Channels))
			for _, channel := range healthSnapshot.Channels {
				diagnostic := channelHealthDiagnosticResponse(channel)
				providerHealth.Channels = append(providerHealth.Channels, channelHealthResponse{
					Model:               channel.Model,
					RequestType:         string(channel.RequestType),
					State:               diagnostic.State,
					BackoffLevel:        diagnostic.BackoffLevel,
					ConsecutiveFailures: diagnostic.ConsecutiveFailures,
					ObservedFailures:    diagnostic.ObservedFailures,
					CooldownUntil:       diagnostic.CooldownUntil,
					LastSuccessAt:       diagnostic.LastSuccessAt,
					LastFailureAt:       diagnostic.LastFailureAt,
					ProbeInFlight:       diagnostic.ProbeInFlight,
					LastUpstreamURL:     diagnostic.LastUpstreamURL,
					LastError:           diagnostic.LastError,
					LastSessionID:       diagnostic.LastSessionID,
				})
			}
		}
		if h.selector != nil {
			assignments := h.selector.Assignments(item.ID)
			providerHealth.Sessions = make([]sessionHealthResponse, 0, len(assignments))
			for _, assignment := range assignments {
				providerHealth.Sessions = append(providerHealth.Sessions, sessionHealthResponse{
					SessionID:   assignment.Key.SessionID,
					Model:       assignment.Key.Model,
					RequestType: string(assignment.Key.RequestType),
					CreatedAt:   assignment.CreatedAt.UTC(),
					LastUsedAt:  assignment.LastUsedAt.UTC(),
				})
			}
		}
		providerHealth.ActiveSessionCount = len(providerHealth.Sessions)
		result.Providers = append(result.Providers, providerHealth)
	}
	writeJSON(w, http.StatusOK, result)
}

func globalHealthResponse(item health.GlobalSnapshot) healthDiagnosticResponse {
	result := diagnosticResponse(item.Diagnostic)
	result.State = string(item.State)
	return result
}

func channelHealthDiagnosticResponse(item health.ChannelSnapshot) healthDiagnosticResponse {
	result := diagnosticResponse(item.Diagnostic)
	result.State = string(item.State)
	return result
}

func diagnosticResponse(item health.Diagnostic) healthDiagnosticResponse {
	return healthDiagnosticResponse{
		BackoffLevel:        item.BackoffLevel,
		ConsecutiveFailures: item.ConsecutiveFailures,
		ObservedFailures:    item.ObservedFailures,
		CooldownUntil:       item.CooldownUntil,
		LastSuccessAt:       item.LastSuccessAt,
		LastFailureAt:       item.LastFailureAt,
		ProbeInFlight:       item.ProbeInFlight,
		LastUpstreamURL:     item.LastUpstreamURL,
		LastError:           item.LastError,
		LastSessionID:       item.LastSessionID,
	}
}

func (h *Handler) decodeClientConfigUpdate(w http.ResponseWriter, r *http.Request) (config.ClientConfigUpdate, error) {
	data, err := readBody(w, r, h.maxBodyBytes)
	if err != nil {
		return config.ClientConfigUpdate{}, err
	}
	update, err := config.DecodeClientUpdate(data)
	if err != nil {
		h.writeDecodeError(w, err)
		return config.ClientConfigUpdate{}, err
	}
	return update, nil
}

func (h *Handler) decodeProvider(w http.ResponseWriter, r *http.Request) (config.ProviderConfig, error) {
	data, err := readBody(w, r, h.maxBodyBytes)
	if err != nil {
		return config.ProviderConfig{}, err
	}
	input, err := config.DecodeProvider(data)
	if err != nil {
		h.writeDecodeError(w, err)
		return config.ProviderConfig{}, err
	}
	return input, nil
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is required")
		return nil, errors.New("request body is nil")
	}
	defer r.Body.Close()
	reader := http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return nil, err
		}
		writeError(w, http.StatusBadRequest, "invalid_body", "could not read request body")
		return nil, err
	}
	return data, nil
}

func (h *Handler) writeDecodeError(w http.ResponseWriter, err error) {
	var syntax *config.SyntaxError
	if errors.As(err, &syntax) {
		writeError(w, http.StatusBadRequest, "invalid_json", syntax.Error())
		return
	}
	if errors.Is(err, config.ErrActiveProfileReadOnly) {
		writeError(w, http.StatusConflict, "active_profile_read_only", "active profile state is server-managed")
		return
	}
	h.writeSemanticError(w, err)
}

func (h *Handler) writeSemanticError(w http.ResponseWriter, err error) {
	var conflictErr *config.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, http.StatusConflict, "conflict", conflictErr.Error())
		return
	}
	writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
}

func (h *Handler) writeApplyError(w http.ResponseWriter, err error) {
	if errors.Is(err, runtime.ErrRestartInProgress) {
		writeError(w, http.StatusConflict, "restart_in_progress", "restart_in_progress")
		return
	}
	if errors.Is(err, errProviderNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	var conflictErr *config.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, http.StatusConflict, "conflict", conflictErr.Error())
		return
	}
	var validationErr *config.ValidationError
	if errors.As(err, &validationErr) || errors.Is(err, patch.ErrUnknownPatch) || errors.Is(err, patch.ErrDuplicatePatch) || errors.Is(err, patch.ErrPatchConflict) || errors.Is(err, patch.ErrPatchNotApplicable) || errors.Is(err, patch.ErrInvalidDefinition) {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	var preflightErr *runtime.PreflightError
	if errors.As(err, &preflightErr) {
		writeError(w, http.StatusConflict, "resource_conflict", preflightErr.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "configuration could not be applied")
}

func (h *Handler) writeApplyResult(w http.ResponseWriter, result runtime.ApplyResult) {
	status := http.StatusOK
	var startRestart func()
	if result.RestartRequired {
		status = http.StatusAccepted
		// Reserve the transaction before writing the response so concurrent
		// submissions receive 409. The actual starter is invoked only after the
		// 202 body has been written, satisfying the self-reexec ordering contract.
		var err error
		startRestart, err = h.manager.PrepareRestart()
		if err != nil {
			h.writeApplyError(w, err)
			return
		}
	}
	writeJSON(w, status, result)
	if startRestart != nil {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startRestart()
	}
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
