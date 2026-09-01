package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type fakeSnapshot struct {
	revision           uint64
	gatewayKey         string
	attemptPolicy      scheduler.AttemptPolicy
	attemptPolicyCalls atomic.Int32
	providers          []*provider.CompiledProvider
	providerCalls      atomic.Int32
	scanOnce           sync.Once
	scanPaths          []string
	rawMarkers         []string
	fixedTarget        *provider.CompiledFixedTarget
	requestScan        *bodyfile.CompiledScanSpec
}

func (s *fakeSnapshot) Revision() uint64 { return s.revision }
func (s *fakeSnapshot) GatewayKey() string {
	return s.gatewayKey
}
func (s *fakeSnapshot) AttemptPolicy() scheduler.AttemptPolicy {
	s.attemptPolicyCalls.Add(1)
	if s.attemptPolicy == (scheduler.AttemptPolicy{}) {
		return scheduler.DefaultAttemptPolicy()
	}
	return s.attemptPolicy
}
func (s *fakeSnapshot) Candidates(model string) []*provider.CompiledProvider {
	var result []*provider.CompiledProvider
	for _, item := range s.providers {
		if item.SupportsModel(model) {
			result = append(result, item)
		}
	}
	return result
}
func (s *fakeSnapshot) Providers() []*provider.CompiledProvider {
	s.providerCalls.Add(1)
	return append([]*provider.CompiledProvider(nil), s.providers...)
}

func (s *fakeSnapshot) RequestScanSpec() *bodyfile.CompiledScanSpec {
	if s == nil {
		return nil
	}
	s.scanOnce.Do(func() {
		paths := append([]string(nil), s.scanPaths...)
		for _, item := range s.providers {
			if item == nil || !item.Enabled || len(item.Models) == 0 {
				continue
			}
			for _, requestType := range []traffic.RequestType{traffic.RequestTypeNormal, traffic.RequestTypeClassifier} {
				required, err := item.PatchPlan.RequiredPaths(patch.StageRequest, requestType)
				if err != nil {
					panic(err)
				}
				paths = appendFakeScanPaths(paths, required...)
			}
		}
		if s.fixedTarget != nil {
			required, err := s.fixedTarget.PatchPlan.RequiredPaths(patch.StageRequest, traffic.RequestTypeClassifier)
			if err != nil {
				panic(err)
			}
			paths = appendFakeScanPaths(paths, required...)
		}
		spec, err := bodyfile.RequestScanSpecWithRawMarkers(paths, s.rawMarkers...)
		if err != nil {
			panic(err)
		}
		s.requestScan, err = bodyfile.CompileScanSpec(spec)
		if err != nil {
			panic(err)
		}
	})
	return s.requestScan
}

func appendFakeScanPaths(paths []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(additions))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range additions {
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

type fakeSelector struct {
	mu       sync.Mutex
	leases   []scheduler.AttemptLease
	updates  []scheduler.HealthUpdate
	next     int
	reports  []scheduler.Outcome
	keys     []scheduler.StickyKey
	acquires int
	err      error
}

func (s *fakeSelector) Acquire(_ scheduler.Snapshot, key scheduler.StickyKey, _ map[string]struct{}) (scheduler.AttemptLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquires++
	s.keys = append(s.keys, key)
	if s.next >= len(s.leases) {
		if s.err != nil {
			return scheduler.AttemptLease{}, s.err
		}
		return scheduler.AttemptLease{}, errors.New("no provider")
	}
	lease := s.leases[s.next]
	s.next++
	return lease, nil
}

func (s *fakeSelector) stickyKeys() []scheduler.StickyKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scheduler.StickyKey(nil), s.keys...)
}
func (s *fakeSelector) Report(_ scheduler.AttemptLease, outcome scheduler.Outcome) scheduler.HealthUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, outcome)
	if index := len(s.reports) - 1; index < len(s.updates) {
		return s.updates[index]
	}
	return scheduler.HealthUpdate{}
}
func (*fakeSelector) Reconcile(scheduler.Snapshot) {}
func (*fakeSelector) Assignments(string) []scheduler.Assignment {
	return nil
}
func (*fakeSelector) ActiveAssignmentCount() int { return 0 }

func (s *fakeSelector) snapshot() (int, []scheduler.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquires, append([]scheduler.Outcome(nil), s.reports...)
}

