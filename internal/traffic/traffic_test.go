package traffic

import (
	"bytes"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

type testDetector struct {
	typ     RequestType
	matched bool
	err     error
	seen    RequestView
}

type panicDetector struct{ typ RequestType }

type markerDetector struct {
	typ     RequestType
	markers []string
}

type oneShotTypeDetector struct{ calls int }

func (d *panicDetector) Type() RequestType { return d.typ }
func (d *panicDetector) Detect(RequestView) (bool, error) {
	panic("detector panic fixture")
}

func (d *markerDetector) Type() RequestType                { return d.typ }
func (d *markerDetector) Detect(RequestView) (bool, error) { return false, nil }
func (d *markerDetector) RequiredRawMarkers() []string     { return d.markers }

func (d *oneShotTypeDetector) Type() RequestType {
	d.calls++
	if d.calls > 1 {
		panic("detector type called after registry construction")
	}
	return RequestTypeClassifier
}

func (*oneShotTypeDetector) Detect(RequestView) (bool, error) { return false, nil }

func (d *testDetector) Type() RequestType { return d.typ }
func (d *testDetector) Detect(view RequestView) (bool, error) {
	d.seen = view
	return d.matched, d.err
}

func TestProductionRegistryFallsBackToNormal(t *testing.T) {
	registry := DefaultRegistry()
	if got, err := registry.Classify(RequestView{}); err != nil || got != RequestTypeNormal {
		t.Fatalf("Classify = %q, %v", got, err)
	}
	if registry.Len() != 0 || len(registry.Types()) != 0 {
		t.Fatalf("production registry unexpectedly has detectors")
	}
}

func TestDetectorRegistrySingleAmbiguousAndError(t *testing.T) {
	body, err := bodyfile.Capture(bytes.NewReader([]byte(`{"model":"m"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	index, err := bodyfile.Index(body)
	if err != nil {
		t.Fatal(err)
	}
	view := RequestView{Body: body, Index: index, OriginalModel: "m", Headers: EmptyHeaders{}}
	one := &testDetector{typ: RequestTypeClassifier, matched: true}
	registry, err := NewRegistry(one)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := registry.Classify(view); err != nil || got != RequestTypeClassifier {
		t.Fatalf("single classify = %q, %v", got, err)
	}
	if one.seen.Body != body || one.seen.OriginalModel != "m" {
		t.Fatalf("detector received wrong view: %#v", one.seen)
	}

	second := &testDetector{typ: RequestType("future"), matched: true}
	ambiguous, err := NewRegistry(one, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ambiguous.Classify(view); !errors.Is(err, ErrAmbiguousRequestType) {
		t.Fatalf("ambiguous error = %v", err)
	}

	failure := errors.New("fixture failure")
	failed, err := NewRegistry(&testDetector{typ: RequestType("broken"), err: failure})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Classify(view); !errors.Is(err, ErrDetectionFailed) || !errors.Is(err, failure) {
		t.Fatalf("detector error = %v", err)
	}
}

func TestDetectorRegistryRejectsMalformedDefinitions(t *testing.T) {
	var nilDetector *testDetector
	tests := []struct {
		name string
		args []Detector
		want error
	}{
		{"nil", []Detector{nilDetector}, ErrNilDetector},
		{"normal", []Detector{&testDetector{typ: RequestTypeNormal}}, ErrNormalDetector},
		{"empty", []Detector{&testDetector{}}, ErrInvalidRequestType},
		{"whitespace", []Detector{&testDetector{typ: RequestType("a b")}}, ErrInvalidRequestType},
		{"wildcard", []Detector{&testDetector{typ: RequestType("*")}}, ErrInvalidRequestType},
		{"duplicate", []Detector{&testDetector{typ: "x"}, &testDetector{typ: "x"}}, ErrDuplicateDetector},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.args...); !errors.Is(err, test.want) {
				t.Fatalf("NewRegistry error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDetectorPanicFailsClosed(t *testing.T) {
	registry, err := NewRegistry(&panicDetector{typ: "panic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Classify(RequestView{}); !errors.Is(err, ErrDetectionFailed) {
		t.Fatalf("panic classification error = %v", err)
	}
}

func TestDetectorRegistrySnapshotsRawMarkerUnion(t *testing.T) {
	registry, err := NewRegistry(
		&markerDetector{typ: "first", markers: []string{"alpha", "shared"}},
		&markerDetector{typ: "second", markers: []string{"shared", "beta"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "shared", "beta"}
	if got := registry.RequiredRawMarkers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("raw markers = %#v, want %#v", got, want)
	}
	copy := registry.RequiredRawMarkers()
	copy[0] = "mutated"
	if got := registry.RequiredRawMarkers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("raw marker union shares caller mutation: %#v", got)
	}
	if _, err := NewRegistry(&markerDetector{typ: "empty-marker", markers: []string{""}}); err == nil {
		t.Fatal("empty raw marker was accepted")
	}
}

func TestDetectorRegistrySnapshotsTypesAtConstruction(t *testing.T) {
	detector := &oneShotTypeDetector{}
	registry, err := NewRegistry(detector)
	if err != nil {
		t.Fatal(err)
	}
	want := []RequestType{RequestTypeClassifier}
	first := registry.Types()
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("types = %#v, want %#v", first, want)
	}
	first[0] = "mutated"
	if got := registry.Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached types share caller mutation: %#v", got)
	}
	if detector.calls != 1 {
		t.Fatalf("Type calls = %d, want 1", detector.calls)
	}
}

func TestIngressAndPreparedConstructorsValidateAndBindIndex(t *testing.T) {
	body, err := bodyfile.Capture(bytes.NewReader([]byte(`{"model":"m"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	index, err := bodyfile.Index(body)
	if err != nil {
		t.Fatal(err)
	}
	detection := NewDetectionRequest(body, index, "m", "session", EmptyHeaders{})
	ingress, err := NewIngressRequest(detection, RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	if ingress.RequestType != RequestTypeClassifier || ingress.OriginalSessionID != "session" {
		t.Fatalf("ingress = %#v", ingress)
	}
	plan, err := NewRequestPlan("m", "effective", "session", RequestTypeClassifier)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := NewPreparedRequest(plan, body, index)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.BaseBody != body || !reflect.DeepEqual(prepared.Plan, plan) {
		t.Fatalf("prepared = %#v", prepared)
	}
	other, err := bodyfile.Capture(bytes.NewReader([]byte(`{"model":"m"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := NewPreparedRequest(plan, other, index); !errors.Is(err, bodyfile.ErrIndexBodyMismatch) {
		t.Fatalf("cross-body prepared error = %v", err)
	}
	if _, err := NewIngressRequest(DetectionRequest{}, RequestTypeNormal); !errors.Is(err, ErrInvalidIngressRequest) {
		t.Fatalf("nil ingress error = %v", err)
	}
}

func TestHTTPHeaderViewIsCaseInsensitiveAndDefensive(t *testing.T) {
	headers := http.Header{"X-Test": {"one", "two"}, "x-Other": {"value"}, "Authorization": {"secret"}, "X-Api-Key": {"provider-secret"}}
	view := NewHTTPHeaders(headers)
	if got, ok := view.Get("x-test"); !ok || got != "one" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	values := view.Values("X-TEST")
	values[0] = "mutated"
	if got, _ := view.Get("x-test"); got != "one" {
		t.Fatalf("Values leaked mutable slice: %q", got)
	}
	headers.Set("X-Test", "changed")
	if got, _ := view.Get("x-test"); got != "one" {
		t.Fatalf("source header mutation leaked: %q", got)
	}
	if got := view.Values("missing"); got == nil {
		t.Fatal("missing Values returned nil")
	}
	if got, ok := view.Get("authorization"); ok || got != "" {
		t.Fatalf("credential header leaked: %q, %v", got, ok)
	}
	if got := view.Values("x-api-key"); got == nil || len(got) != 0 {
		t.Fatalf("credential Values = %#v", got)
	}
}
