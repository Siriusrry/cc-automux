package gateway

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
)

func TestClientPoolUsesCompiledCustomCAAndGatewayAuthHeaders(t *testing.T) {
	const providerKey = "provider-key"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+providerKey {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("X-Api-Key = %q", got)
		}
		if got := request.Header.Get("X-End-To-End"); got != "preserved" {
			t.Errorf("X-End-To-End = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := provider.Compile(config.ProviderConfig{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "custom-ca",
		BaseURL: upstream.URL,
		APIKey:  providerKey,
		Models:  []string{"model"},
		Enabled: true,
		TLS:     config.TLSConfig{CAFile: caPath},
	})
	if err != nil {
		t.Fatal(err)
	}

	headers, err := prepareUpstreamHeaders(http.Header{
		"Authorization": []string{"Bearer gateway-key"},
		"X-Api-Key":     []string{"client-key"},
		"X-End-To-End":  []string{"preserved"},
	}, item)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, upstream.URL+MessagesPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = headers

	pool := NewClientPool()
	defer pool.Close()
	client, err := pool.Client(item)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("custom-CA request through ClientPool failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("custom-CA status = %d", response.StatusCode)
	}
}
