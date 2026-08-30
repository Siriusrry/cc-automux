package bodyfile

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestIndexValidatesAndPreservesRanges(t *testing.T) {
	original := []byte(" \n{\"unknown\":1.2300e+04, \"model\" : \"claude-\\u0061\", \"thinking\":{\"type\":\"disabled\",\"budget\":17},\"items\":[true,null,{\"text\":\"x\"}]}\t")
	body := mustCapture(t, original)
	defer body.Close()
	index, err := BuildIndex(body)
	if err != nil {
		t.Fatal(err)
	}
	if index.Model != "claude-a" || index.OriginalModel() != "claude-a" {
		t.Fatalf("Model = %q", index.Model)
	}
	model, ok := index.Lookup("/model")
	if !ok || model.Type != JSONString {
		t.Fatalf("model field = %#v, %v", model, ok)
	}
	if got := original[model.ValueRange.Start:model.ValueRange.End]; string(got) != `"claude-\u0061"` {
		t.Fatalf("model bytes = %q", got)
	}
	thinking, ok := index.Lookup("/thinking/type")
	if !ok || thinking.Key != "type" || thinking.Type != JSONString {
		t.Fatalf("thinking.type = %#v, %v", thinking, ok)
	}
	if fields := index.Find("/thinking/type"); len(fields) != 1 {
		t.Fatalf("Find = %#v", fields)
	}
	arrayObject, ok := index.Lookup("/items/2/text")
	if !ok || arrayObject.Type != JSONString {
		t.Fatalf("array field = %#v, %v", arrayObject, ok)
	}
	itemsObject, ok := index.Lookup("/items/2")
	if !ok || itemsObject.Depth != 1 {
		t.Fatalf("array object depth = %#v, %v", itemsObject, ok)
	}
	if arrayObject.Depth != 2 {
		t.Fatalf("array object child depth = %d", arrayObject.Depth)
	}
	if index.Size() != int64(len(original)) {
		t.Fatalf("index.Size = %d", index.Size())
	}
	if err := index.ValidateBody(body); err != nil {
		t.Fatal(err)
	}
	other := mustCapture(t, original)
	defer other.Close()
	if err := index.ValidateBody(other); !errors.Is(err, ErrIndexBodyMismatch) {
		t.Fatalf("ValidateBody(other) = %v", err)
	}
}

func TestIndexRejectsMalformedTrailingAndInvalidModel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"empty", "", ErrInvalidJSON},
		{"array", `[]`, ErrInvalidJSON},
		{"malformed", `{"model":"m",}`, ErrInvalidJSON},
		{"trailing", `{"model":"m"} []`, ErrTrailingJSON},
		{"missing", `{"messages":[]}`, ErrModelMissing},
		{"repeated", `{"model":"a","model":"b"}`, ErrModelRepeated},
		{"not string", `{"model":17}`, ErrModelNotString},
		{"empty model", `{"model":""}`, ErrModelEmpty},
		{"bad nested", `{"model":"m","x":[1,]}`, ErrInvalidJSON},
		{"bad number", `{"model":"m","x":01}`, ErrInvalidJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := mustCapture(t, []byte(test.input))
			defer body.Close()
			_, err := Index(body)
			if !errors.Is(err, test.want) {
				t.Fatalf("Index error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestIndexDoesNotRetainLargeUnknownString(t *testing.T) {
	const large = 4 * 1024 * 1024
	prefix := strings.NewReader(`{"model":"m","large":"`)
	content := io.LimitReader(&repeatingReader{pattern: []byte("z")}, large)
	suffix := strings.NewReader(`"}`)
	body, err := Capture(io.MultiReader(prefix, content, suffix), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	index, err := Index(body)
	if err != nil {
		t.Fatal(err)
	}
	field, ok := index.Lookup("/large")
	if !ok || field.ValueRange.Len() != large+2 {
		t.Fatalf("large field = %#v, %v", field, ok)
	}
	if len(index.Fields()) != 2 {
		t.Fatalf("indexed fields = %d", len(index.Fields()))
	}
}

func TestIndexEscapesJSONPointerPaths(t *testing.T) {
	body := mustCapture(t, []byte(`{"model":"m","a/b~c":{"x":1}}`))
	defer body.Close()
	index, err := Index(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Lookup("/a~1b~0c/x"); !ok {
		t.Fatalf("escaped path absent: %#v", index.Fields())
	}
}

func TestIndexAcceptsJSONSlashEscapesInModel(t *testing.T) {
	body := mustCapture(t, []byte(`{"model":"a\/b"}`))
	defer body.Close()
	index, err := Index(body)
	if err != nil {
		t.Fatal(err)
	}
	if index.Model != "a/b" {
		t.Fatalf("decoded model = %q", index.Model)
	}
}

func TestIndexObjectDoesNotRequireOrInterpretModel(t *testing.T) {
	tests := []string{
		`{}`,
		`{"type":"message","content":[]}`,
		`{"model":17}`,
		`{"model":"","model":null}`,
	}
	for _, input := range tests {
		body := mustCapture(t, []byte(input))
		index, err := IndexObject(body)
		if err != nil {
			_ = body.Close()
			t.Fatalf("IndexObject(%s): %v", input, err)
		}
		if index.Model != "" || index.ModelRange != (ByteRange{}) {
			_ = body.Close()
			t.Fatalf("general object index interpreted model: %#v", index)
		}
		if err := index.ValidateBody(body); err != nil {
			_ = body.Close()
			t.Fatal(err)
		}
		_ = body.Close()
	}

	body := mustCapture(t, []byte(`{"type":"message"}`))
	defer body.Close()
	if _, err := Index(body); !errors.Is(err, ErrModelMissing) {
		t.Fatalf("strict Index error = %v", err)
	}
}

func TestIndexObjectStillRequiresOneValidObject(t *testing.T) {
	tests := []struct {
		input string
		want  error
	}{
		{"[]", ErrInvalidJSON},
		{"{} []", ErrTrailingJSON},
		{"{", ErrInvalidJSON},
	}
	for _, test := range tests {
		body := mustCapture(t, []byte(test.input))
		_, err := IndexObject(body)
		_ = body.Close()
		if !errors.Is(err, test.want) {
			t.Fatalf("IndexObject(%q) = %v, want %v", test.input, err, test.want)
		}
	}
}

func mustCapture(t *testing.T, data []byte) Body {
	t.Helper()
	body, err := Capture(bytes.NewReader(data), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return body
}