type eventCollector struct {
	mu     sync.Mutex
	events []Event
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type cancelingResponseWriter struct {
	header      http.Header
	status      int
	headerCalls int
	writeCalls  int
	cancel      context.CancelFunc
	cancelOnce  sync.Once
	cancelOn    string
}

func (w *cancelingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	if w.cancelOn == "header" {
		w.cancelOnce.Do(w.cancel)
	}
	return w.header
}

func (w *cancelingResponseWriter) WriteHeader(status int) {
	w.headerCalls++
	if w.status == 0 {
		w.status = status
	}
	if w.cancelOn == "write_header" {
		w.cancelOnce.Do(w.cancel)
	}
}

func (w *cancelingResponseWriter) Write(data []byte) (int, error) {
	w.writeCalls++
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.cancelOn == "write" {
		w.cancelOnce.Do(w.cancel)
	}
	return len(data), nil
}

func (c *eventCollector) RecordGatewayEvent(event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *eventCollector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func compileTestProvider(t *testing.T, id, name, baseURL, key, model string, useXAPIKey bool) *provider.CompiledProvider {
	return compileTestProviderWithPatches(t, id, name, baseURL, key, model, useXAPIKey)
}

func compileTestProviderWithPatches(t *testing.T, id, name, baseURL, key, model string, useXAPIKey bool, patchIDs ...string) *provider.CompiledProvider {
	t.Helper()
	item, err := provider.Compile(config.ProviderConfig{
		ID:         id,
		Name:       name,
		BaseURL:    baseURL,
		APIKey:     key,
		Models:     []string{model},
		Enabled:    true,
		UseXAPIKey: useXAPIKey,
		Patches:    append([]string(nil), patchIDs...),
	}, testProviderRuntimeContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func testProviderRuntimeContext(t *testing.T) provider.RuntimeContext {
	t.Helper()
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func normalResponsePatchPlan(t *testing.T, response patch.ResponsePatch) patch.Plan {
	t.Helper()
	definition := patch.PatchDefinition{
		ID:            "normal-response-test",
		Name:          "Normal response test",
		Description:   "Exercises the normal buffered response boundary.",
		RequestTypes:  []patch.RequestType{patch.RequestTypeNormal},
		Stages:        []patch.Stage{patch.StageResponse},
		ResponsePaths: []string{},
		Idempotence:   patch.PerExecution,
		Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
			return patch.NewHooksInstance(patch.Hooks{Response: response}), nil
		},
	}
	registry, err := patch.NewRegistry([]patch.PatchDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Compile([]string{definition.ID}, patch.RequestTypeNormal)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRequestScanSpecUsesOnlyReachableProviderPlans(t *testing.T) {
	active := compileTestProviderWithPatches(t, "11111111-1111-4111-8111-111111111111", "active", "https://active.example", "key", "m", false, patch.AnyRouterSubagentThinkingID)
	disabled := compileTestProviderWithPatches(t, "22222222-2222-4222-8222-222222222222", "disabled", "https://disabled.example", "key", "m", false, patch.AnyRouterSubagentThinkingID)
	disabled.Enabled = false
	noModels := compileTestProviderWithPatches(t, "33333333-3333-4333-8333-333333333333", "no-models", "https://empty.example", "key", "m", false, patch.AnyRouterSubagentThinkingID)
	noModels.Models = nil
	snapshot := &fakeSnapshot{providers: []*provider.CompiledProvider{active, disabled, noModels}}
	if !fakeSnapshotRetainsPath(t, snapshot, "/thinking/type") {
		t.Fatal("active provider path missing")
	}
	// The active provider still contributes the path; remove it and ensure the
	// same disabled/no-model plans cannot do so on their own.
	active.Enabled = false
	snapshot = &fakeSnapshot{providers: []*provider.CompiledProvider{active, disabled, noModels}}
	if fakeSnapshotRetainsPath(t, snapshot, "/thinking/type") {
		t.Fatal("unreachable provider path retained")
	}
}

type fixedTargetScanSnapshot struct {
	*fakeSnapshot
	auto flow.AutoModeSnapshot
}

func (s *fixedTargetScanSnapshot) NormalAttemptPolicy() scheduler.AttemptPolicy {
	return s.AttemptPolicy()
}

func (s *fixedTargetScanSnapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy {
	return scheduler.DefaultClassifierAttemptPolicy()
}

func (s *fixedTargetScanSnapshot) AutoMode() flow.AutoModeSnapshot { return s.auto }

func TestRequestScanSpecIncludesFixedClassifierTargetRequestPaths(t *testing.T) {
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	plan, err := registry.Compile([]string{
		patch.AnyRouterClassifierRequestID,
		patch.GPTClassifierResponseReassemblyID,
	}, patch.RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	target := &provider.CompiledFixedTarget{PatchPlan: plan, Protocol: config.ProtocolOpenAIResponses}
	snapshot := &fixedTargetScanSnapshot{
		fakeSnapshot: &fakeSnapshot{revision: 1, gatewayKey: "gateway", fixedTarget: target},
		auto:         flow.AutoModeSnapshot{Mode: "fixed_provider", FixedTarget: target},
	}
	if !fakeSnapshotRetainsPath(t, snapshot.fakeSnapshot, "/system/0/text") || !fakeSnapshotRetainsPath(t, snapshot.fakeSnapshot, "/stop_sequences/0") {
		t.Fatal("fixed-target request paths missing")
	}
	for _, responsePath := range []string{"/type", "/content", "/content/0/text", "/stop_reason", "/stop_sequence"} {
		if fakeSnapshotRetainsPath(t, snapshot.fakeSnapshot, responsePath) {
			t.Fatalf("response path %q leaked into request scan", responsePath)
		}
	}
}

func fakeSnapshotRetainsPath(t *testing.T, snapshot *fakeSnapshot, path string) bool {
	t.Helper()
	input := `{"model":"m","thinking":{"type":"enabled"},"system":[{"text":"classifier"}],"stop_sequences":["STOP"],"type":"message","content":[{"text":"response"}],"stop_reason":"end_turn","stop_sequence":null}`
	body, index, err := bodyfile.CaptureAndScanCompiled(strings.NewReader(input), snapshot.RequestScanSpec(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, ok := index.Lookup(path)
	return ok
}

func leaseFor(item *provider.CompiledProvider, model string) scheduler.AttemptLease {
	return scheduler.AttemptLease{
		SnapshotRevision: 1,
		Provider:         item,
		Model:            model,
		RequestType:      traffic.RequestTypeNormal,
		Generation:       item.Generation,
	}
}

func gatewayRequest(method, target, auth, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	return request
}

func TestGatewayExactRouteMethodAndAuthentication(t *testing.T) {
	selector := &fakeSelector{}
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway-key"}
	handler := New(func() scheduler.Snapshot { return snapshot }, selector)
	t.Cleanup(func() { _ = handler.Close() })

	tests := []struct {
		name      string
		method    string
		target    string
		auth      string
		want      int
		challenge string
		allow     string
	}{
		{name: "unknown", method: http.MethodPost, target: "/other", want: http.StatusNotFound},
		{name: "encoded alias", method: http.MethodPost, target: "/v1/%6dessages", want: http.StatusNotFound},
		{name: "missing auth", method: http.MethodPost, target: MessagesPath, want: http.StatusUnauthorized, challenge: "Bearer"},
		{name: "management key", method: http.MethodPost, target: MessagesPath, auth: "Bearer management-key", want: http.StatusUnauthorized, challenge: "Bearer"},
		{name: "wrong scheme", method: http.MethodPost, target: MessagesPath, auth: "Basic gateway-key", want: http.StatusUnauthorized, challenge: "Bearer"},
		{name: "repeated space", method: http.MethodPost, target: MessagesPath, auth: "Bearer  gateway-key", want: http.StatusUnauthorized, challenge: "Bearer"},
		{name: "method", method: http.MethodGet, target: MessagesPath, auth: "bearer gateway-key", want: http.StatusMethodNotAllowed, allow: http.MethodPost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, gatewayRequest(test.method, test.target, test.auth, `{"model":"m"}`))
			if response.Code != test.want {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("WWW-Authenticate"); got != test.challenge {
				t.Fatalf("challenge = %q, want %q", got, test.challenge)
			}
			if got := response.Header().Get("Allow"); got != test.allow {
				t.Fatalf("Allow = %q, want %q", got, test.allow)
			}
		})
	}

	unconfigured := New(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1}
	}, selector)
	t.Cleanup(func() { _ = unconfigured.Close() })
	response := httptest.NewRecorder()
	unconfigured.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer anything", `{"model":"m"}`))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "gateway_not_configured") {
		t.Fatalf("unconfigured = %d %s", response.Code, response.Body.String())
	}
}

func TestGatewayParsesModelBeforeSelection(t *testing.T) {
	selector := &fakeSelector{}
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway-key"}
	handler := New(func() scheduler.Snapshot { return snapshot }, selector)
	t.Cleanup(func() { _ = handler.Close() })

	for _, body := range []string{
		`{`,
		`{"model":1}`,
		`{"model":""}`,
		`{"model":"m","model":"m"}`,
		`{"model":"m"}{}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway-key", body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d %s", body, response.Code, response.Body.String())
		}
	}
	overlong := httptest.NewRecorder()
	handler.ServeHTTP(overlong, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway-key", `{"model":"`+strings.Repeat("a", 257)+`"}`))
	if overlong.Code != http.StatusBadRequest || !strings.Contains(overlong.Body.String(), "invalid_model") || !strings.Contains(overlong.Body.String(), "256") {
		t.Fatalf("overlong model = %d %s", overlong.Code, overlong.Body.String())
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway-key", `{"model":"unknown","future":true}`))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "model_not_configured") {
		t.Fatalf("unknown model = %d %s", response.Code, response.Body.String())
	}
	if acquires, _ := selector.snapshot(); acquires != 0 {
		t.Fatalf("selector acquired %d times", acquires)
	}
}

func TestGatewayComposesURLCleansHeadersAndRecordsSession(t *testing.T) {
	for _, useXAPIKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("x_api_key_%v", useXAPIKey), func(t *testing.T) {
			var received struct {
				path, query, host string
				header            http.Header
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.path = r.URL.EscapedPath()
				received.query = r.URL.RawQuery
				received.host = r.Host
				received.header = r.Header.Clone()
				w.Header().Set("Connection", "X-Upstream-Hop")
				w.Header().Set("X-Upstream-Hop", "remove")
				w.Header().Set("X-End-To-End", "keep")
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL+"/api/", "provider-secret", "Model-X", useXAPIKey)
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "Model-X")}}
			events := &eventCollector{}
			handler := NewWithOptions(func() scheduler.Snapshot {
				return &fakeSnapshot{revision: 1, gatewayKey: "gateway-secret", providers: []*provider.CompiledProvider{item}}
			}, selector, Options{Recorder: events})
			defer handler.Close()

			request := gatewayRequest(http.MethodPost, MessagesPath+"?beta=one&beta=two", "Bearer gateway-secret", `{"model":"Model-X","metadata":{"user_id":"body-session"}}`)
			request.Host = "127.0.0.1:9999"
			request.Header.Set("X-Api-Key", "client-x-key")
			request.Header.Set("Connection", "X-Remove, Keep-Alive")
			request.Header.Set("X-Remove", "remove")
			request.Header.Set("Proxy-Authorization", "remove")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			request.Header.Set("X-Claude-Code-Session-Id", " header-session ")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != "ok" {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if received.path != "/api/v1/messages" || received.query != "beta=one&beta=two" {
				t.Fatalf("upstream URL = path %q query %q", received.path, received.query)
			}
			parsed, _ := url.Parse(upstream.URL)
			if received.host != parsed.Host {
				t.Fatalf("upstream Host = %q, want %q", received.host, parsed.Host)
			}
			if received.header.Get("Connection") != "" || received.header.Get("X-Remove") != "" || received.header.Get("Proxy-Authorization") != "" {
				t.Fatalf("hop headers reached upstream: %#v", received.header)
			}
			if received.header.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatal("end-to-end Anthropic header was removed")
			}
			if useXAPIKey {
				if received.header.Get("X-Api-Key") != "provider-secret" || received.header.Get("Authorization") != "" {
					t.Fatalf("provider auth = %#v", received.header)
				}
			} else if received.header.Get("Authorization") != "Bearer provider-secret" || received.header.Get("X-Api-Key") != "" {
				t.Fatalf("provider auth = %#v", received.header)
			}
			if response.Header().Get("X-Upstream-Hop") != "" || response.Header().Get("Connection") != "" || response.Header().Get("X-End-To-End") != "keep" {
				t.Fatalf("response headers = %#v", response.Header())
			}

			gotEvents := events.snapshot()
			if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventSuccess {
				t.Fatalf("events = %#v", gotEvents)
			}
			for _, event := range gotEvents {
				if event.SessionID != "header-session" {
					t.Fatalf("event session = %q", event.SessionID)
				}
			}
			_, reports := selector.snapshot()
			if len(reports) != 1 || reports[0].Class != scheduler.FailureNone || reports[0].SessionID != "header-session" {
				t.Fatalf("reports = %#v", reports)
			}
			if keys := selector.stickyKeys(); len(keys) != 1 || keys[0].SessionID != "header-session" {
				t.Fatalf("sticky keys = %#v", keys)
			}
		})
	}
}

func TestGatewaySessionUsesOnlyOneNonEmptyHeader(t *testing.T) {
	const body = `{"model":"m","metadata":{"user_id":"body-session"}}`
	for _, test := range []struct {
		name        string
		headers     []string
		wantSession string
	}{
		{name: "missing"},
		{name: "blank", headers: []string{"   "}},
		{name: "multiple", headers: []string{"session-one", "session-two"}},
		{name: "unique", headers: []string{" header-session "}, wantSession: "header-session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamBody string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
					return
				}
				upstreamBody = string(data)
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
			events := &eventCollector{}
			handler := NewWithOptions(func() scheduler.Snapshot {
				return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
			}, selector, Options{Recorder: events})
			defer handler.Close()

			request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", body)
			if test.headers != nil {
				request.Header["X-Claude-Code-Session-Id"] = append([]string(nil), test.headers...)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || upstreamBody != body {
				t.Fatalf("response=%d upstream body=%q", response.Code, upstreamBody)
			}
			keys := selector.stickyKeys()
			_, reports := selector.snapshot()
			gotEvents := events.snapshot()
			if len(keys) != 1 || keys[0].SessionID != test.wantSession {
				t.Fatalf("sticky keys = %#v", keys)
			}
			if len(reports) != 1 || reports[0].SessionID != test.wantSession {
				t.Fatalf("reports = %#v", reports)
			}
			if len(gotEvents) != 2 || gotEvents[0].SessionID != test.wantSession || gotEvents[1].SessionID != test.wantSession {
				t.Fatalf("events = %#v", gotEvents)
			}
		})
	}
}

func TestGatewayFailoverAndFinalResponseSemantics(t *testing.T) {
	statuses := []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusServiceUnavailable}
	bodies := []string{"first complete raw error", "second complete raw error", "third complete raw error"}
	providers := make([]*provider.CompiledProvider, 0, 3)
	for index := range statuses {
		status, body := statuses[index], bodies[index]
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Final", fmt.Sprintf("%d", status))
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "19")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		defer upstream.Close()
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1)
		providers = append(providers, compileTestProvider(t, id, fmt.Sprintf("p%d", index+1), upstream.URL, fmt.Sprintf("key-%d", index+1), "m", false))
	}
	leasing := make([]scheduler.AttemptLease, len(providers))
	for index, item := range providers {
		leasing[index] = leaseFor(item, "m")
	}
	cooldownUntil := time.Unix(2_000, 0).UTC()
	selector := &fakeSelector{
		leases: leasing,
		updates: []scheduler.HealthUpdate{{
			GlobalState:           scheduler.GlobalCooldown,
			ChannelState:          scheduler.ChannelUnknown,
			GlobalEnteredCooldown: true,
			CooldownUntil:         &cooldownUntil,
		}},
	}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: providers}
	}, selector, Options{Recorder: events, Now: func() time.Time { return time.Unix(1_000, 0) }})
	defer handler.Close()
	response := httptest.NewRecorder()
	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","metadata":{"user_id":"ignored-body-session"}}`)
	request.Header.Set("X-Claude-Code-Session-Id", "session-exact")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != bodies[2] || response.Header().Get("X-Final") != "503" {
		t.Fatalf("final response = %d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	acquires, reports := selector.snapshot()
	if acquires != 3 || len(reports) != 3 {
		t.Fatalf("acquires=%d reports=%d", acquires, len(reports))
	}
	if !reports[1].HasRetryAfter || reports[1].RetryAfter != 19*time.Second {
		t.Fatalf("Retry-After report = %#v", reports[1])
	}
	gotEvents := events.snapshot()
	wantKinds := []EventKind{EventForward, EventFailover, EventForward, EventFailover, EventForward, EventFailure}
	if len(gotEvents) != len(wantKinds) {
		t.Fatalf("events = %#v", gotEvents)
	}
	for index, event := range gotEvents {
		if event.Kind != wantKinds[index] {
			t.Fatalf("event %d kind = %q, want %q: %#v", index, event.Kind, wantKinds[index], gotEvents)
		}
		if event.SessionID != "session-exact" {
			t.Fatalf("event session = %q", event.SessionID)
		}
	}
	firstFailover, secondFailover, finalFailure := gotEvents[1], gotEvents[3], gotEvents[5]
	if firstFailover.ProviderID != providers[0].ID || firstFailover.Attempt != 1 || firstFailover.RawError != bodies[0] ||
		firstFailover.NextProviderID != providers[1].ID || firstFailover.NextProviderName != providers[1].Name || firstFailover.NextAttempt != 2 || firstFailover.NextUpstreamURL == "" {
		t.Fatalf("first failover = %#v", firstFailover)
	}
	if firstFailover.GlobalHealth != scheduler.GlobalCooldown || firstFailover.ChannelHealth != scheduler.ChannelUnknown ||
		!firstFailover.GlobalEnteredCooldown || firstFailover.CooldownUntil == nil || !firstFailover.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("first failover health = %#v", firstFailover)
	}
	if secondFailover.ProviderID != providers[1].ID || secondFailover.Attempt != 2 || secondFailover.RawError != bodies[1] ||
		secondFailover.NextProviderID != providers[2].ID || secondFailover.NextAttempt != 3 || secondFailover.NextUpstreamURL == "" {
		t.Fatalf("second failover = %#v", secondFailover)
	}
	if finalFailure.ProviderID != providers[2].ID || finalFailure.Attempt != 3 || finalFailure.RawError != bodies[2] ||
		finalFailure.NextProviderID != "" || finalFailure.NextAttempt != 0 || finalFailure.NextUpstreamURL != "" {
		t.Fatalf("final failure = %#v", finalFailure)
	}
}

func TestGatewayFinalRetryableFailureRecordsOnlyFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "final-provider-error")
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "only-provider", upstream.URL, "provider-key", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{Recorder: events})
	defer handler.Close()

	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`)
	request.Header.Set("X-Claude-Code-Session-Id", "final-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "final-provider-error" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	got := events.snapshot()
	if len(got) != 2 || got[0].Kind != EventForward || got[1].Kind != EventFailure {
		t.Fatalf("events = %#v", got)
	}
	failure := got[1]
	if failure.ProviderID != item.ID || failure.Attempt != 1 || failure.RawError != "final-provider-error" ||
		failure.NextProviderID != "" || failure.NextProviderName != "" || failure.NextAttempt != 0 || failure.NextUpstreamURL != "" {
		t.Fatalf("final failure event = %#v", failure)
	}
}

func TestGatewayCancellationAfterDoStopsBeforeResponseAndFailover(t *testing.T) {
	var secondCalls atomic.Int32
	first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "first", "https://first.invalid", "key-1", "m", false)
	second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "second", "https://second.invalid", "key-2", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
	events := &eventCollector{}
	pool := NewClientPool()
	ctx, cancel := context.WithCancel(context.Background())
	pool.clients[clientKey{providerID: first.ID, generation: first.Generation}] = &pooledClient{
		client: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("retryable")),
				Request:    request,
			}, nil
		})},
		transport: &http.Transport{},
	}
	pool.clients[clientKey{providerID: second.ID, generation: second.Generation}] = &pooledClient{
		client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			secondCalls.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unexpected"))}, nil
		})},
		transport: &http.Transport{},
	}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	}, selector, Options{ClientPool: pool, Recorder: events})
	defer handler.Close()
	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`)
	request = request.WithContext(ctx)
	w := &cancelingResponseWriter{}
	handler.ServeHTTP(w, request)

	acquires, reports := selector.snapshot()
	if w.headerCalls != 0 || w.writeCalls != 0 || secondCalls.Load() != 0 || acquires != 1 {
		t.Fatalf("writes=%d/%d second=%d acquires=%d", w.headerCalls, w.writeCalls, secondCalls.Load(), acquires)
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureClientCanceled || !reports[0].ClientCanceled {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 2 || got[0].Kind != EventForward || got[1].Kind != EventFailure || got[1].NextProviderID != "" {
		t.Fatalf("events = %#v", got)
	}
}

func TestStreamResponseCancellationBeforeWriteHeaderReportsOnce(t *testing.T) {
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", "https://one.invalid", "key", "m", false)
	selector := &fakeSelector{}
	events := &eventCollector{}
	handler := NewWithOptions(nil, selector, Options{Recorder: events})
	defer handler.Close()
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelingResponseWriter{cancel: cancel, cancelOn: "header"}
	handler.streamResponse(w, ctx, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Test": []string{"value"}},
		Body:       io.NopCloser(strings.NewReader("body")),
	}, leaseFor(item, "m"), scheduler.Outcome{Class: scheduler.FailureNone, UpstreamURL: "https://one.invalid/v1/messages", SessionID: "session"}, 1)

	_, reports := selector.snapshot()
	if w.headerCalls != 0 || w.writeCalls != 0 {
		t.Fatalf("canceled stream wrote response: headers=%d writes=%d", w.headerCalls, w.writeCalls)
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureClientCanceled || reports[0].ResponseStarted {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 1 || got[0].Kind != EventFailure {
		t.Fatalf("events = %#v", got)
	}
}

func TestBufferedResponseCancellationAfterPatchStopsBeforeWriteHeader(t *testing.T) {
	plan := normalResponsePatchPlan(t, fixedTestResponsePatch(func(_ patch.PatchContext, response *patch.MutableResponse) error {
		response.Status = http.StatusAccepted
		return nil
	}))
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", "https://one.invalid", "key", "m", false)
	item.PatchPlan = plan
	selector := &fakeSelector{}
	events := &eventCollector{}
	handler := NewWithOptions(nil, selector, Options{Recorder: events})
	defer handler.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, MessagesPath, nil).WithContext(ctx)
	w := &cancelingResponseWriter{cancel: cancel, cancelOn: "header"}
	execution, err := plan.NewInstance(patch.PatchContext{
		RequestType:    traffic.RequestTypeNormal,
		OriginalModel:  "m",
		EffectiveModel: "m",
		TargetID:       item.ID,
		Generation:     item.Generation.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := plan.ResponseScanSpec(traffic.RequestTypeNormal)
	if !ok {
		t.Fatal("compiled response scan missing")
	}
	handler.executeBufferedResponse(w, request, leaseFor(item, "m"), "session", 1, &http.Response{
		StatusCode: http.StatusCreated,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, execution, "https://one.invalid/v1/messages", spec)

	_, reports := selector.snapshot()
	if w.headerCalls != 0 || w.writeCalls != 0 {
		t.Fatalf("canceled buffered response wrote: headers=%d writes=%d", w.headerCalls, w.writeCalls)
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureClientCanceled || reports[0].ResponseStarted {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 1 || got[0].Kind != EventFailure {
		t.Fatalf("events = %#v", got)
	}
}

func TestGatewayCapturesAttemptPolicyOncePerRequest(t *testing.T) {
	for _, maxAttempts := range []int{2, 5} {
		t.Run(fmt.Sprintf("maximum_%d", maxAttempts), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := upstreamCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprintf(w, "failure-%d", call)
			}))
			defer upstream.Close()
			providers := make([]*provider.CompiledProvider, maxAttempts+1)
			leases := make([]scheduler.AttemptLease, len(providers))
			for index := range providers {
				providers[index] = compileTestProvider(t,
					fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1),
					fmt.Sprintf("provider-%d", index+1), upstream.URL, fmt.Sprintf("key-%d", index+1), "m", false)
				leases[index] = leaseFor(providers[index], "m")
			}
			selector := &fakeSelector{leases: leases}
			snapshot := &fakeSnapshot{
				revision:      1,
				gatewayKey:    "gateway",
				attemptPolicy: scheduler.AttemptPolicy{MaxAttempts: maxAttempts},
				providers:     providers,
			}
			handler := New(func() scheduler.Snapshot { return snapshot }, selector)
			defer handler.Close()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
			wantBody := fmt.Sprintf("failure-%d", maxAttempts)
			if response.Code != http.StatusInternalServerError || response.Body.String() != wantBody {
				t.Fatalf("final response = %d %q, want %q", response.Code, response.Body.String(), wantBody)
			}
			acquires, reports := selector.snapshot()
			if acquires != maxAttempts || len(reports) != maxAttempts || upstreamCalls.Load() != int32(maxAttempts) {
				t.Fatalf("acquires=%d reports=%d upstream=%d", acquires, len(reports), upstreamCalls.Load())
			}
			if calls := snapshot.attemptPolicyCalls.Load(); calls != 1 {
				t.Fatalf("AttemptPolicy() calls = %d", calls)
			}
		})
	}
}

func TestGatewayTransportFailuresFailOverWithCompleteDiagnostics(t *testing.T) {
	failures := []struct {
		name string
		err  error
		text string
	}{
		{name: "dns", err: &net.DNSError{Err: "resolver unavailable", Name: "upstream.invalid", IsTemporary: true}, text: "resolver unavailable"},
		{name: "tls", err: errors.New("tls: handshake failure from fake upstream"), text: "tls: handshake failure from fake upstream"},
		{name: "connection", err: errors.New("dial tcp 127.0.0.1: connection refused by fake upstream"), text: "connection refused by fake upstream"},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "fallback-success")
			}))
			defer success.Close()
			first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "failing", "https://upstream.invalid", "key-1", "m", false)
			second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "fallback", success.URL, "key-2", "m", false)
			selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
			events := &eventCollector{}
			pool := NewClientPool()
			pool.clients[clientKey{providerID: first.ID, generation: first.Generation}] = &pooledClient{
				client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, failure.err
				})},
				transport: &http.Transport{},
			}
			handler := NewWithOptions(func() scheduler.Snapshot {
				return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
			}, selector, Options{ClientPool: pool, Recorder: events})
			defer handler.Close()

			response := httptest.NewRecorder()
			request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","metadata":{"user_id":"ignored-body-session"}}`)
			request.Header.Set("X-Claude-Code-Session-Id", "transport-session")
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != "fallback-success" {
				t.Fatalf("fallback response = %d %q", response.Code, response.Body.String())
			}
			_, reports := selector.snapshot()
			if len(reports) != 2 || reports[0].Class != scheduler.FailureGlobalTransient || !strings.Contains(reports[0].RawError, failure.text) || reports[1].Class != scheduler.FailureNone {
				t.Fatalf("transport reports = %#v", reports)
			}
			gotEvents := events.snapshot()
			if len(gotEvents) != 4 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailover ||
				gotEvents[2].Kind != EventForward || gotEvents[3].Kind != EventSuccess ||
				!strings.Contains(gotEvents[1].RawError, failure.text) || gotEvents[1].SessionID != "transport-session" ||
				gotEvents[1].ProviderID != first.ID || gotEvents[1].NextProviderID != second.ID || gotEvents[1].NextAttempt != 2 {
				t.Fatalf("transport events = %#v", gotEvents)
			}
		})
	}
}

