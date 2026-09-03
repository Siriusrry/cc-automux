package management

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

// handleLogRecord returns one complete persisted record. Both interfaces bound
// oversized fields, so this is how a client reads content that was withheld,
// whether it saw the summary in a history page or on the live stream.
func (h *Handler) handleLogRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	values := r.URL.Query()
	reference := values.Get("ref")
	if len(values) != 1 || len(values["ref"]) != 1 || reference == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "ref: must appear exactly once")
		return
	}
	position, err := logstore.DecodeReference(reference)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	if h.logs == nil {
		writeError(w, http.StatusInternalServerError, "log_read_failed", "log history is unavailable")
		return
	}
	record, err := h.logs.Record(r.Context(), position)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return
		case errors.Is(err, logstore.ErrRecordNotFound):
			writeError(w, http.StatusNotFound, "not_found", "log record is no longer retained")
		default:
			writeError(w, http.StatusInternalServerError, "log_read_failed", "could not read the log record")
		}
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (h *Handler) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	parameters, err := logstore.ParseParameters(r.URL.Query(), logstore.ParseOptions{History: false})
	if err != nil {
		if logstore.IsValidationError(err) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "log_stream_unavailable", "could not prepare log stream")
		return
	}
	if h.logStream == nil {
		writeError(w, http.StatusServiceUnavailable, "log_stream_unavailable", "log stream is unavailable")
		return
	}
	subscription, err := h.logStream.Subscribe(parameters.Filter)
	if err != nil {
		if errors.Is(err, logstore.ErrStreamUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "log_stream_unavailable", "log stream is unavailable")
			return
		}
		writeError(w, http.StatusInternalServerError, "log_stream_unavailable", "could not open log stream")
		return
	}
	defer subscription.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "log_stream_unavailable", "streaming response is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buffer := bufio.NewWriter(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case message, open := <-subscription.Messages():
			if !open {
				return
			}
			if err := writeStreamMessage(buffer, message); err != nil {
				return
			}
			if err := buffer.Flush(); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeStreamMessage(writer *bufio.Writer, message logstore.Message) error {
	switch message.Kind {
	case logstore.MessageRecord:
		if _, err := fmt.Fprintf(writer, "event: record\nid: %d\ndata: ", message.Record.Seq); err != nil {
			return err
		}
		if _, err := writer.Write(message.Record.Bytes()); err != nil {
			return err
		}
		_, err := writer.WriteString("\n\n")
		return err
	case logstore.MessageDropped:
		_, err := fmt.Fprintf(writer, "event: dropped\ndata: {\"dropped\":%d}\n\n", message.Dropped)
		return err
	default:
		return errors.New("unknown log stream message")
	}
}
