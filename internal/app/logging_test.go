package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/gateway"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func decodeLogLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	result := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("invalid JSON log %q: %v", line, err)
		}
		result = append(result, record)
	}
	return result
}

func TestLoggerFormatsServiceEventsAndConcurrentSequence(t *testing.T) {
	var output bytes.Buffer
	logs, err := openLogger(appLogOpenerFor(&output), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	application := &App{logs: logs}
	application.logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", "127.0.0.1:8765"))

	const workers = 32
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "quoted=\"value\"\nnext"))
		}()
	}
	wait.Wait()
	records := decodeLogLines(t, output.Bytes())
	if len(records) != workers+1 {
		t.Fatalf("record count = %d", len(records))
	}
	for i, record := range records {
		if got := uint64(record["seq"].(float64)); got != uint64(i+1) {
			t.Fatalf("record %d seq = %d", i, got)
		}
		if record["msg"] != "service" || record["kind"] != nil {
			t.Fatalf("service record = %#v", record)
		}
		if _, err := time.Parse(time.RFC3339Nano, record["time"].(string)); err != nil {
			t.Fatalf("time = %q: %v", record["time"], err)
		}
	}
	if records[0]["level"] != "INFO" || records[0]["event"] != "listening" || records[0]["listen_addr"] != "127.0.0.1:8765" || records[0]["error"] != nil {
		t.Fatalf("listening record = %#v", records[0])
	}
	if records[1]["level"] != "WARN" || records[1]["event"] != "restart_failed" || records[1]["error"] != "quoted=\"value\"\nnext" {
		t.Fatalf("warning record = %#v", records[1])
	}
}

func TestGatewayEventFieldsAreSparseByKind(t *testing.T) {
	var output bytes.Buffer
	logs, err := openLogger(appLogOpenerFor(&output), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	application := &App{logs: logs}
	base := gateway.Event{
		ProviderID:   "provider-id",
		ProviderName: "provider-name",
		SessionID:    "session-id",
		Model:        "model-name",
		RequestType:  traffic.RequestTypeNormal,
		Attempt:      1,
		UpstreamURL:  "https://provider.example/v1/messages",
	}
	forward := base
	forward.Kind = gateway.EventForward
	forward.HTTPStatus = 999
	forward.RawError = "must-not-appear"
	application.recordGatewayEvent(forward)

	success := base
	success.Kind = gateway.EventSuccess
	success.HTTPStatus = 200
	success.RawError = "must-not-appear"
	success.GlobalHealth = scheduler.GlobalHealthy
	success.ChannelHealth = scheduler.ChannelHealthy
	application.recordGatewayEvent(success)

	failure := base
	failure.Kind = gateway.EventFailure
	failure.HTTPStatus = 502
	failure.RawError = "raw=\"failure\"\nline"
	failure.PatchID = "patch-id"
	failure.PatchStage = "request"
	failure.GlobalHealth = scheduler.GlobalCooldown
	failure.ChannelHealth = scheduler.ChannelHealthy
	failure.GlobalEnteredCooldown = true
	cooldown := time.Date(2026, 9, 3, 10, 12, 12, 13, time.UTC)
	failure.CooldownUntil = &cooldown
	application.recordGatewayEvent(failure)

	failover := failure
	failover.Kind = gateway.EventFailover
	failover.NextProviderID = "next-id"
	failover.NextProviderName = "next-name"
	failover.NextAttempt = 2
	failover.NextUpstreamURL = "https://next.example/v1/messages"
	application.recordGatewayEvent(failover)

	records := decodeLogLines(t, output.Bytes())
	if len(records) != 4 {
		t.Fatalf("records = %#v", records)
	}
	if records[0]["level"] != "INFO" || records[0]["http_status"] != nil || records[0]["raw_error"] != nil || records[0]["global_health"] != nil || records[0]["next_provider_id"] != nil {
		t.Fatalf("forward record = %#v", records[0])
	}
	if records[1]["level"] != "INFO" || records[1]["http_status"] != float64(200) || records[1]["raw_error"] != nil || records[1]["global_health"] != string(scheduler.GlobalHealthy) || records[1]["channel_health"] != string(scheduler.ChannelHealthy) {
		t.Fatalf("success record = %#v", records[1])
	}
	if records[2]["level"] != "ERROR" || records[2]["raw_error"] != failure.RawError || records[2]["patch_id"] != "patch-id" || records[2]["global_entered_cooldown"] != true || records[2]["channel_entered_cooldown"] != false || records[2]["next_provider_id"] != nil {
		t.Fatalf("failure record = %#v", records[2])
	}
	if records[3]["level"] != "ERROR" || records[3]["next_provider_id"] != "next-id" || records[3]["next_attempt"] != float64(2) {
		t.Fatalf("failover record = %#v", records[3])
	}
	for _, record := range records {
		if record["msg"] != "gateway" || record["event"] != nil {
			t.Fatalf("gateway discriminator = %#v", record)
		}
	}

	output.Reset()
	fixed := failure
	fixed.ProviderID = "fixed-classifier"
	fixed.ProviderName = ""
	fixed.GlobalHealth = ""
	fixed.ChannelHealth = ""
	fixed.GlobalEnteredCooldown = false
	fixed.ChannelEnteredCooldown = false
	fixed.CooldownUntil = nil
	application.recordGatewayEvent(fixed)
	fixedRecord := decodeLogLines(t, output.Bytes())[0]
	for _, absent := range []string{"provider_name", "global_health", "channel_health", "global_entered_cooldown", "channel_entered_cooldown", "cooldown_until", "next_provider_id"} {
		if fixedRecord[absent] != nil {
			t.Fatalf("fixed target record contains %s: %#v", absent, fixedRecord)
		}
	}
}

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortLogWriter struct{}

func (shortLogWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestLoggerIgnoresRuntimeWriteFailure(t *testing.T) {
	logs, err := openLogger(appLogOpenerFor(failingLogWriter{}), 10)
	if err != nil {
		t.Fatal(err)
	}
	(&App{logs: logs}).logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "failure"))
}

