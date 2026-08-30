package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type requestPatchFunc func(patch.PatchContext, *patch.MutableRequest) error

func (f requestPatchFunc) ApplyRequest(context patch.PatchContext, request *patch.MutableRequest) error {
	return f(context, request)
}

type responsePatchFunc func(patch.PatchContext, *patch.MutableResponse) error

func (f responsePatchFunc) ApplyResponse(context patch.PatchContext, response *patch.MutableResponse) error {
	return f(context, response)
}

func testRequestPatchRegistry(t *testing.T, id string, hook func(patch.FactoryContext) patch.RequestPatch) patch.Registry {
	return testTypedRequestPatchRegistry(t, id, patch.RequestTypeNormal, hook)
}

func testTypedRequestPatchRegistry(t *testing.T, id string, requestType patch.RequestType, hook func(patch.FactoryContext) patch.RequestPatch) patch.Registry {
	t.Helper()
	registry, err := patch.NewRegistry([]patch.PatchDefinition{{
		ID:           id,
		Name:         id,
		Description:  "focused gateway integration patch",
		RequestTypes: []patch.RequestType{requestType},
		Stages:       []patch.Stage{patch.StageRequest},
		Conflicts:    []string{},
		Idempotence:  patch.PerExecution,
		Factory: func(context patch.FactoryContext) (patch.PatchInstance, error) {
			return patch.NewHooksInstance(patch.Hooks{Request: hook(context)}), nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func compileWithRegistry(t *testing.T, registry patch.Registry, id, name, baseURL, key, model string, patchIDs ...string) *provider.CompiledProvider {
	t.Helper()
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	item, err := provider.Compile(config.ProviderConfig{
		ID:      id,
		Name:    name,
		BaseURL: baseURL,
		APIKey:  key,
		Models:  []string{model},
		Enabled: true,
		Patches: append([]string(nil), patchIDs...),
	}, context)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestGatewayRequestPatchCannotObserveOrOverrideCredentials(t *testing.T) {
	const patchID = "test-credential-boundary"
	registry := testRequestPatchRegistry(t, patchID, func(patch.FactoryContext) patch.RequestPatch {
		return requestPatchFunc(func(_ patch.PatchContext, request *patch.MutableRequest) error {
			if _, ok := request.Headers.Get("Authorization"); ok {
				return errors.New("patch observed Authorization")
			}
			if _, ok := request.Headers.Get("X-Api-Key"); ok {
				return errors.New("patch observed X-Api-Key")
			}
			request.Headers.Set("Authorization", "Bearer patch-value")
			request.Headers.Set("X-Api-Key", "patch-value")
			return nil
		})
	})
	var received http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		received = request.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	item := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "patched", upstream.URL, "provider-secret", "m", patchID)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(item, "m")}}
	handler := New(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway-secret", providers: []*provider.CompiledProvider{item}}
	}, selector)
	defer handler.Close()

	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway-secret", `{"model":"m"}`)
	request.Header.Set("X-Api-Key", "client-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if received.Get("Authorization") != "Bearer provider-secret" || received.Get("X-Api-Key") != "" {
		t.Fatalf("upstream credentials = %#v", received)
	}
}

func TestGatewayRequestPatchFailureFailsOverWithOneTerminalEvent(t *testing.T) {
	const patchID = "test-semantic-failure"
	registry := testRequestPatchRegistry(t, patchID, func(patch.FactoryContext) patch.RequestPatch {
		return requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error {
			return errors.New("semantic request patch failure")
		})
	})
	var firstCalls atomic.Int32
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		firstCalls.Add(1)
	}))
	defer firstUpstream.Close()
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fallback")
	}))
	defer secondUpstream.Close()
	first := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "first", firstUpstream.URL, "key-a", "m", patchID)
	second := compileWithRegistry(t, registry, "22222222-2222-4222-8222-222222222222", "second", secondUpstream.URL, "key-b", "m")
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	}, selector, Options{Recorder: events})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	if response.Code != http.StatusOK || response.Body.String() != "fallback" || firstCalls.Load() != 0 {
		t.Fatalf("response=%d %q first_calls=%d", response.Code, response.Body.String(), firstCalls.Load())
	}
	_, reports := selector.snapshot()
	if len(reports) != 2 || reports[0].Class != scheduler.FailureNeutral || reports[1].Class != scheduler.FailureNone {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 3 || got[0].Kind != EventFailover || got[1].Kind != EventForward || got[2].Kind != EventSuccess {
		t.Fatalf("events = %#v", got)
	}
	if got[0].PatchID != patchID || got[0].PatchStage != string(patch.StageRequest) || got[0].NextProviderID != second.ID || got[0].UpstreamURL == "" {
		t.Fatalf("patch failover = %#v", got[0])
	}
	for _, event := range got {
		if event.Kind == EventFailure && event.ProviderID == first.ID {
			t.Fatalf("failed attempt emitted both failure and failover: %#v", got)
		}
	}
}

