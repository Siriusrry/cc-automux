package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestRequestStreamFactsAndEvents(t *testing.T) {
	for _, test := range []struct {
		fields string
		want   bool
	}{
		{``, false}, {`,"stream":true`, true}, {`,"stream":false`, false},
		{`,"stream":null`, false}, {`,"stream":"true"`, false},
		{`,"stream":1`, false}, {`,"stream":{}`, false}, {`,"stream":[]`, false},
		{`,"stream":true,"stream":false`, false}, {`,"stream":false,"stream":true`, true},
		{`,"stream":true,"stream":null`, false}, {`,"nested":{"stream":true}`, false},
		{`,"str\u0065am":true`, true},
	} {
		t.Run(test.fields, func(t *testing.T) {
			input := `{"model":"m"` + test.fields + `}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != input {
					t.Errorf("upstream request = %q, %v", data, err)
				}
				if r.Header.Get("Authorization") == "Bearer fail" {
					w.WriteHeader(503)
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "fail", "m", false)
			second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", upstream.URL, "key", "m", false)
			// A failed first attempt and successful second attempt share the same ingress fact.
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
			events := &eventCollector{}
			snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
			handler := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events})
			defer handler.Close()
			handler.ServeHTTP(httptest.NewRecorder(), gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", input))
			got := waitGatewayEvents(t, events, 4)
			if len(got) != 4 {
				t.Fatalf("events = %#v", got)
			}
			for _, event := range got {
				if event.Stream != test.want {
					t.Errorf("stream = %v, want %v: %#v", event.Stream, test.want, event)
				}
			}
		})
	}
}

func TestClassifierAndUnconfiguredModelStream(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"retry"}`)
		}))
		defer upstream.Close()
		target := fixedTestTarget(t, upstream.URL, config.ProtocolAnthropicMessages, patch.Plan{})
		snapshot := &flowAwareSnapshot{classifierAttempts: scheduler.DefaultClassifierAttemptPolicy(), fakeSnapshot: &fakeSnapshot{revision: 1, gatewayKey: "gateway", scanPaths: []string{"/system", "/system/0", "/system/0/text"}, rawMarkers: []string{automode.SecurityMarker}}, auto: flow.AutoModeSnapshot{Mode: config.AutoModeProviderPool, ClassifierModel: "m"}}
		if fixed {
			snapshot.auto.Mode = config.AutoModeFixedProvider
			snapshot.auto.FixedTarget = target
		}
		registry, err := flow.NewRegistry(flow.NewNormalPlanner(), automode.NewClassifierPlanner())
		if err != nil {
			t.Fatal(err)
		}
		detectors, err := traffic.NewRegistry(automode.NewClassifierDetector())
		if err != nil {
			t.Fatal(err)
		}
		events := &eventCollector{}
		handler := NewWithOptions(func() scheduler.Snapshot { return snapshot }, &fakeSelector{}, Options{Recorder: events, DetectorRegistry: detectors, FlowDispatcher: flow.NewDispatcher(registry)})
		defer handler.Close()
		input := `{"model":"original","stream":true,"system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", input))
		got := events.snapshot()
		if len(got) == 0 {
			t.Fatalf("no events: %d %s", response.Code, response.Body.String())
		}
		for _, event := range got {
			if !event.Stream || event.RequestType != traffic.RequestTypeClassifier || len(event.TraceID) != 32 {
				t.Fatalf("event = %#v", event)
			}
		}
		if !fixed && (response.Code != 404 || !strings.Contains(response.Body.String(), "model_not_configured")) {
			t.Fatalf("unconfigured response = %d %s", response.Code, response.Body.String())
		}
		if fixed && response.Code != 429 {
			t.Fatalf("fixed response = %d %s", response.Code, response.Body.String())
		}
	}
}

func TestRequestStreamDoesNotOpenBody(t *testing.T) {
	snapshot := &fakeSnapshot{}
	body, index, err := bodyfile.CaptureAndScanCompiled(strings.NewReader(`{"model":"m","stream":false,"stream":true}`), snapshot.RequestScanSpec(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	// The sealed body is already closed; opening it would fail.
	if !requestStream(index) {
		t.Fatal("stream fact was not retained in the index")
	}
}
