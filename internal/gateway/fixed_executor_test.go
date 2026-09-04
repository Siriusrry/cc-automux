package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/automode"
	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/protocol"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type fixedTestAdapter struct {
	protocol string
	encode   func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error)
	decode   func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error)
}

func (a *fixedTestAdapter) Protocol() string { return a.protocol }

func (a *fixedTestAdapter) EncodeRequest(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
	return a.encode(body, headers)
}

func (a *fixedTestAdapter) DecodeResponse(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
	return a.decode(body, headers)
}

type fixedTestRequestPatch func(patch.PatchContext, *patch.MutableRequest) error

func (f fixedTestRequestPatch) ApplyRequest(context patch.PatchContext, request *patch.MutableRequest) error {
	return f(context, request)
}

type fixedTestResponsePatch func(patch.PatchContext, *patch.MutableResponse) error

func (f fixedTestResponsePatch) ApplyResponse(context patch.PatchContext, response *patch.MutableResponse) error {
	return f(context, response)
}

type fixedTrackingBody struct {
	body bodyfile.Body

	mu           sync.Mutex
	openCount    int
	readerCloses int
	closeCount   int
}

func (b *fixedTrackingBody) OpenReader() (io.ReadCloser, error) {
	reader, err := b.body.OpenReader()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.openCount++
	b.mu.Unlock()
	return &fixedTrackingReader{ReadCloser: reader, owner: b}, nil
}

func (b *fixedTrackingBody) Size() int64 { return b.body.Size() }

func (b *fixedTrackingBody) Close() error {
	b.mu.Lock()
	b.closeCount++
	b.mu.Unlock()
	return b.body.Close()
}

func (b *fixedTrackingBody) counts() (opens, readerCloses, closes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openCount, b.readerCloses, b.closeCount
}

type fixedTrackingReader struct {
	io.ReadCloser
	owner *fixedTrackingBody
	once  sync.Once
}

type fixedWriteRecorder struct {
	header      http.Header
	headerCalls int
	writeCalls  int
	status      int
	onWrite     func()
}

func (w *fixedWriteRecorder) Header() http.Header { return w.header }

func (w *fixedWriteRecorder) WriteHeader(status int) {
	w.headerCalls++
	w.status = status
}

func (w *fixedWriteRecorder) Write(data []byte) (int, error) {
	w.writeCalls++
	if w.onWrite != nil {
		w.onWrite()
	}
	return len(data), nil
}

type fixedCloseErrorBody struct {
	body bodyfile.Body
	err  error
}

func (b *fixedCloseErrorBody) OpenReader() (io.ReadCloser, error) { return b.body.OpenReader() }
func (b *fixedCloseErrorBody) Size() int64                        { return b.body.Size() }
func (b *fixedCloseErrorBody) Close() error                       { return errors.Join(b.body.Close(), b.err) }

func (r *fixedTrackingReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		r.owner.mu.Lock()
		r.owner.readerCloses++
		r.owner.mu.Unlock()
	})
	return err
}

func TestFixedUpstreamURLAlwaysAppendsProtocolPathAndPreservesClientQuery(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		protocolID string
		incoming   string
		want       string
		forceQuery bool
	}{
		{
			name:       "responses root",
			base:       "https://classifier.example",
			protocolID: "openai_responses",
			incoming:   "https://gateway.example/v1/messages?x=%2F&x=a+b",
			want:       "https://classifier.example/v1/responses?x=%2F&x=a+b",
		},
		{
			name:       "anthropic messages root",
			base:       "https://classifier.example",
			protocolID: config.ProtocolAnthropicMessages,
			incoming:   "https://gateway.example/v1/messages?beta=true&beta=false",
			want:       "https://classifier.example/v1/messages?beta=true&beta=false",
		},
		{
			name:       "compatible path prefix",
			base:       "https://classifier.example/prefix/",
			protocolID: "openai_compatible",
			incoming:   "https://gateway.example/v1/messages?trace=one%2Btwo",
			want:       "https://classifier.example/prefix/v1/chat/completions?trace=one%2Btwo",
		},
		{
			name:       "existing suffix is still a base path",
			base:       "https://classifier.example/v1/responses",
			protocolID: "openai_responses",
			incoming:   "https://gateway.example/v1/messages?",
			want:       "https://classifier.example/v1/responses/v1/responses?",
			forceQuery: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, err := url.Parse(test.base)
			if err != nil {
				t.Fatal(err)
			}
			incoming, err := url.Parse(test.incoming)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fixedUpstreamURL(&provider.CompiledFixedTarget{BaseURL: base, Protocol: test.protocolID}, incoming)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != test.want || got.RawQuery != incoming.RawQuery || got.ForceQuery != test.forceQuery {
				t.Fatalf("fixedUpstreamURL() = %q raw=%q force=%v, want %q raw=%q force=%v", got, got.RawQuery, got.ForceQuery, test.want, incoming.RawQuery, test.forceQuery)
			}
		})
	}
}

