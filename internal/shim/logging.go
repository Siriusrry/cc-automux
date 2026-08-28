package shim

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultMaxLogBytes int64 = 100 << 20

// logRingCapacity is the number of most-recent log lines kept in the in-process
// ring buffer that backs GET /admin/logs/tail. It is an entry count, not a byte
// budget: each line is already summary-level (the Logging red line forbids
// dumping full request/response bodies), so ~2000 lines is a generous live-tail
// window at a negligible, bounded resident cost. Older lines are evicted as new
// ones arrive.
const logRingCapacity = 2000

// Stream labels carried on every captured log entry. There are exactly two real
// streams (stdout/stderr); no synthetic warn/error levels are invented.
const (
	logStreamStdout = "stdout"
	logStreamStderr = "stderr"
)

// logBuffer mirrors every line written through stdoutLogger/stderrLogger into a
// bounded in-process ring (see logRing). It is fed by teeing the loggers' output
// (io.MultiWriter below, and again in configureLoggingFromEnv when output is
// redirected to files), so it captures exactly the same already-emitted,
// summary-level lines that go to stdout/stderr or the log files — it introduces
// no new logging and dumps no request/response bodies. It is the only log source
// that is live in both dev (stdout) and launchd (file) mode and is decoupled from
// the file rotation's Truncate(0); GET /admin/logs/tail serves from it.
// "Real-time" means lines produced after process start — history stays in the log
// files (logs.sh). Raw-fd writes that bypass the loggers (e.g. a panic) are not
// captured, an accepted gap.
var logBuffer = newLogRing(logRingCapacity)

var (
	// The ring tee is listed first in each MultiWriter so a buffered line is
	// captured even if the real destination (stdout/stderr or a capped file)
	// returns a write error; the destination still receives every line in order.
	stdoutLogger = log.New(io.MultiWriter(logBuffer.streamWriter(logStreamStdout), os.Stdout), "", log.LstdFlags)
	stderrLogger = log.New(io.MultiWriter(logBuffer.streamWriter(logStreamStderr), os.Stderr), "", log.LstdFlags)
)

// logWritersMu guards the file-backed capped writers installed on the package
// loggers. They are (re)built only at startup — once from the env for the
// bootstrap window (configureLoggingFromEnv in Main) and once more from the
// authoritative config cap (configureLogging in Run, after loadConfig) — both on
// the main goroutine before any request is served. The mutex keeps the package
// state self-consistent and documents that these writers are shared.
var (
	logWritersMu sync.Mutex
	stdoutCapped *cappedLogWriter
	stderrCapped *cappedLogWriter
)

// configureLogging (re)builds the file log writers to the given per-file byte cap.
// It is the single entry point that binds the stdout/stderr capped writers to a
// cap. On the FIRST call for a stream (its *_LOG path env is set) it opens a
// cappedLogWriter at maxBytes and tees it with the in-process ring (GET
// /admin/logs/tail), so the ring stays live when launchd writes to files; on a LATER
// call it retunes the already-open writer's cap in place — same fd, no reopen. A
// stream whose path env is unset keeps its default destination (os.Stdout/
// os.Stderr in dev), where no cap applies. It is called once from the env for the
// bootstrap window (configureLoggingFromEnv) and once more from Run with the
// config's authoritative cap.
func configureLogging(maxBytes int64) error {
	logWritersMu.Lock()
	defer logWritersMu.Unlock()

	if path := strings.TrimSpace(os.Getenv("CC_AUTO_SHIM_STDOUT_LOG")); path != "" {
		if stdoutCapped != nil {
			stdoutCapped.setMaxBytes(maxBytes)
		} else {
			writer, err := newCappedLogWriter(path, maxBytes)
			if err != nil {
				return fmt.Errorf("open stdout log: %w", err)
			}
			stdoutCapped = writer
			stdoutLogger.SetOutput(io.MultiWriter(logBuffer.streamWriter(logStreamStdout), writer))
		}
	}
	if path := strings.TrimSpace(os.Getenv("CC_AUTO_SHIM_STDERR_LOG")); path != "" {
		if stderrCapped != nil {
			stderrCapped.setMaxBytes(maxBytes)
		} else {
			writer, err := newCappedLogWriter(path, maxBytes)
			if err != nil {
				return fmt.Errorf("open stderr log: %w", err)
			}
			stderrCapped = writer
			stderrLogger.SetOutput(io.MultiWriter(logBuffer.streamWriter(logStreamStderr), writer))
		}
	}
	return nil
}

// configureLoggingFromEnv configures the log writers for the bootstrap window —
// before the runtime config has been read — using CC_AUTO_SHIM_LOG_MAX_BYTES (or
// the default). Run later calls configureLogging again with the authoritative
// config cap, demoting this env value to a bootstrap + first-run seed: it
// seeds the config only on first file creation, mirroring CC_AUTO_SHIM_LISTEN →
// listen_addr (see seedRuntimeConfigFromEnv).
func configureLoggingFromEnv() error {
	maxBytes, err := configuredMaxLogBytes()
	if err != nil {
		return err
	}
	return configureLogging(maxBytes)
}

func configuredMaxLogBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("CC_AUTO_SHIM_LOG_MAX_BYTES"))
	if raw == "" {
		return defaultMaxLogBytes, nil
	}
	maxBytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || maxBytes <= 0 {
		return 0, fmt.Errorf("CC_AUTO_SHIM_LOG_MAX_BYTES must be a positive integer, got %q", raw)
	}
	return maxBytes, nil
}

func infof(format string, args ...any) {
	stdoutLogger.Printf(format, args...)
}

func errorf(format string, args ...any) {
	stderrLogger.Printf(format, args...)
}