type closeErrorPatchInstance struct {
	request patch.RequestPatch
}

func (i *closeErrorPatchInstance) RequestPatch() patch.RequestPatch { return i.request }
func (*closeErrorPatchInstance) ResponsePatch() patch.ResponsePatch { return nil }
func (*closeErrorPatchInstance) Close() error                       { return errors.New("patch instance close failed") }

func TestGatewayPatchCloseFailureIsNotClassifiedAsBodyfileFailure(t *testing.T) {
	const patchID = "test-close-failure"
	registry, err := patch.NewRegistry([]patch.PatchDefinition{{
		ID:           patchID,
		Name:         patchID,
		Description:  "fails while closing a request-scoped patch instance",
		RequestTypes: []patch.RequestType{patch.RequestTypeNormal},
		Stages:       []patch.Stage{patch.StageRequest},
		Conflicts:    []string{},
		Idempotence:  patch.PerExecution,
		Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
			return &closeErrorPatchInstance{request: requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error {
				return nil
			})}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "retryable upstream failure")
	}))
	defer firstUpstream.Close()
	var secondCalls atomic.Int32
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondCalls.Add(1)
	}))
	defer secondUpstream.Close()
	first := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "first", firstUpstream.URL, "key-a", "m", patchID)
	second := compileWithRegistry(t, registry, "22222222-2222-4222-8222-222222222222", "second", secondUpstream.URL, "key-b", "m")
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	}, selector, Options{Recorder: events})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	acquires, reports := selector.snapshot()
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "patch_failed") || strings.Contains(response.Body.String(), "replay_unavailable") || acquires != 1 || secondCalls.Load() != 0 {
		t.Fatalf("response=%d %s acquires=%d second_calls=%d", response.Code, response.Body.String(), acquires, secondCalls.Load())
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureNeutral || !strings.Contains(reports[0].RawError, "patch instance close failed") {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 2 || got[0].Kind != EventForward || got[1].Kind != EventFailure || got[1].PatchStage != string(patch.StageRequest) {
		t.Fatalf("events = %#v", got)
	}
}

func TestGatewayBodyfilePatchFailureTerminatesWithoutFailover(t *testing.T) {
	const patchID = "test-local-io-failure"
	registry := testRequestPatchRegistry(t, patchID, func(patch.FactoryContext) patch.RequestPatch {
		return requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error {
			return &bodyfile.LocalIOError{Operation: bodyfile.LocalIOWrite, Err: errors.New("private temp path must not leak")}
		})
	})
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	first := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "first", upstream.URL, "key-a", "m", patchID)
	second := compileWithRegistry(t, registry, "22222222-2222-4222-8222-222222222222", "second", upstream.URL, "key-b", "m")
	selector := &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	}, selector, Options{Recorder: events})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	acquires, reports := selector.snapshot()
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "replay_unavailable") || acquires != 1 || upstreamCalls.Load() != 0 {
		t.Fatalf("response=%d %s acquires=%d calls=%d", response.Code, response.Body.String(), acquires, upstreamCalls.Load())
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureNeutral || reports[0].RawError != bodyfile.ErrLocalIO.Error() {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 1 || got[0].Kind != EventFailure || got[0].PatchID != patchID || got[0].PatchStage != string(patch.StageRequest) || strings.Contains(got[0].RawError, "private temp path") {
		t.Fatalf("events = %#v", got)
	}
}

type classifierDetector struct{}

func (classifierDetector) Type() traffic.RequestType                { return traffic.RequestTypeClassifier }
func (classifierDetector) Detect(traffic.RequestView) (bool, error) { return true, nil }

type classifierPlanner struct{}

func (classifierPlanner) RequestType() traffic.RequestType { return traffic.RequestTypeClassifier }

func (classifierPlanner) Build(_ context.Context, snapshot flow.SnapshotView, ingress traffic.IngressRequest) (flow.ExecutionPlan, error) {
	plan, err := traffic.NewRequestPlan(ingress.OriginalModel, ingress.OriginalModel, ingress.OriginalSessionID, traffic.RequestTypeClassifier)
	if err != nil {
		return flow.ExecutionPlan{}, err
	}
	prepared, err := traffic.NewPreparedRequest(plan, ingress.CapturedBody, ingress.CapturedIndex)
	if err != nil {
		return flow.ExecutionPlan{}, err
	}
	return flow.ExecutionPlan{PreparedRequest: prepared, TargetMode: flow.TargetModeProviderPool, AttemptPolicy: snapshot.ClassifierAttemptPolicy()}, nil
}

type retainedUnionClassifierPlanner struct{}

func (retainedUnionClassifierPlanner) RequestType() traffic.RequestType {
	return traffic.RequestTypeClassifier
}

func (retainedUnionClassifierPlanner) Build(ctx context.Context, snapshot flow.SnapshotView, ingress traffic.IngressRequest) (flow.ExecutionPlan, error) {
	for _, path := range []string{"/normal_marker", "/classifier_marker"} {
		if _, ok := ingress.CapturedIndex.Lookup(path); !ok {
			return flow.ExecutionPlan{}, fmt.Errorf("ingress request union lost %s", path)
		}
	}
	for _, path := range []string{"/response_only", "/unrelated"} {
		if _, ok := ingress.CapturedIndex.Lookup(path); ok {
			return flow.ExecutionPlan{}, fmt.Errorf("ingress request index retained %s", path)
		}
	}
	return (classifierPlanner{}).Build(ctx, snapshot, ingress)
}

type flowAwareSnapshot struct {
	*fakeSnapshot
	classifierAttempts scheduler.AttemptPolicy
	auto               flow.AutoModeSnapshot
}

func (s *flowAwareSnapshot) NormalAttemptPolicy() scheduler.AttemptPolicy {
	return s.AttemptPolicy()
}

func (s *flowAwareSnapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy {
	return s.classifierAttempts
}

func (s *flowAwareSnapshot) AutoMode() flow.AutoModeSnapshot { return s.auto }

type snapshotCheckingClassifierPlanner struct {
	wantAttempts scheduler.AttemptPolicy
	wantAuto     flow.AutoModeSnapshot
}

func (snapshotCheckingClassifierPlanner) RequestType() traffic.RequestType {
	return traffic.RequestTypeClassifier
}

func (p snapshotCheckingClassifierPlanner) Build(ctx context.Context, snapshot flow.SnapshotView, ingress traffic.IngressRequest) (flow.ExecutionPlan, error) {
	if snapshot.ClassifierAttemptPolicy() != p.wantAttempts || snapshot.AutoMode() != p.wantAuto {
		return flow.ExecutionPlan{}, errors.New("planner received a masked flow snapshot")
	}
	return (classifierPlanner{}).Build(ctx, snapshot, ingress)
}

func classifierGatewayOptions(t *testing.T, recorder EventRecorder, directory string) Options {
	t.Helper()
	detectors, err := traffic.NewRegistry(classifierDetector{})
	if err != nil {
		t.Fatal(err)
	}
	flows, err := flow.NewRegistry(classifierPlanner{})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Recorder: recorder, ReplayDirectory: directory, DetectorRegistry: detectors, FlowDispatcher: flow.NewDispatcher(flows)}
}

func TestGatewayRetainsIngressRequestUnionWithoutPostClassificationProjection(t *testing.T) {
	definitions := []patch.PatchDefinition{
		{
			ID: "normal-request-field", Name: "normal-request-field",
			RequestTypes: []patch.RequestType{patch.RequestTypeNormal}, Stages: []patch.Stage{patch.StageRequest},
			RequestPaths: []string{"/normal_marker"}, Idempotence: patch.Idempotent,
			Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
				return patch.NewHooksInstance(patch.Hooks{Request: requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error { return nil })}), nil
			},
		},
		{
			ID: "classifier-request-field", Name: "classifier-request-field",
			RequestTypes: []patch.RequestType{patch.RequestTypeClassifier}, Stages: []patch.Stage{patch.StageRequest},
			RequestPaths: []string{"/classifier_marker"}, Idempotence: patch.Idempotent,
			Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
				return patch.NewHooksInstance(patch.Hooks{Request: requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error { return nil })}), nil
			},
		},
		{
			ID: "classifier-response-field", Name: "classifier-response-field",
			RequestTypes: []patch.RequestType{patch.RequestTypeClassifier}, Stages: []patch.Stage{patch.StageResponse},
			ResponsePaths: []string{"/response_only"}, Idempotence: patch.Idempotent,
			Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
				return patch.NewHooksInstance(patch.Hooks{Response: responsePatchFunc(func(patch.PatchContext, *patch.MutableResponse) error { return nil })}), nil
			},
		},
	}
	registry, err := patch.NewRegistry(definitions)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	item := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "union", upstream.URL, "key", "m",
		"normal-request-field", "classifier-request-field", "classifier-response-field")
	snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	selector := &fakeSelector{leases: []scheduler.AttemptLease{classifierLease(item, "m"), classifierLease(item, "m")}}
	detectors, err := traffic.NewRegistry(classifierDetector{})
	if err != nil {
		t.Fatal(err)
	}
	flows, err := flow.NewRegistry(retainedUnionClassifierPlanner{})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{
		DetectorRegistry: detectors,
		FlowDispatcher:   flow.NewDispatcher(flows),
	})
	defer handler.Close()
	requestBody := `{"model":"m","normal_marker":true,"classifier_marker":true,"response_only":true,"unrelated":true}`
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", requestBody))
		if response.Code != http.StatusOK || response.Body.String() != `{}` {
			t.Fatalf("request %d response = %d %q", attempt, response.Code, response.Body.String())
		}
	}
	// The first request needs one additional Provider copy to reconcile the
	// client pool. Every request performs exactly one scan-union read.
	if got := snapshot.providerCalls.Load(); got != 3 {
		t.Fatalf("Providers calls = %d, want 3", got)
	}
}