func TestFixedExecutionMissingAdapterStopsBeforePatchUpstreamAndScheduler(t *testing.T) {
	var patchCalls atomic.Int32
	plan := fixedTestPatchPlan(t,
		fixedTestRequestPatch(func(patch.PatchContext, *patch.MutableRequest) error {
			patchCalls.Add(1)
			return nil
		}), nil,
	)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	target := fixedTestTarget(t, upstream.URL, "openai_responses", plan)
	selector := &fakeSelector{}
	events := &eventCollector{}
	handler := NewWithOptions(nil, selector, Options{ProtocolAdapters: protocol.EmptyRegistry(), Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.forwardFixedExecution(response, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, target))

	acquires, reports := selector.snapshot()
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "protocol_not_implemented") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if patchCalls.Load() != 0 || upstreamCalls.Load() != 0 || acquires != 0 || len(reports) != 0 {
		t.Fatalf("patch=%d upstream=%d acquires=%d reports=%d", patchCalls.Load(), upstreamCalls.Load(), acquires, len(reports))
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 1 || gotEvents[0].Kind != EventFailure || gotEvents[0].Attempt != 1 {
		t.Fatalf("events = %#v", gotEvents)
	}
}

func TestFixedExecutionAnthropicMessagesSkipsProtocolAdapter(t *testing.T) {
	var encodeCalls atomic.Int32
	var decodeCalls atomic.Int32
	adapter := &fixedTestAdapter{
		protocol: config.ProtocolAnthropicMessages,
		encode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			encodeCalls.Add(1)
			return protocol.ProtocolMessage{}, errors.New("Anthropic request must not be encoded")
		},
		decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			decodeCalls.Add(1)
			return protocol.ProtocolMessage{}, errors.New("Anthropic response must not be decoded")
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotQuery, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotQuery = request.URL.RawQuery
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read request body: %v", readErr)
		}
		gotBody = string(body)
		if request.Header.Get("Authorization") != "Bearer fixed-secret" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "identity")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()
	target := fixedTestTarget(t, upstream.URL, config.ProtocolAnthropicMessages, patch.Plan{})
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry})
	defer handler.Close()
	response := httptest.NewRecorder()
	incoming := fixedTestIncoming("beta=true&beta=false")
	handler.forwardFixedExecution(response, incoming, nil, fixedTestExecutionPlan(t, target))
	if response.Code != http.StatusOK || response.Body.String() != `{"type":"message","content":[]}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Encoding") != "identity" {
		t.Fatalf("Content-Encoding = %q, want direct upstream value", response.Header().Get("Content-Encoding"))
	}
	if gotPath != MessagesPath || gotQuery != "beta=true&beta=false" {
		t.Fatalf("upstream URL path/query = %q?%s", gotPath, gotQuery)
	}
	if gotBody != `{"model":"original-model","metadata":{"user_id":"original-session"}}` {
		t.Fatalf("upstream body = %q", gotBody)
	}
	if encodeCalls.Load() != 0 || decodeCalls.Load() != 0 {
		t.Fatalf("adapter calls encode=%d decode=%d", encodeCalls.Load(), decodeCalls.Load())
	}
}

func TestFixedExecutionConvertedResponseRebuildsRepresentationHeaders(t *testing.T) {
	const converted = `{"type":"message","content":[{"type":"text","text":"converted"}]}`
	decoded := fixedTestTrackingBody(t, converted)
	adapter := &fixedTestAdapter{
		protocol: config.ProtocolOpenAIResponses,
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{
				Body: decoded,
				Headers: http.Header{
					"Content-Encoding": []string{"gzip"},
					"Content-Length":   []string{"1"},
					"X-Converted":      []string{"yes"},
				},
			}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"wire":true}`)
	}))
	defer upstream.Close()
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.forwardFixedExecution(response, fixedTestIncoming(""), nil,
		fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, config.ProtocolOpenAIResponses, patch.Plan{})))

	if response.Code != http.StatusOK || response.Body.String() != converted || response.Header().Get("X-Converted") != "yes" {
		t.Fatalf("response = %d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got, want := response.Header().Get("Content-Length"), strconv.Itoa(len(converted)); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
}

func TestFixedExecutionOrdersConversionAndPatchesAndInstallsFinalAuth(t *testing.T) {
	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}
	plan := fixedTestPatchPlan(t,
		fixedTestRequestPatch(func(context patch.PatchContext, request *patch.MutableRequest) error {
			appendOrder("request patch")
			if context.OriginalSessionID != "original-session" || context.EffectiveModel != "classifier-model" {
				return errors.New("patch context lost classifier facts")
			}
			request.Headers.Set("X-Request-Patch", "applied")
			return nil
		}),
		fixedTestResponsePatch(func(context patch.PatchContext, response *patch.MutableResponse) error {
			appendOrder("response patch")
			if context.OriginalSessionID != "original-session" || context.EffectiveModel != "classifier-model" {
				return errors.New("response patch context lost classifier facts")
			}
			response.Headers.Set("X-Response-Patch", "applied")
			return nil
		}),
	)
	encoded := fixedTestTrackingBody(t, `{"encoded":true}`)
	decoded := fixedTestTrackingBody(t, `{"decoded":true}`)
	adapter := &fixedTestAdapter{protocol: "openai_responses"}
	adapter.encode = func(_ bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
		appendOrder("encode")
		for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "X-Gateway-Key", "X-Management-Key", "X-Provider-Key"} {
			if value := headers.Get(name); value != "" {
				return protocol.ProtocolMessage{}, errors.New("client credential reached adapter: " + name)
			}
		}
		if headers.Get("X-Request-Patch") != "applied" {
			return protocol.ProtocolMessage{}, errors.New("request patch header missing")
		}
		return protocol.ProtocolMessage{Body: encoded, Headers: http.Header{
			"Authorization":       []string{"Bearer adapter-value"},
			"Proxy-Authorization": []string{"adapter-value"},
			"X-Api-Key":           []string{"adapter-value"},
			"Api-Key":             []string{"adapter-value"},
			"X-Gateway-Key":       []string{"adapter-value"},
			"X-Management-Key":    []string{"adapter-value"},
			"X-Provider-Key":      []string{"adapter-value"},
			"X-Encoded":           []string{"yes"},
		}}, nil
	}
	adapter.decode = func(_ bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
		appendOrder("decode")
		if headers.Get("X-Upstream") != "yes" {
			return protocol.ProtocolMessage{}, errors.New("upstream response header missing")
		}
		return protocol.ProtocolMessage{Body: decoded, Headers: http.Header{"Content-Type": []string{"application/json"}, "X-Decoded": []string{"yes"}}}, nil
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}

	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		appendOrder("upstream")
		upstreamURL = request.URL.String()
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer fixed-secret" || request.Header.Get("X-Encoded") != "yes" {
			t.Errorf("upstream request method=%s headers=%v", request.Method, request.Header)
		}
		for _, name := range []string{"Proxy-Authorization", "X-Api-Key", "Api-Key", "X-Gateway-Key", "X-Management-Key", "X-Provider-Key"} {
			if value := request.Header.Get(name); value != "" {
				t.Errorf("credential %s = %q", name, value)
			}
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil || string(body) != `{"encoded":true}` {
			t.Errorf("upstream body=%q err=%v", body, readErr)
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"wire":true}`))
	}))
	defer upstream.Close()
	target := fixedTestTarget(t, upstream.URL+"/classifier-prefix", "openai_responses", plan)
	selector := &fakeSelector{}
	events := &eventCollector{}
	diagnostics := automode.NewDiagnostics()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	handler := NewWithOptions(nil, selector, Options{ProtocolAdapters: registry, Recorder: events, FixedDiagnostics: diagnostics, Now: func() time.Time { return now }})
	defer handler.Close()
	incoming := fixedTestIncoming("trace=%2F&trace=a+b")
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "X-Gateway-Key", "X-Management-Key", "X-Provider-Key"} {
		incoming.Header.Set(name, "client-secret")
	}
	response := httptest.NewRecorder()
	handler.forwardFixedExecution(response, incoming, nil, fixedTestExecutionPlan(t, target))

	if response.Code != http.StatusCreated || response.Body.String() != `{"decoded":true}` || response.Header().Get("X-Response-Patch") != "applied" {
		t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	wantOrder := []string{"request patch", "encode", "upstream", "decode", "response patch"}
	if strings.Join(gotOrder, " -> ") != strings.Join(wantOrder, " -> ") {
		t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
	}
	if upstreamURL != "/classifier-prefix/v1/responses?trace=%2F&trace=a+b" {
		t.Fatalf("upstream URL = %q", upstreamURL)
	}
	if opens, readerCloses, closes := encoded.counts(); opens != 1 || readerCloses != 1 || closes != 1 {
		t.Fatalf("encoded body lifecycle opens=%d reader_closes=%d closes=%d", opens, readerCloses, closes)
	}
	if opens, readerCloses, closes := decoded.counts(); opens != 1 || readerCloses != 1 || closes != 1 {
		t.Fatalf("decoded body lifecycle opens=%d reader_closes=%d closes=%d", opens, readerCloses, closes)
	}
	acquires, reports := selector.snapshot()
	if acquires != 0 || len(reports) != 0 {
		t.Fatalf("fixed target touched scheduler: acquires=%d reports=%d", acquires, len(reports))
	}
	fullURL := upstream.URL + "/classifier-prefix/v1/responses?trace=%2F&trace=a+b"
	call := diagnostics.Snapshot()
	if call == nil || !call.ObservedAt.Equal(now.UTC()) || call.UpstreamURL != fullURL || call.GatewayStatus != http.StatusCreated || call.UpstreamStatus != http.StatusCreated || call.SessionID != "original-session" || call.GatewayError != "" {
		t.Fatalf("diagnostics = %#v", call)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventSuccess {
		t.Fatalf("events = %#v", gotEvents)
	}
	for _, event := range gotEvents {
		if event.ProviderID != provider.FixedTargetID || event.SessionID != "original-session" || event.Model != "classifier-model" || event.UpstreamURL != fullURL || event.Attempt != 1 {
			t.Fatalf("event lost fixed facts: %#v", event)
		}
	}
}

func TestFixedExecutionAdapterFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		encode     func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error)
		decode     func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error)
		wantCalls  int32
		wantDecode int32
	}{
		{name: "encode error", encode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{}, errors.New("encode failed")
		}},
		{name: "encode nil body", encode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Headers: make(http.Header)}, nil
		}},
		{name: "decode error", decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{}, errors.New("decode failed")
		}, wantCalls: 1, wantDecode: 1},
		{name: "decode nil body", decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Headers: make(http.Header)}, nil
		}, wantCalls: 1, wantDecode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			var decodeCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"wire":true}`))
			}))
			defer upstream.Close()
			adapter := &fixedTestAdapter{protocol: "openai_responses"}
			adapter.encode = test.encode
			if adapter.encode == nil {
				adapter.encode = func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
					return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
				}
			}
			adapter.decode = func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
				decodeCalls.Add(1)
				if test.decode != nil {
					return test.decode(body, headers)
				}
				return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
			}
			registry, err := protocol.NewRegistry(adapter)
			if err != nil {
				t.Fatal(err)
			}
			target := fixedTestTarget(t, upstream.URL, "openai_responses", patch.Plan{})
			handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry})
			defer handler.Close()
			response := httptest.NewRecorder()
			handler.forwardFixedExecution(response, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, target))
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "protocol_conversion_failed") {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if upstreamCalls.Load() != test.wantCalls || decodeCalls.Load() != test.wantDecode {
				t.Fatalf("upstream=%d decode=%d, want %d/%d", upstreamCalls.Load(), decodeCalls.Load(), test.wantCalls, test.wantDecode)
			}
		})
	}
}

