package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/health"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
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

func marshalManagementClientConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var harnesses map[string]json.RawMessage
	if err := json.Unmarshal(root["harnesses"], &harnesses); err != nil {
		t.Fatal(err)
	}
	var claude map[string]json.RawMessage
	if err := json.Unmarshal(harnesses["claude_code"], &claude); err != nil {
		t.Fatal(err)
	}
	delete(claude, "active_profile_id")
	harnesses["claude_code"], _ = json.Marshal(claude)
	root["harnesses"], _ = json.Marshal(harnesses)
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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
	var resourceFields map[string]json.RawMessage
	if err := json.Unmarshal(get.Body.Bytes(), &resourceFields); err != nil {
		t.Fatal(err)
	}
	var resourceHarnesses map[string]json.RawMessage
	if err := json.Unmarshal(resourceFields["harnesses"], &resourceHarnesses); err != nil {
		t.Fatal(err)
	}
	var resourceClaude map[string]json.RawMessage
	if err := json.Unmarshal(resourceHarnesses["claude_code"], &resourceClaude); err != nil {
		t.Fatal(err)
	}
	if _, ok := resourceClaude["active_profile_id"]; !ok {
		t.Fatal("GET config omitted server-owned active_profile_id")
	}
	if rec := request(handler, http.MethodPut, "/api/v1/config", auth, get.Body.String()); rec.Code != http.StatusConflict {
		t.Fatalf("resource response accepted as PUT request: %d %s", rec.Code, rec.Body.String())
	}
	clientBody := marshalManagementClientConfig(t, got)
	if rec := request(handler, http.MethodPut, "/api/v1/config", auth, clientBody); rec.Code != http.StatusOK {
		t.Fatalf("client update without server-owned field = %d %s", rec.Code, rec.Body.String())
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
	body := marshalManagementClientConfig(t, replacement)
	rotated := request(handler, http.MethodPut, "/api/v1/config", auth, body)
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

func TestManagementConfigRoundTripsFixedAutoModeAndStatusOmitsKey(t *testing.T) {
	manager := testManager(t, nil)
	diagnostics := automode.NewDiagnostics()
	diagnostics.Record(automode.FixedTargetCall{
		ObservedAt:     time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC),
		UpstreamURL:    "https://classifier.example/prefix/v1/responses?trace=%2F",
		GatewayStatus:  http.StatusBadGateway,
		GatewayError:   "protocol_conversion_failed",
		UpstreamStatus: http.StatusOK,
		SessionID:      "original-session",
		Error:          "complete conversion error",
		UpstreamHeaders: http.Header{
			"X-Upstream": []string{"raw"},
		},
		UpstreamBody: `{"raw":"response"}`,
	})
	handler := NewWithOptions(manager, Options{AutoModeDiagnostics: diagnostics})
	next := manager.Snapshot().Config()
	next.AutoMode = config.AutoModeConfig{
		Mode:  config.AutoModeFixedProvider,
		Model: "classifier-model",
		FixedProvider: &config.FixedProviderConfig{
			BaseURL: "https://classifier.example/prefix", APIKey: "fixed-secret",
			UseXAPIKey: true, Protocol: config.ProtocolOpenAIResponses,
			TLS:     config.TLSConfig{InsecureSkipVerify: true},
			Patches: []string{patch.AnyRouterClassifierRequestID},
		},
	}
	body := marshalManagementClientConfig(t, next)
	updated := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", body)
	if updated.Code != http.StatusOK {
		t.Fatalf("PUT fixed Auto Mode = %d %s", updated.Code, updated.Body.String())
	}
	gotResponse := request(handler, http.MethodGet, "/api/v1/config", "Bearer management-key", "")
	var got config.Config
	if gotResponse.Code != http.StatusOK {
		t.Fatalf("GET fixed Auto Mode = %d %s", gotResponse.Code, gotResponse.Body.String())
	}
	if err := json.Unmarshal(gotResponse.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AutoMode.FixedProvider == nil || got.AutoMode.Mode != config.AutoModeFixedProvider ||
		got.AutoMode.Model != "classifier-model" || got.AutoMode.FixedProvider.BaseURL != "https://classifier.example/prefix" ||
		got.AutoMode.FixedProvider.APIKey != "fixed-secret" || !got.AutoMode.FixedProvider.UseXAPIKey ||
		got.AutoMode.FixedProvider.Protocol != config.ProtocolOpenAIResponses || !got.AutoMode.FixedProvider.TLS.InsecureSkipVerify ||
		len(got.AutoMode.FixedProvider.Patches) != 1 || got.AutoMode.FixedProvider.Patches[0] != patch.AnyRouterClassifierRequestID {
		t.Fatalf("fixed Auto Mode round trip = %#v", got.AutoMode)
	}

	status := request(handler, http.MethodGet, "/api/v1/status", "Bearer management-key", "")
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), "fixed-secret") {
		t.Fatalf("fixed status = %d %s", status.Code, status.Body.String())
	}
	var decoded statusResponse
	if err := json.Unmarshal(status.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	last := decoded.AutoMode.FixedTargetLastCall
	if !decoded.AutoMode.FixedProviderConfigured || decoded.AutoMode.FixedProviderProtocol != config.ProtocolOpenAIResponses ||
		last == nil || last.UpstreamURL != "https://classifier.example/prefix/v1/responses?trace=%2F" ||
		last.UpstreamStatus != http.StatusOK || last.UpstreamHeaders.Get("X-Upstream") != "raw" || last.UpstreamBody != `{"raw":"response"}` {
		t.Fatalf("fixed status data = %#v", decoded.AutoMode)
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

func TestLogHistoryEndpoint(t *testing.T) {
	dir := t.TempDir()
	contents := strings.Join([]string{
		`{"time":"2026-09-03T12:00:00Z","level":"INFO","msg":"service","seq":1,"event":"listening","listen_addr":"127.0.0.1:8765"}`,
		`malformed`,
		`{"time":"2026-09-03T12:00:01Z","level":"ERROR","msg":"gateway","seq":2,"kind":"failure","provider_id":"provider-a","http_status":503,"raw_error":"complete"}`,
		`{"time":"2026-09-03T12:00:02Z","level":"ERROR","msg":"gateway","seq":3,"kind":"failure","provider_id":"provider-b","http_status":502,"raw_error":"newest"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, logstore.ActiveFileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := logstore.NewDirectorySource(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := logstore.NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithOptions(testManager(t, nil), Options{Logs: reader})

	response := request(handler, http.MethodGet, "/api/v1/logs?level=ERROR&http_status=502&http_status=503&limit=1", "Bearer management-key", "")
	if response.Code != http.StatusOK {
		t.Fatalf("log history = %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Items            []map[string]any `json:"items"`
		HasMore          bool             `json:"has_more"`
		NextCursor       string           `json:"next_cursor"`
		SkippedMalformed int              `json:"skipped_malformed"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0]["seq"] != float64(3) || !page.HasMore || page.NextCursor == "" || page.SkippedMalformed != 0 {
		t.Fatalf("first log page = %#v", page)
	}
	second := request(handler, http.MethodGet, "/api/v1/logs?level=ERROR&http_status=502&http_status=503&limit=1&cursor="+page.NextCursor, "Bearer management-key", "")
	if second.Code != http.StatusOK {
		t.Fatalf("second log page = %d %s", second.Code, second.Body.String())
	}
	page = struct {
		Items            []map[string]any `json:"items"`
		HasMore          bool             `json:"has_more"`
		NextCursor       string           `json:"next_cursor"`
		SkippedMalformed int              `json:"skipped_malformed"`
	}{}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0]["seq"] != float64(2) || page.HasMore || page.NextCursor != "" || page.SkippedMalformed != 1 {
		t.Fatalf("second log page = %#v", page)
	}

	for _, path := range []string{
		"/api/v1/logs?unknown=value",
		"/api/v1/logs?level=",
		"/api/v1/logs?limit=1001",
		"/api/v1/logs?cursor=invalid!",
	} {
		invalid := request(handler, http.MethodGet, path, "Bearer management-key", "")
		if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), `"error":"validation_failed"`) {
			t.Fatalf("invalid log query %s = %d %s", path, invalid.Code, invalid.Body.String())
		}
	}
	if method := request(handler, http.MethodPost, "/api/v1/logs", "Bearer management-key", ""); method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("log method = %d Allow=%q", method.Code, method.Header().Get("Allow"))
	}
}

