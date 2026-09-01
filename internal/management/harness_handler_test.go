package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	runtimeconfig "github.com/Siriusrry/cc-automux/internal/runtime"
)

type harnessHandlerFixture struct {
	handler *Handler
	runtime *runtimeconfig.Manager
	harness *harnessconfig.Manager
	store   *config.Store
	home    string
	target  string
}

func newHarnessHandlerFixture(t *testing.T, gatewayKey string) harnessHandlerFixture {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "runtime", "config.json")
	store, err := config.NewStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Service.ListenAddr = freeManagementTestAddress(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = gatewayKey
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	patchRegistry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	runtimeContext, err := provider.NewRuntimeContext(patchRegistry)
	if err != nil {
		t.Fatal(err)
	}
	runtimeManager, err := runtimeconfig.NewManager(store, cfg, runtimeconfig.Options{
		RuntimeContext: runtimeContext,
		Preflight:      func(config.Config, config.Config) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter := harnessconfig.NewClaudeCodeAdapterWithHomeResolver(func() (string, error) { return home, nil })
	registry, err := harnessconfig.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	harness, err := harnessconfig.NewManager(runtimeManager, registry)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithOptions(runtimeManager, Options{Harnesses: harness})
	return harnessHandlerFixture{
		handler: handler,
		runtime: runtimeManager,
		harness: harness,
		store:   store,
		home:    home,
		target:  filepath.Join(home, ".claude", "settings.json"),
	}
}

func freeManagementTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func harnessRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer management-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validHarnessProfile(id, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name,
		"haiku_model": "haiku-" + name, "sonnet_model": "sonnet-" + name,
		"opus_model": "opus-" + name, "fable_model": "fable-" + name,
		"subagent_model":         "subagent-" + name,
		"teammate_default_model": "teammate-" + name,
	}
}

func encodeHarnessValue(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHarnessHTTPDiscoveryStatusCRUDAndActivation(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "gateway-key")
	handler := fixture.handler

	discovery := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses", "")
	if discovery.Code != http.StatusOK {
		t.Fatalf("discovery = %d %s", discovery.Code, discovery.Body.String())
	}
	var adapters []harnessconfig.AdapterInfo
	if err := json.Unmarshal(discovery.Body.Bytes(), &adapters); err != nil {
		t.Fatal(err)
	}
	if len(adapters) != 1 || adapters[0].ID != harnessconfig.ClaudeCodeAdapterID || !adapters[0].ProfileSupport ||
		len(adapters[0].PathModes) != 2 || adapters[0].PathModes[0] != config.PathModeDefault ||
		adapters[0].PathModes[1] != config.PathModeCustom || adapters[0].DefaultPathMode != config.PathModeDefault {
		t.Fatalf("discovery body = %#v", adapters)
	}

	status := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses/claude-code", "")
	if status.Code != http.StatusOK {
		t.Fatalf("initial harness status = %d %s", status.Code, status.Body.String())
	}
	var initial harnessconfig.HarnessStatus
	if err := json.Unmarshal(status.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.State != harnessconfig.StateInactive || initial.ActiveProfileID != "" || initial.ResolvedSettingsPath != fixture.target {
		t.Fatalf("initial harness status = %#v", initial)
	}

	// Harness PUT uses the explicitly selected partial-update contract: omitted
	// fields retain their current values, and an empty object is a no-op.
	partial := harnessRequest(handler, http.MethodPut, "/api/v1/harnesses/claude-code", `{"disable_telemetry":false}`)
	if partial.Code != http.StatusOK {
		t.Fatalf("partial harness update = %d %s", partial.Code, partial.Body.String())
	}
	var partialStatus harnessconfig.HarnessStatus
	if err := json.Unmarshal(partial.Body.Bytes(), &partialStatus); err != nil {
		t.Fatal(err)
	}
	if partialStatus.PathMode != config.PathModeDefault || partialStatus.SettingsPath != "" || partialStatus.DisableTelemetry {
		t.Fatalf("partial harness update lost omitted fields = %#v", partialStatus)
	}
	noOp := harnessRequest(handler, http.MethodPut, "/api/v1/harnesses/claude-code", `{}`)
	if noOp.Code != http.StatusOK {
		t.Fatalf("empty harness update = %d %s", noOp.Code, noOp.Body.String())
	}
	var noOpStatus harnessconfig.HarnessStatus
	if err := json.Unmarshal(noOp.Body.Bytes(), &noOpStatus); err != nil {
		t.Fatal(err)
	}
	if noOpStatus.PathMode != partialStatus.PathMode || noOpStatus.SettingsPath != partialStatus.SettingsPath ||
		noOpStatus.DisableTelemetry != partialStatus.DisableTelemetry {
		t.Fatalf("empty harness update changed state = %#v", noOpStatus)
	}

	firstBody := encodeHarnessValue(t, validHarnessProfile("11111111-1111-4111-8111-111111111111", "Daily"))
	created := harnessRequest(handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles", firstBody)
	if created.Code != http.StatusCreated || !strings.HasSuffix(created.Header().Get("Location"), "/profiles/11111111-1111-4111-8111-111111111111") {
		t.Fatalf("create profile = %d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	var createdProfile harnessconfig.ProfileView
	if err := json.Unmarshal(created.Body.Bytes(), &createdProfile); err != nil {
		t.Fatal(err)
	}
	if createdProfile.ID != "11111111-1111-4111-8111-111111111111" || createdProfile.Active {
		t.Fatalf("created profile = %#v", createdProfile)
	}
	duplicate := harnessRequest(handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles", encodeHarnessValue(t, validHarnessProfile("22222222-2222-4222-8222-222222222222", "Daily")))
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), `"error":"conflict"`) {
		t.Fatalf("duplicate profile = %d %s", duplicate.Code, duplicate.Body.String())
	}

	profiles := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses/claude-code/profiles", "")
	if profiles.Code != http.StatusOK {
		t.Fatalf("profiles = %d %s", profiles.Code, profiles.Body.String())
	}
	var profileList []harnessconfig.ProfileView
	if err := json.Unmarshal(profiles.Body.Bytes(), &profileList); err != nil || len(profileList) != 1 {
		t.Fatalf("profile list = %#v, err %v", profileList, err)
	}

	activate := harnessRequest(handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles/11111111-1111-4111-8111-111111111111/activate", "{}")
	if activate.Code != http.StatusOK {
		t.Fatalf("activate = %d %s", activate.Code, activate.Body.String())
	}
	var activation harnessconfig.ActivationResult
	if err := json.Unmarshal(activate.Body.Bytes(), &activation); err != nil {
		t.Fatal(err)
	}
	if !activation.Active || activation.Harness.State != harnessconfig.StateInSync || activation.Harness.ActiveProfileID != createdProfile.ID {
		t.Fatalf("activation = %#v", activation)
	}
	if _, err := os.Stat(fixture.target); err != nil {
		t.Fatal(err)
	}

	activeProfile := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses/claude-code/profiles/11111111-1111-4111-8111-111111111111", "")
	if activeProfile.Code != http.StatusOK || !strings.Contains(activeProfile.Body.String(), `"active":true`) {
		t.Fatalf("active profile = %d %s", activeProfile.Code, activeProfile.Body.String())
	}
	deleteActive := harnessRequest(handler, http.MethodDelete, "/api/v1/harnesses/claude-code/profiles/11111111-1111-4111-8111-111111111111", "")
	if deleteActive.Code != http.StatusConflict || !strings.Contains(deleteActive.Body.String(), `"error":"active_profile"`) {
		t.Fatalf("delete active = %d %s", deleteActive.Code, deleteActive.Body.String())
	}

	// A path update is server-side merged and invalidates active without
	// rewriting the external file.
	customPath := filepath.Join(fixture.home, "custom", "settings.json")
	updateHarness := encodeHarnessValue(t, map[string]any{
		"path_mode": config.PathModeCustom, "settings_path": customPath,
	})
	updated := harnessRequest(handler, http.MethodPut, "/api/v1/harnesses/claude-code", updateHarness)
	if updated.Code != http.StatusOK {
		t.Fatalf("harness update = %d %s", updated.Code, updated.Body.String())
	}
	var updatedStatus harnessconfig.HarnessStatus
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedStatus); err != nil {
		t.Fatal(err)
	}
	if updatedStatus.State != harnessconfig.StateInactive || updatedStatus.ActiveProfileID != "" || updatedStatus.ResolvedSettingsPath != customPath {
		t.Fatalf("updated harness status = %#v", updatedStatus)
	}
	if _, err := os.Stat(fixture.target); err != nil {
		t.Fatal("path update unexpectedly removed old target")
	}
}

func TestHarnessHTTPStrictBodiesErrorsAndMethods(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "")
	handler := NewWithOptions(fixture.runtime, Options{Harnesses: fixture.harness})

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		error  string
	}{
		{name: "unknown harness field", method: http.MethodPut, path: "/api/v1/harnesses/claude-code", body: `{"unknown":true}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "duplicate harness field", method: http.MethodPut, path: "/api/v1/harnesses/claude-code", body: `{"path_mode":"default","path_mode":"default"}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "null harness field", method: http.MethodPut, path: "/api/v1/harnesses/claude-code", body: `{"path_mode":null}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "active harness field", method: http.MethodPut, path: "/api/v1/harnesses/claude-code", body: `{"active_profile_id":""}`, status: http.StatusConflict, error: "active_profile_read_only"},
		{name: "trailing harness JSON", method: http.MethodPut, path: "/api/v1/harnesses/claude-code", body: `{} {}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "unknown profile field", method: http.MethodPost, path: "/api/v1/harnesses/claude-code/profiles", body: `{"unknown":true}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "active profile field", method: http.MethodPost, path: "/api/v1/harnesses/claude-code/profiles", body: `{"active":false}`, status: http.StatusConflict, error: "active_profile_read_only"},
		{name: "profile null", method: http.MethodPost, path: "/api/v1/harnesses/claude-code/profiles", body: `{"name":null}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "activation body", method: http.MethodPost, path: "/api/v1/harnesses/claude-code/profiles/nope/activate", body: `{"x":1}`, status: http.StatusBadRequest, error: "invalid_json"},
		{name: "gateway missing", method: http.MethodPost, path: "/api/v1/harnesses/claude-code/profiles/nope/activate", body: `{}`, status: http.StatusNotFound, error: "profile_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := harnessRequest(handler, tc.method, tc.path, tc.body)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"error":"`+tc.error+`"`) {
				t.Fatalf("response = %d %s, want %d/%s", response.Code, response.Body.String(), tc.status, tc.error)
			}
		})
	}

	if response := harnessRequest(handler, http.MethodPatch, "/api/v1/harnesses", ""); response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("discovery method = %d allow=%q", response.Code, response.Header().Get("Allow"))
	}
	if response := harnessRequest(handler, http.MethodPatch, "/api/v1/harnesses/claude-code/profiles", ""); response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("profile collection method = %d allow=%q", response.Code, response.Header().Get("Allow"))
	}
	if response := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses/unknown", ""); response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"error":"harness_not_found"`) {
		t.Fatalf("unknown harness = %d %s", response.Code, response.Body.String())
	}
	if response := harnessRequest(handler, http.MethodGet, "/api/v1/harnesses/claude-code/profiles/one/extra", ""); response.Code != http.StatusNotFound {
		t.Fatalf("unknown harness route = %d", response.Code)
	}

	oversized := strings.Repeat("x", 1<<20+1)
	limited := NewWithOptions(fixture.runtime, Options{Harnesses: fixture.harness, MaxBodyBytes: 1 << 20})
	response := harnessRequest(limited, http.MethodPut, "/api/v1/harnesses/claude-code", oversized)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"error":"request_too_large"`) {
		t.Fatalf("oversized harness body = %d %s", response.Code, response.Body.String())
	}
}

func TestHarnessHTTPProfileIDImmutabilityAndConfigClientReadOnly(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "gateway-key")
	handler := fixture.handler
	profileID := "11111111-1111-4111-8111-111111111111"
	created := harnessRequest(handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles", encodeHarnessValue(t, validHarnessProfile(profileID, "Daily")))
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	changedID := validHarnessProfile("22222222-2222-4222-8222-222222222222", "Daily")
	if response := harnessRequest(handler, http.MethodPut, "/api/v1/harnesses/claude-code/profiles/"+profileID, encodeHarnessValue(t, changedID)); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"id_immutable"`) {
		t.Fatalf("profile id change = %d %s", response.Code, response.Body.String())
	}

	full := fixture.runtime.Config()
	full.Auth.GatewayKey = "new-gateway-key"
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	response := request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", string(body))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"active_profile_read_only"`) {
		t.Fatalf("full config active field = %d %s", response.Code, response.Body.String())
	}

	// The client-side config decoder still accepts a body with the active field
	// omitted, and the runtime preserves the server-owned empty state.
	full.Harnesses.ClaudeCode.ActiveProfileID = ""
	fullData, err := config.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(fullData, &object); err != nil {
		t.Fatal(err)
	}
	var harnessObject map[string]json.RawMessage
	if err := json.Unmarshal(object["harnesses"], &harnessObject); err != nil {
		t.Fatal(err)
	}
	var claudeObject map[string]json.RawMessage
	if err := json.Unmarshal(harnessObject["claude_code"], &claudeObject); err != nil {
		t.Fatal(err)
	}
	delete(claudeObject, "active_profile_id")
	harnessObject["claude_code"], _ = json.Marshal(claudeObject)
	object["harnesses"], _ = json.Marshal(harnessObject)
	clientData, _ := json.Marshal(object)
	response = request(handler, http.MethodPut, "/api/v1/config", "Bearer management-key", string(clientData))
	if response.Code != http.StatusOK {
		t.Fatalf("config body without active field = %d %s", response.Code, response.Body.String())
	}
}