func TestFixedExecutionConversionAndPatchDiagnosticsKeepRawUpstreamFacts(t *testing.T) {
	const rawBody = `{"wire":"raw upstream"}`
	for _, test := range []struct {
		name      string
		plan      func(*testing.T) patch.Plan
		decode    func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error)
		wantError string
	}{
		{
			name: "decode failure",
			plan: func(*testing.T) patch.Plan { return patch.Plan{} },
			decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
				return protocol.ProtocolMessage{}, errors.New("decode failed with complete detail")
			},
			wantError: "decode failed with complete detail",
		},
		{
			name: "response patch failure",
			plan: func(t *testing.T) patch.Plan {
				return fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(patch.PatchContext, *patch.MutableResponse) error {
					return errors.New("response patch failed with complete detail")
				}))
			},
			decode: func(body bodyfile.Body, _ http.Header) (protocol.ProtocolMessage, error) {
				return protocol.ProtocolMessage{Body: body, Headers: http.Header{"X-Decoded-Only": []string{"yes"}}}, nil
			},
			wantError: "response patch failed with complete detail",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Raw-Upstream", "yes")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, rawBody)
			}))
			defer upstream.Close()
			adapter := &fixedTestAdapter{
				protocol: "openai_responses",
				encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
					return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
				},
				decode: test.decode,
			}
			registry, err := protocol.NewRegistry(adapter)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics := automode.NewDiagnostics()
			handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics})
			defer handler.Close()
			response := httptest.NewRecorder()
			handler.forwardFixedExecution(response, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, "openai_responses", test.plan(t))))

			if response.Code != http.StatusBadGateway {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			call := diagnostics.Snapshot()
			if call == nil || call.UpstreamStatus != http.StatusCreated || call.GatewayStatus != http.StatusBadGateway ||
				call.UpstreamBody != rawBody || call.UpstreamHeaders.Get("X-Raw-Upstream") != "yes" ||
				call.UpstreamHeaders.Get("X-Decoded-Only") != "" || !strings.Contains(call.Error, test.wantError) {
				t.Fatalf("diagnostics = %#v", call)
			}
		})
	}
}