func TestGatewayDoesNotFollowRedirectAndMapsFinalContractError(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL+"/credential-target")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "unredacted redirect failure")
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "redirect", upstream.URL, "provider-key", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if redirected.Load() {
		t.Fatal("provider redirect was followed")
	}
	if response.Code != http.StatusBadGateway || response.Header().Get("Location") != "" {
		t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	_, reports := selector.snapshot()
	if len(reports) != 1 || reports[0].Class != scheduler.FailureGlobalImmediate || reports[0].RawError != "unredacted redirect failure" {
		t.Fatalf("reports = %#v", reports)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailure ||
		gotEvents[1].RawError != "unredacted redirect failure" || gotEvents[1].NextProviderID != "" || gotEvents[1].NextAttempt != 0 {
		t.Fatalf("events = %#v", gotEvents)
	}
}

type retryAtError struct {
	at time.Time
}

func (e retryAtError) Error() string                  { return "unavailable" }
func (e retryAtError) RetryAtTime() (time.Time, bool) { return e.at, true }

func TestGatewayUnavailableIncludesRetryAfter(t *testing.T) {
	now := time.Unix(2_000, 0)
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", "https://provider.invalid", "key", "m", false)
	selector := &fakeSelector{err: retryAtError{at: now.Add(1500 * time.Millisecond)}}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{Now: func() time.Time { return now }})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("response = %d Retry-After=%q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestClientPoolIsGenerationScopedAndDisablesAutomaticEncoding(t *testing.T) {
	first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", "https://one.invalid", "key", "m", false)
	second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", "https://two.invalid", "key", "m", false)
	pool := NewClientPool()
	defer pool.Close()
	clientA, err := pool.Client(first)
	if err != nil {
		t.Fatal(err)
	}
	clientAgain, err := pool.Client(first)
	if err != nil || clientA != clientAgain {
		t.Fatal("same generation did not reuse the client")
	}
	clientB, err := pool.Client(second)
	if err != nil || clientA == clientB {
		t.Fatal("different generation reused the client")
	}
	transport, ok := clientA.Transport.(*http.Transport)
	if !ok || !transport.DisableCompression || clientA.Timeout != 0 || transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("client transport = %#v", clientA.Transport)
	}
	pool.Reconcile([]*provider.CompiledProvider{second})
	pool.mu.Lock()
	_, hasFirst := pool.clients[clientKey{providerID: first.ID, generation: first.Generation}]
	_, hasSecond := pool.clients[clientKey{providerID: second.ID, generation: second.Generation}]
	pool.mu.Unlock()
	if hasFirst || !hasSecond {
		t.Fatalf("reconciled generations first=%v second=%v", hasFirst, hasSecond)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{value: "19", want: 19 * time.Second, valid: true},
		{value: now.Add(2 * time.Minute).Format(http.TimeFormat), want: 2 * time.Minute, valid: true},
		{value: "999999999999999999999999999", want: time.Duration(1<<63 - 1), valid: true},
		{value: "invalid", valid: false},
	}
	for _, test := range tests {
		headers := http.Header{"Retry-After": []string{test.value}}
		if got := parseRetryAfter(headers, now); got != test.want {
			t.Fatalf("%q duration = %v, want %v", test.value, got, test.want)
		}
		if got := hasValidRetryAfter(headers); got != test.valid {
			t.Fatalf("%q valid = %v", test.value, got)
		}
	}
	multiple := http.Header{"Retry-After": []string{"1", "2"}}
	if hasValidRetryAfter(multiple) {
		t.Fatal("multiple Retry-After values were accepted")
	}
}

