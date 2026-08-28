package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestLogRingAppendAndAfter covers a fresh ring: appended lines come back in
// order with consecutive seqs starting at 1, the stream label is preserved, the
// trailing newline is trimmed off Text, TS is an RFC3339Nano timestamp, and head
// equals the count appended.
func TestLogRingAppendAndAfter(t *testing.T) {
	r := newLogRing(8)
	r.append(logStreamStdout, []byte("first line\n"))
	r.append(logStreamStderr, []byte("second line\n"))

	entries, head := r.after(0)
	if head != 2 {
		t.Fatalf("head = %d, want 2", head)
	}
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", entries[0].Seq, entries[1].Seq)
	}
	if entries[0].Stream != logStreamStdout || entries[1].Stream != logStreamStderr {
		t.Fatalf("streams = %q,%q, want stdout,stderr", entries[0].Stream, entries[1].Stream)
	}
	if entries[0].Text != "first line" {
		t.Fatalf("text[0] = %q, want %q (trailing newline trimmed)", entries[0].Text, "first line")
	}
	if entries[1].Text != "second line" {
		t.Fatalf("text[1] = %q, want %q", entries[1].Text, "second line")
	}
	if _, err := time.Parse(time.RFC3339Nano, entries[0].TS); err != nil {
		t.Fatalf("ts %q not RFC3339Nano: %v", entries[0].TS, err)
	}
}

// TestLogRingWraparoundEvictsOldest fills past capacity and asserts the oldest
// entries are evicted while seq keeps climbing across the wraparound, so the
// retained window is exactly the newest `capacity` lines in order.
func TestLogRingWraparoundEvictsOldest(t *testing.T) {
	r := newLogRing(3)
	for i := 1; i <= 5; i++ {
		r.append(logStreamStdout, []byte(fmt.Sprintf("line-%d\n", i)))
	}

	entries, head := r.after(0)
	if head != 5 {
		t.Fatalf("head = %d, want 5 (seq survives eviction)", head)
	}
	if len(entries) != 3 {
		t.Fatalf("entries len = %d, want 3 (capacity bound)", len(entries))
	}
	wantSeq := []int64{3, 4, 5}
	wantText := []string{"line-3", "line-4", "line-5"}
	for i, e := range entries {
		if e.Seq != wantSeq[i] || e.Text != wantText[i] {
			t.Fatalf("entries[%d] = {seq:%d text:%q}, want {seq:%d text:%q}", i, e.Seq, e.Text, wantSeq[i], wantText[i])
		}
	}
}

// TestLogRingAfterCursorIsIncremental pins the cursor contract: after(k) returns
// only entries with seq>k, after(head) returns a non-nil empty slice, a future
// cursor returns empty, and a cursor at/below the oldest returns everything
// retained.
func TestLogRingAfterCursorIsIncremental(t *testing.T) {
	r := newLogRing(8)
	for i := 1; i <= 4; i++ {
		r.append(logStreamStdout, []byte(fmt.Sprintf("l%d\n", i)))
	}

	entries, head := r.after(2)
	if head != 4 {
		t.Fatalf("head = %d, want 4", head)
	}
	if len(entries) != 2 || entries[0].Seq != 3 || entries[1].Seq != 4 {
		t.Fatalf("after(2) = %+v, want only seq 3,4", entries)
	}

	entries, head = r.after(head)
	if head != 4 {
		t.Fatalf("head = %d, want 4 unchanged", head)
	}
	if entries == nil {
		t.Fatal("after(head) entries = nil, want non-nil empty slice (JSON []) ")
	}
	if len(entries) != 0 {
		t.Fatalf("after(head) len = %d, want 0", len(entries))
	}

	if e, _ := r.after(9999); len(e) != 0 {
		t.Fatalf("after(future) len = %d, want 0", len(e))
	}
	if e, _ := r.after(0); len(e) != 4 {
		t.Fatalf("after(0) len = %d, want all 4 retained", len(e))
	}
}

// TestLogRingStreamWriterRecordsOneEntryPerWrite checks the io.Writer tee adapter:
// one Write becomes one entry (log.Logger emits a record per Write), it returns
// (len(p), nil) so it never breaks an io.MultiWriter chain, the stream label is
// attached, and only the trailing newline is trimmed (an internal one is kept).
func TestLogRingStreamWriterRecordsOneEntryPerWrite(t *testing.T) {
	r := newLogRing(8)
	w := r.streamWriter(logStreamStderr)

	line := []byte("2026/06/21 00:00:00 something happened: detail line\n")
	n, err := w.Write(line)
	if err != nil || n != len(line) {
		t.Fatalf("Write = (%d,%v), want (%d,nil)", n, err, len(line))
	}

	entries, head := r.after(0)
	if head != 1 || len(entries) != 1 {
		t.Fatalf("head=%d entries=%d, want 1/1 (one Write == one entry)", head, len(entries))
	}
	if entries[0].Stream != logStreamStderr {
		t.Fatalf("stream = %q, want %q", entries[0].Stream, logStreamStderr)
	}
	const want = "2026/06/21 00:00:00 something happened: detail line"
	if entries[0].Text != want {
		t.Fatalf("text = %q, want %q (trailing newline trimmed, prefix kept)", entries[0].Text, want)
	}
}

