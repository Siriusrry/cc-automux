package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func testManager(t *testing.T, restart func() error) *runtime.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.ManagementKey = "management-key"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager, err := runtime.NewManager(store, cfg, runtime.Options{
		Restart:        restart,
		RuntimeContext: testRuntimeContext(t),
		Preflight:      func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func testRuntimeContext(t *testing.T) provider.RuntimeContext {
	t.Helper()
	registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	context, err := provider.NewRuntimeContext(registry)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func request(handler http.Handler, method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestManagementAuthenticationUsesStandardChallenge(t *testing.T) {
	handler := New(testManager(t, nil))
	responses := []*httptest.ResponseRecorder{
		request(handler, http.MethodGet, "/api/v1/status", "", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Basic management-key", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Bearer wrong", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Bearer ", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Bearer\tmanagement-key", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Bearer  management-key", ""),
		request(handler, http.MethodGet, "/api/v1/status", " Bearer management-key", ""),
		request(handler, http.MethodGet, "/api/v1/status", "Bearer management-key ", ""),
	}
	for i, rec := range responses {
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("response %d status = %d", i, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("response %d challenge = %q", i, got)
		}
		if i > 0 && rec.Body.String() != responses[0].Body.String() {
			t.Fatalf("auth failure bodies differ: %q vs %q", responses[0].Body.String(), rec.Body.String())
		}
	}
	if rec := request(handler, http.MethodGet, "/api/v1/status", "bearer management-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("case-insensitive Bearer scheme = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigAndProviderCRUDAndKeyRotation(t *testing.T) {
	manager := testManager(t, nil)
	withGateway := manager.Snapshot().Config()
	withGateway.Auth.GatewayKey = "gateway-key"
	if _, err := manager.Apply(withGateway); err != nil {
		t.Fatal(err)
	}
	handler := New(manager)
	auth := "Bearer management-key"
	if rec := request(handler, http.MethodGet, "/api/v1/status", "Bearer gateway-key", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gateway key authenticated management API: %d", rec.Code)
	}

	get := request(handler, http.MethodGet, "/api/v1/config", auth, "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET config = %d %s", get.Code, get.Body.String())
	}
	var got config.Config
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Auth.ManagementKey != "management-key" {
		t.Fatalf("GET config did not return full management key")
	}

	providerBody := `{"name":"test-provider","base_url":"https://provider.example/anthropic","api_key":"provider-key","models":["Model"],"enabled":true,"disable_health":true}`
	created := request(handler, http.MethodPost, "/api/v1/providers", auth, providerBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST provider = %d %s", created.Code, created.Body.String())
	}
	var p config.ProviderConfig
	if err := json.Unmarshal(created.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !config.IsUUID(p.ID) || !p.DisableHealth {
		t.Fatalf("created provider = id %q, disable_health %v", p.ID, p.DisableHealth)
	}
	duplicateIDBody := `{"id":"` + p.ID + `","name":"different-name","base_url":"https://different.example","api_key":"key","models":["m"],"enabled":true}`
	if rec := request(handler, http.MethodPost, "/api/v1/providers", auth, duplicateIDBody); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate provider id = %d", rec.Code)
	}
	getProvider := request(handler, http.MethodGet, "/api/v1/providers/"+p.ID, auth, "")
	if getProvider.Code != http.StatusOK {
		t.Fatalf("GET provider = %d", getProvider.Code)
	}
	var fetched config.ProviderConfig
	if err := json.Unmarshal(getProvider.Body.Bytes(), &fetched); err != nil || !fetched.DisableHealth {
		t.Fatalf("GET provider disable_health = %v, err %v", fetched.DisableHealth, err)
	}

	p.Priority = -1
	p.DisableHealth = false
	updatedBody, _ := json.Marshal(p)
	updated := request(handler, http.MethodPut, "/api/v1/providers/"+p.ID, auth, string(updatedBody))
	if updated.Code != http.StatusOK {
		t.Fatalf("PUT provider = %d %s", updated.Code, updated.Body.String())
	}
	var updatedProvider config.ProviderConfig
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedProvider); err != nil || updatedProvider.DisableHealth {
		t.Fatalf("PUT provider disable_health = %v, err %v", updatedProvider.DisableHealth, err)
	}
	changedID := p
	changedID.ID = "99999999-9999-4999-8999-999999999999"
	changedIDBody, _ := json.Marshal(changedID)
	if rec := request(handler, http.MethodPut, "/api/v1/providers/"+p.ID, auth, string(changedIDBody)); rec.Code != http.StatusConflict {
		t.Fatalf("provider id mutation = %d", rec.Code)
	}
	deleted := request(handler, http.MethodDelete, "/api/v1/providers/"+p.ID, auth, "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE provider = %d", deleted.Code)
	}
	missing := request(handler, http.MethodGet, "/api/v1/providers/"+p.ID, auth, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing provider = %d", missing.Code)
	}

	var replacement config.Config
	replacement = got
	replacement.Auth.ManagementKey = "new-management-key"
	body, _ := json.Marshal(replacement)
	rotated := request(handler, http.MethodPut, "/api/v1/config", auth, string(body))
	if rotated.Code != http.StatusOK {
		t.Fatalf("key rotation = %d %s", rotated.Code, rotated.Body.String())
	}
	if rec := request(handler, http.MethodGet, "/api/v1/status", auth, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old key after rotation = %d", rec.Code)
	}
	if rec := request(handler, http.MethodGet, "/api/v1/status", "Bearer new-management-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("new key after rotation = %d %s", rec.Code, rec.Body.String())
	}
}

func TestManagementErrorsAndMethodContracts(t *testing.T) {
	handler := New(testManager(t, nil))
	auth := "Bearer management-key"
	if rec := request(handler, http.MethodGet, "/api/v1/unknown", auth, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d", rec.Code)
	}
	if rec := request(handler, http.MethodGet, "/api/v10/status", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("path outside v1 namespace = %d", rec.Code)
	}
	if rec := request(handler, http.MethodPatch, "/api/v1/status", auth, ""); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method response = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := request(handler, http.MethodPut, "/api/v1/config", auth, `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed config = %d", rec.Code)
	}
	if rec := request(handler, http.MethodPost, "/api/v1/providers", auth, `{"name":"x","base_url":"https://x.test","models":["m"],"enabled":true}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing provider key = %d", rec.Code)
	}
	if rec := request(handler, http.MethodGet, "/api/v1/providers/nope", auth, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing provider = %d", rec.Code)
	}
}

func TestProviderPatchDiscoveryContract(t *testing.T) {
	handler := New(testManager(t, nil))
	response := request(handler, http.MethodGet, "/api/v1/provider-patches", "Bearer management-key", "")
	if response.Code != http.StatusOK {
		t.Fatalf("provider patches = %d %s", response.Code, response.Body.String())
	}

	var rawItems []map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &rawItems); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]struct{}{
		"id": {}, "name": {}, "description": {}, "request_types": {},
		"stages": {}, "conflicts": {}, "idempotence": {},
	}
	for index, item := range rawItems {
		if len(item) != len(wantKeys) {
			t.Fatalf("patch[%d] fields = %#v", index, item)
		}
		for key := range wantKeys {
			if _, ok := item[key]; !ok {
				t.Fatalf("patch[%d] missing %q: %#v", index, key, item)
			}
		}
		for _, arrayField := range []string{"request_types", "stages", "conflicts"} {
			if string(item[arrayField]) == "null" {
				t.Fatalf("patch[%d].%s is null", index, arrayField)
			}
		}
	}

	var got []patch.PatchMetadata
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []patch.PatchMetadata{
		{
			ID:           "anyrouter-subagent-thinking",
			Name:         "AnyRouter Subagent Thinking Compatibility",
			Description:  "Promotes disabled thinking on normal requests sent to compatible AnyRouter targets",
			RequestTypes: []patch.RequestType{patch.RequestTypeNormal},
			Stages:       []patch.Stage{patch.StageRequest},
			Conflicts:    []string{},
			Idempotence:  patch.Idempotent,
		},
		{
			ID:           "anyrouter-classifier-request-compat",
			Name:         "AnyRouter Classifier Request Compatibility",
			Description:  "Adds Claude Code identity and correction markers for classifier requests",
			RequestTypes: []patch.RequestType{patch.RequestTypeClassifier},
			Stages:       []patch.Stage{patch.StageRequest},
			Conflicts:    []string{},
			Idempotence:  patch.Idempotent,
		},
		{
			ID:           "cliproxyapi-classifier-session-isolation",
			Name:         "CLIProxyAPI Classifier Session Isolation",
			Description:  "Isolates classifier sessions with a stable per-target UUID",
			RequestTypes: []patch.RequestType{patch.RequestTypeClassifier},
			Stages:       []patch.Stage{patch.StageRequest},
			Conflicts:    []string{},
			Idempotence:  patch.PerExecution,
		},
		{
			ID:           "gpt-classifier-response-reassembly",
			Name:         "GPT Classifier Response Reassembly",
			Description:  "Reassembles successful classifier responses into the expected Anthropic message shape",
			RequestTypes: []patch.RequestType{patch.RequestTypeClassifier},
			Stages:       []patch.Stage{patch.StageRequest, patch.StageResponse},
			Conflicts:    []string{},
			Idempotence:  patch.PerExecution,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider patches = %#v, want %#v", got, want)
	}
}

func TestProviderHealthReturnsCompleteDiagnosticsAndStatusAggregates(t *testing.T) {
	manager := testManager(t, nil)
	next := manager.Snapshot().Config()
	next.Auth.GatewayKey = "gateway-key"
	next.Providers = []config.ProviderConfig{{
		ID:            "11111111-1111-4111-8111-111111111111",
		Name:          "provider-a",
		BaseURL:       "https://provider.example/prefix",
		APIKey:        "complete-provider-key",
		Models:        []string{"model-a"},
		Priority:      -7,
		Enabled:       true,
		DisableHealth: true,
	}}
	if _, err := manager.Apply(next); err != nil {
		t.Fatal(err)
	}
	healthStore := health.NewDefault()
	selector, err := scheduler.NewSelector(healthStore, scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	selector.Reconcile(snapshot)
	lease, err := selector.Acquire(snapshot, scheduler.StickyKey{
		SessionID:   "raw-session-id",
		Model:       "model-a",
		RequestType: traffic.RequestTypeNormal,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selector.Report(lease, scheduler.Outcome{
		Class:       scheduler.FailureNeutral,
		HTTPStatus:  http.StatusBadRequest,
		UpstreamURL: "https://provider.example/prefix/v1/messages?beta=1",
		RawError:    "complete upstream error text",
		SessionID:   "raw-session-id",
	})

	handler := NewWithOptions(manager, Options{
		Health:         healthStore,
		Selector:       selector,
		Sync:           func() { selector.Reconcile(manager.Snapshot()) },
		ActiveRequests: func() int64 { return 3 },
	})
	rec := request(handler, http.MethodGet, "/api/v1/provider-health", "Bearer management-key", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("provider health = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"request_type":"normal"`) {
		t.Fatalf("provider health request type representation = %s", rec.Body.String())
	}
	var healthResponse providerHealthListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &healthResponse); err != nil {
		t.Fatal(err)
	}
	if healthResponse.Revision != snapshot.Revision() || len(healthResponse.Providers) != 1 {
		t.Fatalf("health response = %#v", healthResponse)
	}
	item := healthResponse.Providers[0]
	if item.APIKey != "complete-provider-key" || item.BaseURL != "https://provider.example/prefix" ||
		item.StaticAvailability != scheduler.StaticActive || len(item.Channels) != 1 || len(item.Sessions) != 1 {
		t.Fatalf("provider diagnostic = %#v", item)
	}
	if item.Channels[0].LastUpstreamURL != "https://provider.example/prefix/v1/messages?beta=1" ||
		item.Channels[0].LastError != "complete upstream error text" || item.Channels[0].LastSessionID != "raw-session-id" ||
		item.Channels[0].RequestType != string(traffic.RequestTypeNormal) ||
		item.Sessions[0].SessionID != "raw-session-id" || item.Sessions[0].RequestType != string(traffic.RequestTypeNormal) {
		t.Fatalf("complete diagnostic fields = %#v", item)
	}

	status := request(handler, http.MethodGet, "/api/v1/status", "Bearer management-key", "")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	var got statusResponse
	if err := json.Unmarshal(status.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.GatewayConfigured || got.ActiveProviderCount != 1 || got.InactiveProviderCount != 0 ||
		got.HealthDisabledProviderCount != 1 || got.ActiveDataRequests != 3 || got.StickyAssignmentCount != 1 ||
		got.GlobalHealth.Disabled != 1 || got.ChannelHealth.Disabled != 1 {
		t.Fatalf("status aggregates = %#v", got)
	}
}

func TestManagementBodyLimit(t *testing.T) {
	manager := testManager(t, nil)
	handler := NewWithOptions(manager, Options{MaxBodyBytes: 16})
	rec := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", strings.Repeat("x", 100))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRestartResponseAndConflict(t *testing.T) {
	var calls atomic.Int32
	// Keep the transaction visibly in progress while the second request is
	// issued; production uses a short response-flush delay as well.
	manager := func() *runtime.Manager {
		path := filepath.Join(t.TempDir(), "config.json")
		store, err := config.NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Auth.ManagementKey = "management-key"
		if err := store.Save(cfg); err != nil {
			t.Fatal(err)
		}
		m, err := runtime.NewManager(store, cfg, runtime.Options{
			Restart:        func() error { calls.Add(1); return errors.New("exec failed") },
			RestartDelay:   50 * time.Millisecond,
			RuntimeContext: testRuntimeContext(t),
			Preflight:      func(config.Config, config.Config) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}()
	handler := New(manager)
	cfg := manager.Snapshot().Config()
	cfg.Service.LogMaxBytes++
	body, _ := json.Marshal(cfg)
	rec := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", string(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d %s", rec.Code, rec.Body.String())
	}
	if second := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", string(body)); second.Code != http.StatusConflict {
		t.Fatalf("second config during restart = %d", second.Code)
	}
	// The manager's default delay is asynchronous; wait briefly for the injected
	// failure to roll back the pending file.
	for i := 0; i < 100; i++ {
		// A short busy wait keeps this test independent of a production delay value.
		time.Sleep(time.Millisecond)
		if calls.Load() == 1 && !manager.RestartStatus().InProgress {
			break
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("restart callback calls = %d", calls.Load())
	}
	if status := manager.RestartStatus(); status.InProgress {
		t.Fatalf("restart remains in progress: %#v", status)
	}
}

func TestRestartStartsAfterAcceptedResponseWrite(t *testing.T) {
	var responseWritten atomic.Bool
	var startedBeforeWrite atomic.Bool
	manager := testManager(t, func() error {
		if !responseWritten.Load() {
			startedBeforeWrite.Store(true)
		}
		return errors.New("exec failed")
	})
	handler := New(manager)
	cfg := manager.Snapshot().Config()
	cfg.Service.LogMaxBytes++
	body, _ := json.Marshal(cfg)
	recorder := &orderedRecorder{ResponseRecorder: httptest.NewRecorder(), responseWritten: &responseWritten}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer management-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	if startedBeforeWrite.Load() {
		t.Fatal("restart callback began before the 202 response was written")
	}
}

type orderedRecorder struct {
	*httptest.ResponseRecorder
	responseWritten *atomic.Bool
}

func (r *orderedRecorder) WriteHeader(code int) {
	r.ResponseRecorder.WriteHeader(code)
	r.responseWritten.Store(true)
}

type failingManagementStore struct {
	*config.Store
}

func (s *failingManagementStore) Save(config.Config) error {
	return errors.New("disk unavailable")
}

func TestPersistenceFailureReturns500WithoutPublishingSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.ManagementKey = "management-key"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager, err := runtime.NewManager(&failingManagementStore{Store: store}, cfg, runtime.Options{RuntimeContext: testRuntimeContext(t)})
	if err != nil {
		t.Fatal(err)
	}
	next := cfg.Clone()
	next.Auth.GatewayKey = "gateway-key"
	body, _ := json.Marshal(next)
	rec := request(New(manager), http.MethodPut, "/api/v1/config", "Bearer management-key", string(body))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("persistence failure = %d %s", rec.Code, rec.Body.String())
	}
	if manager.Snapshot().Config().Auth.GatewayKey != "" {
		t.Fatal("snapshot changed after persistence failure")
	}
}