func TestUpstreamURLAlwaysAppendsMessagesPathAndPreservesRawClientQuery(t *testing.T) {
	for _, test := range []struct {
		name     string
		base     string
		incoming string
		want     string
	}{
		{
			name:     "escaped prefix and repeated encoded query",
			base:     "https://example.test/prefix%2Ffixed/",
			incoming: MessagesPath + "?x=%2F&x=a+b",
			want:     "https://example.test/prefix%2Ffixed/v1/messages?x=%2F&x=a+b",
		},
		{
			name:     "existing suffix remains a base path and empty query is retained",
			base:     "https://example.test/v1/messages",
			incoming: MessagesPath + "?",
			want:     "https://example.test/v1/messages/v1/messages?",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, err := url.Parse(test.base)
			if err != nil {
				t.Fatal(err)
			}
			requestURL, err := url.Parse(test.incoming)
			if err != nil {
				t.Fatal(err)
			}
			result, err := upstreamURL(&provider.CompiledProvider{CompiledTarget: provider.CompiledTarget{BaseURL: base, Protocol: config.ProtocolAnthropicMessages}}, requestURL)
			if err != nil {
				t.Fatal(err)
			}
			if result.String() != test.want || result.RawQuery != requestURL.RawQuery || result.ForceQuery != requestURL.ForceQuery {
				t.Fatalf("URL = %q raw=%q force=%v, want %q", result.String(), result.RawQuery, result.ForceQuery, test.want)
			}
		})
	}
}