func TestFixedExecutionSuccessDistinguishesGatewayAndUpstreamStatus(t *testing.T) {
	plan := fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(_ patch.PatchContext, response *patch.MutableResponse) error {
		response.Status = http.StatusAccepted
		return nil
	}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	adapter := &fixedTestAdapter{
		protocol: "openai_responses",
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := automode.NewDiagnostics()
	events := &eventCollector{}
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics, Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.forwardFixedExecution(response, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, "openai_responses", plan)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("gateway status = %d", response.Code)
	}
	call := diagnostics.Snapshot()
	if call == nil || call.GatewayStatus != http.StatusAccepted || call.UpstreamStatus != http.StatusCreated {
		t.Fatalf("diagnostics = %#v", call)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[1].Kind != EventSuccess || gotEvents[1].HTTPStatus != http.StatusCreated {
		t.Fatalf("events = %#v", gotEvents)
	}
}

func TestFixedExecutionCancellationAfterPatchedStatusPreservesWrittenGatewayStatus(t *testing.T) {
	plan := fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(_ patch.PatchContext, response *patch.MutableResponse) error {
		response.Status = http.StatusAccepted
		return nil
	}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	adapter := &fixedTestAdapter{
		protocol: "openai_responses",
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := automode.NewDiagnostics()
	events := &eventCollector{}
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics, Recorder: events})
	defer handler.Close()
	incoming := fixedTestIncoming("")
	ctx, cancel := context.WithCancel(incoming.Context())
	incoming = incoming.WithContext(ctx)
	response := &fixedWriteRecorder{header: make(http.Header), onWrite: cancel}
	handler.forwardFixedExecution(response, incoming, nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, "openai_responses", plan)))

	if response.status != http.StatusAccepted || response.headerCalls != 1 || response.writeCalls != 1 {
		t.Fatalf("written response status=%d headers=%d writes=%d", response.status, response.headerCalls, response.writeCalls)
	}
	call := diagnostics.Snapshot()
	if call == nil || call.GatewayStatus != http.StatusAccepted || call.UpstreamStatus != http.StatusCreated || call.GatewayError != "client_canceled" {
		t.Fatalf("diagnostics = %#v", call)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailure {
		t.Fatalf("events = %#v", gotEvents)
	}
}

