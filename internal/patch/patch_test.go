package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

func bodyFromString(t *testing.T, value string) bodyfile.Body {
	t.Helper()
	body, err := bodyfile.Capture(strings.NewReader(value), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.Close() })
	return body
}

func readBody(t *testing.T, body bodyfile.Body) string {
	t.Helper()
	reader, err := body.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	value, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func patchContext(typ RequestType) PatchContext {
	return PatchContext{RequestType: typ, OriginalModel: "original", EffectiveModel: "effective", TargetID: "target", Generation: "generation"}
}

func executeRequest(t *testing.T, id string, context PatchContext, input string, headers http.Header) (*MutableRequest, error) {
	t.Helper()
	registry := DefaultRegistry(Services{AliasStore: NewAliasStore()})
	plan, err := registry.Compile([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := plan.NewInstance(context)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = execution.Close() })
	request := &MutableRequest{Body: bodyFromString(t, input), Headers: NewHTTPHeaderSet(headers)}
	return request, execution.ApplyRequestOnly(request)
}

func TestDefaultRegistryMetadataOrderAndFiltering(t *testing.T) {
	registry := DefaultRegistry(Services{AliasStore: NewAliasStore()})
	list := registry.List()
	want := []string{
		AnyRouterSubagentThinkingID,
		AnyRouterClassifierRequestID,
		CLIProxyAPIClassifierSessionID,
		GPTClassifierResponseReassemblyID,
	}
	if len(list) != len(want) {
		t.Fatalf("List length = %d", len(list))
	}
	for i, id := range want {
		if list[i].ID != id || list[i].Conflicts == nil {
			t.Fatalf("List[%d] = %#v", i, list[i])
		}
	}
	plan, err := registry.Compile([]string{AnyRouterClassifierRequestID}, RequestTypeClassifier)
	if err != nil || plan.Len() != 1 {
		t.Fatalf("classifier plan = %#v, %v", plan.IDs(), err)
	}
	if _, err := registry.Compile([]string{AnyRouterSubagentThinkingID}, RequestTypeClassifier); err == nil {
		t.Fatal("normal-only patch compiled for classifier target")
	}
	if _, err := registry.Compile([]string{AnyRouterSubagentThinkingID, AnyRouterSubagentThinkingID}); !errors.Is(err, ErrDuplicatePatch) {
		t.Fatalf("duplicate = %v", err)
	}
}

func TestDefaultRegistryRequiresCallerOwnedAliasStore(t *testing.T) {
	if _, err := NewDefaultRegistry(Services{}); err == nil || !strings.Contains(err.Error(), "alias store service is required") {
		t.Fatalf("missing AliasStore error = %v", err)
	}
	store := NewAliasStore()
	registry, err := NewDefaultRegistry(Services{AliasStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if registry.AliasStore() != store || !registry.RequiresAliasStore() {
		t.Fatal("default registry did not retain the caller-owned AliasStore")
	}
}

func TestRegistryValidatesFactoryCapabilitiesForEveryDeclaredType(t *testing.T) {
	definition := PatchDefinition{
		ID:           "multi-type-capability",
		Name:         "multi-type-capability",
		RequestTypes: []RequestType{RequestTypeNormal, RequestTypeClassifier},
		Stages:       []Stage{StageRequest},
		Idempotence:  Idempotent,
		Factory: func(context FactoryContext) (PatchInstance, error) {
			if context.RequestType == RequestTypeClassifier {
				return NewHooksInstance(Hooks{}), nil
			}
			return NewHooksInstance(Hooks{Request: orderPatch{}}), nil
		},
	}
	if _, err := NewRegistry([]PatchDefinition{definition}); !errors.Is(err, ErrHookUnavailable) {
		t.Fatalf("type-specific capability mismatch = %v", err)
	}

	wildcard := definition
	wildcard.ID = "wildcard-capability"
	wildcard.RequestTypes = []RequestType{RequestTypeAny}
	wildcard.Factory = func(context FactoryContext) (PatchInstance, error) {
		if context.RequestType == RequestTypeClassifier {
			return NewHooksInstance(Hooks{}), nil
		}
		return NewHooksInstance(Hooks{Request: orderPatch{}}), nil
	}
	if _, err := NewRegistry([]PatchDefinition{wildcard}); !errors.Is(err, ErrHookUnavailable) {
		t.Fatalf("wildcard capability mismatch = %v", err)
	}
}

type orderPatch struct {
	id     string
	record *[]string
}

func (p orderPatch) ApplyRequest(PatchContext, *MutableRequest) error {
	*p.record = append(*p.record, "request:"+p.id)
	return nil
}

func (p orderPatch) ApplyResponse(PatchContext, *MutableResponse) error {
	*p.record = append(*p.record, "response:"+p.id)
	return nil
}

func TestExecutionRequestForwardResponseReverseAndAtMostOnce(t *testing.T) {
	var record []string
	definition := func(id string) PatchDefinition {
		return PatchDefinition{
			ID: id, Name: id, RequestTypes: []RequestType{RequestTypeNormal},
			Stages: []Stage{StageRequest, StageResponse}, Idempotence: PerExecution,
			Factory: func(FactoryContext) (PatchInstance, error) {
				hook := orderPatch{id: id, record: &record}
				return NewHooksInstance(Hooks{Request: hook, Response: hook}), nil
			},
		}
	}
	registry, err := NewRegistry([]PatchDefinition{definition("a"), definition("b")})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Compile([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := plan.NewInstance(patchContext(RequestTypeNormal))
	if err != nil {
		t.Fatal(err)
	}
	body := bodyFromString(t, `{"model":"m"}`)
	request := &MutableRequest{Body: body, Headers: NewHTTPHeaderSet(nil)}
	response := &MutableResponse{Status: 200, Body: body, Headers: NewHTTPHeaderSet(nil)}
	if err := execution.ApplyRequestOnly(request); err != nil {
		t.Fatal(err)
	}
	if err := execution.ApplyResponseOnly(response); err != nil {
		t.Fatal(err)
	}
	want := []string{"request:a", "request:b", "response:b", "response:a"}
	if strings.Join(record, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %#v", record)
	}
	if err := execution.ApplyRequestOnly(request); err == nil {
		t.Fatal("second request hook call succeeded")
	}
}

func TestAliasStoreConcurrentStableTTLAndLRU(t *testing.T) {
	now := time.Unix(1_000, 0)
	random := bytes.NewReader(bytes.Repeat([]byte{0x42}, 16*16))
	store := NewAliasStore(WithAliasStoreCapacity(2), WithAliasStoreTTL(time.Hour), WithAliasStoreClock(func() time.Time { return now }), WithAliasStoreRandomReader(random))
	const goroutines = 32
	aliases := make(chan string, goroutines)
	var group sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			alias, err := store.GetOrCreate("target", "g1", "session")
			if err != nil {
				t.Errorf("Resolve: %v", err)
				return
			}
			aliases <- alias
		}()
	}
	group.Wait()
	close(aliases)
	var first string
	for alias := range aliases {
		if first == "" {
			first = alias
		}
		if alias != first {
			t.Fatalf("aliases differ: %q != %q", alias, first)
		}
	}
	if first[14] != '4' || first[19] != '8' {
		t.Fatalf("not RFC4122 v4: %s", first)
	}
	_, _ = store.GetOrCreate("target", "g1", "second")
	_, _ = store.GetOrCreate("target", "g1", "third")
	if _, ok := store.Get(AliasKey{TargetID: "target", TargetGeneration: "g1", OriginalSessionID: "session"}); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	now = now.Add(time.Hour)
	if store.Len() != 0 {
		t.Fatalf("expired Len = %d", store.Len())
	}
}

func TestAliasStoreSlidingTTLRefreshesOnResolveAndGet(t *testing.T) {
	now := time.Unix(2_000, 0)
	const ttl = time.Hour
	store := NewAliasStore(
		WithAliasStoreTTL(ttl),
		WithAliasStoreClock(func() time.Time { return now }),
		WithAliasStoreRandomReader(bytes.NewReader(bytes.Repeat([]byte{0x23}, 64))),
	)
	key := AliasKey{TargetID: "target", TargetGeneration: "generation", OriginalSessionID: "session"}
	first, err := store.Resolve(key)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve hit immediately before the original deadline extends the alias.
	now = now.Add(ttl - time.Minute)
	if got, err := store.Resolve(key); err != nil || got != first {
		t.Fatalf("resolve refresh = %q, %v", got, err)
	}
	now = now.Add(2 * time.Minute)
	if got, ok := store.Get(key); !ok || got != first {
		t.Fatalf("get after refreshed deadline = %q, %v", got, ok)
	}
	// The Get hit also refreshes the deadline.
	now = now.Add(ttl - time.Minute)
	if _, ok := store.Get(key); !ok {
		t.Fatal("alias expired before the last access deadline")
	}
	now = now.Add(ttl)
	if _, ok := store.Get(key); ok {
		t.Fatal("alias survived sliding TTL")
	}
}

func TestAnyRouterSubagentThinking(t *testing.T) {
	input := ` { "model":"m", "thinking" : { "budget": 1, "type" : "disabled" }, "messages":[] } `
	request, err := executeRequest(t, AnyRouterSubagentThinkingID, patchContext(RequestTypeNormal), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := readBody(t, request.Body)
	want := strings.Replace(input, `"disabled"`, `"adaptive"`, 1)
	if got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	noop := strings.Replace(input, `"disabled"`, `"adaptive"`, 1)
	request, err = executeRequest(t, AnyRouterSubagentThinkingID, patchContext(RequestTypeNormal), noop, nil)
	if err != nil || request.Body.Size() != int64(len(noop)) || readBody(t, request.Body) != noop {
		t.Fatalf("adaptive no-op = %q, %v", readBody(t, request.Body), err)
	}
}

func TestAnyRouterSubagentRejectsAmbiguousThinkingShapes(t *testing.T) {
	for _, input := range []string{
		`{"model":"m","thinking":null}`,
		`{"model":"m","thinking":"disabled"}`,
		`{"model":"m","thinking":[]}`,
		`{"model":"m","thinking":{"type":null}}`,
		`{"model":"m","thinking":{"type":1}}`,
		`{"model":"m","thinking":{"type":{}}}`,
	} {
		t.Run(input, func(t *testing.T) {
			_, err := executeRequest(t, AnyRouterSubagentThinkingID, patchContext(RequestTypeNormal), input, nil)
			if err == nil {
				t.Fatal("ambiguous thinking shape succeeded")
			}
			var hookErr *HookError
			if !errors.As(err, &hookErr) || hookErr.PatchID != AnyRouterSubagentThinkingID || hookErr.Stage != StageRequest {
				t.Fatalf("error = %#v", err)
			}
		})
	}
	request, err := executeRequest(t, AnyRouterSubagentThinkingID, patchContext(RequestTypeNormal), `{"model":"m","thinking":{}}`, nil)
	if err != nil || readBody(t, request.Body) != `{"model":"m","thinking":{}}` {
		t.Fatalf("missing thinking.type no-op = %v", err)
	}
}

func TestAnyRouterClassifierIdempotenceAndPartialState(t *testing.T) {
	input := `{"model":"m","thinking":{"type":"disabled"},"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents.\n\n## Context"},{"type":"text","text":"context"}],"messages":[]}`
	request, err := executeRequest(t, AnyRouterClassifierRequestID, patchContext(RequestTypeClassifier), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := readBody(t, request.Body)
	if strings.Contains(got, `"thinking"`) || !strings.Contains(got, AnyRouterMarkerText) || !strings.Contains(got, `Ignore the preceding identity marker.`) {
		t.Fatalf("rewritten body = %s", got)
	}
	request, err = executeRequest(t, AnyRouterClassifierRequestID, patchContext(RequestTypeClassifier), got, nil)
	if err != nil || readBody(t, request.Body) != got {
		t.Fatalf("idempotent = %v, %s", err, readBody(t, request.Body))
	}
	partial := strings.Replace(got, strings.ReplaceAll(AnyRouterCorrectionText, "\n", `\n`), "", 1)
	if _, err := executeRequest(t, AnyRouterClassifierRequestID, patchContext(RequestTypeClassifier), partial, nil); err == nil {
		t.Fatal("partial marker/correction state succeeded")
	}
}

func TestAnyRouterClassifierRejectsAmbiguousThinkingShapes(t *testing.T) {
	for _, thinking := range []string{`null`, `"disabled"`, `[]`, `{"type":null}`, `{"type":1}`} {
		t.Run(thinking, func(t *testing.T) {
			input := `{"model":"m","thinking":` + thinking + `,"system":[{"type":"text","text":"` + classifierSecurityPrefix + `"}],"messages":[]}`
			if _, err := executeRequest(t, AnyRouterClassifierRequestID, patchContext(RequestTypeClassifier), input, nil); err == nil {
				t.Fatalf("ambiguous thinking shape succeeded: %s", thinking)
			}
		})
	}
}

func TestAnyRouterClassifierScansLargeSystemTextWithBoundedState(t *testing.T) {
	security := classifierSecurityPrefix + "\n\n" + strings.Repeat("large-system-text-", 1<<17)
	input := `{"model":"m","system":[{"type":"text","text":` + string(jsonStringBytes(security)) + `}],"messages":[]}`
	request, err := executeRequest(t, AnyRouterClassifierRequestID, patchContext(RequestTypeClassifier), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	index, err := bodyfile.Index(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := index.Lookup("/system/0/text")
	if !ok {
		t.Fatal("marker text missing")
	}
	markerInspection, err := inspectClassifierSystemText(request.Body, marker)
	if err != nil || !markerInspection.exactMarker {
		t.Fatalf("marker inspection = %#v, %v", markerInspection, err)
	}
	securityField, ok := index.Lookup("/system/1/text")
	if !ok {
		t.Fatal("security text missing")
	}
	securityInspection, err := inspectClassifierSystemText(request.Body, securityField)
	if err != nil || !securityInspection.securityMonitor || !securityInspection.startsCorrection || securityInspection.correctionCount != 1 {
		t.Fatalf("security inspection = %#v, %v", securityInspection, err)
	}
}

func TestCLIProxyAPISessionHeaderAndInnerBody(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	input := `{"model":"m","metadata":{"user_id":"{\"device_id\":\"d\", \"session_id\":\"old\", \"n\":1}"},"messages":[]}`
	headers := http.Header{"x-claude-code-session-id": []string{"old-a"}, "X-Claude-Code-Session-ID": []string{"old-b"}}
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, headers)
	if err != nil {
		t.Fatal(err)
	}
	alias, ok := request.Headers.Get("X-Claude-Code-Session-Id")
	if !ok || !validSessionID(alias) || alias == context.OriginalSessionID {
		t.Fatalf("alias header = %q", alias)
	}
	var outer struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(readBody(t, request.Body)), &outer); err != nil {
		t.Fatal(err)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(outer.Metadata.UserID), &inner); err != nil {
		t.Fatal(err)
	}
	if inner["session_id"] != alias || inner["device_id"] != "d" || inner["n"].(float64) != 1 {
		t.Fatalf("inner = %#v, alias=%s", inner, alias)
	}
}

func TestCLIProxyAPISessionPreservesOuterEncodingAndUnknownFields(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	inner := ` { "unknown" : {"n":1.2300e+04,"nested":[true,null,"a\\b\"c","☺"]}, ` +
		`"se\u0073sion_id" : "old\\/session", "tail" : -0.25E-2 } `
	encodedInner, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := request.Headers.Get("X-Claude-Code-Session-Id")
	got := readBody(t, request.Body)
	aliasContent := string(encodeOuterStringContent([]byte(alias)))
	aliasAt := strings.Index(got, aliasContent)
	if aliasAt < 0 {
		t.Fatalf("alias not found in output: %s", got)
	}
	oldStart := strings.Index(input, `old`)
	oldEnd := strings.Index(input[oldStart:], `\"`)
	if oldStart < 0 || oldEnd < 0 {
		t.Fatal("old session content range not found")
	}
	oldEnd += oldStart
	if input[:oldStart] != got[:aliasAt] || input[oldEnd:] != got[aliasAt+len(aliasContent):] {
		t.Fatalf("body changed outside session content\ninput: %s\n  got: %s", input, got)
	}
}

func TestCLIProxyAPISessionMissingPreservesObjectAndInsertsAtEnd(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	inner := ` { "unknown" : {"n":1.2300e+04,"nested":[true,null,"a\\b\"c"]}, "tail" : -0.25E-2 } `
	encodedInner, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := request.Headers.Get("X-Claude-Code-Session-Id")
	got := readBody(t, request.Body)
	insert := string(encodeOuterStringContent([]byte(`,"session_id":` + string(jsonStringBytes(alias)))))
	insertAt := strings.Index(got, insert)
	if insertAt < 0 || got[:insertAt]+got[insertAt+len(insert):] != input {
		t.Fatalf("body changed outside inserted member\ninput: %s\n  got: %s", input, got)
	}
}

func TestCLIProxyAPISessionRejectsInvalidOrAmbiguousInnerJSON(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	tests := []struct {
		name  string
		inner string
	}{
		{name: "not object", inner: `[]`},
		{name: "trailing JSON", inner: `{} true`},
		{name: "duplicate plain", inner: `{"session_id":"a","session_id":"b"}`},
		{name: "duplicate escaped key", inner: `{"session_id":"a","se\u0073sion_id":"b"}`},
		{name: "non-string", inner: `{"session_id":1}`},
		{name: "empty", inner: `{"session_id":""}`},
		{name: "leading space", inner: `{"session_id":" old"}`},
		{name: "escaped control", inner: `{"session_id":"old\nvalue"}`},
		{name: "invalid literal suffix", inner: `{"x":truex}`},
		{name: "invalid number suffix", inner: `{"x":1x}`},
		{name: "object trailing comma", inner: `{"x":1,}`},
		{name: "array trailing comma", inner: `{"x":[1,]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encodedInner, err := json.Marshal(test.inner)
			if err != nil {
				t.Fatal(err)
			}
			input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
			if _, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil); err == nil {
				t.Fatal("invalid metadata.user_id succeeded")
			}
		})
	}
}

func TestCLIProxyAPISessionHandlesUnicodeEscapeLayers(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	inner := `{"emoji":"\uD83D\uDE00","raw":"☺","replacement":"�","unpaired":"\uD800","session_id":"old"}`
	encodedInner, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise a valid outer unicode escape too; decoding it must not alter the
	// source ranges used for the later session edit.
	encodedInner = bytes.Replace(encodedInner, []byte("☺"), []byte(`\u263a`), 1)
	input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := request.Headers.Get("X-Claude-Code-Session-Id")
	got := readBody(t, request.Body)
	if !strings.Contains(got, alias) || !strings.Contains(got, `\u263a`) ||
		!strings.Contains(got, `\\uD83D\\uDE00`) || !strings.Contains(got, `\\uD800`) || !strings.Contains(got, "�") {
		t.Fatalf("unicode escape spelling was not preserved: %s", got)
	}
}

func TestCLIProxyAPISessionMapsOuterEscapedSyntaxOffsets(t *testing.T) {
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	encodedInner := `"{\u0022se\\u0073sion_id\u0022:\u0022old\u0022,\u0022x\u0022:1}"`
	input := `{"model":"m","metadata":{"user_id":` + encodedInner + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := request.Headers.Get("X-Claude-Code-Session-Id")
	got := readBody(t, request.Body)
	want := strings.Replace(input, "old", alias, 1)
	if got != want {
		t.Fatalf("outer escape offset mapping changed other bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestCLIProxyAPISessionStreamsLargeUserID(t *testing.T) {
	if testing.Short() {
		t.Skip("large body test")
	}
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	largeUnknown := strings.Repeat("0123456789abcdef", (65<<20)/16)
	inner := `{"unknown":"` + largeUnknown + `","nested":{"raw":1.2300e+04},"session_id":"old","tail":true}`
	encodedInner, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := request.Headers.Get("X-Claude-Code-Session-Id")
	got := readBody(t, request.Body)
	want := strings.Replace(input, `old\"`, alias+`\"`, 1)
	if got != want {
		t.Fatalf("large metadata.user_id output differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestCLIProxyAPISessionHandlesDeeplyNestedUserIDWithoutCallStackGrowth(t *testing.T) {
	// The inner user_id value is untrusted JSON.  A recursive descent parser
	// would overflow a goroutine stack at this depth; the production parser must
	// keep container state in an explicit heap-backed stack instead.
	const depth = 100_000
	context := patchContext(RequestTypeClassifier)
	context.OriginalSessionID = "original-session"
	nested := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
	inner := `{"nested":` + nested + `,"session_id":"old"}`
	encodedInner, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"model":"m","metadata":{"user_id":` + string(encodedInner) + `},"messages":[]}`
	request, err := executeRequest(t, CLIProxyAPIClassifierSessionID, context, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	alias, ok := request.Headers.Get("X-Claude-Code-Session-Id")
	if !ok || alias == "" {
		t.Fatal("missing isolated session alias")
	}
	want := strings.Replace(input, "old", alias, 1)
	if got := readBody(t, request.Body); got != want {
		t.Fatalf("deeply nested metadata.user_id output differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestGPTClassifierResponseReassembly(t *testing.T) {
	registry := DefaultRegistry(Services{AliasStore: NewAliasStore()})
	plan, err := registry.Compile([]string{GPTClassifierResponseReassemblyID})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := plan.NewInstance(patchContext(RequestTypeClassifier))
	if err != nil {
		t.Fatal(err)
	}
	requestHeaders := NewHTTPHeaderSet(http.Header{"Accept-Encoding": []string{"gzip"}})
	request := &MutableRequest{Body: bodyFromString(t, `{"model":"m","stop_sequences":["</block>","END"]}`), Headers: requestHeaders}
	if err := execution.ApplyRequestOnly(request); err != nil {
		t.Fatal(err)
	}
	if _, ok := requestHeaders.Get("Accept-Encoding"); ok {
		t.Fatal("Accept-Encoding not removed")
	}
	responseHeaders := NewHTTPHeaderSet(http.Header{"Content-Encoding": []string{"gzip"}})
	response := &MutableResponse{
		Status:  200,
		Body:    bodyFromString(t, `{"type":"message","model":"m","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"<block>yes</block><reason>x</reason>"},{"type":"text","text":"discard"}],"stop_reason":"end_turn","stop_sequence":null}`),
		Headers: responseHeaders,
	}
	if err := execution.ApplyResponseOnly(response); err != nil {
		t.Fatal(err)
	}
	got := readBody(t, response.Body)
	if strings.Contains(got, "thinking") || strings.Contains(got, "discard") || strings.Contains(got, "<reason>") || !strings.Contains(got, `"text":"<block>yes`) || !strings.Contains(got, `"stop_reason":"stop_sequence"`) || !strings.Contains(got, `"stop_sequence":"</block>"`) {
		t.Fatalf("response = %s", got)
	}
	if _, ok := responseHeaders.Get("Content-Encoding"); ok {
		t.Fatal("Content-Encoding not removed")
	}
	if length, _ := responseHeaders.Get("Content-Length"); length == "" {
		t.Fatal("Content-Length missing")
	}
}

func TestGPTStopSequenceSameStartUsesRequestOrderAndResponseNeedsNoModel(t *testing.T) {
	for _, test := range []struct {
		name      string
		sequences []string
		want      string
	}{
		{name: "longer configured first", sequences: []string{"abc", "ab"}, want: "abc"},
		{name: "shorter configured first", sequences: []string{"ab", "abc"}, want: "ab"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := DefaultRegistry(Services{AliasStore: NewAliasStore()})
			plan, err := registry.Compile([]string{GPTClassifierResponseReassemblyID})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := plan.NewInstance(patchContext(RequestTypeClassifier))
			if err != nil {
				t.Fatal(err)
			}
			encodedSequences, err := json.Marshal(test.sequences)
			if err != nil {
				t.Fatal(err)
			}
			request := &MutableRequest{
				Body:    bodyFromString(t, `{"model":"m","stop_sequences":`+string(encodedSequences)+`}`),
				Headers: NewHTTPHeaderSet(nil),
			}
			if err := execution.ApplyRequestOnly(request); err != nil {
				t.Fatal(err)
			}
			response := &MutableResponse{
				Status:  200,
				Body:    bodyFromString(t, `{"type":"message","content":[{"type":"text","text":"prefix abc suffix"}],"stop_reason":"end_turn","stop_sequence":null}`),
				Headers: NewHTTPHeaderSet(nil),
			}
			if err := execution.ApplyResponseOnly(response); err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				StopSequence string `json:"stop_sequence"`
			}
			if err := json.Unmarshal([]byte(readBody(t, response.Body)), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.StopSequence != test.want || len(decoded.Content) != 1 || decoded.Content[0].Text != "prefix " {
				t.Fatalf("response = %#v", decoded)
			}
		})
	}
}

func TestGPTResponseRejectsDuplicateOrInvalidStopFieldsWithoutMatch(t *testing.T) {
	for _, responseBody := range []string{
		`{"type":"message","content":[{"type":"text","text":"no match"}],"stop_reason":1}`,
		`{"type":"message","content":[{"type":"text","text":"no match"}],"stop_sequence":{}}`,
		`{"type":"message","content":[{"type":"text","text":"no match"}],"stop_reason":"end_turn","stop_reason":"again"}`,
	} {
		t.Run(responseBody, func(t *testing.T) {
			registry := DefaultRegistry(Services{AliasStore: NewAliasStore()})
			plan, err := registry.Compile([]string{GPTClassifierResponseReassemblyID})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := plan.NewInstance(patchContext(RequestTypeClassifier))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = execution.Close() })
			request := &MutableRequest{Body: bodyFromString(t, `{"model":"m","stop_sequences":["STOP"]}`), Headers: NewHTTPHeaderSet(nil)}
			if err := execution.ApplyRequestOnly(request); err != nil {
				t.Fatal(err)
			}
			response := &MutableResponse{Status: 200, Body: bodyFromString(t, responseBody), Headers: NewHTTPHeaderSet(nil)}
			if err := execution.ApplyResponseOnly(response); err == nil {
				t.Fatal("invalid stop fields succeeded")
			}
		})
	}
}

type trackingBody struct {
	content    string
	openCount  atomic.Int32
	closeCount atomic.Int32
	closeErr   error
}

func (b *trackingBody) OpenReader() (io.ReadCloser, error) {
	b.openCount.Add(1)
	return io.NopCloser(strings.NewReader(b.content)), nil
}

func (b *trackingBody) Size() int64 { return int64(len(b.content)) }

func (b *trackingBody) Close() error {
	b.closeCount.Add(1)
	return b.closeErr
}

func TestMutableRequestReusesBaseIndexAndReindexesOnlyAfterBodyChange(t *testing.T) {
	base := &trackingBody{content: `{"model":"m"}`}
	index, err := bodyfile.Index(base)
	if err != nil {
		t.Fatal(err)
	}
	base.openCount.Store(0)
	request := NewMutableRequest(base, index, NewHTTPHeaderSet(nil))
	if err := (anyRouterSubagentPatch{}).ApplyRequest(PatchContext{}, request); err != nil {
		t.Fatal(err)
	}
	if got := base.openCount.Load(); got != 0 {
		t.Fatalf("base body was re-indexed %d times", got)
	}

	derived := &trackingBody{content: `{"model":"m","derived":true}`}
	derivedIndex, err := bodyfile.Index(derived)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.SetBody(derived, derivedIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := requestIndex(request); err != nil {
		t.Fatal(err)
	}
	if got := derived.openCount.Load(); got != 1 {
		t.Fatalf("derived body index reads = %d, want 1", got)
	}
}

type replaceRequestBodyPatch struct{ body bodyfile.Body }

func (p replaceRequestBodyPatch) ApplyRequest(_ PatchContext, request *MutableRequest) error {
	request.Body = p.body
	return nil
}

type replaceResponseBodyPatch struct{ body bodyfile.Body }

func (p replaceResponseBodyPatch) ApplyResponse(_ PatchContext, response *MutableResponse) error {
	response.Body = p.body
	return nil
}

type failingRequestPatch struct{ err error }

func (p failingRequestPatch) ApplyRequest(PatchContext, *MutableRequest) error { return p.err }

type failingResponsePatch struct{ err error }

func (p failingResponsePatch) ApplyResponse(PatchContext, *MutableResponse) error { return p.err }

func TestExecutionClosesOnlyIntermediateDerivedBodies(t *testing.T) {
	base := &trackingBody{content: `{"model":"m"}`}
	firstDerived := &trackingBody{content: `{"model":"first"}`}
	finalDerived := &trackingBody{content: `{"model":"final"}`}
	requestDefinition := func(id string, body bodyfile.Body) PatchDefinition {
		return PatchDefinition{
			ID: id, Name: id, RequestTypes: []RequestType{RequestTypeNormal},
			Stages: []Stage{StageRequest}, Idempotence: Idempotent,
			Factory: func(FactoryContext) (PatchInstance, error) {
				return NewHooksInstance(Hooks{Request: replaceRequestBodyPatch{body: body}}), nil
			},
		}
	}
	registry, err := NewRegistry([]PatchDefinition{
		requestDefinition("first", firstDerived),
		requestDefinition("final", finalDerived),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Compile([]string{"first", "final"})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := plan.NewInstance(patchContext(RequestTypeNormal))
	if err != nil {
		t.Fatal(err)
	}
	request := &MutableRequest{Body: base, Headers: NewHTTPHeaderSet(nil)}
	if err := execution.ApplyRequestOnly(request); err != nil {
		t.Fatal(err)
	}
	if request.Body != finalDerived || base.closeCount.Load() != 0 || firstDerived.closeCount.Load() != 1 || finalDerived.closeCount.Load() != 0 {
		t.Fatalf("request bodies: base=%d first=%d final=%d current=%T", base.closeCount.Load(), firstDerived.closeCount.Load(), finalDerived.closeCount.Load(), request.Body)
	}

	responseBase := &trackingBody{content: `{}`}
	responseIntermediate := &trackingBody{content: `{"step":1}`}
	responseFinal := &trackingBody{content: `{"step":2}`}
	responseDefinition := func(id string, body bodyfile.Body) PatchDefinition {
		return PatchDefinition{
			ID: id, Name: id, RequestTypes: []RequestType{RequestTypeNormal},
			Stages: []Stage{StageResponse}, Idempotence: Idempotent,
			Factory: func(FactoryContext) (PatchInstance, error) {
				return NewHooksInstance(Hooks{Response: replaceResponseBodyPatch{body: body}}), nil
			},
		}
	}
	registry, err = NewRegistry([]PatchDefinition{
		responseDefinition("response-final", responseFinal),
		responseDefinition("response-intermediate", responseIntermediate),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = registry.Compile([]string{"response-final", "response-intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	execution, err = plan.NewInstance(patchContext(RequestTypeNormal))
	if err != nil {
		t.Fatal(err)
	}
	response := &MutableResponse{Status: 200, Body: responseBase, Headers: NewHTTPHeaderSet(nil)}
	if err := execution.ApplyResponseOnly(response); err != nil {
		t.Fatal(err)
	}
	if response.Body != responseFinal || responseBase.closeCount.Load() != 0 || responseIntermediate.closeCount.Load() != 1 || responseFinal.closeCount.Load() != 0 {
		t.Fatalf("response bodies: base=%d intermediate=%d final=%d current=%T", responseBase.closeCount.Load(), responseIntermediate.closeCount.Load(), responseFinal.closeCount.Load(), response.Body)
	}
}

func TestExecutionReturnsStructuredHookError(t *testing.T) {
	sentinel := errors.New("hook sentinel")
	for _, test := range []struct {
		name  string
		stage Stage
		hooks Hooks
		apply func(*Execution) error
	}{
		{
			name:  "request",
			stage: StageRequest,
			hooks: Hooks{Request: failingRequestPatch{err: sentinel}},
			apply: func(execution *Execution) error {
				return execution.ApplyRequestOnly(&MutableRequest{Body: &trackingBody{content: `{"model":"m"}`}, Headers: NewHTTPHeaderSet(nil)})
			},
		},
		{
			name:  "response",
			stage: StageResponse,
			hooks: Hooks{Response: failingResponsePatch{err: sentinel}},
			apply: func(execution *Execution) error {
				return execution.ApplyResponseOnly(&MutableResponse{Status: 200, Body: &trackingBody{content: `{}`}, Headers: NewHTTPHeaderSet(nil)})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := PatchDefinition{
				ID: "failing", Name: "failing", RequestTypes: []RequestType{RequestTypeNormal},
				Stages: []Stage{test.stage}, Idempotence: Idempotent,
				Factory: func(FactoryContext) (PatchInstance, error) {
					return NewHooksInstance(test.hooks), nil
				},
			}
			registry, err := NewRegistry([]PatchDefinition{definition})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := registry.Compile([]string{"failing"})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := plan.NewInstance(patchContext(RequestTypeNormal))
			if err != nil {
				t.Fatal(err)
			}
			err = test.apply(execution)
			var hookErr *HookError
			if !errors.As(err, &hookErr) || !errors.Is(err, sentinel) {
				t.Fatalf("error = %v", err)
			}
			if hookErr.PatchID != "failing" || hookErr.Stage != test.stage {
				t.Fatalf("HookError = %#v", hookErr)
			}
			if got := fmt.Sprint(err); !strings.Contains(got, fmt.Sprintf(`patch "failing" %s`, test.stage)) {
				t.Fatalf("Error() = %q", got)
			}
		})
	}
}