func TestGatewayNeutralErrorIsReturnedAndRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "complete request error")
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if response.Code != http.StatusBadRequest || response.Body.String() != "complete request error" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	_, reports := selector.snapshot()
	if len(reports) != 1 || reports[0].Class != scheduler.FailureNeutral || reports[0].RawError != "complete request error" || !reports[0].ResponseStarted {
		t.Fatalf("reports = %#v", reports)
	}
	if got := events.snapshot(); len(got) != 2 || got[1].RawError != "complete request error" {
		t.Fatalf("events = %#v", got)
	}
}

func TestGatewaySuccessDoesNotRecordResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, bytes.NewBufferString("successful body must only stream"))
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	got := events.snapshot()
	if len(got) != 2 || got[1].Kind != EventSuccess || got[1].RawError != "" {
		t.Fatalf("events = %#v", got)
	}
}

func TestGatewayMidStreamFailureIsNotRetried(t *testing.T) {
	var secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		// Closing this handler's body before a clean EOF simulates an upstream
		// stream interruption after response bytes became visible.
		return
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "second")
	}))
	defer second.Close()
	firstItem := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "first", first.URL, "key-1", "m", false)
	secondItem := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "second", second.URL, "key-2", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(firstItem, "m"), leaseFor(secondItem, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{firstItem, secondItem}}
	}, selector, Options{Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if response.Code != http.StatusOK || response.Body.String() != "data: first\n\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if secondCalls.Load() != 0 {
		t.Fatal("gateway retried after response stream had started")
	}
	_, reports := selector.snapshot()
	if len(reports) != 1 || reports[0].Class != scheduler.FailureChannelStream || !reports[0].ResponseStarted {
		t.Fatalf("reports = %#v", reports)
	}
}

