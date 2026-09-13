package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

func TestResponseHeaderTimeoutAndFailover(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer blocked.Close()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer ready.Close()
	for _, recover := range []bool{false, true} {
		first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", blocked.URL, "key", "m", false)
		providers := []*provider.CompiledProvider{first}
		leases := []scheduler.AttemptLease{leaseFor(first, "m")}
		if recover {
			second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", ready.URL, "key", "m", false)
			providers = append(providers, second)
			leases = append(leases, leaseFor(second, "m"))
		}
		events := &eventCollector{}
		selector := &fakeSelector{leases: leases}
		snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: providers}
		h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events, UpstreamLimits: UpstreamLimits{ResponseHeader: 25 * time.Millisecond}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stream":true}`))
		h.Close()
		want := 504
		if recover {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("response = %d %s", w.Code, w.Body.String())
		}
		if !recover && !strings.Contains(w.Body.String(), "upstream_response_timeout") {
			t.Fatal(w.Body.String())
		}
		_, reports := selector.snapshot()
		if reports[0].Class != scheduler.FailureChannelImmediate {
			t.Fatalf("reports = %#v", reports)
		}
		if got := events.snapshot(); got[1].EndReason != "response_header_timeout" {
			t.Fatalf("events = %#v", got)
		}
	}
}

func TestNonStreamingHeaderWaitAndFixedTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(60 * time.Millisecond):
			_, _ = io.WriteString(w, `{}`)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}, Options{UpstreamLimits: UpstreamLimits{ResponseHeader: 15 * time.Millisecond}})
	defer h.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if w.Code != 200 {
		t.Fatalf("non-stream = %d", w.Code)
	}
	plan := fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, config.ProtocolAnthropicMessages, patch.Plan{}))
	plan.PreparedRequest.Plan.Stream = true
	w = httptest.NewRecorder()
	h.forwardFixedExecution(w, fixedTestIncoming(""), nil, plan)
	if w.Code != 504 || h.FixedTargetDiagnostics().GatewayError != "upstream_response_timeout" {
		t.Fatalf("fixed = %d %#v", w.Code, h.FixedTargetDiagnostics())
	}
}

func TestHeaderTimeoutDiscardsLateResponse(t *testing.T) {
	h := NewWithOptions(nil, &fakeSelector{}, Options{UpstreamLimits: UpstreamLimits{ResponseHeader: time.Hour}})
	defer h.Close()
	attempt := h.newUpstreamAttempt(context.Background(), true)
	defer attempt.close()
	attempt.stop("response_header_timeout", "timeout", false)
	body := &observedCloseBody{Reader: strings.NewReader("late")}
	response, err := attempt.receiveHeaders(&http.Response{StatusCode: 200, Body: body}, nil)
	if response != nil || err == nil || !body.closed {
		t.Fatalf("late response accepted: %v %v", response, err)
	}
}

type observedCloseBody struct {
	io.Reader
	closed bool
}

func (b *observedCloseBody) Close() error { b.closed = true; return nil }
