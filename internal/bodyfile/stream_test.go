package bodyfile

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/modelname"
)

func TestCaptureAndScanSelectsOnlyRequiredPaths(t *testing.T) {
	input := []byte(`{"model":"m","unrelated":{"huge":[1,2,3]},"thinking":{"type":"disabled","budget":9},"system":[{"type":"text","text":"keep"},{"type":"text","text":"other"}]}`)
	spec, err := RequestScanSpec("/thinking", "/thinking/type", "/system", "/system/*", "/system/*/text")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(&oneByteReader{data: input}, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if index.Model != "m" {
		t.Fatalf("model = %q", index.Model)
	}
	for _, path := range []string{"/model", "/thinking", "/thinking/type", "/system", "/system/0", "/system/0/text", "/system/1/text"} {
		if _, ok := index.Lookup(path); !ok {
			t.Errorf("missing selected path %s", path)
		}
	}
	if _, ok := index.Lookup("/unrelated"); ok {
		t.Fatal("unrelated field was retained")
	}
	if _, ok := index.Lookup("/thinking/budget"); ok {
		t.Fatal("unselected sibling was retained")
	}
	if got := index.DirectChildren("/system"); len(got) != 2 {
		t.Fatalf("system direct children = %#v", got)
	}
	if got := index.Find("/system/0/text"); len(got) != 1 {
		t.Fatalf("Find = %#v", got)
	}
}

func TestCaptureAndScanEmptyPathsRetainsOnlyRequiredModel(t *testing.T) {
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(`{"model":"m","messages":[{"content":"ignored"}],"metadata":{"x":1}}`), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	fields := index.Fields()
	if len(fields) != 1 || fields[0].Path != "/model" {
		t.Fatalf("fields = %#v", fields)
	}
}

func TestSelectiveFieldDeleteRangeUsesUnretainedSiblingBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"first", `{"model":"m","target":1,"unrelated":2}`, `{"model":"m","unrelated":2}`},
		{"middle", `{"model":"m","left":1,"target":2,"right":3}`, `{"model":"m","left":1,"right":3}`},
		{"last", `{"model":"m","unrelated":1,"target":2}`, `{"model":"m","unrelated":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := RequestScanSpec("/target")
			if err != nil {
				t.Fatal(err)
			}
			body, index, err := CaptureAndScan(strings.NewReader(test.input), spec, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			field, ok := index.Lookup("/target")
			if !ok || field.DeleteRange.End <= field.DeleteRange.Start {
				t.Fatalf("target = %#v, %v", field, ok)
			}
			edited, err := ApplyEdits(body, []Edit{{Start: field.DeleteRange.Start, End: field.DeleteRange.End}}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer edited.Close()
			reader, err := edited.OpenReader()
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || string(got) != test.want {
				t.Fatalf("edited = %q, err=%v, want=%q", got, err, test.want)
			}
		})
	}
}

func TestNestedObjectAndArrayDeleteRanges(t *testing.T) {
	tests := []struct {
		name, input, path, want string
	}{
		{"object first", `{"outer":{"target":1, "b":2}}`, "/outer/target", `{"outer":{"b":2}}`},
		{"object middle", `{"outer":{"a":1, "target":2 , "b":3}}`, "/outer/target", `{"outer":{"a":1, "b":3}}`},
		{"object last", `{"outer":{"a":1 , "target":2}}`, "/outer/target", `{"outer":{"a":1}}`},
		{"object sole", `{"outer":{  "target":1  }}`, "/outer/target", `{"outer":{    }}`},
		{"array first", `{"items":[1, 2,3]}`, "/items/0", `{"items":[2,3]}`},
		{"array middle", `{"items":[1, 2 , 3]}`, "/items/1", `{"items":[1, 3]}`},
		{"array last", `{"items":[1 , 2]}`, "/items/1", `{"items":[1]}`},
		{"array sole", `{"items":[  1  ]}`, "/items/0", `{"items":[    ]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := ResponseScanSpec(test.path)
			if err != nil {
				t.Fatal(err)
			}
			body, index, err := CaptureAndScan(strings.NewReader(test.input), spec, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			field, ok := index.Lookup(test.path)
			if !ok {
				t.Fatalf("missing %s", test.path)
			}
			edited, err := ApplyEdits(body, []Edit{{Start: field.DeleteRange.Start, End: field.DeleteRange.End}}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer edited.Close()
			reader, _ := edited.OpenReader()
			got, readErr := io.ReadAll(reader)
			_ = reader.Close()
			if readErr != nil || string(got) != test.want {
				t.Fatalf("got=%q err=%v want=%q range=%#v", got, readErr, test.want, field.DeleteRange)
			}
		})
	}
}