func TestGatewayReplaysMoreThan64MiBByteForByteAcrossThreeProviders(t *testing.T) {
	const bodySize = (64 << 20) + 12345
	type observation struct {
		size int64
		sum  [sha256.Size]byte
	}
	observations := make([]observation, 3)
	servers := make([]*httptest.Server, 0, 3)
	providers := make([]*provider.CompiledProvider, 0, 3)
	for index := 0; index < 3; index++ {
		index := index
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			digest := sha256.New()
			n, err := io.Copy(digest, r.Body)
			if err != nil {
				t.Errorf("provider %d read body: %v", index+1, err)
				return
			}
			observations[index] = observation{size: n}
			copy(observations[index].sum[:], digest.Sum(nil))
			if r.ContentLength != bodySize {
				t.Errorf("provider %d Content-Length = %d, want %d", index+1, r.ContentLength, bodySize)
			}
			if index < 2 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "retry-"+fmt.Sprint(index+1))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "complete")
		}))
		servers = append(servers, server)
		var patchIDs []string
		if index != 1 {
			patchIDs = []string{patch.AnyRouterSubagentThinkingID}
		}
		providers = append(providers, compileTestProviderWithPatches(t,
			fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1),
			fmt.Sprintf("provider-%d", index+1), server.URL, fmt.Sprintf("key-%d", index+1), "large-model", false, patchIDs...))
	}
	for _, server := range servers {
		server := server
		t.Cleanup(server.Close)
	}

	largeBody := func() io.Reader { return &largeJSONReader{payloadRemaining: bodySize - largeJSONOverhead} }
	expectedOriginal := sha256.New()
	if _, err := io.Copy(expectedOriginal, largeBody()); err != nil {
		t.Fatal(err)
	}
	var expectedOriginalSum [sha256.Size]byte
	copy(expectedOriginalSum[:], expectedOriginal.Sum(nil))
	expectedPatched := sha256.New()
	if _, err := io.Copy(expectedPatched, &largeJSONReader{
		prefix:           strings.Replace(largeJSONPrefix, `"disabled"`, `"adaptive"`, 1),
		payloadRemaining: bodySize - largeJSONOverhead,
	}); err != nil {
		t.Fatal(err)
	}
	var expectedPatchedSum [sha256.Size]byte
	copy(expectedPatchedSum[:], expectedPatched.Sum(nil))
	leases := make([]scheduler.AttemptLease, 3)
	for index, item := range providers {
		leases[index] = leaseFor(item, "large-model")
	}
	selector := &fakeSelector{leases: leases}
	tempDir := t.TempDir()
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: providers}
	}, selector, Options{ReplayDirectory: tempDir})
	t.Cleanup(func() { _ = handler.Close() })
	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", "")
	request.Body = io.NopCloser(largeBody())
	request.ContentLength = bodySize
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "complete" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	for index, got := range observations {
		want := expectedPatchedSum
		if index == 1 {
			want = expectedOriginalSum
		}
		if got.size != bodySize || got.sum != want {
			t.Fatalf("provider %d observation = size %d hash %x, want size %d hash %x", index+1, got.size, got.sum, bodySize, want)
		}
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replay files remain after request: %v", entries)
	}
}

