package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
	logstore "github.com/Siriusrry/cc-automux/internal/logs"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func freeListenAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func discardAppLogOpener(int64) (io.Writer, func() error, error) {
	return io.Discard, func() error { return nil }, nil
}

func appLogOpenerFor(writer io.Writer) LogOpener {
	return func(int64) (io.Writer, func() error, error) {
		return writer, func() error { return nil }, nil
	}
}

func writeAppConfig(t *testing.T, path string, cfg config.Config) *config.Store {
	t.Helper()
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return store
}

func marshalAppClientConfig(t *testing.T, cfg config.Config) []byte {
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
	return data
}

func baseAppConfig(t *testing.T, path string) (*config.Store, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	return writeAppConfig(t, path, cfg), cfg
}

func TestNewBindsLoopbackAndServesManagementAndMessagesRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer application.Close()
	if host, _, err := net.SplitHostPort(application.Listener().Addr().String()); err != nil || host != "127.0.0.1" {
		t.Fatalf("listener = %s, err %v", application.Listener().Addr(), err)
	}
	handler := application.server.Handler
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d %s", rec.Code, rec.Body.String())
	}
	for _, route := range []string{"/any", "/cpa", "/admin", "/v1/messages/count_tokens"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("retired route %s = %d", route, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "gateway_not_configured") {
		t.Fatalf("unconfigured Messages route = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAppWiresHarnessManagerAndAPI(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	writeAppConfig(t, path, cfg)
	home := filepath.Join(root, "home")
	application, err := New(Options{
		ConfigPath:     path,
		LogOpener:      discardAppLogOpener,
		HarnessHomeDir: func() (string, error) { return home, nil },
		Restart:        func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.HarnessManager() == nil {
		t.Fatal("app did not create the harness manager")
	}
	handler := application.server.Handler
	request := httptest.NewRequest(http.MethodGet, "/api/v1/harnesses", nil)
	request.Header.Set("Authorization", "Bearer management-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"claude-code"`) {
		t.Fatalf("app harness discovery = %d %s", response.Code, response.Body.String())
	}
}

func TestAppStartupReconcilesHarnessBeforeServing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	home := filepath.Join(root, "home")
	profile := config.Profile{
		ID:                   "11111111-1111-4111-8111-111111111111",
		Name:                 "Daily",
		HaikuModel:           "haiku",
		SonnetModel:          "sonnet",
		OpusModel:            "opus",
		FableModel:           "fable",
		SubagentModel:        "subagent",
		TeammateDefaultModel: "teammate",
	}
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Harnesses.ClaudeCode.Profiles = []config.Profile{profile}
	cfg.Harnesses.ClaudeCode.ActiveProfileID = profile.ID
	adapter := harnessconfig.NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return home, nil })
	projection, err := adapter.BuildManagedProjection(harnessconfig.ActivationInput{
		ListenAddr:       cfg.Service.ListenAddr,
		GatewayKey:       cfg.Auth.GatewayKey,
		Profile:          harnessconfig.ProfileFromConfig(profile),
		DisableTelemetry: cfg.Harnesses.ClaudeCode.DisableTelemetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := adapter.Merge([]byte(`{"preserved":true}`), projection)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	store := writeAppConfig(t, path, cfg)
	application, err := New(Options{
		ConfigPath:     path,
		LogOpener:      discardAppLogOpener,
		HarnessHomeDir: func() (string, error) { return home, nil },
		Restart:        func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	status, err := application.HarnessManager().Status(harnessconfig.ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != harnessconfig.StateInSync || status.ActiveProfileID != profile.ID {
		t.Fatalf("startup harness status = %#v", status)
	}
	persisted, err := store.Load()
	if err != nil || persisted.Harnesses.ClaudeCode.ActiveProfileID != profile.ID {
		t.Fatalf("startup active state = %q, err %v", persisted.Harnesses.ClaudeCode.ActiveProfileID, err)
	}

	// A stale external file is invalidated during the next startup, while the
	// loopback application still initializes successfully.
	if err := os.WriteFile(target, []byte(`{"env":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = New(Options{
		ConfigPath:     path,
		LogOpener:      discardAppLogOpener,
		HarnessHomeDir: func() (string, error) { return home, nil },
		Restart:        func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	status, err = application.HarnessManager().Status(harnessconfig.ClaudeCodeAdapterID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveProfileID != "" || status.State != harnessconfig.StateInactive {
		t.Fatalf("stale startup status = %#v", status)
	}
	persisted, err = store.Load()
	if err != nil || persisted.Harnesses.ClaudeCode.ActiveProfileID != "" {
		t.Fatalf("stale startup persisted active = %q, err %v", persisted.Harnesses.ClaudeCode.ActiveProfileID, err)
	}
}

func TestAppProductionClassifierProviderPoolUsesOverrideSingleCallAndRawQuery(t *testing.T) {
	var calls atomic.Int32
	var upstreamPath, upstreamQuery, upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstreamPath = r.URL.EscapedPath()
		upstreamQuery = r.URL.RawQuery
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read classifier body: %v", err)
		}
		upstreamBody = string(data)
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("classifier auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeProviderPool, Model: "classifier-model"}
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "classifier-provider",
		BaseURL: upstream.URL + "/prefix",
		APIKey:  "provider-key",
		Models:  []string{"classifier-model"},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	requestBody := `{"model":"client-model","system":[{"type":"text","text":"` + automode.SecurityMarker + `. classify"}],"future":{"kept":true}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?trace=%2F&trace=a+b", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("X-Claude-Code-Session-Id", "classifier-session")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"type":"message","content":[]}` {
		t.Fatalf("classifier response = %d %s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 || upstreamPath != "/prefix/v1/messages" || upstreamQuery != "trace=%2F&trace=a+b" {
		t.Fatalf("classifier calls=%d path=%q query=%q", calls.Load(), upstreamPath, upstreamQuery)
	}
	wantBody := strings.Replace(requestBody, `"client-model"`, `"classifier-model"`, 1)
	if upstreamBody != wantBody {
		t.Fatalf("classifier body = %q, want %q", upstreamBody, wantBody)
	}
	assignments := application.selector.Assignments("")
	if len(assignments) != 1 || assignments[0].ProviderID != cfg.Providers[0].ID ||
		assignments[0].Key.SessionID != "classifier-session" || assignments[0].Key.Model != "classifier-model" ||
		assignments[0].Key.RequestType != traffic.RequestTypeClassifier {
		t.Fatalf("classifier assignment = %#v", assignments)
	}
}

func TestAppClassifierProviderPoolFailureUsesOneProviderAndNoFailoverEvent(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("X-Classifier-Error", "raw")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "complete classifier failure")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { secondCalls.Add(1) }))
	defer second.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.AutoMode = config.AutoModeConfig{Mode: config.AutoModeProviderPool, Model: "classifier-model"}
	cfg.Providers = []config.ProviderConfig{
		{ID: "11111111-1111-4111-8111-111111111111", Name: "first", BaseURL: first.URL, APIKey: "first-key", Models: []string{"classifier-model"}, Enabled: true},
		{ID: "22222222-2222-4222-8222-222222222222", Name: "second", BaseURL: second.URL, APIKey: "second-key", Models: []string{"classifier-model"}, Enabled: true},
	}
	writeAppConfig(t, path, cfg)
	var logOutput bytes.Buffer
	application, err := New(Options{ConfigPath: path, LogOpener: appLogOpenerFor(&logOutput), Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	body := `{"model":"client-model","system":[{"text":"` + automode.SecurityMarker + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("X-Claude-Code-Session-Id", "classifier-failure-session")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "complete classifier failure" ||
		response.Header().Get("X-Classifier-Error") != "raw" || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("classifier failure = %d headers=%v body=%q calls=%d/%d", response.Code, response.Header(), response.Body.String(), firstCalls.Load(), secondCalls.Load())
	}
	if strings.Contains(logOutput.String(), `"kind":"failover"`) || strings.Count(logOutput.String(), `"kind":"failure"`) != 1 ||
		!strings.Contains(logOutput.String(), `"request_type":"classifier"`) || !strings.Contains(logOutput.String(), `"attempt":1`) {
		t.Fatalf("classifier failure events = %q", logOutput.String())
	}
}

func TestAppDisabledClassifierNeverFallsBackToNormal(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID: "11111111-1111-4111-8111-111111111111", Name: "normal-provider", BaseURL: upstream.URL,
		APIKey: "provider-key", Models: []string{"client-model"}, Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	body := `{"model":"client-model","system":[{"text":"` + automode.SecurityMarker + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "auto_mode_not_configured") || calls.Load() != 0 {
		t.Fatalf("disabled classifier = %d %s calls=%d", response.Code, response.Body.String(), calls.Load())
	}
}

func TestAppFixedModeNormalUsesPoolAndClassifierReportsMissingAdapter(t *testing.T) {
	var normalCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normalCalls.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("normal path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "normal-ok")
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.AutoMode = config.AutoModeConfig{
		Mode:  config.AutoModeFixedProvider,
		Model: "classifier-model",
		FixedProvider: &config.FixedProviderConfig{
			BaseURL: "https://classifier.example/prefix", APIKey: "fixed-secret",
			Protocol: config.ProtocolOpenAIResponses, TLS: config.TLSConfig{}, Patches: []string{},
		},
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "11111111-1111-4111-8111-111111111111", Name: "normal-provider", BaseURL: upstream.URL,
		APIKey: "provider-key", Models: []string{"normal-model"}, Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	normal := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"normal-model","system":[{"text":"ordinary"}]}`))
	normal.Header.Set("Authorization", "Bearer gateway-key")
	normalResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(normalResponse, normal)
	if normalResponse.Code != http.StatusOK || normalResponse.Body.String() != "normal-ok" || normalCalls.Load() != 1 || application.gateway.FixedTargetDiagnostics() != nil {
		t.Fatalf("normal fixed-mode request = %d %q calls=%d diagnostic=%#v", normalResponse.Code, normalResponse.Body.String(), normalCalls.Load(), application.gateway.FixedTargetDiagnostics())
	}

	classifierBody := `{"model":"client-model","system":[{"text":"` + automode.SecurityMarker + `"}]}`
	classifier := httptest.NewRequest(http.MethodPost, "/v1/messages?trace=%2F&trace=a+b", strings.NewReader(classifierBody))
	classifier.Header.Set("Authorization", "Bearer gateway-key")
	classifier.Header.Set("X-Claude-Code-Session-Id", "fixed-session")
	classifierResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(classifierResponse, classifier)
	if classifierResponse.Code != http.StatusNotImplemented || !strings.Contains(classifierResponse.Body.String(), "protocol_not_implemented") || normalCalls.Load() != 1 {
		t.Fatalf("fixed classifier = %d %s normal_calls=%d", classifierResponse.Code, classifierResponse.Body.String(), normalCalls.Load())
	}
	call := application.gateway.FixedTargetDiagnostics()
	if call == nil || call.UpstreamURL != "https://classifier.example/prefix/v1/responses?trace=%2F&trace=a+b" ||
		call.GatewayStatus != http.StatusNotImplemented || call.UpstreamStatus != 0 || call.SessionID != "fixed-session" {
		t.Fatalf("fixed diagnostics = %#v", call)
	}
	if len(application.selector.Assignments(provider.FixedTargetID)) != 0 {
		t.Fatal("fixed target entered scheduler assignments")
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	statusRequest.Header.Set("Authorization", "Bearer management-key")
	status := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(status, statusRequest)
	var statusBody struct {
		AutoMode struct {
			FixedProviderConfigured bool   `json:"fixed_provider_configured"`
			FixedProviderProtocol   string `json:"fixed_provider_protocol"`
			FixedTargetLastCall     *struct {
				UpstreamURL string `json:"upstream_url"`
			} `json:"fixed_target_last_call"`
		} `json:"auto_mode"`
	}
	decodeErr := json.Unmarshal(status.Body.Bytes(), &statusBody)
	if status.Code != http.StatusOK || decodeErr != nil || !statusBody.AutoMode.FixedProviderConfigured ||
		statusBody.AutoMode.FixedProviderProtocol != config.ProtocolOpenAIResponses ||
		statusBody.AutoMode.FixedTargetLastCall == nil || statusBody.AutoMode.FixedTargetLastCall.UpstreamURL != call.UpstreamURL ||
		strings.Contains(status.Body.String(), "fixed-secret") {
		t.Fatalf("fixed status = %d %s", status.Code, status.Body.String())
	}
	healthRequest := httptest.NewRequest(http.MethodGet, "/api/v1/provider-health", nil)
	healthRequest.Header.Set("Authorization", "Bearer management-key")
	healthResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK || strings.Contains(healthResponse.Body.String(), provider.FixedTargetID) || strings.Contains(healthResponse.Body.String(), "fixed-secret") {
		t.Fatalf("fixed target leaked into provider health = %d %s", healthResponse.Code, healthResponse.Body.String())
	}
}

func TestAppWiresMessagesHealthDiagnosticsAndRawLogging(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/messages" || r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("upstream request = path %q auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "raw-app-upstream-error")
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL + "/prefix",
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	var logOutput bytes.Buffer
	application, err := New(Options{
		ConfigPath: path,
		LogOpener:  appLogOpenerFor(&logOutput),
		Restart:    func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	crossDataRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model-a"}`))
	crossDataRequest.Header.Set("Authorization", "Bearer management-key")
	crossDataResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(crossDataResponse, crossDataRequest)
	if crossDataResponse.Code != http.StatusUnauthorized {
		t.Fatalf("management key authenticated Messages route = %d", crossDataResponse.Code)
	}
	crossManagementRequest := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	crossManagementRequest.Header.Set("Authorization", "Bearer gateway-key")
	crossManagementResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(crossManagementResponse, crossManagementRequest)
	if crossManagementResponse.Code != http.StatusUnauthorized {
		t.Fatalf("gateway key authenticated management route = %d", crossManagementResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=1", strings.NewReader(`{"model":"model-a","metadata":{"user_id":"app-session"}}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("X-Claude-Code-Session-Id", "app-session")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "bad_gateway") {
		t.Fatalf("Messages response = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(logOutput.String(), `"request_type":"normal"`) ||
		!strings.Contains(logOutput.String(), `"session_id":"app-session"`) ||
		!strings.Contains(logOutput.String(), `raw-app-upstream-error`) ||
		!strings.Contains(logOutput.String(), `"global_health":"cooldown"`) ||
		!strings.Contains(logOutput.String(), `"global_entered_cooldown":true`) ||
		!strings.Contains(logOutput.String(), `"channel_entered_cooldown":false`) ||
		!strings.Contains(logOutput.String(), `"cooldown_until":`) {
		t.Fatalf("gateway error log = %q", logOutput.String())
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/api/v1/provider-health", nil)
	healthRequest.Header.Set("Authorization", "Bearer management-key")
	healthResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK ||
		!strings.Contains(healthResponse.Body.String(), `"api_key":"provider-key"`) ||
		!strings.Contains(healthResponse.Body.String(), `"last_session_id":"app-session"`) ||
		!strings.Contains(healthResponse.Body.String(), `"last_error":"raw-app-upstream-error"`) ||
		!strings.Contains(healthResponse.Body.String(), upstream.URL+`/prefix/v1/messages?beta=1`) {
		t.Fatalf("provider health = %d %s", healthResponse.Code, healthResponse.Body.String())
	}
}

func TestAppLogsStructuredFailoverWithCompleteSourceAndNextProvider(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "raw-failover-error")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fallback-success")
	}))
	defer second.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{
		{
			ID:       "11111111-1111-4111-8111-111111111111",
			Name:     "first-provider",
			BaseURL:  first.URL,
			APIKey:   "first-key",
			Models:   []string{"model-a"},
			Priority: 1,
			Enabled:  true,
		},
		{
			ID:       "22222222-2222-4222-8222-222222222222",
			Name:     "second-provider",
			BaseURL:  second.URL,
			APIKey:   "second-key",
			Models:   []string{"model-a"},
			Priority: 0,
			Enabled:  true,
		},
	}
	writeAppConfig(t, path, cfg)
	var logOutput bytes.Buffer
	application, err := New(Options{
		ConfigPath: path,
		LogOpener:  appLogOpenerFor(&logOutput),
		Restart:    func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model-a","metadata":{"user_id":"body-session"}}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("X-Claude-Code-Session-Id", "header-session")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "fallback-success" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}

	logText := logOutput.String()
	if !strings.Contains(logText, `"kind":"forward"`) || !strings.Contains(logText, `"kind":"success"`) ||
		!strings.Contains(logText, `"request_type":"normal"`) {
		t.Fatalf("structured events = %q", logText)
	}
	if count := strings.Count(logText, `"kind":"failover"`); count != 1 {
		t.Fatalf("failover count = %d, log=%q", count, logText)
	}
	for _, field := range []string{
		`"provider_id":"11111111-1111-4111-8111-111111111111"`,
		`"provider_name":"first-provider"`,
		`"session_id":"header-session"`,
		`"model":"model-a"`,
		`"attempt":1`,
		`"upstream_url":"` + first.URL + `/v1/messages"`,
		`"raw_error":"raw-failover-error"`,
		`"next_provider_id":"22222222-2222-4222-8222-222222222222"`,
		`"next_provider_name":"second-provider"`,
		`"next_attempt":2`,
		`"next_upstream_url":"` + second.URL + `/v1/messages"`,
	} {
		if !strings.Contains(logText, field) {
			t.Fatalf("structured log missing %s: %q", field, logText)
		}
	}
}

func TestAppHotUpdateUsesNewProviderGenerationOnNextRequest(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider-a")
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider-b")
	}))
	defer upstreamB.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider",
		BaseURL: upstreamA.URL,
		APIKey:  "provider-key-a",
		Models:  []string{"model-a"},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	request := func(session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model-a"}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		req.Header.Set("X-Claude-Code-Session-Id", session)
		response := httptest.NewRecorder()
		application.server.Handler.ServeHTTP(response, req)
		return response
	}
	if first := request("before-update"); first.Code != http.StatusOK || first.Body.String() != "provider-a" {
		t.Fatalf("first response = %d %q", first.Code, first.Body.String())
	}
	next := application.Manager().Snapshot().Config()
	next.Providers[0].BaseURL = upstreamB.URL
	next.Providers[0].APIKey = "provider-key-b"
	if _, err := application.Manager().Apply(next); err != nil {
		t.Fatal(err)
	}
	if second := request("after-update"); second.Code != http.StatusOK || second.Body.String() != "provider-b" {
		t.Fatalf("second response = %d %q", second.Code, second.Body.String())
	}
}

func TestPendingConfigPromotesOnlyAfterResourcesAreReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	pending := active.Clone()
	pending.Service.ListenAddr = freeListenAddr(t)
	pending.Auth.GatewayKey = "new-gateway"
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(pending) error = %v", err)
	}
	defer application.Close()
	loaded, err := store.Load()
	if err != nil || loaded.Service.ListenAddr != pending.Service.ListenAddr || loaded.Auth.GatewayKey != "new-gateway" {
		t.Fatalf("active after promotion = %#v, %v", loaded, err)
	}
	if exists, _ := store.PendingExists(); exists {
		t.Fatal("pending file remains after promotion")
	}
	if application.Listener().Addr().String() != pending.Service.ListenAddr {
		t.Fatalf("bound listener = %s, want %s", application.Listener().Addr(), pending.Service.ListenAddr)
	}
}

func TestPendingResourceFailureRollsBackToActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	pending := active.Clone()
	pending.Service.ListenAddr = freeListenAddr(t)
	pending.Service.LogMaxBytes++
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	var logOutput bytes.Buffer
	logOpener := func(maxBytes int64) (io.Writer, func() error, error) {
		if maxBytes == pending.Service.LogMaxBytes {
			return nil, nil, errors.New("log resource failed")
		}
		return &logOutput, func() error { return nil }, nil
	}
	application, err := New(Options{ConfigPath: path, LogOpener: logOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(rollback) error = %v", err)
	}
	defer application.Close()
	loaded, err := store.Load()
	if err != nil || loaded.Service.ListenAddr != active.Service.ListenAddr || loaded.Service.LogMaxBytes != active.Service.LogMaxBytes {
		t.Fatalf("active after rollback = %#v, %v", loaded, err)
	}
	if exists, _ := store.PendingExists(); exists {
		t.Fatal("pending file remains after rollback")
	}
	if application.Listener().Addr().String() != active.Service.ListenAddr {
		t.Fatalf("fallback listener = %s, want %s", application.Listener().Addr(), active.Service.ListenAddr)
	}
	if status := application.Manager().RestartStatus(); status.State != "failed" || !strings.Contains(status.LastError, "log resource failed") {
		t.Fatalf("rollback status = %#v", status)
	}
	if !strings.Contains(logOutput.String(), `"level":"WARN","msg":"service"`) ||
		!strings.Contains(logOutput.String(), `"event":"pending_rejected"`) ||
		!strings.Contains(logOutput.String(), `"error":"initialize logs: log resource failed"`) {
		t.Fatalf("pending rejection log = %q", logOutput.String())
	}
}

func TestActiveLogFailureClosesAlreadyBoundListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	logOpener := func(int64) (io.Writer, func() error, error) {
		return nil, nil, errors.New("active log failed")
	}
	if application, err := New(Options{ConfigPath: path, LogOpener: logOpener}); err == nil || application != nil {
		t.Fatalf("New(active log failure) = %#v, %v", application, err)
	}
	listener, err := net.Listen("tcp", cfg.Service.ListenAddr)
	if err != nil {
		t.Fatalf("listener leaked after startup failure: %v", err)
	}
	_ = listener.Close()
}

func TestPendingPortConflictRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	occupied := freeListenAddr(t)
	blocker, err := net.Listen("tcp", occupied)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	pending := active.Clone()
	pending.Service.ListenAddr = occupied
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(port conflict) error = %v", err)
	}
	defer application.Close()
	loaded, _ := store.Load()
	if loaded.Service.ListenAddr != active.Service.ListenAddr {
		t.Fatalf("port-conflict active config = %s", loaded.Service.ListenAddr)
	}
}

