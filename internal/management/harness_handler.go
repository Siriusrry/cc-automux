package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
	"github.com/Siriusrry/cc-automux/internal/runtime"
)

func (h *Handler) handleHarnessCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if h.harnesses == nil {
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
		return
	}
	writeJSON(w, http.StatusOK, h.harnesses.Discover())
}

func (h *Handler) handleHarnessPath(w http.ResponseWriter, r *http.Request, raw string) {
	parts, ok := harnessPathParts(raw)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	switch len(parts) {
	case 1:
		h.handleHarnessResource(w, r, parts[0])
	case 2:
		if parts[1] != "profiles" {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		h.handleProfileCollection(w, r, parts[0])
	case 3:
		if parts[1] != "profiles" {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		h.handleProfileResource(w, r, parts[0], parts[2])
	case 4:
		if parts[1] != "profiles" || parts[3] != "activate" {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		h.handleProfileActivation(w, r, parts[0], parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func harnessPathParts(raw string) ([]string, bool) {
	if raw == "" || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") {
		return nil, false
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func (h *Handler) handleHarnessResource(w http.ResponseWriter, r *http.Request, id string) {
	if h.harnesses == nil {
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		status, err := h.harnesses.Status(id)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodPut:
		data, err := readBody(w, r, h.maxBodyBytes)
		if err != nil {
			return
		}
		update, err := decodeHarnessUpdate(data)
		if err != nil {
			h.writeHarnessDecodeError(w, err)
			return
		}
		status, err := h.harnesses.UpdatePatch(id, update)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (h *Handler) handleProfileCollection(w http.ResponseWriter, r *http.Request, id string) {
	if h.harnesses == nil {
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		profiles, err := h.harnesses.Profiles(id)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, profiles)
	case http.MethodPost:
		data, err := readBody(w, r, h.maxBodyBytes)
		if err != nil {
			return
		}
		profile, err := decodeProfile(data)
		if err != nil {
			h.writeHarnessDecodeError(w, err)
			return
		}
		created, err := h.harnesses.CreateProfile(id, profile)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		w.Header().Set("Location", apiPrefix+"/harnesses/"+id+"/profiles/"+created.ID)
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (h *Handler) handleProfileResource(w http.ResponseWriter, r *http.Request, id, profileID string) {
	if h.harnesses == nil {
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		profile, err := h.harnesses.GetProfile(id, profileID)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, profile)
	case http.MethodPut:
		data, err := readBody(w, r, h.maxBodyBytes)
		if err != nil {
			return
		}
		profile, err := decodeProfile(data)
		if err != nil {
			h.writeHarnessDecodeError(w, err)
			return
		}
		updated, err := h.harnesses.UpdateProfile(id, profileID, profile)
		if err != nil {
			h.writeHarnessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := h.harnesses.DeleteProfile(id, profileID); err != nil {
			h.writeHarnessError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (h *Handler) handleProfileActivation(w http.ResponseWriter, r *http.Request, id, profileID string) {
	if h.harnesses == nil {
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	data, err := readBody(w, r, h.maxBodyBytes)
	if err != nil {
		return
	}
	if err := validateActivationBody(data); err != nil {
		h.writeHarnessDecodeError(w, err)
		return
	}
	result, err := h.harnesses.Activate(id, profileID)
	if err != nil {
		h.writeHarnessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) writeHarnessDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrActiveProfileReadOnly) {
		writeError(w, http.StatusConflict, "active_profile_read_only", "active profile state is server-managed")
		return
	}
	if errors.Is(err, harnessconfig.ErrInvalidJSON) {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	h.writeHarnessError(w, err)
}

func (h *Handler) writeHarnessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRestartInProgress):
		writeError(w, http.StatusConflict, "restart_in_progress", "restart_in_progress")
	case errors.Is(err, harnessconfig.ErrHarnessNotFound):
		writeError(w, http.StatusNotFound, "harness_not_found", "harness not found")
	case errors.Is(err, harnessconfig.ErrProfileNotFound):
		writeError(w, http.StatusNotFound, "profile_not_found", "profile not found")
	case errors.Is(err, harnessconfig.ErrProfileIDImmutable):
		writeError(w, http.StatusConflict, "id_immutable", "profile id cannot be changed")
	case errors.Is(err, harnessconfig.ErrActiveProfile):
		writeError(w, http.StatusConflict, "active_profile", "active profile must be changed before deletion")
	case errors.Is(err, config.ErrActiveProfileReadOnly):
		writeError(w, http.StatusConflict, "active_profile_read_only", "active profile state is server-managed")
	case errors.Is(err, harnessconfig.ErrGatewayKeyRequired):
		writeError(w, http.StatusConflict, "gateway_not_configured", "gateway key is not configured")
	case errors.Is(err, harnessconfig.ErrHarnessConfigConflict):
		writeError(w, http.StatusConflict, "harness_config_conflict", "harness target configuration conflicts with the requested operation")
	case errors.Is(err, harnessconfig.ErrHarnessConfigIOFailed):
		writeError(w, http.StatusInternalServerError, "harness_config_io_failed", "harness target configuration could not be written")
	case errors.Is(err, harnessconfig.ErrActiveProfileStateFailed):
		writeError(w, http.StatusInternalServerError, "active_profile_state_failed", "active profile state could not be persisted")
	case errors.Is(err, harnessconfig.ErrInvalidPathConfig), errors.Is(err, harnessconfig.ErrInvalidTargetPath),
		errors.Is(err, harnessconfig.ErrInvalidActivation), errors.Is(err, harnessconfig.ErrInvalidModel),
		errors.Is(err, harnessconfig.ErrInvalidProjection):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case errors.Is(err, harnessconfig.ErrManagerNotInitialized):
		writeError(w, http.StatusInternalServerError, "internal_error", "harness manager is unavailable")
	default:
		var conflictErr *config.ConflictError
		var validationErr *config.ValidationError
		switch {
		case errors.As(err, &conflictErr):
			writeError(w, http.StatusConflict, "conflict", conflictErr.Error())
		case errors.As(err, &validationErr):
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", validationErr.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "harness operation failed")
		}
	}
}