// logEntry is one captured log line in the in-process ring buffer. Its JSON shape
// is the GET /admin/logs/tail contract (the entries[] elements). Seq is a
// process-wide monotonic cursor (the first captured line is seq 1); Stream is
// "stdout" or "stderr"; TS is the capture time (RFC3339Nano, UTC); Text is the
// verbatim log line with its trailing newline trimmed (it still carries the
// logger's own LstdFlags time prefix).
type logEntry struct {
	Seq    int64  `json:"seq"`
	Stream string `json:"stream"`
	TS     string `json:"ts"`
	Text   string `json:"text"`
}

// logRing is a fixed-capacity, mutex-guarded ring buffer of the most recent
// logEntry values. Appends arrive from the stdout and stderr loggers (two
// distinct loggers sharing one ring), and concurrent reads come from the
// /admin/logs/tail handler; mu serializes all of them. When the ring is full the
// oldest entry is overwritten, but seq keeps increasing across evictions so the
// tail cursor stays stable.
type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	start   int   // index of the oldest retained entry
	size    int   // number of retained entries (<= len(entries))
	nextSeq int64 // seq to assign to the next appended entry
}

func newLogRing(capacity int) *logRing {
	if capacity < 1 {
		capacity = 1
	}
	return &logRing{entries: make([]logEntry, capacity), nextSeq: 1}
}

// append records one log line under the given stream label, assigning it the next
// seq and a capture timestamp. p is one full logger write (log.Logger emits each
// record in a single Write), so one Write becomes one entry; its trailing newline
// is trimmed. When the ring is full the oldest line is evicted. It always returns
// (len(p), nil) so it never disrupts the io.MultiWriter tee it sits behind.
func (r *logRing) append(stream string, p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := logEntry{
		Seq:    r.nextSeq,
		Stream: stream,
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Text:   text,
	}
	r.nextSeq++
	pos := (r.start + r.size) % len(r.entries)
	r.entries[pos] = entry
	if r.size < len(r.entries) {
		r.size++
	} else {
		// Full: pos == start, so the oldest entry was just overwritten; advance
		// start to the new oldest.
		r.start = (r.start + 1) % len(r.entries)
	}
	return len(p), nil
}

// after returns every retained entry whose Seq is greater than the cursor,
// oldest-first, plus head — the largest seq assigned so far (0 before any line).
// The returned slice is non-nil, so the /admin/logs/tail JSON always carries an
// array (never null) even when empty. A poller passes after=head from the prior
// response to receive only newer lines.
func (r *logRing) after(cursor int64) ([]logEntry, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	head := r.nextSeq - 1
	out := make([]logEntry, 0, r.size)
	for i := 0; i < r.size; i++ {
		entry := r.entries[(r.start+i)%len(r.entries)]
		if entry.Seq > cursor {
			out = append(out, entry)
		}
	}
	return out, head
}

// streamWriter returns an io.Writer that tees one logger's output into the ring,
// tagged with the given stream label. It is installed via io.MultiWriter so the
// real destination (stdout/stderr or the capped file) is unaffected.
func (r *logRing) streamWriter(stream string) io.Writer {
	return logStreamWriter{ring: r, stream: stream}
}

type logStreamWriter struct {
	ring   *logRing
	stream string
}

func (w logStreamWriter) Write(p []byte) (int, error) {
	return w.ring.append(w.stream, p)
}

type cappedLogWriter struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	maxBytes  int64
	keepBytes int64
}

func newCappedLogWriter(path string, maxBytes int64) (*cappedLogWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &cappedLogWriter{
		path:      path,
		file:      file,
		maxBytes:  maxBytes,
		keepBytes: logKeepBytes(maxBytes),
	}, nil
}

// logKeepBytes is the amount of the newest log retained when a capped file is
// trimmed: 80% of the cap, falling back to half when that would be non-positive
// or not actually smaller than the cap (tiny caps). Shared by newCappedLogWriter
// and setMaxBytes so the cap and its keep-window are always derived together.
func logKeepBytes(maxBytes int64) int64 {
	keepBytes := maxBytes * 8 / 10
	if keepBytes <= 0 || keepBytes >= maxBytes {
		keepBytes = maxBytes / 2
	}
	return keepBytes
}

// setMaxBytes retunes the writer's cap (and its derived keep-window) in place,
// under the writer's lock so the authoritative config cap can replace
// the bootstrap (env/default) cap on the SAME open file, without reopening it.
func (w *cappedLogWriter) setMaxBytes(maxBytes int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.maxBytes = maxBytes
	w.keepBytes = logKeepBytes(maxBytes)
}

func (w *cappedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	if err := w.trimIfNeededLocked(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cc-auto-mode-shim log trim failed for %s: %v\n", w.path, err)
	}
	return n, nil
}

func (w *cappedLogWriter) trimIfNeededLocked() error {
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= w.maxBytes {
		return nil
	}

	keepBytes := w.keepBytes
	if keepBytes > info.Size() {
		keepBytes = info.Size()
	}
	offset := info.Size() - keepBytes
	kept := make([]byte, int(keepBytes))
	n, err := w.file.ReadAt(kept, offset)
	if err != nil && err != io.EOF {
		return err
	}
	kept = kept[:n]
	if idx := bytes.IndexByte(kept, '\n'); idx >= 0 && idx+1 < len(kept) {
		kept = kept[idx+1:]
	}

	marker := []byte(fmt.Sprintf("%s log truncated; kept newest entries under configured %d byte limit\n", time.Now().Format("2006/01/02 15:04:05"), w.maxBytes))
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := w.file.Write(marker); err != nil {
		return err
	}
	_, err = w.file.Write(kept)
	return err
}