func TestFixedExecutionCancellationDuringResponsePatchWinsAndCleansBodies(t *testing.T) {
	incoming := fixedTestIncoming("")
	ctx, cancel := context.WithCancel(incoming.Context())
	incoming = incoming.WithContext(ctx)
	decodedBody := fixedTestTrackingBody(t, `{"decoded":true}`)
	replacementBody := fixedTestTrackingBody(t, `{"replacement":true}`)
	plan := fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(_ patch.PatchContext, response *patch.MutableResponse) error {
		response.Body = replacementBody
		cancel()
		return errors.New("response patch failed after cancellation")
	}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"wire":true}`)
	}))
	defer upstream.Close()
	adapter := &fixedTestAdapter{
		protocol: "openai_responses",
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: decodedBody, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := automode.NewDiagnostics()
	events := &eventCollector{}
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics, Recorder: events})
	defer handler.Close()
	response := &fixedWriteRecorder{header: make(http.Header)}
	handler.forwardFixedExecution(response, incoming, nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, "openai_responses", plan)))

	if response.headerCalls != 0 || response.writeCalls != 0 {
		t.Fatalf("canceled response patch wrote response: headers=%d writes=%d", response.headerCalls, response.writeCalls)
	}
	call := diagnostics.Snapshot()
	if call == nil || call.GatewayStatus != 0 || call.GatewayError != "client_canceled" || call.UpstreamStatus != http.StatusCreated {
		t.Fatalf("diagnostics = %#v", call)
	}
	if opens, readerCloses, closes := decodedBody.counts(); opens != 1 || readerCloses != 1 || closes != 1 {
		t.Fatalf("decoded body lifecycle = opens:%d reader_closes:%d closes:%d", opens, readerCloses, closes)
	}
	if opens, readerCloses, closes := replacementBody.counts(); opens != 0 || readerCloses != 0 || closes != 1 {
		t.Fatalf("replacement body lifecycle = opens:%d reader_closes:%d closes:%d", opens, readerCloses, closes)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailure {
		t.Fatalf("events = %#v", gotEvents)
	}
}

func TestFixedExecutionCleanupFailureAfterPatchedStatusPreservesWrittenGatewayStatus(t *testing.T) {
	plan := fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(_ patch.PatchContext, response *patch.MutableResponse) error {
		response.Status = http.StatusAccepted
		return nil
	}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"wire":true}`)
	}))
	defer upstream.Close()
	decodedBody, err := bodyfile.Capture(strings.NewReader(`{"decoded":true}`))
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fixedTestAdapter{
		protocol: "openai_responses",
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{
				Body:    &fixedCloseErrorBody{body: decodedBody, err: bodyfile.ErrLocalIO},
				Headers: http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		_ = decodedBody.Close()
		t.Fatal(err)
	}
	diagnostics := automode.NewDiagnostics()
	events := &eventCollector{}
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics, Recorder: events})
	defer handler.Close()
	response := httptest.NewRecorder()
	handler.forwardFixedExecution(response, fixedTestIncoming(""), nil, fixedTestExecutionPlan(t, fixedTestTarget(t, upstream.URL, "openai_responses", plan)))

	if response.Code != http.StatusAccepted || response.Body.String() != `{"decoded":true}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	call := diagnostics.Snapshot()
	if call == nil || call.GatewayStatus != http.StatusAccepted || call.UpstreamStatus != http.StatusCreated || call.GatewayError != "replay_unavailable" || !strings.Contains(call.Error, bodyfile.ErrLocalIO.Error()) {
		t.Fatalf("diagnostics = %#v", call)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailure {
		t.Fatalf("events = %#v", gotEvents)
	}
}

func TestFixedExecutionCanceledBeforeStartDoesNotWriteOrDiagnose(t *testing.T) {
	target := fixedTestTarget(t, "https://classifier.example", "openai_responses", patch.Plan{})
	handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: protocol.EmptyRegistry()})
	defer handler.Close()
	incoming := fixedTestIncoming("")
	ctx, cancel := context.WithCancel(incoming.Context())
	cancel()
	incoming = incoming.WithContext(ctx)
	w := &fixedWriteRecorder{header: make(http.Header)}
	handler.forwardFixedExecution(w, incoming, nil, fixedTestExecutionPlan(t, target))
	if w.headerCalls != 0 || w.writeCalls != 0 || handler.FixedTargetDiagnostics() != nil {
		t.Fatalf("canceled request wrote response or diagnostics: headers=%d writes=%d call=%#v", w.headerCalls, w.writeCalls, handler.FixedTargetDiagnostics())
	}
}

func TestFixedExecutionNonSuccessResponsesBypassDecodeAndResponsePatch(t *testing.T) {
	tests := []struct {
		status     int
		wantStatus int
		preserved  bool
	}{
		{http.StatusBadRequest, http.StatusBadRequest, true},
		{http.StatusNotFound, http.StatusNotFound, true},
		{http.StatusConflict, http.StatusConflict, true},
		{http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, true},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, true},
		{http.StatusTooManyRequests, http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable, true},
		{http.StatusUnauthorized, http.StatusBadGateway, false},
		{http.StatusForbidden, http.StatusBadGateway, false},
		{http.StatusMethodNotAllowed, http.StatusBadGateway, false},
		{http.StatusMovedPermanently, http.StatusBadGateway, false},
		{http.StatusTemporaryRedirect, http.StatusBadGateway, false},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			var responsePatchCalls atomic.Int32
			plan := fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(patch.PatchContext, *patch.MutableResponse) error {
				responsePatchCalls.Add(1)
				return nil
			}))
			rawBody := `{"upstream_error":"raw failure"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Upstream-Error", "raw")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(rawBody))
			}))
			defer upstream.Close()
			var decodeCalls atomic.Int32
			adapter := &fixedTestAdapter{
				protocol: "openai_responses",
				encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
					return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
				},
				decode: func(bodyfile.Body, http.Header) (protocol.ProtocolMessage, error) {
					decodeCalls.Add(1)
					return protocol.ProtocolMessage{}, errors.New("must not decode non-2xx")
				},
			}
			registry, err := protocol.NewRegistry(adapter)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics := automode.NewDiagnostics()
			events := &eventCollector{}
			selector := &fakeSelector{}
			target := fixedTestTarget(t, upstream.URL, "openai_responses", plan)
			handler := NewWithOptions(nil, selector, Options{ProtocolAdapters: registry, FixedDiagnostics: diagnostics, Recorder: events})
			defer handler.Close()
			response := httptest.NewRecorder()
			handler.forwardFixedExecution(response, fixedTestIncoming("failure=raw"), nil, fixedTestExecutionPlan(t, target))

			if response.Code != test.wantStatus || decodeCalls.Load() != 0 || responsePatchCalls.Load() != 0 {
				t.Fatalf("response=%d decode=%d response_patch=%d", response.Code, decodeCalls.Load(), responsePatchCalls.Load())
			}
			if test.preserved {
				if response.Body.String() != rawBody || response.Header().Get("X-Upstream-Error") != "raw" {
					t.Fatalf("preserved response headers=%v body=%q", response.Header(), response.Body.String())
				}
			} else if response.Body.String() == rawBody || !strings.Contains(response.Body.String(), "bad_gateway") {
				t.Fatalf("mapped response body = %q", response.Body.String())
			}
			call := diagnostics.Snapshot()
			wantURL := upstream.URL + "/v1/responses?failure=raw"
			if call == nil || call.UpstreamURL != wantURL || call.UpstreamStatus != test.status || call.GatewayStatus != test.wantStatus || call.SessionID != "original-session" || call.UpstreamBody != rawBody || call.UpstreamHeaders.Get("X-Upstream-Error") != "raw" || call.Error != rawBody {
				t.Fatalf("diagnostics = %#v", call)
			}
			gotEvents := events.snapshot()
			if len(gotEvents) != 2 || gotEvents[0].Kind != EventForward || gotEvents[1].Kind != EventFailure || gotEvents[1].RawError != rawBody || gotEvents[1].HTTPStatus != test.status || gotEvents[1].Model != "classifier-model" || gotEvents[1].SessionID != "original-session" {
				t.Fatalf("events = %#v", gotEvents)
			}
			acquires, reports := selector.snapshot()
			if acquires != 0 || len(reports) != 0 {
				t.Fatalf("fixed target touched scheduler: acquires=%d reports=%d", acquires, len(reports))
			}
		})
	}
}

