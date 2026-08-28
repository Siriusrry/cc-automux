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

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	productversion "github.com/Siriusrry/cc-automux/internal/version"
)

const (
	apiPrefix          = "/api/v1"
	defaultMaxBodySize = int64(1 << 20)
)

var errProviderNotFound = errors.New("provider not found")

type Options struct {
	MaxBodyBytes int64
	Version      string
	Registry     *provider.Registry
}

type Handler struct {
	manager      *runtime.Manager
	maxBodyBytes int64
	version      string
	registry     provider.Registry
}

func New(manager *runtime.Manager) *Handler {
	return NewWithOptions(manager, Options{})
}

func NewWithOptions(manager *runtime.Manager, options Options) *Handler {
	registry := provider.DefaultRegistry()
	if manager != nil {
		registry = manager.Registry()
	}
	if options.Registry != nil && !options.Registry.Empty() {
		registry = *options.Registry
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
		manager:      manager,
		maxBodyBytes: maxBodyBytes,
		version:      version,
		registry:     registry,
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
	default:
		const providerPrefix = apiPrefix + "/providers/"
		if strings.HasPrefix(r.URL.Path, providerPrefix) && strings.Count(strings.TrimPrefix(r.URL.Path, providerPrefix), "/") == 0 && strings.TrimPrefix(r.URL.Path, providerPrefix) != "" {
			h.handleProvider(w, r, strings.TrimPrefix(r.URL.Path, providerPrefix))
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
	parts := strings.Fields(strings.TrimSpace(values[0]))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return false
	}
	key := h.manager.Snapshot().ManagementKey()
	// Hashing both values makes the constant-time comparison independent of
	// their original lengths while preserving exact key comparison.
	want := sha256.Sum256([]byte(key))
	got := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1 && parts[1] == key
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.manager.Snapshot().Config())
	case http.MethodPut:
		cfg, err := h.decodeConfig(w, r)
		if err != nil {
			return
		}
		result, err := h.manager.Apply(cfg)
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
		items = []provider.PatchMetadata{}
	}
	writeJSON(w, http.StatusOK, items)
}

type statusResponse struct {
	Product              string                `json:"product"`
	Version              string                `json:"version"`
	Revision             uint64                `json:"revision"`
	ListenAddr           string                `json:"listen_addr"`
	LogMaxBytes          int64                 `json:"log_max_bytes"`
	ProviderCount        int                   `json:"provider_count"`
	EnabledProviderCount int                   `json:"enabled_provider_count"`
	UptimeSeconds        int64                 `json:"uptime_seconds"`
	StartTime            string                `json:"start_time"`
	Restart              runtime.RestartStatus `json:"restart"`
	RestartInProgress    bool                  `json:"restart_in_progress"`
	Pending              bool                  `json:"pending"`
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	snapshot := h.manager.Snapshot()
	cfg := snapshot.Config()
	enabled := 0
	for _, item := range cfg.Providers {
		if item.Enabled && len(item.Models) > 0 {
			enabled++
		}
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
	writeJSON(w, http.StatusOK, statusResponse{
		Product:              productversion.ProductName,
		Version:              h.version,
		Revision:             snapshot.Revision(),
		ListenAddr:           cfg.Service.ListenAddr,
		LogMaxBytes:          cfg.Service.LogMaxBytes,
		ProviderCount:        len(cfg.Providers),
		EnabledProviderCount: enabled,
		UptimeSeconds:        uptime,
		StartTime:            started.UTC().Format(time.RFC3339),
		Restart:              restart,
		RestartInProgress:    restart.InProgress,
		Pending:              restart.Pending,
	})
}

func (h *Handler) decodeConfig(w http.ResponseWriter, r *http.Request) (config.Config, error) {
	data, err := readBody(w, r, h.maxBodyBytes)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Decode(data)
	if err != nil {
		h.writeDecodeError(w, err)
		return config.Config{}, err
	}
	return cfg, nil
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
	if errors.As(err, &validationErr) || errors.Is(err, provider.ErrUnknownPatch) || errors.Is(err, provider.ErrPatchNotImplemented) {
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
