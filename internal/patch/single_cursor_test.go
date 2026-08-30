package patch

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

type cursorCountingBody struct {
	content   string
	openCount atomic.Int32
}

func (b *cursorCountingBody) OpenReader() (io.ReadCloser, error) {
	b.openCount.Add(1)
	return io.NopCloser(strings.NewReader(b.content)), nil
}

func (b *cursorCountingBody) Size() int64 { return int64(len(b.content)) }

func (b *cursorCountingBody) Close() error { return nil }

func TestGPTResponseChecksSelectedContentWithOneForwardReader(t *testing.T) {
	body := &cursorCountingBody{content: `{"content":[{"text":"first","type":"text"},{"type":"text","text":"second"},{"text":"third","type":"text"}],"type":"message","stop_reason":"end_turn","stop_sequence":null}`}
	spec, err := bodyfile.ResponseScanSpec(
		"/type", "/content", "/content/*", "/content/*/type", "/content/*/text",
		"/stop_reason", "/stop_sequence",
	)
	if err != nil {
		t.Fatal(err)
	}
	index, err := bodyfile.IndexSelective(body, spec)
	if err != nil {
		t.Fatal(err)
	}
	body.openCount.Store(0)
	response := NewMutableResponse(200, body, index, NewHTTPHeaderSet(nil))
	patch := &gptClassifierResponsePatch{stopSequences: []string{"STOP"}}
	if err := patch.ApplyResponse(PatchContext{}, response); err != nil {
		t.Fatal(err)
	}
	if got := body.openCount.Load(); got != 1 {
		t.Fatalf("response content inspection opened body %d times, want one forward reader", got)
	}
}
