package gateway

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestConcurrentRequestsKeepDistinctTracesAcrossFailover(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Trace-Id") != "" || r.Header.Get("X-Trace-Id") != "" {
			t.Error("trace escaped to upstream")
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(503)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "bad", "m", false)
	second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", upstream.URL, "bad", "m", false)
	third := compileTestProvider(t, "33333333-3333-4333-8333-333333333333", "three", upstream.URL, "good", "m", false)
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second, third}}
	events := &eventCollector{}
	h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, &traceSelector{}, Options{Recorder: events})
	defer h.Close()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`)
			request.Header.Set("X-Claude-Code-Session-Id", "shared-session")
			h.ServeHTTP(w, request)
			if w.Code != 200 || w.Header().Get("Trace-Id") != "" {
				t.Errorf("response=%d", w.Code)
			}
		}()
	}
	wg.Wait()
	got := waitGatewayEvents(t, events, 12)
	groups := map[string][]Event{}
	for _, event := range got {
		decoded, err := hex.DecodeString(event.TraceID)
		if err != nil || len(decoded) != 16 {
			t.Fatalf("invalid trace: %#v", event)
		}
		groups[event.TraceID] = append(groups[event.TraceID], event)
	}
	if len(groups) != 2 {
		t.Fatalf("traces=%#v", groups)
	}
	for _, group := range groups {
		counts := map[EventKind]int{}
		for _, event := range group {
			counts[event.Kind]++
		}
		if len(group) != 6 || counts[EventForward] != 3 || counts[EventFailover] != 2 || counts[EventSuccess] != 1 {
			t.Fatalf("trace events=%#v", group)
		}
	}
}

type traceSelector struct{ fakeSelector }

func (s *traceSelector) Acquire(snapshot scheduler.Snapshot, key scheduler.StickyKey, excluded map[string]struct{}) (scheduler.AttemptLease, error) {
	for _, p := range snapshot.Candidates(key.Model) {
		if _, done := excluded[p.ID]; !done {
			return leaseFor(p, key.Model), nil
		}
	}
	return scheduler.AttemptLease{}, io.EOF
}

func TestTraceGenerationOccursBeforeIngressRead(t *testing.T) {
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway"}
	events := &eventCollector{}
	h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, &fakeSelector{}, Options{Recorder: events})
	defer h.Close()
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"missing","stream":true}`))
	}
	got := events.snapshot()
	if len(got) != 2 || len(got[0].TraceID) != 32 || got[0].TraceID == got[1].TraceID || !strings.Contains(got[0].ErrorCode, "model_not_configured") {
		t.Fatalf("events=%#v", got)
	}
}