// TestLogRingConcurrentAppendsAreSafe exercises the lock under -race: many
// goroutines append while a reader polls. Every append must get a unique,
// monotonic seq (head == total appended), and the retained window is exactly the
// newest `capacity` entries with strictly increasing seqs.
func TestLogRingConcurrentAppendsAreSafe(t *testing.T) {
	r := newLogRing(64)
	const goroutines, perG = 16, 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				r.append(logStreamStdout, []byte("x\n"))
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				r.after(0)
			}
		}
	}()

	wg.Wait()
	close(done)

	total := int64(goroutines * perG)
	entries, head := r.after(0)
	if head != total {
		t.Fatalf("head = %d, want %d (each append assigns a unique seq)", head, total)
	}
	if len(entries) != 64 {
		t.Fatalf("retained = %d, want capacity 64", len(entries))
	}
	seen := make(map[int64]bool, len(entries))
	for i, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
		if i > 0 && e.Seq <= entries[i-1].Seq {
			t.Fatalf("seqs not strictly increasing at %d: %d then %d", i, entries[i-1].Seq, e.Seq)
		}
	}
}

// restoreLogWriters snapshots the package-level capped log writers and the
// loggers' current outputs, resets the writers to a clean slate for the test, and
// restores everything when the test ends. configureLogging mutates this shared
// state, so without this a test would (a) inherit a writer opened by an earlier
// test instead of opening its own, and (b) leak its temp-file-backed writer into
// later tests. Resetting to nil makes the "first call opens" path deterministic
// regardless of test order.
func restoreLogWriters(t *testing.T) {
	t.Helper()
	logWritersMu.Lock()
	savedStdout, savedStderr := stdoutCapped, stderrCapped
	stdoutCapped, stderrCapped = nil, nil
	logWritersMu.Unlock()
	savedStdoutOut := stdoutLogger.Writer()
	savedStderrOut := stderrLogger.Writer()
	t.Cleanup(func() {
		logWritersMu.Lock()
		stdoutCapped, stderrCapped = savedStdout, savedStderr
		logWritersMu.Unlock()
		stdoutLogger.SetOutput(savedStdoutOut)
		stderrLogger.SetOutput(savedStderrOut)
	})
}

// TestConfigureLoggingConfigCapOverridesBootstrapEnv reproduces the persisted-config authority startup
// sequence at the log-writer level: Main configures the writers from
// CC_AUTOMUX_LOG_MAX_BYTES for the bootstrap window, then Run calls
// configureLogging again with the authoritative config cap. The second call must
// retune the SAME open file in place (no reopen) to the config cap, so the config
// value wins over the bootstrap env. This guards both the "config overrides env"
// authority and the "rebuild = retune-in-place, not reopen" mechanism of persisted-config authority's
// logging entry point — the part Run exercises that the blocking Run() itself
// cannot be unit-tested for directly.
func TestConfigureLoggingConfigCapOverridesBootstrapEnv(t *testing.T) {
	restoreLogWriters(t)

	logPath := filepath.Join(t.TempDir(), "stdout.log")
	t.Setenv("CC_AUTOMUX_STDOUT_LOG", logPath)
	t.Setenv("CC_AUTOMUX_STDERR_LOG", "") // dev default: no stderr file writer
	const envCap int64 = 2 << 20
	const configCap int64 = 9 << 20
	t.Setenv("CC_AUTOMUX_LOG_MAX_BYTES", strconv.FormatInt(envCap, 10))

	// Bootstrap window (Main): open the writer at the env cap.
	if err := configureLoggingFromEnv(); err != nil {
		t.Fatalf("configureLoggingFromEnv: %v", err)
	}
	if stdoutCapped == nil {
		t.Fatal("bootstrap configureLoggingFromEnv did not open the stdout writer")
	}
	bootstrapWriter := stdoutCapped
	if got := bootstrapWriter.maxBytes; got != envCap {
		t.Fatalf("bootstrap cap = %d, want env %d", got, envCap)
	}

	// Run's call: retune to the authoritative config cap.
	if err := configureLogging(configCap); err != nil {
		t.Fatalf("configureLogging(configCap): %v", err)
	}
	if stdoutCapped != bootstrapWriter {
		t.Fatal("configureLogging reopened the writer; want the same fd retuned in place")
	}
	if got := stdoutCapped.maxBytes; got != configCap {
		t.Fatalf("after Run cap = %d, want config %d (config must override the bootstrap env)", got, configCap)
	}
	if got := stdoutCapped.keepBytes; got != logKeepBytes(configCap) {
		t.Fatalf("keepBytes = %d, want %d (derived from the config cap)", got, logKeepBytes(configCap))
	}
}

func TestLegacyLogEnvironmentIsIgnored(t *testing.T) {
	restoreLogWriters(t)
	legacyStdout := filepath.Join(t.TempDir(), "legacy-stdout.log")
	legacyStderr := filepath.Join(t.TempDir(), "legacy-stderr.log")
	t.Setenv("CC_AUTOMUX_STDOUT_LOG", "")
	t.Setenv("CC_AUTOMUX_STDERR_LOG", "")
	t.Setenv("CC_AUTOMUX_LOG_MAX_BYTES", "")
	t.Setenv("CC_AUTO_SHIM_STDOUT_LOG", legacyStdout)
	t.Setenv("CC_AUTO_SHIM_STDERR_LOG", legacyStderr)
	t.Setenv("CC_AUTO_SHIM_LOG_MAX_BYTES", "1")
	if err := configureLoggingFromEnv(); err != nil {
		t.Fatalf("configureLoggingFromEnv returned error: %v", err)
	}
	if stdoutCapped != nil || stderrCapped != nil {
		t.Fatal("legacy log path environment opened a file writer")
	}
	for _, path := range []string{legacyStdout, legacyStderr} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy log path was used: %s: %v", path, err)
		}
	}
	got, err := configuredMaxLogBytes()
	if err != nil {
		t.Fatalf("configuredMaxLogBytes returned error: %v", err)
	}
	if got != defaultMaxLogBytes {
		t.Fatalf("configuredMaxLogBytes = %d, want default %d", got, defaultMaxLogBytes)
	}
}