func TestLogPreflightFailureDoesNotWritePendingOrChangeActiveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, cfg := baseAppConfig(t, path)
	logOpener := func(maxBytes int64) (io.Writer, func() error, error) {
		if maxBytes != cfg.Service.LogMaxBytes {
			return nil, nil, errors.New("candidate log failed")
		}
		return io.Discard, func() error { return nil }, nil
	}
	application, err := New(Options{ConfigPath: path, LogOpener: logOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body := marshalAppClientConfig(t, next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("log preflight response = %d %s", response.Code, response.Body.String())
	}
	if exists, err := store.PendingExists(); err != nil || exists {
		t.Fatalf("pending after log preflight failure = %v, %v", exists, err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Service.LogMaxBytes != cfg.Service.LogMaxBytes {
		t.Fatalf("active disk changed: %#v, %v", loaded, err)
	}
	if application.Manager().Snapshot().Config().Service.LogMaxBytes != cfg.Service.LogMaxBytes {
		t.Fatal("active snapshot changed after log preflight failure")
	}
}

func TestSelfExecFailureRebindsOldListenerAndKeepsServing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("recovered upstream request = path %q auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "recovered-data-plane")
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	store, cfg := baseAppConfig(t, path)
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL,
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Patches: []string{},
		Enabled: true,
	}}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var logOutput bytes.Buffer
	application, err := New(Options{
		ConfigPath:   path,
		LogOpener:    appLogOpenerFor(&logOutput),
		Exec:         func() error { return errors.New("exec denied") },
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()
	defer func() {
		_ = application.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("Serve did not stop")
		}
	}()

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body := marshalAppClientConfig(t, next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d %s", response.Code, response.Body.String())
	}
	for i := 0; i < 200; i++ {
		status := application.Manager().RestartStatus()
		if !status.InProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := application.Manager().RestartStatus(); status.InProgress || status.State != "failed" {
		t.Fatalf("restart failure status = %#v", status)
	}
	if !strings.Contains(logOutput.String(), `"level":"WARN","msg":"service"`) ||
		!strings.Contains(logOutput.String(), `"event":"restart_failed"`) ||
		!strings.Contains(logOutput.String(), `"error":"exec denied"`) {
		t.Fatalf("restart failure log = %q", logOutput.String())
	}
	// The old listener was closed during the failed exec and then rebound. A
	// request through the actual server proves the recovery loop resumed.
	client := &http.Client{Timeout: time.Second}
	statusReq, _ := http.NewRequest(http.MethodGet, "http://"+application.Listener().Addr().String()+"/api/v1/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	statusResp, err := client.Do(statusReq)
	if err != nil {
		t.Fatalf("status after exec failure: %v", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status after exec failure = %d", statusResp.StatusCode)
	}
	messagesReq, _ := http.NewRequest(http.MethodPost, "http://"+application.Listener().Addr().String()+"/v1/messages", strings.NewReader(`{"model":"model-a"}`))
	messagesReq.Header.Set("Authorization", "Bearer gateway-key")
	messagesResp, err := client.Do(messagesReq)
	if err != nil {
		t.Fatalf("Messages after exec failure: %v", err)
	}
	messagesBody, readErr := io.ReadAll(messagesResp.Body)
	_ = messagesResp.Body.Close()
	if readErr != nil || messagesResp.StatusCode != http.StatusOK || string(messagesBody) != "recovered-data-plane" {
		t.Fatalf("Messages after exec failure = %d %q, %v", messagesResp.StatusCode, messagesBody, readErr)
	}
}

func TestRestartClosesInFlightMessagesWithoutDrain(t *testing.T) {
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-releaseUpstream
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL,
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Patches: []string{},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	execStarted := make(chan struct{})
	application, err := New(Options{
		ConfigPath: path,
		LogOpener:  discardAppLogOpener,
		Exec: func() error {
			close(execStarted)
			return errors.New("exec denied")
		},
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()
	defer func() {
		_ = application.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	}()
	ready := false
	for i := 0; i < 100; i++ {
		request, _ := http.NewRequest(http.MethodGet, "http://"+cfg.Service.ListenAddr+"/api/v1/status", nil)
		request.Header.Set("Authorization", "Bearer management-key")
		response, requestErr := (&http.Client{Timeout: 100 * time.Millisecond}).Do(request)
		if requestErr == nil && response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			ready = true
			break
		}
		if response != nil {
			_ = response.Body.Close()
		}
		time.Sleep(time.Millisecond)
	}
	if !ready {
		t.Fatal("application did not become ready")
	}

	dataDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, "http://"+cfg.Service.ListenAddr+"/v1/messages", strings.NewReader(`{"model":"model-a"}`))
		request.Header.Set("Authorization", "Bearer gateway-key")
		response, requestErr := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		dataDone <- requestErr
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Messages request did not reach upstream")
	}

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body := marshalAppClientConfig(t, next)
	restartRequest := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
	restartRequest.Header.Set("Authorization", "Bearer management-key")
	restartResponse := httptest.NewRecorder()
	startedAt := time.Now()
	application.server.Handler.ServeHTTP(restartResponse, restartRequest)
	if restartResponse.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d %s", restartResponse.Code, restartResponse.Body.String())
	}
	select {
	case <-execStarted:
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("restart waited for drain: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart waited instead of starting exec")
	}
	select {
	case <-dataDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight client request was not interrupted")
	}
	for i := 0; i < 100 && application.gateway.ActiveRequests() != 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if active := application.gateway.ActiveRequests(); active != 0 {
		t.Fatalf("active Messages requests after restart = %d", active)
	}
}

func TestCloseDuringRestartDoesNotLeaveServeBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	execStarted := make(chan struct{})
	releaseExec := make(chan struct{})
	application, err := New(Options{
		ConfigPath: path,
		LogOpener:  discardAppLogOpener,
		Exec: func() error {
			close(execStarted)
			<-releaseExec
			return errors.New("exec denied")
		},
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body := marshalAppClientConfig(t, next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d", response.Code)
	}
	select {
	case <-execStarted:
	case <-time.After(time.Second):
		t.Fatal("exec did not start")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	close(releaseExec)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve remained blocked after Close during restart")
	}
}

func TestRotatingWriterPreservesOversizedRecordAndLatestHistory(t *testing.T) {
	dir := t.TempDir()
	oldArchive := filepath.Join(dir, logstore.ArchiveFileName)
	if err := os.WriteFile(oldArchive, []byte("oldest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := newRotatingWriter(dir, 12)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	oversized := []byte("oversized-record\n")
	if n, err := writer.Write(oversized); err != nil || n != len(oversized) {
		t.Fatalf("oversized Write() = %d, %v", n, err)
	}
	if got, err := os.ReadFile(oldArchive); err != nil || string(got) != "oldest\n" {
		t.Fatalf("empty-file write discarded archive: %q, %v", got, err)
	}
	if n, err := writer.Write([]byte("next\n")); err != nil || n != 5 {
		t.Fatalf("rotation Write() = %d, %v", n, err)
	}
	archive, err := os.ReadFile(oldArchive)
	if err != nil || string(archive) != string(oversized) {
		t.Fatalf("archive = %q, %v", archive, err)
	}
	active, err := os.ReadFile(filepath.Join(dir, logstore.ActiveFileName))
	if err != nil || string(active) != "next\n" {
		t.Fatalf("active = %q, %v", active, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("closed\n")); err == nil {
		t.Fatal("write after Close succeeded")
	}
}

func TestRotatingWriterKeepsRegularTotalWithinLimit(t *testing.T) {
	dir := t.TempDir()
	writer, err := newRotatingWriter(dir, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for i := 0; i < 20; i++ {
		if _, err := writer.Write([]byte("12345\n")); err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, name := range []string{logstore.ActiveFileName, logstore.ArchiveFileName} {
			if info, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil {
				total += info.Size()
			} else if !os.IsNotExist(statErr) {
				t.Fatal(statErr)
			}
		}
		if total > 20 {
			t.Fatalf("total log bytes = %d after write %d", total, i)
		}
	}
}
