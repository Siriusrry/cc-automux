package management

import (
	"context"
	"errors"
	"net/http"

	logstore "github.com/Siriusrry/cc-automux/internal/logs"
)

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	parameters, err := logstore.ParseParameters(r.URL.Query(), logstore.ParseOptions{History: true})
	if err != nil {
		if logstore.IsValidationError(err) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "log_read_failed", "could not prepare log query")
		return
	}
	if h.logs == nil {
		writeError(w, http.StatusInternalServerError, "log_read_failed", "log history is unavailable")
		return
	}
	page, err := h.logs.Query(r.Context(), parameters.HistoryQuery())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeError(w, http.StatusInternalServerError, "log_read_failed", "could not read log history")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
