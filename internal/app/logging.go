package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/gateway"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
)

// LogOpener abstracts startup log-resource acquisition so restart transactions
// can preflight it and tests can inject failures without touching real files.
// The returned writer must preserve each Write call as one indivisible record.
type LogOpener func(maxBytes int64) (writer io.Writer, close func() error, err error)

func logOpenerForDir(dir string) LogOpener {
	return func(maxBytes int64) (io.Writer, func() error, error) {
		writer, err := newRotatingWriter(dir, maxBytes)
		if err != nil {
			return nil, nil, err
		}
		return writer, writer.Close, nil
	}
}

type rotatingWriter struct {
	mu        sync.Mutex
	active    string
	archive   string
	threshold int64
	file      *os.File
	size      int64
	closed    bool
}

func newRotatingWriter(dir string, maxBytes int64) (*rotatingWriter, error) {
	if maxBytes <= 0 {
		return nil, errors.New("log_max_bytes must be positive")
	}
	if dir == "" {
		return nil, errors.New("log directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	w := &rotatingWriter{
		active:    filepath.Join(dir, logstore.ActiveFileName),
		archive:   filepath.Join(dir, logstore.ArchiveFileName),
		threshold: maxBytes / 2,
	}
	if err := w.openActive(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) openActive() error {
	file, err := os.OpenFile(w.active, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open active log: %w", err)
	}
	position, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("seek active log: %w", err)
	}
	w.file = file
	w.size = position
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if w.file == nil {
		if err := w.openActive(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && crossesThreshold(w.size, int64(len(p)), w.threshold) {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func crossesThreshold(current, incoming, threshold int64) bool {
	if current > threshold {
		return true
	}
	return incoming > threshold-current
}

func (w *rotatingWriter) rotateLocked() error {
	if w.file == nil {
		return errors.New("active log is not open")
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close active log before rotation: %w", err)
	}
	w.file = nil
	if err := replaceLogArchive(w.active, w.archive); err != nil {
		// The namespace was not changed. Reopen the active file so a transient
		// replacement failure does not permanently disable later log writes.
		reopenErr := w.openActive()
		if reopenErr != nil {
			return fmt.Errorf("rotate log: %w; reopen active log: %v", err, reopenErr)
		}
		return fmt.Errorf("rotate log: %w", err)
	}
	if err := w.openActive(); err != nil {
		return fmt.Errorf("create active log after rotation: %w", err)
	}
	return nil
}

func (w *rotatingWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// OpenSnapshot binds both generations while holding the writer lock. A
// rotation can proceed after the handles are open without changing which file
// contents the history request observes.
func (w *rotatingWriter) OpenSnapshot() (logstore.Snapshot, error) {
	if w == nil {
		return logstore.Snapshot{}, errors.New("log writer is nil")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	source, err := logstore.NewDirectorySource(filepath.Dir(w.active))
	if err != nil {
		return logstore.Snapshot{}, err
	}
	return source.OpenSnapshot()
}

type sequenceState struct {
	mu   sync.Mutex
	next uint64
}

// sequencedHandler serializes complete slog records. Sequence allocation,
// formatting and the writer's single Write call therefore occur in the same
// order, so file order and seq order cannot diverge under concurrent logging.
type sequencedHandler struct {
	state    *sequenceState
	delegate slog.Handler
}

func (h *sequencedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h != nil && h.delegate != nil && h.delegate.Enabled(ctx, level)
}

func (h *sequencedHandler) Handle(ctx context.Context, record slog.Record) error {
	if h == nil || h.delegate == nil || h.state == nil {
		return errors.New("log handler is not initialized")
	}
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	h.state.next++
	record.AddAttrs(slog.Uint64("seq", h.state.next))
	return h.delegate.Handle(ctx, record)
}

func (h *sequencedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sequencedHandler{state: h.state, delegate: h.delegate.WithAttrs(attrs)}
}

func (h *sequencedHandler) WithGroup(name string) slog.Handler {
	return &sequencedHandler{state: h.state, delegate: h.delegate.WithGroup(name)}
}

type logger struct {
	handler slog.Handler
	source  logstore.SnapshotSource
	close   func() error
}

func openLogger(opener LogOpener, maxBytes int64) (*logger, error) {
	if opener == nil {
		return nil, errors.New("log opener is nil")
	}
	writer, closeFunc, err := opener(maxBytes)
	if err != nil {
		return nil, err
	}
	if closeFunc == nil {
		closeFunc = func() error { return nil }
	}
	if writer == nil {
		_ = closeFunc()
		return nil, errors.New("log opener returned a nil writer")
	}
	jsonHandler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})
	source, _ := writer.(logstore.SnapshotSource)
	return &logger{
		handler: &sequencedHandler{state: &sequenceState{}, delegate: jsonHandler},
		source:  source,
		close:   closeFunc,
	}, nil
}

func (l *logger) Close() error {
	if l == nil || l.close == nil {
		return nil
	}
	return l.close()
}

func (l *logger) log(at time.Time, level slog.Level, message string, attrs ...slog.Attr) {
	if l == nil || l.handler == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	record := slog.NewRecord(at.UTC(), level, message, 0)
	record.AddAttrs(attrs...)
	_ = l.handler.Handle(context.Background(), record)
}

func (a *App) recordGatewayEvent(event gateway.Event) {
	if a == nil || a.logs == nil {
		return
	}
	level := slog.LevelInfo
	if event.Kind == gateway.EventFailure || event.Kind == gateway.EventFailover {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("kind", string(event.Kind)),
		slog.String("model", event.Model),
		slog.String("request_type", string(event.RequestType)),
		slog.Int("attempt", event.Attempt),
		slog.String("upstream_url", event.UpstreamURL),
	}
	if event.ProviderID != "" {
		attrs = append(attrs, slog.String("provider_id", event.ProviderID))
	}
	if event.ProviderName != "" {
		attrs = append(attrs, slog.String("provider_name", event.ProviderName))
	}
	if event.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", event.SessionID))
	}
	if event.Kind != gateway.EventForward && event.HTTPStatus != 0 {
		attrs = append(attrs, slog.Int("http_status", event.HTTPStatus))
	}
	if event.Kind == gateway.EventFailure || event.Kind == gateway.EventFailover {
		attrs = append(attrs, slog.String("raw_error", event.RawError))
		if event.PatchID != "" {
			attrs = append(attrs,
				slog.String("patch_id", event.PatchID),
				slog.String("patch_stage", event.PatchStage),
			)
		}
	}
	if event.Kind != gateway.EventForward {
		if event.GlobalHealth != "" {
			attrs = append(attrs, slog.String("global_health", string(event.GlobalHealth)))
		}
		if event.ChannelHealth != "" {
			attrs = append(attrs, slog.String("channel_health", string(event.ChannelHealth)))
		}
	}
	if (event.Kind == gateway.EventFailure || event.Kind == gateway.EventFailover) && (event.GlobalEnteredCooldown || event.ChannelEnteredCooldown) {
		attrs = append(attrs,
			slog.Bool("global_entered_cooldown", event.GlobalEnteredCooldown),
			slog.Bool("channel_entered_cooldown", event.ChannelEnteredCooldown),
		)
		if event.CooldownUntil != nil {
			attrs = append(attrs, slog.Time("cooldown_until", event.CooldownUntil.UTC()))
		}
	}
	if event.Kind == gateway.EventFailover {
		attrs = append(attrs,
			slog.String("next_provider_id", event.NextProviderID),
			slog.String("next_provider_name", event.NextProviderName),
			slog.Int("next_attempt", event.NextAttempt),
			slog.String("next_upstream_url", event.NextUpstreamURL),
		)
	}
	a.logs.log(event.Time, level, "gateway", attrs...)
}

func (a *App) logServiceEvent(level slog.Level, event string, attrs ...slog.Attr) {
	if a == nil || a.logs == nil {
		return
	}
	values := make([]slog.Attr, 0, len(attrs)+1)
	values = append(values, slog.String("event", event))
	values = append(values, attrs...)
	a.logs.log(time.Time{}, level, "service", values...)
}
