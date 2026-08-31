package gateway

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
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
	}, func() provider.RuntimeContext {
		registry := patch.DefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
		context, contextErr := provider.NewRuntimeContext(registry)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		return context
	}())
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

func TestClientPoolRetiresOldGenerationWithoutRepersistingIt(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	old := &provider.CompiledProvider{CompiledTarget: provider.CompiledTarget{ID: id, Generation: provider.ProviderGeneration("old")}}
	current := &provider.CompiledProvider{CompiledTarget: provider.CompiledTarget{ID: id, Generation: provider.ProviderGeneration("current")}}
	oldKey := clientKey{providerID: id, generation: old.Generation}
	currentKey := clientKey{providerID: id, generation: current.Generation}

	pool := NewClientPool()
	defer pool.Close()
	pool.Reconcile([]*provider.CompiledProvider{old})
	inFlight, err := pool.Acquire(old)
	if err != nil {
		t.Fatal(err)
	}
	pool.Reconcile([]*provider.CompiledProvider{current})
	if !inFlight.entry.retired || inFlight.entry.idleClosed {
		t.Fatalf("in-flight retired entry = %#v", inFlight.entry)
	}

	late, err := pool.Acquire(old)
	if err != nil {
		t.Fatal(err)
	}
	if !late.entry.retired {
		t.Fatal("stale generation received an active pooled entry")
	}
	pool.mu.Lock()
	_, oldPersisted := pool.clients[oldKey]
	_, currentPersistedBeforeAcquire := pool.clients[currentKey]
	pool.mu.Unlock()
	if oldPersisted || currentPersistedBeforeAcquire {
		t.Fatalf("pool state before current acquire: old=%v current=%v", oldPersisted, currentPersistedBeforeAcquire)
	}

	currentLease, err := pool.Acquire(current)
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	_, currentPersisted := pool.clients[currentKey]
	pool.mu.Unlock()
	if !currentPersisted || currentLease.entry.retired {
		t.Fatal("current generation was not retained in the pool")
	}

	late.Release()
	if !late.entry.idleClosed || late.entry.refs != 0 {
		t.Fatalf("late stale lease was not closed: %#v", late.entry)
	}
	inFlight.Release()
	if !inFlight.entry.idleClosed || inFlight.entry.refs != 0 {
		t.Fatalf("in-flight stale lease was not closed: %#v", inFlight.entry)
	}
	currentLease.Release()
	if currentLease.entry.idleClosed || currentLease.entry.refs != 0 {
		t.Fatalf("active current lease was retired: %#v", currentLease.entry)
	}
}

func TestClientPoolConcurrentStaleAcquireCannotRestoreGeneration(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	old := &provider.CompiledProvider{CompiledTarget: provider.CompiledTarget{ID: id, Generation: provider.ProviderGeneration("old")}}
	current := &provider.CompiledProvider{CompiledTarget: provider.CompiledTarget{ID: id, Generation: provider.ProviderGeneration("current")}}
	pool := NewClientPool()
	defer pool.Close()
	pool.Reconcile([]*provider.CompiledProvider{current})

	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			lease, err := pool.Acquire(old)
			if err != nil {
				t.Errorf("Acquire(old): %v", err)
				return
			}
			lease.Release()
		}()
	}
	close(start)
	wait.Wait()

	pool.mu.Lock()
	_, oldPersisted := pool.clients[clientKey{providerID: id, generation: old.Generation}]
	pool.mu.Unlock()
	if oldPersisted {
		t.Fatal("concurrent stale requests restored the retired generation")
	}
}