func fixedTestPatchPlan(t *testing.T, request patch.RequestPatch, response patch.ResponsePatch) patch.Plan {
	t.Helper()
	var stages []patch.Stage
	if request != nil {
		stages = append(stages, patch.StageRequest)
	}
	if response != nil {
		stages = append(stages, patch.StageResponse)
	}
	definition := patch.PatchDefinition{
		ID:           "fixed-executor-test",
		Name:         "Fixed executor test",
		Description:  "Exercises the fixed-target execution boundary.",
		RequestTypes: []patch.RequestType{patch.RequestTypeClassifier},
		Stages:       stages,
		Idempotence:  patch.PerExecution,
		Factory: func(patch.FactoryContext) (patch.PatchInstance, error) {
			return patch.NewHooksInstance(patch.Hooks{Request: request, Response: response}), nil
		},
	}
	registry, err := patch.NewRegistry([]patch.PatchDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Compile([]string{definition.ID}, patch.RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func fixedTestTarget(t *testing.T, baseURL, protocolID string, plan patch.Plan) *provider.CompiledFixedTarget {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.CompiledFixedTarget{
		ID:         provider.FixedTargetID,
		BaseURL:    parsed,
		APIKey:     "fixed-secret",
		Protocol:   protocolID,
		PatchPlan:  plan,
		Generation: provider.ProviderGeneration("fixed-test-generation"),
	}
}

func fixedTestExecutionPlan(t *testing.T, target *provider.CompiledFixedTarget) flow.ExecutionPlan {
	t.Helper()
	body, index := fixedTestPreparedBody(t, `{"model":"original-model","metadata":{"user_id":"original-session"}}`)
	requestPlan, err := traffic.NewRequestPlan("original-model", "classifier-model", "original-session", traffic.RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := traffic.NewPreparedRequest(requestPlan, body, index)
	if err != nil {
		t.Fatal(err)
	}
	return flow.ExecutionPlan{
		PreparedRequest: prepared,
		TargetMode:      flow.TargetModeFixedTarget,
		AttemptPolicy:   scheduler.DefaultClassifierAttemptPolicy(),
		FixedTarget:     target,
	}
}

func fixedTestPreparedBody(t *testing.T, value string) (bodyfile.Body, bodyfile.JSONIndex) {
	t.Helper()
	body, err := bodyfile.Capture(strings.NewReader(value))
	if err != nil {
		t.Fatal(err)
	}
	index, err := bodyfile.Index(body)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	return body, index
}

func fixedTestTrackingBody(t *testing.T, value string) *fixedTrackingBody {
	t.Helper()
	body, err := bodyfile.Capture(strings.NewReader(value))
	if err != nil {
		t.Fatal(err)
	}
	return &fixedTrackingBody{body: body}
}

func fixedTestIncoming(rawQuery string) *http.Request {
	target := "http://gateway.example" + MessagesPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	request := httptest.NewRequest(http.MethodPost, target, nil)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestFixedExecutionPinsUncompressedUpstreamOnlyWhenRewritingResponse(t *testing.T) {
	adapter := &fixedTestAdapter{
		protocol: config.ProtocolOpenAIResponses,
		encode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
		decode: func(body bodyfile.Body, headers http.Header) (protocol.ProtocolMessage, error) {
			return protocol.ProtocolMessage{Body: body, Headers: headers}, nil
		},
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		protocolID  string
		responsable bool
		want        string
	}{
		{"anthropic passthrough keeps client negotiation", config.ProtocolAnthropicMessages, false, "gzip"},
		{"anthropic response patch pins identity", config.ProtocolAnthropicMessages, true, "identity"},
		{"protocol conversion pins identity", config.ProtocolOpenAIResponses, false, "identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received http.Header
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				received = request.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
			}))
			defer upstream.Close()
			plan := patch.Plan{}
			if test.responsable {
				plan = fixedTestPatchPlan(t, nil, fixedTestResponsePatch(func(patch.PatchContext, *patch.MutableResponse) error { return nil }))
			}
			target := fixedTestTarget(t, upstream.URL, test.protocolID, plan)
			handler := NewWithOptions(nil, &fakeSelector{}, Options{ProtocolAdapters: registry})
			defer handler.Close()
			response := httptest.NewRecorder()
			incoming := fixedTestIncoming("")
			incoming.Header.Set("Accept-Encoding", "gzip")
			handler.forwardFixedExecution(response, incoming, nil, fixedTestExecutionPlan(t, target))
			if response.Code != http.StatusOK {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if got := received.Get("Accept-Encoding"); got != test.want {
				t.Fatalf("upstream Accept-Encoding = %q, want %q", got, test.want)
			}
		})
	}
}
