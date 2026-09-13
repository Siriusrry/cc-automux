package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestBackgroundErrorBodyDoesNotDelayFailover(t *testing.T) {
	for _, ending := range []string{"complete", "timeout", "interrupted", "truncated"} {
		t.Run(ending, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Header.Get("Authorization") == "Bearer good" {
					_, _ = io.WriteString(w, `{}`)
					return
				}
				if ending == "interrupted" {
					w.Header().Set("Content-Length", "500")
				}
				w.WriteHeader(503)
				w.(http.Flusher).Flush()
				if ending == "timeout" {
					<-r.Context().Done()
					return
				}
				<-release
				text := "failure body"
				if ending == "truncated" {
					text = strings.Repeat("界", 100)
				}
				_, _ = io.WriteString(w, text)
			}))
			defer upstream.Close()
			first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "bad", "m", false)
			second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", upstream.URL, "good", "m", false)
			snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
			events := &eventCollector{}
			h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events, UpstreamLimits: UpstreamLimits{ErrorBody: 70 * time.Millisecond, ErrorTextBytes: 32}})
			defer h.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`).WithContext(ctx))
			if w.Code != 200 {
				t.Fatalf("response=%d", w.Code)
			}
			before := events.snapshot()
			if len(before) != 3 || before[1].Kind != EventForward || before[1].Attempt != 2 {
				t.Fatalf("failure delayed forward: %#v", before)
			}
			cancel()
			close(release)
			got := waitGatewayEvents(t, events, 4)
			event := got[3]
			want := ending
			if ending == "complete" {
				want = ""
			}
			if event.Kind != EventFailover || event.RawErrorIncomplete != want || event.EndReason != "http_error" || len(event.RawError) > 32 || !utf8.ValidString(event.RawError) {
				t.Fatalf("event=%#v", event)
			}
			_, reports := selector.snapshot()
			if len(reports) != 2 || !reports[0].ErrorPending {
				t.Fatalf("reports=%#v", reports)
			}
		})
	}
}

func TestFinalErrorStreamsAndKeepsOneHealthResult(t *testing.T) {
	for _, ending := range []string{"complete", "idle", "interrupted", "large"} {
		t.Run(ending, func(t *testing.T) {
			release := make(chan struct{})
			text := "prefix"
			if ending == "large" {
				text = strings.Repeat("界", 40000)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("X-Preserved", "yes")
				w.Header().Set("Retry-After", "7")
				if ending == "interrupted" {
					w.Header().Set("Content-Length", "9999")
				}
				w.WriteHeader(429)
				_, _ = io.WriteString(w, "prefix")
				w.(http.Flusher).Flush()
				<-release
				if ending == "idle" {
					<-r.Context().Done()
					return
				}
				if ending == "large" {
					_, _ = io.WriteString(w, text)
				}
			}))
			defer upstream.Close()
			item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
			snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
			events := &eventCollector{}
			h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events, UpstreamLimits: UpstreamLimits{ResponseIdle: 50 * time.Millisecond, ErrorBody: 10 * time.Millisecond}})
			defer h.Close()
			server := httptest.NewServer(h)
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL+MessagesPath, strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer gateway")
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			prefix := make([]byte, 6)
			_, err = io.ReadFull(response.Body, prefix)
			close(release)
			if err != nil || string(prefix) != "prefix" || response.StatusCode != 429 || response.Header.Get("X-Preserved") != "yes" {
				t.Fatalf("not streaming: %v", err)
			}
			rest, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if ending == "idle" || ending == "interrupted" {
				if readErr == nil {
					t.Fatal("abnormal response ended normally")
				}
			} else if readErr != nil {
				t.Fatal(readErr)
			}
			if ending == "large" && string(rest) != text {
				t.Fatal("log cap truncated downstream")
			}
			event := waitGatewayEvents(t, events, 2)[1]
			want := ""
			switch ending {
			case "idle":
				want = "timeout"
			case "interrupted":
				want = "interrupted"
			case "large":
				want = "truncated"
			}
			_, reports := selector.snapshot()
			if event.EndReason != "http_error" || event.RawErrorIncomplete != want || len(event.RawError) > DefaultErrorTextLimit || !utf8.ValidString(event.RawError) || len(reports) != 1 || reports[0].Class != scheduler.FailureChannelTransient {
				t.Fatalf("event=%#v reports=%#v", event, reports)
			}
		})
	}
}

func TestFixedErrorBodyIsBoundedAndPassedThrough(t *testing.T) {
	text := strings.Repeat("界", 40000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429); _, _ = io.WriteString(w, text) }))
	defer upstream.Close()
	events := &eventCollector{}
	h := NewWithOptions(nil, &fakeSelector{}, Options{Recorder: events})
	defer h.Close()
	w := httptest.NewRecorder()
	h.forwardFixedExecution(w, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, config.ProtocolAnthropicMessages, patch.Plan{})))
	call := h.FixedTargetDiagnostics()
	event := waitGatewayEvents(t, events, 2)[1]
	if w.Code != 429 || w.Body.String() != text || !call.UpstreamBodyTruncated || len(call.UpstreamBody) > 8192 || len(call.Error) > 8192 || event.RawErrorIncomplete != "truncated" || len(event.RawError) > 65536 || !utf8.ValidString(event.RawError) {
		t.Fatalf("bounded fixed body lengths=%d/%d event=%s", len(call.UpstreamBody), len(event.RawError), event.RawErrorIncomplete)
	}
}

func TestGatewayPendingDiagnosticUsesObservationToken(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer good" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(503)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "delayed original error")
	}))
	defer upstream.Close()
	first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "bad", "m", false)
	second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", upstream.URL, "good", "m", false)
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	store := health.NewDefault()
	selector, err := scheduler.New(store, scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	selector.Reconcile(snapshot)
	events := &eventCollector{}
	h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events})
	defer h.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	before, _ := store.ProviderSnapshot(first.ID, first.Generation)
	if w.Code != 200 || len(before.Channels) != 1 || !before.Channels[0].LastErrorPending || before.Channels[0].LastError != "HTTP 503 Service Unavailable" {
		close(release)
		t.Fatalf("pending=%#v status=%d", before, w.Code)
	}
	close(release)
	waitGatewayEvents(t, events, 4)
	after, _ := store.ProviderSnapshot(first.ID, first.Generation)
	if after.Channels[0].LastErrorPending || after.Channels[0].LastError != "delayed original error" || after.Channels[0].ConsecutiveFailures != 1 {
		t.Fatalf("filled=%#v", after)
	}
}

func TestFinalErrorCancellationIsIncompleteFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "error prefix")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	events := &eventCollector{}
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events})
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &fixedWriteRecorder{header: make(http.Header), onWrite: cancel}
	h.ServeHTTP(writer, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`).WithContext(ctx))
	event := waitGatewayEvents(t, events, 2)[1]
	_, reports := selector.snapshot()
	if event.Kind != EventFailure || event.EndReason != "http_error" || event.RawErrorIncomplete != "canceled" || event.RawError != "error prefix" || len(reports) != 1 {
		t.Fatalf("event=%#v reports=%#v", event, reports)
	}
}