const largeJSONPrefix = `{"model":"large-model","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"`
const largeJSONSuffix = `"}]}`
const largeJSONOverhead = int64(len(largeJSONPrefix) + len(largeJSONSuffix))

type largeJSONReader struct {
	prefix           string
	prefixPosition   int
	payloadRemaining int64
	payloadPosition  int64
	suffixPosition   int
}

func (r *largeJSONReader) Read(p []byte) (int, error) {
	prefix := r.prefix
	if prefix == "" {
		prefix = largeJSONPrefix
	}
	if r.prefixPosition < len(prefix) {
		n := copy(p, prefix[r.prefixPosition:])
		r.prefixPosition += n
		return n, nil
	}
	if r.payloadRemaining > 0 {
		count := len(p)
		if int64(count) > r.payloadRemaining {
			count = int(r.payloadRemaining)
		}
		for i := 0; i < count; i++ {
			p[i] = byte('a' + (r.payloadPosition+int64(i))%26)
		}
		r.payloadPosition += int64(count)
		r.payloadRemaining -= int64(count)
		return count, nil
	}
	if r.suffixPosition < len(largeJSONSuffix) {
		n := copy(p, largeJSONSuffix[r.suffixPosition:])
		r.suffixPosition += n
		return n, nil
	}
	return 0, io.EOF
}