func TestGatewayPlannerReceivesOriginalFlowSnapshot(t *testing.T) {
	const patchID = "test-classifier-semantic-failure"
	registry := testTypedRequestPatchRegistry(t, patchID, patch.RequestTypeClassifier, func(patch.FactoryContext) patch.RequestPatch {
		return requestPatchFunc(func(patch.PatchContext, *patch.MutableRequest) error {
			return errors.New("semantic classifier patch failure")
		})
	})
	var firstCalls atomic.Int32
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		firstCalls.Add(1)
	}))
	defer firstUpstream.Close()
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fallback")
	}))
	defer secondUpstream.Close()
	first := compileWithRegistry(t, registry, "11111111-1111-4111-8111-111111111111", "first", firstUpstream.URL, "key-a", "m", patchID)
	second := compileWithRegistry(t, registry, "22222222-2222-4222-8222-222222222222", "second", secondUpstream.URL, "key-b", "m")
	selector := &fakeSelector{leases: []scheduler.AttemptLease{classifierLease(first, "m"), classifierLease(second, "m")}}
	detectors, err := traffic.NewRegistry(classifierDetector{})
	if err != nil {
		t.Fatal(err)
	}
	wantAttempts := scheduler.AttemptPolicy{MaxAttempts: 2}
	wantAuto := flow.AutoModeSnapshot{Mode: "test-mode", ClassifierModel: "test-classifier"}
	flows, err := flow.NewRegistry(snapshotCheckingClassifierPlanner{wantAttempts: wantAttempts, wantAuto: wantAuto})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &flowAwareSnapshot{
		fakeSnapshot:       &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}},
		classifierAttempts: wantAttempts,
		auto:               wantAuto,
	}
	handler := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{
		DetectorRegistry: detectors,
		FlowDispatcher:   flow.NewDispatcher(flows),
	})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`))
	acquires, reports := selector.snapshot()
	if response.Code != http.StatusOK || response.Body.String() != "fallback" || firstCalls.Load() != 0 || acquires != 2 {
		t.Fatalf("response=%d %q first_calls=%d acquires=%d", response.Code, response.Body.String(), firstCalls.Load(), acquires)
	}
	if len(reports) != 2 || reports[0].Class != scheduler.FailureNeutral || reports[1].Class != scheduler.FailureNone {
		t.Fatalf("reports = %#v", reports)
	}
}

func classifierLease(item *provider.CompiledProvider, model string) scheduler.AttemptLease {
	lease := leaseFor(item, model)
	lease.RequestType = traffic.RequestTypeClassifier
	return lease
}

func TestGatewayExplicitClassifierPlanRunsGPTRequestAndResponseHooks(t *testing.T) {
	var acceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		acceptEncoding = request.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("X-Upstream", "kept")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"allow STOP reason"}],"stop_reason":"end_turn","stop_sequence":null}`)
	}))
	defer upstream.Close()
	item := compileTestProviderWithPatches(t, "11111111-1111-4111-8111-111111111111", "classifier", upstream.URL, "key", "m", false, patch.GPTClassifierResponseReassemblyID)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{classifierLease(item, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, classifierGatewayOptions(t, events, t.TempDir()))
	defer handler.Close()

	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stop_sequences":["STOP"]}`)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || acceptEncoding != "" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("X-Upstream") != "kept" {
		t.Fatalf("response=%d headers=%v accept_encoding=%q body=%s", response.Code, response.Header(), acceptEncoding, response.Body.String())
	}
	if length, err := strconv.Atoi(response.Header().Get("Content-Length")); err != nil || length != response.Body.Len() {
		t.Fatalf("Content-Length=%q body_len=%d err=%v", response.Header().Get("Content-Length"), response.Body.Len(), err)
	}
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Type != "text" || decoded.Content[0].Text != "allow " || decoded.StopReason != "stop_sequence" || decoded.StopSequence != "STOP" {
		t.Fatalf("decoded response = %#v", decoded)
	}
	if keys := selector.stickyKeys(); len(keys) != 1 || keys[0].RequestType != traffic.RequestTypeClassifier {
		t.Fatalf("sticky keys = %#v", keys)
	}
	if got := events.snapshot(); len(got) != 2 || got[0].Kind != EventForward || got[1].Kind != EventSuccess || got[0].RequestType != traffic.RequestTypeClassifier {
		t.Fatalf("events = %#v", got)
	}
}

