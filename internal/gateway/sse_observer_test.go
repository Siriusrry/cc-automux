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

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestSSEObserverChunksAndFirstResult(t *testing.T) {
	for _, test := range []struct {
		wire, reason string
		class        scheduler.FailureClass
	}{
		{"event: message_stop\n\nevent: error\ndata: {}\n\n", "completed", scheduler.FailureNone},
		{"event: error\r\ndata: {\r\ndata: \"error\":{\"type\":\"authentication_error\"}}\r\n\r\nevent: message_stop\n\n", "stream_error_event", scheduler.FailureGlobalImmediate},
		{"event: error\ndata: {\"error\":{\"type\":\"billing_error\"}}\n\n", "stream_error_event", scheduler.FailureNeutral},
		{"event: error\ndata: broken\n\n", "stream_error_event", scheduler.FailureChannelTransient},
		{"event: error\ndata: {\"error\":{\"type\":\"not_found_error\"}}\n\n", "stream_error_event", scheduler.FailureChannelImmediate},
	} {
		for chunk := 1; chunk <= len(test.wire); chunk++ {
			observer := sseObserver{data: boundedText{limit: 64 * 1024}}
			for i := 0; i < len(test.wire); i += chunk {
				observer.feed([]byte(test.wire[i:min(i+chunk, len(test.wire))]))
			}
			if observer.result == nil || string(observer.result.reason) != test.reason || observer.result.class != test.class {
				t.Fatalf("chunk=%d result=%#v", chunk, observer.result)
			}
		}
	}
	observer := sseObserver{data: boundedText{limit: 17}}
	observer.feed([]byte("event: error\ndata: " + strings.Repeat("界", 100) + "\n\n"))
	if observer.result == nil || observer.result.incomplete != "truncated" || !utf8.ValidString(observer.result.raw) || len(observer.result.raw) > 17 {
		t.Fatalf("bounded result=%#v", observer.result)
	}
	observer = sseObserver{data: boundedText{limit: 17}}
	observer.feed([]byte("event: content_block_delta\ndata: " + strings.Repeat("x", 100000) + "\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	if len(observer.data.data) != 0 || observer.result != nil {
		t.Fatal("observed unnamed event or ordinary content")
	}
}

func TestSSEWireEndingsAndHealth(t *testing.T) {
	for _, test := range []struct {
		name, wire, encoding, ending, reason, post string
		class                                      scheduler.FailureClass
	}{
		{"complete", "event: message_stop\n\n", "", "eof", "", "", scheduler.FailureNone},
		{"truncated", "event: ping\ndata: {}\n\n", "", "eof", "stream_truncated", "", scheduler.FailureChannelStream},
		{"encoded", "unobserved", "gzip", "eof", "", "", scheduler.FailureNone},
		{"completed then broken", "event: message_stop\n\n", "", "broken", "", "connection_error", scheduler.FailureNone},
		{"completed then idle", "event: message_stop\n\n", "", "idle", "", "idle_timeout", scheduler.FailureNone},
		{"error then eof", "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\"}}\n\n", "", "eof", "stream_error_event", "", scheduler.FailureChannelTransient},
		{"error then idle", "event: error\ndata: {\"error\":{\"type\":\"permission_error\"}}\n\n", "", "idle", "stream_error_event", "", scheduler.FailureGlobalImmediate},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				if test.encoding != "" {
					w.Header().Set("Content-Encoding", test.encoding)
				}
				if test.ending == "broken" {
					w.Header().Set("Content-Length", "9999")
				}
				_, _ = io.WriteString(w, test.wire)
				w.(http.Flusher).Flush()
				if test.ending == "idle" {
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
			snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
			events := &eventCollector{}
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
			h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events, UpstreamLimits: UpstreamLimits{ResponseIdle: 25 * time.Millisecond}})
			defer h.Close()
			server := httptest.NewServer(h)
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL+MessagesPath, strings.NewReader(`{"model":"m","stream":true}`))
			req.Header.Set("Authorization", "Bearer gateway")
			req.Header.Set("Accept-Encoding", "gzip")
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if string(data) != test.wire || (err != nil) != (test.ending != "eof") {
				t.Fatalf("wire=%q err=%v", data, err)
			}
			// Wait for the handler's terminal event after the socket completes.
			deadline := time.Now().Add(time.Second)
			for len(events.snapshot()) < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			got := events.snapshot()
			_, reports := selector.snapshot()
			if len(got) != 2 || string(got[1].EndReason) != test.reason || string(got[1].PostCompletion) != test.post || len(reports) != 1 || reports[0].Class != test.class {
				t.Fatalf("events=%#v reports=%#v", got, reports)
			}
		})
	}
}

func TestSSEConfirmedResultSurvivesClientCancel(t *testing.T) {
	for _, wire := range []string{"event: message_stop\n\n", "event: error\ndata: {}\n\n"} {
		ctx, cancel := context.WithCancel(context.Background())
		h := NewWithOptions(nil, &fakeSelector{}, Options{})
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
		w := &fixedWriteRecorder{header: make(http.Header), onWrite: cancel}
		control := h.newUpstreamAttempt(ctx, false)
		response, _ = control.receiveHeaders(response, nil)
		result := h.copyUpstream(w, ctx, response, control, true)
		control.close()
		h.Close()
		cancel()
		want := "stream_error_event"
		if strings.Contains(wire, "message_stop") {
			want = "completed"
		}
		if string(result.verdict.reason) != want {
			t.Fatalf("result=%#v", result)
		}
	}
}
