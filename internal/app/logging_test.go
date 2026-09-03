package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/gateway"
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
		Time:         time.Date(2026, 9, 3, 10, 11, 12, 13, time.UTC),
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
	cooldown := base.Time.Add(time.Minute)
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

func TestLoggerIgnoresRuntimeWriteFailure(t *testing.T) {
	logs, err := openLogger(appLogOpenerFor(failingLogWriter{}), 10)
	if err != nil {
		t.Fatal(err)
	}
	(&App{logs: logs}).logServiceEvent(slog.LevelWarn, "restart_failed", slog.String("error", "failure"))
}