func TestLoggerPublishesOnlyAfterSuccessfulCompleteWrite(t *testing.T) {
	broker := logstore.NewBroker()
	subscription, err := broker.Subscribe(logstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	var output bytes.Buffer
	logs, err := openLoggerWithBroker(appLogOpenerFor(&output), 1024, broker)
	if err != nil {
		t.Fatal(err)
	}
	(&App{logs: logs}).logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", "127.0.0.1:8765"))
	message := <-subscription.Messages()
	if message.Kind != logstore.MessageRecord {
		t.Fatalf("message = %#v", message)
	}
	written := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	if !bytes.Equal(message.Record.Bytes(), written) {
		t.Fatalf("published bytes = %q, written line = %q", message.Record.Bytes(), written)
	}

	failingBroker := logstore.NewBroker()
	failingSubscription, err := failingBroker.Subscribe(logstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer failingSubscription.Close()
	failingLogs, err := openLoggerWithBroker(appLogOpenerFor(failingLogWriter{}), 1024, failingBroker)
	if err != nil {
		t.Fatal(err)
	}
	(&App{logs: failingLogs}).logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "not persisted"))
	failingBroker.Flush()
	select {
	case unexpected := <-failingSubscription.Messages():
		t.Fatalf("failed write was published: %#v", unexpected)
	default:
	}
	short := &publishingWriter{delegate: shortLogWriter{}, broker: failingBroker}
	if _, err := short.Write([]byte(`{"time":"2026-09-03T12:00:00Z","level":"INFO","msg":"service","seq":1}` + "\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
	failingBroker.Flush()
	select {
	case unexpected := <-failingSubscription.Messages():
		t.Fatalf("short write was published: %#v", unexpected)
	default:
	}
}

// TestLoggerWriteReturnsBeforeDispatch covers the boundary between the write
// lock and dispatch. The lock exists to keep the file intact and the sequence
// monotonic; parsing and summarizing a record for subscribers is the broker's
// work. A record large enough to take a noticeable time to parse must therefore
// neither delay the call that logged it beyond its own write, nor delay the
// small records logged by other requests while it is being parsed.
func TestLoggerWriteReturnsBeforeDispatch(t *testing.T) {
	broker := logstore.NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(logstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	logs, err := openLoggerWithBroker(appLogOpenerFor(&safeWriter{writer: io.Discard}), 1<<30, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	application := &App{logs: logs}

	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", strings.Repeat("x", 16<<20)))
	if len(subscription.Messages()) != 0 {
		t.Fatal("the oversized record was parsed and delivered before its log call returned")
	}
	// The oversized record is now being parsed on the broker's goroutine. Small
	// records from other requests must not queue behind it.
	var latencies [2]time.Duration
	var waiter sync.WaitGroup
	for i := range latencies {
		waiter.Add(1)
		go func(index int) {
			defer waiter.Done()
			started := time.Now()
			application.logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", "127.0.0.1:1"))
			latencies[index] = time.Since(started)
		}(i)
	}
	waiter.Wait()
	for index, latency := range latencies {
		if latency > 100*time.Millisecond {
			t.Fatalf("small record %d waited %v behind the oversized record's dispatch", index, latency)
		}
	}
	broker.Flush()
	for expected := uint64(1); expected <= 3; expected++ {
		message := <-subscription.Messages()
		if message.Kind != logstore.MessageRecord || message.Record.Seq != expected {
			t.Fatalf("message %d = %#v", expected, message)
		}
		if expected == 1 && len(message.Record.Bytes()) > 2*logstore.MaximumFieldBytes {
			t.Fatalf("oversized record was pushed with %d bytes", len(message.Record.Bytes()))
		}
	}
}

func TestLoggerPublishesAcrossRotation(t *testing.T) {
	broker := logstore.NewBroker()
	subscription, err := broker.Subscribe(logstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	dir := t.TempDir()
	logs, err := openLoggerWithBroker(logOpenerForDir(dir), 256, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	application := &App{logs: logs}
	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", strings.Repeat("a", 100)))
	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", strings.Repeat("b", 100)))
	for expected := uint64(1); expected <= 2; expected++ {
		select {
		case message := <-subscription.Messages():
			if message.Kind != logstore.MessageRecord || message.Record.Seq != expected {
				t.Fatalf("message %d = %#v", expected, message)
			}
		case <-time.After(time.Second):
			t.Fatalf("record %d was not published", expected)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, logstore.ArchiveFileName)); err != nil {
		t.Fatalf("rotation did not create archive: %v", err)
	}
}

// TestLoggerTimestampsFollowSequenceUnderConcurrency covers the ordering
// contract the history reader depends on. The reader scans one file backwards and
// treats file order as descending (time, seq); if a record could carry a
// timestamp taken before it acquired the write lock, a later-positioned record
// could hold an earlier time and cursor pagination would skip records for good.
func TestLoggerTimestampsFollowSequenceUnderConcurrency(t *testing.T) {
	var output bytes.Buffer
	logs, err := openLogger(appLogOpenerFor(&safeWriter{writer: &output}), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	application := &App{logs: logs}

	var waiter sync.WaitGroup
	for i := 0; i < 64; i++ {
		waiter.Add(1)
		go func() {
			defer waiter.Done()
			application.recordGatewayEvent(gateway.Event{
				Kind:        gateway.EventForward,
				Model:       "model",
				RequestType: traffic.RequestTypeNormal,
				Attempt:     1,
			})
		}()
	}
	waiter.Wait()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 64 {
		t.Fatalf("wrote %d records", len(lines))
	}
	var previousSeq uint64
	var previousTime time.Time
	for index, line := range lines {
		var record struct {
			Time time.Time `json:"time"`
			Seq  uint64    `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Seq != previousSeq+1 {
			t.Fatalf("record %d has seq %d after %d", index, record.Seq, previousSeq)
		}
		if record.Time.Before(previousTime) {
			t.Fatalf("record %d time %s precedes %s", index, record.Time, previousTime)
		}
		previousSeq = record.Seq
		previousTime = record.Time
	}
}

// TestLoggerClampsBackwardsClock keeps the same ordering when the wall clock
// steps backwards, which would otherwise reintroduce the inversion without any
// concurrency at all.
func TestLoggerClampsBackwardsClock(t *testing.T) {
	var output bytes.Buffer
	logs, err := openLogger(appLogOpenerFor(&output), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	forward := time.Date(2026, 9, 3, 12, 0, 5, 0, time.UTC)
	backward := forward.Add(-2 * time.Second)
	times := []time.Time{forward, backward, backward.Add(time.Millisecond)}
	index := 0
	logs.handler.(*sequencedHandler).now = func() time.Time {
		value := times[index]
		if index < len(times)-1 {
			index++
		}
		return value
	}
	application := &App{logs: logs}
	for i := 0; i < 3; i++ {
		application.logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", "127.0.0.1:1"))
	}

	var previous time.Time
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Time.Before(previous) {
			t.Fatalf("clamped time %s precedes %s", record.Time, previous)
		}
		previous = record.Time
	}
	if !previous.Equal(forward) {
		t.Fatalf("clamped tail = %s, want %s", previous, forward)
	}
}

// safeWriter serializes concurrent writes into the test buffer, which is not
// safe for concurrent use on its own.
type safeWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *safeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

// TestLoggerReportsWriteFailureWithoutStopping covers the degradation contract.
// A log write failure must not fail the request path, but it must stop being
// invisible: the state is queryable, and the first transition reaches the same
// stderr fallback that carries fatal startup errors.
func TestLoggerReportsWriteFailureWithoutStopping(t *testing.T) {
	var fallback bytes.Buffer
	failing := &toggleWriter{}
	logs, err := openLogger(appLogOpenerFor(failing), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	logs.fallback = &fallback
	application := &App{logs: logs}

	if health := application.loggingHealth(); !health.Healthy || health.Failures != 0 {
		t.Fatalf("initial health = %#v", health)
	}

	failing.fail(errors.New("no space left on device"))
	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "first"))
	health := application.loggingHealth()
	if health.Healthy || health.Failures != 1 || health.LastError != "no space left on device" || health.LastFailureAt == nil {
		t.Fatalf("degraded health = %#v", health)
	}
	if !strings.Contains(fallback.String(), "no space left on device") {
		t.Fatalf("fallback = %q", fallback.String())
	}

	// Only the transition writes to the fallback. Reporting every failure would
	// flood the fallback channel in exactly the disk-full case it exists for.
	written := fallback.Len()
	for i := 0; i < 5; i++ {
		application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "again"))
	}
	if fallback.Len() != written {
		t.Fatalf("fallback grew to %d bytes from %d", fallback.Len(), written)
	}
	if health = application.loggingHealth(); health.Failures != 6 {
		t.Fatalf("accumulated failures = %d", health.Failures)
	}

	// Recovery clears the degradation so a transient failure does not pin the
	// process into a permanently unhealthy report.
	failing.fail(nil)
	application.logServiceEvent(slog.LevelInfo, "listening", slog.String("listen_addr", "127.0.0.1:1"))
	if health = application.loggingHealth(); !health.Healthy || health.Failures != 0 || health.LastError != "" || health.LastFailureAt != nil {
		t.Fatalf("recovered health = %#v", health)
	}
	failing.fail(errors.New("second outage"))
	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "later"))
	if !strings.Contains(fallback.String(), "second outage") {
		t.Fatal("a new outage after recovery did not reach the fallback")
	}
}

// TestLoggerFailedWriteIsNotPublished confirms degradation does not leak an
// unpersisted record onto a live stream.
func TestLoggerFailedWriteIsNotPublished(t *testing.T) {
	broker := logstore.NewBroker()
	subscription, err := broker.Subscribe(logstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	failing := &toggleWriter{}
	logs, err := openLoggerWithBroker(appLogOpenerFor(failing), 1<<20, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	logs.fallback = io.Discard
	application := &App{logs: logs}

	failing.fail(errors.New("device removed"))
	application.logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "lost"))
	broker.Flush()
	select {
	case message := <-subscription.Messages():
		t.Fatalf("failed write reached a subscriber: %#v", message)
	default:
	}
	if health := application.loggingHealth(); health.Healthy {
		t.Fatal("failed write left the logger reported as healthy")
	}
}

// toggleWriter fails every write while an error is armed.
type toggleWriter struct {
	mu  sync.Mutex
	err error
}

func (w *toggleWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *toggleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}
