package bodyfile

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestApplyEditsPreservesEveryNonTargetByte(t *testing.T) {
	original := []byte("  { \"model\" : \"old\", \"number\":1.2300e+04,\"tail\":true } \n")
	body := mustCapture(t, original)
	defer body.Close()
	index, err := Index(body)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := ApplyEdits(body, []Edit{{Start: index.ModelRange.Start, End: index.ModelRange.End, Replacement: []byte(`"new"`)}})
	if err != nil {
		t.Fatal(err)
	}
	defer edited.Close()
	want := bytes.Replace(original, []byte(`"old"`), []byte(`"new"`), 1)
	if got := readBody(t, edited); !bytes.Equal(got, want) {
		t.Fatalf("edited bytes = %q, want %q", got, want)
	}
	if err := index.ValidateBody(edited); !errors.Is(err, ErrIndexBodyMismatch) {
		t.Fatalf("old index accepted edited body: %v", err)
	}
	newIndex, err := Index(edited)
	if err != nil || newIndex.Model != "new" {
		t.Fatalf("new index = %#v, %v", newIndex, err)
	}
}

func TestApplyMultipleInsertDeleteAndReplace(t *testing.T) {
	body := mustCapture(t, []byte("0123456789"))
	defer body.Close()
	edited, err := ApplyEdits(body, []Edit{
		{Start: 0, End: 0, Replacement: []byte("A")},
		{Start: 2, End: 4, Replacement: []byte("BC")},
		{Start: 7, End: 9},
		{Start: 10, End: 10, Replacement: []byte("Z")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer edited.Close()
	if got := string(readBody(t, edited)); got != "A01BC4569Z" {
		t.Fatalf("edited = %q", got)
	}
}

func TestApplyEditsRejectsInvalidUnorderedAndOverlapping(t *testing.T) {
	body := mustCapture(t, []byte("0123456789"))
	defer body.Close()
	tests := [][]Edit{
		{{Start: -1, End: 0}},
		{{Start: 3, End: 2}},
		{{Start: 0, End: 11}},
		{{Start: 5, End: 6}, {Start: 2, End: 3}},
		{{Start: 2, End: 6}, {Start: 5, End: 7}},
		{{Start: 2, End: 2}, {Start: 2, End: 2}},
	}
	for _, edits := range tests {
		if result, err := ApplyEdits(body, edits); err == nil {
			if result != body {
				_ = result.Close()
			}
			t.Fatalf("ApplyEdits(%#v) succeeded", edits)
		}
	}
}

func TestApplyEditsEmptyIsNoOp(t *testing.T) {
	body := mustCapture(t, []byte("bytes"))
	defer body.Close()
	got, err := ApplyEdits(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatal("empty edits did not reuse original Body")
	}
}

func readBody(t *testing.T, body Body) []byte {
	t.Helper()
	reader, err := body.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
