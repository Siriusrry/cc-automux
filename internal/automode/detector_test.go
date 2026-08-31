package automode

import (
	"errors"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func classifierView(t *testing.T, input string) (traffic.RequestView, func()) {
	t.Helper()
	spec, err := bodyfile.RequestScanSpecWithRawMarkers([]string{"/system", "/system/0", "/system/0/text"}, SecurityMarker)
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := bodyfile.CaptureAndScan(strings.NewReader(input), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return traffic.RequestView{Body: body, Index: index, OriginalModel: index.Model, Headers: traffic.EmptyHeaders{}}, func() { _ = body.Close() }
}

func TestClassifierDetectorStrictPredicateAndDuplicateLastValue(t *testing.T) {
	detector := NewClassifierDetector()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "positive",
			input: `{"model":"client-model","system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents.\nclassify"}],"messages":[]}`,
			want:  true,
		},
		{
			name:  "marker only transcript",
			input: `{"model":"m","system":[{"type":"text","text":"ordinary"}],"messages":[{"content":"You are a security monitor for autonomous AI coding agents"}]}`,
			want:  false,
		},
		{
			name:  "wrong final system value",
			input: `{"model":"m","system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents"}],"system":[{"type":"text","text":"ordinary"}]}`,
			want:  false,
		},
		{
			name:  "matching final system value",
			input: `{"model":"m","system":[{"type":"text","text":"ordinary"}],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents"}]}`,
			want:  true,
		},
		{
			name:  "wrong final text member",
			input: `{"model":"m","system":[{"text":"You are a security monitor for autonomous AI coding agents","text":"ordinary"}]}`,
			want:  false,
		},
		{
			name:  "matching final text member",
			input: `{"model":"m","system":[{"text":"ordinary","text":"You are a security monitor for autonomous AI coding agents"}]}`,
			want:  true,
		},
		{
			name:  "empty system",
			input: `{"model":"m","system":[]}`,
			want:  false,
		},
		{
			name:  "first item non object",
			input: `{"model":"m","system":["You are a security monitor for autonomous AI coding agents"]}`,
			want:  false,
		},
		{
			name:  "system non array",
			input: `{"model":"m","system":{"0":{"text":"You are a security monitor for autonomous AI coding agents"}}}`,
			want:  false,
		},
		{
			name:  "escaped marker only",
			input: `{"model":"m","system":[{"type":"text","text":"\u0059ou are a security monitor for autonomous AI coding agents"}]}`,
			want:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view, cleanup := classifierView(t, test.input)
			defer cleanup()
			got, err := detector.Detect(view)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Detect = %v, want %v", got, test.want)
			}
		})
	}
}

func TestClassifierDetectorRequiresRawMarkerAndAcceptsDecodedPrefix(t *testing.T) {
	detector := NewClassifierDetector()
	// The marker is escaped in system[0].text but appears literally in an
	// unrelated field. Both conditions are independent and therefore match.
	view, cleanup := classifierView(t, `{"model":"m","note":"You are a security monitor for autonomous AI coding agents","system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`)
	defer cleanup()
	got, err := detector.Detect(view)
	if err != nil || !got {
		t.Fatalf("decoded prefix detection = %v, %v", got, err)
	}

	spec, err := bodyfile.RequestScanSpec("/system", "/system/0", "/system/0/text")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := bodyfile.CaptureAndScan(strings.NewReader(`{"model":"m","system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, err = detector.Detect(traffic.RequestView{Body: body, Index: index, OriginalModel: "m", Headers: traffic.EmptyHeaders{}})
	if !errors.Is(err, ErrRawMarkerNotTracked) {
		t.Fatalf("missing marker tracking error = %v", err)
	}

	view, cleanup = classifierView(t, `{"model":"m","system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`)
	defer cleanup()
	// Replace the body-bound index with a valid body index that tracks a marker
	// which does not occur, exercising the negative raw precheck.
	noMarkerSpec, _ := bodyfile.RequestScanSpecWithRawMarkers([]string{"/system", "/system/0", "/system/0/text"}, SecurityMarker)
	noMarkerBody, noMarkerIndex, err := bodyfile.CaptureAndScan(strings.NewReader(`{"model":"m","system":[{"text":"\u0059ou are a security monitor for autonomous AI coding agents"}]}`), noMarkerSpec)
	if err != nil {
		t.Fatal(err)
	}
	defer noMarkerBody.Close()
	got, err = detector.Detect(traffic.RequestView{Body: noMarkerBody, Index: noMarkerIndex, OriginalModel: "m", Headers: traffic.EmptyHeaders{}})
	if err != nil || got {
		t.Fatalf("absent raw marker detection = %v, %v", got, err)
	}
}

func TestClassifierDetectorUsesOnlyFinalSystemAndTextWithinSelectedOccurrence(t *testing.T) {
	detector := NewClassifierDetector()
	input := `{"model":"m","system":[{"text":"You are a security monitor for autonomous AI coding agents"}],"system":[{"text":"ordinary"}],"tail":"You are a security monitor for autonomous AI coding agents"}`
	view, cleanup := classifierView(t, input)
	defer cleanup()
	if got, err := detector.Detect(view); err != nil || got {
		t.Fatalf("final system containment = %v, %v", got, err)
	}
	input = `{"model":"m","system":[{"text":"ordinary"}],"system":[{"text":"You are a security monitor for autonomous AI coding agents"}]}`
	view, cleanup = classifierView(t, input)
	defer cleanup()
	if got, err := detector.Detect(view); err != nil || !got {
		t.Fatalf("final system match = %v, %v", got, err)
	}
}

func TestClassifierDetectorLargeTextUsesBoundedPrefix(t *testing.T) {
	text := SecurityMarker + strings.Repeat("x", 8<<20)
	input := `{"model":"m","system":[{"text":"` + text + `"}]}`
	view, cleanup := classifierView(t, input)
	defer cleanup()
	if got, err := NewClassifierDetector().Detect(view); err != nil || !got {
		t.Fatalf("large classifier detection = %v, %v", got, err)
	}
}

func TestClassifierDetectorMetadataAndTypeContract(t *testing.T) {
	detector := NewClassifierDetector()
	if detector.Type() != traffic.RequestTypeClassifier {
		t.Fatalf("Type = %q", detector.Type())
	}
	paths := detector.RequiredPaths()
	if len(paths) != 3 || paths[0] != "/system" || paths[1] != "/system/0" || paths[2] != "/system/0/text" {
		t.Fatalf("RequiredPaths = %#v", paths)
	}
	markers := detector.RequiredRawMarkers()
	if len(markers) != 1 || markers[0] != SecurityMarker {
		t.Fatalf("RequiredRawMarkers = %#v", markers)
	}
}