func TestGatewayResponsePatchFailureNeverRetriesOrLeaksUpstreamResponse(t *testing.T) {
	var secondCalls atomic.Int32
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Unconfirmed", "must-not-leak")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer firstUpstream.Close()
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondCalls.Add(1)
	}))
	defer secondUpstream.Close()
	first := compileTestProviderWithPatches(t, "11111111-1111-4111-8111-111111111111", "first", firstUpstream.URL, "key-a", "m", false, patch.GPTClassifierResponseReassemblyID)
	second := compileTestProviderWithPatches(t, "22222222-2222-4222-8222-222222222222", "second", secondUpstream.URL, "key-b", "m", false)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{classifierLease(first, "m"), classifierLease(second, "m")}}
	events := &eventCollector{}
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
	}, selector, classifierGatewayOptions(t, events, t.TempDir()))
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stop_sequences":["STOP"]}`))
	acquires, reports := selector.snapshot()
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "patch_failed") || response.Header().Get("X-Unconfirmed") != "" || acquires != 1 || secondCalls.Load() != 0 {
		t.Fatalf("response=%d headers=%v body=%s acquires=%d second=%d", response.Code, response.Header(), response.Body.String(), acquires, secondCalls.Load())
	}
	if len(reports) != 1 || reports[0].Class != scheduler.FailureNeutral {
		t.Fatalf("reports = %#v", reports)
	}
	got := events.snapshot()
	if len(got) != 2 || got[0].Kind != EventForward || got[1].Kind != EventFailure || got[1].PatchID != patch.GPTClassifierResponseReassemblyID || got[1].PatchStage != string(patch.StageResponse) {
		t.Fatalf("events = %#v", got)
	}
}