func TestLogHistoryEndpointMissingFilesAndReadFailure(t *testing.T) {
	emptySource, err := logstore.NewDirectorySource(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	emptyReader, _ := logstore.NewReader(emptySource)
	emptyHandler := NewWithOptions(testManager(t, nil), Options{Logs: emptyReader})
	empty := request(emptyHandler, http.MethodGet, "/api/v1/logs", "Bearer management-key", "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"items":[]`) || !strings.Contains(empty.Body.String(), `"has_more":false`) || !strings.Contains(empty.Body.String(), `"skipped_malformed":0`) || strings.Contains(empty.Body.String(), `"next_cursor"`) {
		t.Fatalf("empty log history = %d %s", empty.Code, empty.Body.String())
	}

	failingReader, err := logstore.NewReader(logstore.SnapshotSourceFunc(func() (logstore.Snapshot, error) {
		return logstore.Snapshot{}, errors.New("read failed")
	}))
	if err != nil {
		t.Fatal(err)
	}
	failingHandler := NewWithOptions(testManager(t, nil), Options{Logs: failingReader})
	failure := request(failingHandler, http.MethodGet, "/api/v1/logs", "Bearer management-key", "")
	if failure.Code != http.StatusInternalServerError || !strings.Contains(failure.Body.String(), `"error":"log_read_failed"`) {
		t.Fatalf("failed log history = %d %s", failure.Code, failure.Body.String())
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
	body := marshalManagementClientConfig(t, cfg)
	rec := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", body)
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
	body := marshalManagementClientConfig(t, cfg)
	recorder := &orderedRecorder{ResponseRecorder: httptest.NewRecorder(), responseWritten: &responseWritten}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
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
	body := marshalManagementClientConfig(t, next)
	rec := request(New(manager), http.MethodPut, "/api/v1/config", "Bearer management-key", body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("persistence failure = %d %s", rec.Code, rec.Body.String())
	}
	if manager.Snapshot().Config().Auth.GatewayKey != "" {
		t.Fatal("snapshot changed after persistence failure")
	}
}