func TestHarnessHTTPUnauthorizedAndActivationFailureDoesNotTouchTarget(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "")
	profileID := "11111111-1111-4111-8111-111111111111"
	profile := managerProfileForManagementTest(profileID, "Daily")
	if _, err := fixture.harness.CreateProfile(harnessconfig.ClaudeCodeAdapterID, profile); err != nil {
		t.Fatal(err)
	}
	if response := request(fixture.handler, http.MethodGet, "/api/v1/harnesses", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized harness = %d", response.Code)
	}
	if _, err := os.Stat(fixture.target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists before failed activation: %v", err)
	}
	response := harnessRequest(fixture.handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles/"+profileID+"/activate", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"gateway_not_configured"`) {
		t.Fatalf("missing gateway activation = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(fixture.target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target changed after missing gateway activation: %v", err)
	}
}

func managerProfileForManagementTest(id, name string) config.Profile {
	return config.Profile{ID: id, Name: name, HaikuModel: "h", SonnetModel: "s", OpusModel: "o", FableModel: "f"}
}

func TestHarnessHTTPActivationResponseIsStableJSON(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "gateway-key")
	profileID := "11111111-1111-4111-8111-111111111111"
	if _, err := fixture.harness.CreateProfile(harnessconfig.ClaudeCodeAdapterID, managerProfileForManagementTest(profileID, "Daily")); err != nil {
		t.Fatal(err)
	}
	response := harnessRequest(fixture.handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles/"+profileID+"/activate", "")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if !bytes.HasSuffix(response.Body.Bytes(), []byte("\n")) {
		t.Fatal("activation response has no newline")
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["active"] != true {
		t.Fatalf("activation response = %#v", value)
	}
}