func TestGatewayStreamsLargeResponseWithoutBuffering(t *testing.T) {
	const responseSize = (64 << 20) + 54321
	firstWrite := make(chan struct{})
	allowFinish := make(chan struct{})
	var upstreamFinished atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		chunk := bytes.Repeat([]byte("z"), 256*1024)
		remaining := int64(responseSize)
		for remaining > 0 {
			count := len(chunk)
			if int64(count) > remaining {
				count = int(remaining)
			}
			if _, err := w.Write(chunk[:count]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			remaining -= int64(count)
			if remaining == int64(responseSize)-int64(len(chunk)) {
				select {
				case <-allowFinish:
				case <-time.After(5 * time.Second):
					return
				}
			}
		}
		upstreamFinished.Store(true)
	}))
	defer upstream.Close()
	item := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "stream", upstream.URL, "provider-key", "stream-model", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "stream-model")}}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, Options{})
	defer handler.Close()
	destination := &countingWriter{firstWrite: firstWrite}
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(destination, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"stream-model"}`))
		close(done)
	}()
	select {
	case <-firstWrite:
		if upstreamFinished.Load() {
			t.Fatal("gateway waited for complete upstream response before first downstream write")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not observe first streamed response write")
	}
	close(allowFinish)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("large response did not finish")
	}
	if destination.status != http.StatusOK || destination.bytes != responseSize {
		t.Fatalf("downstream status=%d bytes=%d, want status=%d bytes=%d", destination.status, destination.bytes, http.StatusOK, responseSize)
	}
}

type patternReader struct {
	remaining int64
	position  int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for index := range p {
		p[index] = byte((r.position + int64(index)) % 251)
	}
	r.position += int64(len(p))
	r.remaining -= int64(len(p))
	return len(p), nil
}

type countingWriter struct {
	header     http.Header
	status     int
	bytes      int64
	firstWrite chan struct{}
	once       sync.Once
}

func (w *countingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *countingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.once.Do(func() {
		if w.firstWrite != nil {
			close(w.firstWrite)
		}
	})
	w.bytes += int64(len(p))
	return len(p), nil
}

func (w *countingWriter) Flush() {}
