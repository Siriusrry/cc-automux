package protocol

import (
	"net/http"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

type registryTestAdapter struct{ id string }

func (a *registryTestAdapter) Protocol() string { return a.id }

func (*registryTestAdapter) EncodeRequest(body bodyfile.Body, headers http.Header) (ProtocolMessage, error) {
	return ProtocolMessage{Body: body, Headers: headers}, nil
}

func (*registryTestAdapter) DecodeResponse(body bodyfile.Body, headers http.Header) (ProtocolMessage, error) {
	return ProtocolMessage{Body: body, Headers: headers}, nil
}

func TestRegistryValidatesAndLooksUpAdapters(t *testing.T) {
	responses := &registryTestAdapter{id: "openai_responses"}
	compatible := &registryTestAdapter{id: "openai_compatible"}
	registry, err := NewRegistry(responses, compatible)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Lookup(responses.id); !ok || got != responses {
		t.Fatalf("responses lookup = %#v, %v", got, ok)
	}
	if _, ok := registry.Lookup("missing"); ok {
		t.Fatal("missing protocol was found")
	}
	listed := registry.List()
	if len(listed) != 2 || listed[0] != "openai_compatible" || listed[1] != "openai_responses" {
		t.Fatalf("List() = %#v", listed)
	}
	listed[0] = "mutated"
	if again := registry.List(); again[0] != "openai_compatible" {
		t.Fatalf("List() shares state: %#v", again)
	}
}

func TestRegistryRejectsInvalidAdapters(t *testing.T) {
	var nilAdapter *registryTestAdapter
	for _, test := range []struct {
		name     string
		adapters []ProtocolAdapter
	}{
		{name: "nil", adapters: []ProtocolAdapter{nilAdapter}},
		{name: "empty protocol", adapters: []ProtocolAdapter{&registryTestAdapter{}}},
		{name: "whitespace protocol", adapters: []ProtocolAdapter{&registryTestAdapter{id: "bad id"}}},
		{name: "duplicate", adapters: []ProtocolAdapter{&registryTestAdapter{id: "same"}, &registryTestAdapter{id: "same"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.adapters...); err == nil {
				t.Fatal("NewRegistry() unexpectedly succeeded")
			}
		})
	}
	if _, ok := EmptyRegistry().Lookup("openai_responses"); ok {
		t.Fatal("empty production registry exposed an adapter")
	}
}