func TestHarnessHTTPRestartReadStateAndActivationConflict(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "gateway-key")
	profileID := "11111111-1111-4111-8111-111111111111"
	if _, err := fixture.harness.CreateProfile(harnessconfig.ClaudeCodeAdapterID, managerProfileForManagementTest(profileID, "Daily")); err != nil {
		t.Fatal(err)
	}

	next := fixture.runtime.Config()
	next.Service.LogMaxBytes++
	if _, err := fixture.runtime.Apply(next); err != nil {
		t.Fatalf("prepare restart = %v", err)
	}

	// A status read is still useful while the runtime rejects mutation
	// callbacks. It must fail closed about active state rather than presenting
	// the un-reconciled persisted ID as verified.
	statusResponse := harnessRequest(fixture.handler, http.MethodGet, "/api/v1/harnesses/claude-code", "")
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("restart status = %d %s", statusResponse.Code, statusResponse.Body.String())
	}
	var status harnessconfig.HarnessStatus
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != harnessconfig.StateError || status.ActiveProfileID != "" {
		t.Fatalf("restart harness status = %#v", status)
	}

	activation := harnessRequest(fixture.handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles/"+profileID+"/activate", "{}")
	if activation.Code != http.StatusConflict || !strings.Contains(activation.Body.String(), `"error":"restart_in_progress"`) {
		t.Fatalf("restart activation = %d %s", activation.Code, activation.Body.String())
	}
}

func TestHarnessHTTPStatusDoesNotExposeTargetContents(t *testing.T) {
	fixture := newHarnessHandlerFixture(t, "gateway-key")
	profileID := "11111111-1111-4111-8111-111111111111"
	if _, err := fixture.harness.CreateProfile(harnessconfig.ClaudeCodeAdapterID, managerProfileForManagementTest(profileID, "Daily")); err != nil {
		t.Fatal(err)
	}
	if response := harnessRequest(fixture.handler, http.MethodPost, "/api/v1/harnesses/claude-code/profiles/"+profileID+"/activate", "{}"); response.Code != http.StatusOK {
		t.Fatalf("initial activation = %d %s", response.Code, response.Body.String())
	}

	secret := "target-secret-that-must-not-be-returned-by-status"
	if err := os.WriteFile(fixture.target, []byte(`{"env":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	response := harnessRequest(fixture.handler, http.MethodGet, "/api/v1/harnesses/claude-code", "")
	if response.Code != http.StatusOK {
		t.Fatalf("invalid target status = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("target content leaked in status: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"active_profile_id":""`) {
		t.Fatalf("invalid target status still reports active: %s", response.Body.String())
	}
}