type repeatedByteReader struct {
	remaining int64
	value     byte
}

func (r *repeatedByteReader) Read(destination []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(destination)) > r.remaining {
		destination = destination[:r.remaining]
	}
	for index := range destination {
		destination[index] = r.value
	}
	r.remaining -= int64(len(destination))
	return len(destination), nil
}

type hashingResponseWriter struct {
	header http.Header
	status int
	bytes  int64
	hash   hash.Hash
}

func newHashingResponseWriter() *hashingResponseWriter {
	return &hashingResponseWriter{header: make(http.Header), hash: sha256.New()}
}

func (w *hashingResponseWriter) Header() http.Header { return w.header }
func (w *hashingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *hashingResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(data))
	return w.hash.Write(data)
}

func TestGatewayGPTResponsePatchHandlesMoreThan64MiB(t *testing.T) {
	const textSize = int64(64<<20) + 321
	responsePrefix := `{"type":"message","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"`
	responseSuffix := `STOPdiscard"}],"stop_reason":"end_turn","stop_sequence":null}`
	expectedPrefix := `{"type":"message","content":[{"type":"text","text":"`
	expectedSuffix := `"}],"stop_reason":"stop_sequence","stop_sequence":"STOP"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.MultiReader(
			strings.NewReader(responsePrefix),
			&repeatedByteReader{remaining: textSize, value: 'a'},
			strings.NewReader(responseSuffix),
		))
	}))
	defer upstream.Close()
	item := compileTestProviderWithPatches(t, "11111111-1111-4111-8111-111111111111", "large-classifier", upstream.URL, "key", "m", false, patch.GPTClassifierResponseReassemblyID)
	selector := &fakeSelector{leases: []scheduler.AttemptLease{classifierLease(item, "m")}}
	tempDir := t.TempDir()
	handler := NewWithOptions(func() scheduler.Snapshot {
		return &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{item}}
	}, selector, classifierGatewayOptions(t, nil, tempDir))
	defer handler.Close()

	expectedHash := sha256.New()
	_, _ = io.Copy(expectedHash, io.MultiReader(strings.NewReader(expectedPrefix), &repeatedByteReader{remaining: textSize, value: 'a'}, strings.NewReader(expectedSuffix)))
	destination := newHashingResponseWriter()
	handler.ServeHTTP(destination, gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stop_sequences":["STOP"]}`))
	if destination.status != http.StatusOK || !bytes.Equal(destination.hash.Sum(nil), expectedHash.Sum(nil)) {
		t.Fatalf("status=%d bytes=%d hash=%x want=%x", destination.status, destination.bytes, destination.hash.Sum(nil), expectedHash.Sum(nil))
	}
	if got, err := strconv.ParseInt(destination.header.Get("Content-Length"), 10, 64); err != nil || got != destination.bytes || got <= 64<<20 {
		t.Fatalf("Content-Length=%q bytes=%d err=%v", destination.header.Get("Content-Length"), destination.bytes, err)
	}
	if entries, err := os.ReadDir(tempDir); err != nil || len(entries) != 0 {
		t.Fatalf("temporary response bodies remain: %v, err=%v", entries, err)
	}
}
