package app

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/harnessconfig"
)

func TestProfileSwitchChangesNewRequestBudgetOnly(t *testing.T) {
	for _, mode := range []string{"switch", "edit"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer func() { once.Do(func() { close(release) }) }()
			var mu sync.Mutex
			calls := map[string]int{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				query := r.URL.RawQuery
				mu.Lock()
				calls[query]++
				n := calls[query]
				mu.Unlock()
				if query == "old" && n == 1 {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.WriteHeader(503)
				io.WriteString(w, "busy")
			}))
			defer upstream.Close()
			cfg := config.Default()
			cfg.Service.ListenAddr = freeListenAddr(t)
			cfg.Auth.ManagementKey = "management-key"
			cfg.Auth.GatewayKey = "gateway-key"
			cfg.Providers = []config.ProviderConfig{{ID: "11111111-1111-4111-8111-111111111111", Name: "A", BaseURL: upstream.URL, APIKey: "key", Models: []string{"m"}, Enabled: true, DisableHealth: true}}
			for i, id := range []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"} {
				budget := 8
				if i == 1 {
					budget = 2
				}
				cfg.Harnesses.ClaudeCode.Profiles = append(cfg.Harnesses.ClaudeCode.Profiles, config.Profile{ID: id, Name: id, HaikuModel: "m", SonnetModel: "m", OpusModel: "m", FableModel: "m", MaxAttempts: budget})
			}
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			writeAppConfig(t, path, cfg)
			var logs bytes.Buffer
			app, err := New(Options{ConfigPath: path, LogOpener: appLogOpenerFor(&logs), HarnessHomeDir: func() (string, error) { return filepath.Join(root, "home"), nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			if _, err := app.harnesses.Activate(harnessconfig.ClaudeCodeAdapterID, cfg.Harnesses.ClaudeCode.Profiles[0].ID); err != nil {
				t.Fatal(err)
			}
			serve := func(query string) {
				r := httptest.NewRequest(http.MethodPost, "/v1/messages?"+query, strings.NewReader(`{"model":"m"}`))
				r.Header.Set("Authorization", "Bearer gateway-key")
				r.Header.Set("X-Claude-Code-Session-Id", "same-session")
				w := httptest.NewRecorder()
				app.server.Handler.ServeHTTP(w, r)
				if w.Code != 503 {
					t.Errorf("response=%d", w.Code)
				}
			}
			done := make(chan struct{})
			go func() { defer close(done); serve("old") }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("old request did not start")
			}
			if mode == "switch" {
				_, err = app.harnesses.Activate(harnessconfig.ClaudeCodeAdapterID, cfg.Harnesses.ClaudeCode.Profiles[1].ID)
			} else {
				p := cfg.Harnesses.ClaudeCode.Profiles[0]
				p.MaxAttempts = 2
				_, err = app.harnesses.UpdateProfile(harnessconfig.ClaudeCodeAdapterID, p.ID, p)
			}
			if err != nil {
				t.Fatal(err)
			}
			serve("new")
			once.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("old request did not finish")
			}
			mu.Lock()
			defer mu.Unlock()
			if calls["old"] != 8 || calls["new"] != 2 {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestClassifierIgnoresLargeActiveProfileBudget(t *testing.T) {
	for _, mode := range []string{config.AutoModeProviderPool, config.AutoModeFixedProvider} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(503)
				io.WriteString(w, "busy")
			}))
			defer upstream.Close()
			cfg := config.Default()
			cfg.Service.ListenAddr = freeListenAddr(t)
			cfg.Auth.ManagementKey = "management-key"
			cfg.Auth.GatewayKey = "gateway-key"
			cfg.AutoMode = config.AutoModeConfig{Mode: mode, Model: "m"}
			if mode == config.AutoModeFixedProvider {
				cfg.AutoMode.FixedProvider = &config.FixedProviderConfig{BaseURL: upstream.URL, APIKey: "key", Protocol: "anthropic_messages"}
			}
			cfg.Providers = []config.ProviderConfig{{ID: "11111111-1111-4111-8111-111111111111", Name: "A", BaseURL: upstream.URL, APIKey: "key", Models: []string{"m"}, Enabled: true, DisableHealth: true}}
			p := config.Profile{ID: "22222222-2222-4222-8222-222222222222", Name: "Large", HaikuModel: "m", SonnetModel: "m", OpusModel: "m", FableModel: "m", MaxAttempts: 9007199254740991}
			cfg.Harnesses.ClaudeCode.Profiles = []config.Profile{p}
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			writeAppConfig(t, path, cfg)
			var logs bytes.Buffer
			app, err := New(Options{ConfigPath: path, LogOpener: appLogOpenerFor(&logs), HarnessHomeDir: func() (string, error) { return filepath.Join(root, "home"), nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			if _, err := app.harnesses.Activate(harnessconfig.ClaudeCodeAdapterID, p.ID); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`))
			r.Header.Set("Authorization", "Bearer gateway-key")
			w := httptest.NewRecorder()
			app.server.Handler.ServeHTTP(w, r)
			mu.Lock()
			defer mu.Unlock()
			if calls != 1 || w.Code != 503 {
				t.Fatalf("classifier calls=%d status=%d", calls, w.Code)
			}
		})
	}
}