func TestCaptureAndScanUsesExplicitStackForDeepJSON(t *testing.T) {
	const depth = 200_000
	var input bytes.Buffer
	input.WriteString(`{"model":"m","x":`)
	input.Grow(depth*2 + 32)
	for i := 0; i < depth; i++ {
		input.WriteByte('[')
	}
	input.WriteByte('0')
	for i := 0; i < depth; i++ {
		input.WriteByte(']')
	}
	input.WriteByte('}')
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(bytes.NewReader(input.Bytes()), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if index.Model != "m" || body.Size() != int64(input.Len()) {
		t.Fatalf("model=%q size=%d want=%d", index.Model, body.Size(), input.Len())
	}
}

func TestCaptureAndScanValidatesCompleteJSONAndTrailingContent(t *testing.T) {
	tests := []struct {
		input string
		want  error
	}{
		{`[]`, ErrInvalidJSON},
		{`{"model":"m"} []`, ErrTrailingJSON},
		{`{"model":"m","x":[1,]}`, ErrInvalidJSON},
		{`{"model":"m","x":{"a":1,}}`, ErrInvalidJSON},
		{`{"model":"m","x":01}`, ErrInvalidJSON},
		{`{"model":"m","x":"unterminated}`, ErrInvalidJSON},
	}
	spec, err := RequestScanSpec("/x")
	if err != nil {
		t.Fatal(err)
	}
	for index, test := range tests {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			body, _, err := CaptureAndScan(strings.NewReader(test.input), spec, t.TempDir())
			if body != nil {
				_ = body.Close()
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOneByteScannerTokenEdges(t *testing.T) {
	valid := []string{
		`{"model":"m","x":"a\\u0062\\n"}`,
		`{"model":"m","x":true,"y":false,"z":null}`,
		`{"model":"m","x":-12.50e+3}`,
	}
	for _, input := range valid {
		spec, _ := RequestScanSpec("/x")
		body, _, err := CaptureAndScan(&oneByteReader{data: []byte(input)}, spec, t.TempDir())
		if body != nil {
			_ = body.Close()
		}
		if err != nil {
			t.Errorf("valid %q: %v", input, err)
		}
	}
	invalid := []string{
		`{"model":"m","x":"\q"}`,
		"{\"model\":\"m\",\"x\":\"\x01\"}",
		`{"model":"m","x":tru}`,
		`{"model":"m","x":1.}`,
		`{"model":"m","x":1e+}`,
	}
	for _, input := range invalid {
		spec, _ := RequestScanSpec("/x")
		body, _, err := CaptureAndScan(&oneByteReader{data: []byte(input)}, spec, t.TempDir())
		if body != nil {
			_ = body.Close()
		}
		if !errors.Is(err, ErrInvalidJSON) {
			t.Errorf("invalid %q error=%v", input, err)
		}
	}
}

func TestApplyEditsAndScanKeepsSemanticJSONError(t *testing.T) {
	body := mustCapture(t, []byte(`{"model":"m","x":1}`))
	defer body.Close()
	spec, _ := RequestScanSpec("/x")
	_, _, err := ApplyEditsAndScan(body, []Edit{{Start: 17, End: 18, Replacement: []byte(`]`)}}, spec, t.TempDir())
	if !errors.Is(err, ErrInvalidJSON) || errors.Is(err, ErrLocalIO) {
		t.Fatalf("error = %v, want semantic invalid JSON only", err)
	}
}

func TestApplyEditsAndScanBuildsIndexDuringSingleSourcePass(t *testing.T) {
	base := mustCapture(t, []byte(`{"model":"m","x":1}`))
	defer base.Close()
	counted := &countingBody{Body: base}
	spec, _ := RequestScanSpec("/x")
	derived, index, err := ApplyEditsAndScan(counted, []Edit{{Start: 17, End: 18, Replacement: []byte(`2`)}}, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	if counted.opens.Load() != 1 {
		t.Fatalf("source open count = %d", counted.opens.Load())
	}
	field, ok := index.Lookup("/x")
	if !ok || field.Type != JSONNumber || index.ValidateBody(derived) != nil {
		t.Fatalf("derived index = %#v, field=%#v ok=%v", index, field, ok)
	}
}

func TestScanResultBindRejectsZeroAndDifferentSize(t *testing.T) {
	body := mustCapture(t, []byte(`{}`))
	defer body.Close()
	if _, err := (ScanResult{}).Bind(body); !errors.Is(err, ErrIndexBodyMismatch) {
		t.Fatalf("zero Bind error = %v", err)
	}
	scanner, err := NewJSONScanner(ScanSpec{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = scanner.Write([]byte(`{}`))
	result, err := scanner.Finish()
	if err != nil {
		t.Fatal(err)
	}
	other := mustCapture(t, []byte(`{} `))
	defer other.Close()
	if _, err := result.Bind(other); !errors.Is(err, ErrIndexBodyMismatch) {
		t.Fatalf("different-size Bind error = %v", err)
	}
}

func TestScanResultBindRejectsSameSizeDifferentBody(t *testing.T) {
	input := []byte(`{"model":"m","x":1}`)
	first := mustCapture(t, input)
	defer first.Close()
	second := mustCapture(t, []byte(`{"model":"m","y":2}`))
	defer second.Close()
	scanner, err := NewJSONScanner(ScanSpec{RequireModel: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Write(input); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Bind(first); err != nil {
		t.Fatalf("matching body rejected: %v", err)
	}
	if _, err := result.Bind(second); !errors.Is(err, ErrIndexBodyMismatch) {
		t.Fatalf("same-size different body accepted: %v", err)
	}
}

func TestApplyEditsAndScanEmptyRequiresExistingIndex(t *testing.T) {
	base := &countingBody{Body: mustCapture(t, []byte(`{"model":"m"}`))}
	defer base.Close()
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := ApplyEditsAndScan(base, nil, spec, t.TempDir())
	if result != base || !errors.Is(err, ErrIndexRequired) {
		t.Fatalf("empty edit result=%v err=%v", result, err)
	}
	if got := base.opens.Load(); got != 0 {
		t.Fatalf("empty edit path reopened body %d times", got)
	}
	index, err := IndexSelective(base, spec)
	if err != nil {
		t.Fatal(err)
	}
	result, projected, err := ApplyEditsAndScanWithIndex(base, index, nil, spec, t.TempDir())
	if err != nil || result != base || projected.ValidateBody(base) != nil {
		t.Fatalf("indexed empty edit result=%v index=%v err=%v", result, projected, err)
	}
}

func TestSelectiveScannerRetainsLongWildcardKey(t *testing.T) {
	key := strings.Repeat("k", 256*1024)
	spec, err := ResponseScanSpec("/*")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(`{"`+key+`":1}`), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	field, ok := index.Lookup("/" + key)
	if !ok || field.Key != key {
		t.Fatalf("wildcard key was not retained: ok=%v key-len=%d", ok, len(field.Key))
	}
}

func TestSelectiveScannerAcceptsEmptyObjectKey(t *testing.T) {
	spec, err := ResponseScanSpec("/")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(`{"":1,"x":2}`), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	field, ok := index.Lookup("/")
	if !ok || field.ArrayIndex >= 0 || field.Key != "" {
		t.Fatalf("empty-key field = %#v, %v", field, ok)
	}
}

func TestLongUnrelatedKeyDoesNotReachIndex(t *testing.T) {
	key := strings.Repeat("k", 2*1024*1024)
	input := `{"model":"m","` + key + `":{"deep":[[[1]]]},"thinking":{"type":"disabled"}}`
	spec, _ := RequestScanSpec("/thinking/type")
	body, index, err := CaptureAndScan(strings.NewReader(input), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	for _, field := range index.Fields() {
		if strings.Contains(field.Path, key[:100]) || field.Key == key {
			t.Fatal("unrelated long key retained")
		}
	}
}

func TestSelectiveScannerBoundsUnrelatedKeyBuffer(t *testing.T) {
	spec, err := RequestScanSpec("/thinking/type")
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewJSONScanner(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Write([]byte(`{"model":"m","`)); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("k"), 32*1024)
	for written := 0; written < 8*1024*1024; written += len(chunk) {
		if _, err := scanner.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if scanner.token == nil {
			t.Fatal("key token ended before closing quote")
		}
		if len(scanner.token.raw) > scanner.keyRawLimit {
			t.Fatalf("key buffer=%d limit=%d", len(scanner.token.raw), scanner.keyRawLimit)
		}
	}
	if scanner.token == nil || !scanner.token.rawOverflow {
		t.Fatal("long unrelated key did not enter bounded discard mode")
	}
	if _, err := scanner.Write([]byte(`":0,"thinking":{"type":"disabled"}}`)); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fields) == 0 {
		t.Fatal("selected field was not indexed after long unrelated key")
	}
}

func TestScanBodyClosesReaderExactlyOnce(t *testing.T) {
	reader := &singleCloseReader{Reader: strings.NewReader(`{}`)}
	body := &faultBody{size: 2, reader: reader}
	if _, err := ScanBody(body, ScanSpec{}); err != nil {
		t.Fatal(err)
	}
	if got := reader.closes.Load(); got != 1 {
		t.Fatalf("reader close count = %d", got)
	}
}

func TestCaptureAndScanJoinsAbortFailure(t *testing.T) {
	abortCause := errors.New("abort failed")
	builder := &abortErrorBuilder{abortErr: abortCause}
	_, _, err := captureAndScanWithBuilder(strings.NewReader(`{`), ScanSpec{}, func() (Builder, error) {
		return builder, nil
	})
	if !errors.Is(err, ErrInvalidJSON) || !errors.Is(err, abortCause) {
		t.Fatalf("error = %v, want invalid JSON joined with abort cause", err)
	}
	if builder.aborts.Load() != 1 {
		t.Fatalf("abort count = %d", builder.aborts.Load())
	}
}

func TestCaptureAndScanPreservesReadErrorWhenSameChunkIsMalformed(t *testing.T) {
	readCause := errors.New("upstream read failed")
	source := &terminalChunkReader{data: []byte(`{]`), err: readCause}
	body, _, err := CaptureAndScan(source, ScanSpec{}, t.TempDir())
	if body != nil {
		_ = body.Close()
	}
	if !IsReadError(err) || !errors.Is(err, readCause) || !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("error = %v, want read cause joined with invalid JSON", err)
	}
}

func TestModelLimitUsesDecodedUTF8Bytes(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		decoded string
		wantErr bool
	}{
		{name: "ASCII boundary", encoded: strings.Repeat("a", modelname.MaxBytes), decoded: strings.Repeat("a", modelname.MaxBytes)},
		{name: "ASCII over", encoded: strings.Repeat("a", modelname.MaxBytes+1), wantErr: true},
		{name: "raw UTF-8 boundary", encoded: strings.Repeat("界", 85) + "a", decoded: strings.Repeat("界", 85) + "a"},
		{name: "raw UTF-8 over", encoded: strings.Repeat("界", 86), wantErr: true},
		{name: "escaped ASCII boundary", encoded: strings.Repeat(`\u0061`, modelname.MaxBytes), decoded: strings.Repeat("a", modelname.MaxBytes)},
		{name: "escaped ASCII over", encoded: strings.Repeat(`\u0061`, modelname.MaxBytes+1), wantErr: true},
		{name: "escaped multibyte boundary", encoded: strings.Repeat(`\u00E9`, modelname.MaxBytes/2), decoded: strings.Repeat("é", modelname.MaxBytes/2)},
		{name: "escaped multibyte over", encoded: strings.Repeat(`\u00E9`, modelname.MaxBytes/2+1), wantErr: true},
		{name: "surrogate boundary", encoded: strings.Repeat(`\uD83D\uDE00`, modelname.MaxBytes/4), decoded: strings.Repeat("😀", modelname.MaxBytes/4)},
		{name: "surrogate over", encoded: strings.Repeat(`\uD83D\uDE00`, modelname.MaxBytes/4+1), wantErr: true},
		{name: "unpaired surrogate boundary", encoded: strings.Repeat(`\uD800`, 85) + "a", decoded: strings.Repeat("�", 85) + "a"},
		{name: "unpaired surrogate over", encoded: strings.Repeat(`\uD800`, 86), wantErr: true},
	}
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"model":"` + test.encoded + `"}`)
			body, index, scanErr := CaptureAndScan(&oneByteReader{data: input}, spec, t.TempDir())
			if body != nil {
				defer body.Close()
			}
			if test.wantErr {
				if !errors.Is(scanErr, ErrModelTooLong) {
					t.Fatalf("error = %v, want ErrModelTooLong", scanErr)
				}
				return
			}
			if scanErr != nil {
				t.Fatal(scanErr)
			}
			if index.ModelValue() != test.decoded || len(index.ModelValue()) != modelname.MaxBytes {
				t.Fatalf("decoded model bytes = %d, value mismatch=%v", len(index.ModelValue()), index.ModelValue() != test.decoded)
			}
		})
	}
}

func TestCaptureRejectsOverlongModelBeforeWritingOverflowByte(t *testing.T) {
	prefix := `{"model":"`
	source := &oneByteReader{data: []byte(prefix + strings.Repeat("a", modelname.MaxBytes+1) + `"}`)}
	builder := &recordingAbortBuilder{}
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := captureAndScanWithBuilder(source, spec, func() (Builder, error) { return builder, nil })
	if body != nil {
		_ = body.Close()
	}
	if !errors.Is(err, ErrModelTooLong) {
		t.Fatalf("error = %v, want ErrModelTooLong", err)
	}
	want := prefix + strings.Repeat("a", modelname.MaxBytes)
	if builder.buffer.String() != want {
		t.Fatalf("stored bytes = %d, want %d", builder.buffer.Len(), len(want))
	}
	if source.pos != len(want)+1 {
		t.Fatalf("source consumed = %d, want %d", source.pos, len(want)+1)
	}
	if builder.aborts.Load() != 1 {
		t.Fatalf("abort count = %d", builder.aborts.Load())
	}
}

func TestOverlongModelTokenRetainsOnlyBoundedRawSpelling(t *testing.T) {
	spec, err := RequestScanSpec()
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewJSONScanner(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Write([]byte(`{"model":"`)); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("a"), 32*1024)
	for i := 0; i < 64; i++ {
		_, writeErr := scanner.Write(chunk)
		if len(scanner.token.raw) > modelname.MaxBytes*6+2 {
			t.Fatalf("model raw token grew to %d bytes", len(scanner.token.raw))
		}
		if errors.Is(writeErr, ErrModelTooLong) {
			return
		}
		if writeErr != nil {
			t.Fatalf("scanner error = %v", writeErr)
		}
	}
	t.Fatal("overlong model was not rejected")
}

func TestCaptureAndScanLargeUnselectedBody(t *testing.T) {
	const large = 68 * 1024 * 1024
	spec, err := RequestScanSpec("/thinking/type")
	if err != nil {
		t.Fatal(err)
	}
	source := io.MultiReader(
		strings.NewReader(`{"model":"m","unrelated":"`),
		io.LimitReader(&repeatingReader{pattern: []byte("z")}, large),
		strings.NewReader(`","thinking":{"type":"disabled"}}`),
	)
	body, index, err := CaptureAndScan(source, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, ok := index.Lookup("/thinking/type"); !ok {
		t.Fatal("selected field missing")
	}
	if _, ok := index.Lookup("/unrelated"); ok {
		t.Fatal("large unrelated field retained")
	}
}

type oneByteReader struct {
	data []byte
	pos  int
}

type singleCloseReader struct {
	io.Reader
	closes atomic.Int32
}

type abortErrorBuilder struct {
	abortErr error
	aborts   atomic.Int32
}

type recordingAbortBuilder struct {
	buffer bytes.Buffer
	aborts atomic.Int32
}

type terminalChunkReader struct {
	data []byte
	err  error
	done bool
}

func (r *terminalChunkReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

type countingBody struct {
	Body
	opens atomic.Int32
}

func (b *countingBody) OpenReader() (io.ReadCloser, error) {
	b.opens.Add(1)
	return b.Body.OpenReader()
}

func (*abortErrorBuilder) Write(data []byte) (int, error) { return len(data), nil }
func (*abortErrorBuilder) Seal() (Body, error)            { return nil, errors.New("unexpected seal") }
func (b *abortErrorBuilder) Abort() error {
	b.aborts.Add(1)
	return b.abortErr
}

func (b *recordingAbortBuilder) Write(data []byte) (int, error) { return b.buffer.Write(data) }
func (*recordingAbortBuilder) Seal() (Body, error)              { return nil, errors.New("unexpected seal") }
func (b *recordingAbortBuilder) Abort() error {
	b.aborts.Add(1)
	return nil
}

func (r *singleCloseReader) Close() error {
	if r.closes.Add(1) != 1 {
		return errors.New("reader closed more than once")
	}
	return nil
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}
