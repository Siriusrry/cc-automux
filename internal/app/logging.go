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
	mu       sync.Mutex
	next     uint64
	lastTime time.Time
	// publish serializes dispatch in sequence order without holding mu, so
	// parsing and summarizing one record never delays another record's write.
	publish sync.Mutex
}

// sequencedHandler serializes complete slog records. The record timestamp and
// the sequence number are both assigned inside the write lock, immediately
// before the writer's single Write call, and the timestamp is clamped to be
// non-decreasing. File order, seq order and (time, seq) order are therefore the
// same order, which is what the history reader's reverse scan and cursor
// pagination depend on. An event's own occurrence time is not carried into the
// record: it differs from the write time by microseconds, and letting it set
// the timestamp would reorder records relative to their sequence numbers.
type sequencedHandler struct {
	state     *sequenceState
	delegate  slog.Handler
	publisher *publishingWriter
	now       func() time.Time
}

func (h *sequencedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h != nil && h.delegate != nil && h.delegate.Enabled(ctx, level)
}

func (h *sequencedHandler) Handle(ctx context.Context, record slog.Record) error {
	if h == nil || h.delegate == nil || h.state == nil {
		return errors.New("log handler is not initialized")
	}
	h.state.mu.Lock()
	h.state.next++
	record.Time = h.writeTimeLocked()
	record.AddAttrs(slog.Uint64("seq", h.state.next))
	err := h.delegate.Handle(ctx, record)
	var line []byte
	if err == nil && h.publisher != nil {
		line = h.publisher.take()
	}
	// Hand over to the publish lock before releasing the write lock. Dispatch
	// keeps sequence order while the next record is free to be written, so a
	// multi-megabyte record does not make every other request wait for it to be
	// parsed and summarized.
	if line != nil {
		h.state.publish.Lock()
	}
	h.state.mu.Unlock()
	if line != nil {
		h.publisher.dispatch(line)
		h.state.publish.Unlock()
	}
	return err
}

// writeTimeLocked returns a non-decreasing write timestamp. A clock that steps
// backwards would otherwise place a later record before an earlier one and make
// reverse-scan pagination skip records permanently.
func (h *sequencedHandler) writeTimeLocked() time.Time {
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	current := now().UTC()
	if !h.state.lastTime.IsZero() && current.Before(h.state.lastTime) {
		current = h.state.lastTime
	}
	h.state.lastTime = current
	return current
}

func (h *sequencedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sequencedHandler{state: h.state, delegate: h.delegate.WithAttrs(attrs), publisher: h.publisher, now: h.now}
}

func (h *sequencedHandler) WithGroup(name string) slog.Handler {
	return &sequencedHandler{state: h.state, delegate: h.delegate.WithGroup(name), publisher: h.publisher, now: h.now}
}

type logger struct {
	handler slog.Handler
	source  logstore.SnapshotSource
	close   func() error
	health  *logstore.HealthTracker
	// fallback receives one line when log writes first start failing. It is the
	// last resort for the case where even the management interface cannot be
	// reached, and is the same channel that carries fatal startup errors.
	fallback io.Writer
}

func (l *logger) Health() logstore.Health {
	if l == nil {
		return logstore.Health{Healthy: true}
	}
	return l.health.Snapshot()
}

func openLogger(opener LogOpener, maxBytes int64) (*logger, error) {
	return openLoggerWithBroker(opener, maxBytes, nil)
}

func openLoggerWithBroker(opener LogOpener, maxBytes int64, broker *logstore.Broker) (*logger, error) {
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
	source, _ := writer.(logstore.SnapshotSource)
	publisher := &publishingWriter{delegate: writer, broker: broker}
	jsonHandler := slog.NewJSONHandler(publisher, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &logger{
		handler:  &sequencedHandler{state: &sequenceState{}, delegate: jsonHandler, publisher: publisher},
		source:   source,
		close:    closeFunc,
		health:   logstore.NewHealthTracker(),
		fallback: os.Stderr,
	}, nil
}

// publishingWriter records the bytes of each successfully persisted record so
// the handler can dispatch them after releasing the write lock. Write is only
// reached from inside that lock, so the captured line needs no guard of its own.
type publishingWriter struct {
	delegate io.Writer
	broker   *logstore.Broker
	captured []byte
}

func (w *publishingWriter) Write(p []byte) (int, error) {
	if w == nil || w.delegate == nil {
		return 0, errors.New("log publishing writer is not initialized")
	}
	w.captured = nil
	n, err := w.delegate.Write(p)
	if err != nil || n != len(p) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return n, err
	}
	// Only a complete file write is eligible for dispatch, so an unpersisted
	// record can never reach a subscriber.
	w.captured = append([]byte(nil), p...)
	return n, nil
}

func (w *publishingWriter) take() []byte {
	if w == nil || w.broker == nil {
		return nil
	}
	line := w.captured
	w.captured = nil
	return line
}

func (w *publishingWriter) dispatch(line []byte) {
	if w == nil || w.broker == nil {
		return
	}
	// JSONHandler has already completed the record. Parsing failure here cannot
	// undo an authoritative file write, so it only suppresses the impossible
	// malformed real-time message.
	_ = w.broker.Publish(line)
}

func (l *logger) Close() error {
	if l == nil || l.close == nil {
		return nil
	}
	return l.close()
}

// log formats one record. The timestamp argument is ignored: the handler assigns
// the write time inside its lock so that file order, seq order and (time, seq)
// order stay identical.
func (l *logger) log(level slog.Level, message string, attrs ...slog.Attr) {
	if l == nil || l.handler == nil {
		return
	}
	record := slog.NewRecord(time.Time{}, level, message, 0)
	record.AddAttrs(attrs...)
	if err := l.handler.Handle(context.Background(), record); err != nil {
		l.reportWriteFailure(err)
		return
	}
	l.health.RecordSuccess()
}

// reportWriteFailure keeps the degradation in memory and, only on the first
// transition, emits one line to the fallback channel. It never logs: reporting a
// log failure by logging would recurse.
func (l *logger) reportWriteFailure(err error) {
	if !l.health.RecordFailure(err) || l.fallback == nil {
		return
	}
	fmt.Fprintf(l.fallback, "cc-automux: structured logging stopped working: %v\n", err)
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
	a.logs.log(level, "gateway", attrs...)
}

func (a *App) logServiceEvent(level slog.Level, event string, attrs ...slog.Attr) {
	if a == nil || a.logs == nil {
		return
	}
	values := make([]slog.Attr, 0, len(attrs)+1)
	values = append(values, slog.String("event", event))
	values = append(values, attrs...)
	a.logs.log(level, "service", values...)
}

// loggingHealth reports the live logger's write health. Reading the field on
// each call rather than capturing it keeps the report correct after a restart
// transaction replaces the logger.
func (a *App) loggingHealth() logstore.Health {
	if a == nil {
		return logstore.Health{Healthy: true}
	}
	a.lifecycleMu.Lock()
	logs := a.logs
	a.lifecycleMu.Unlock()
	return logs.Health()
}
