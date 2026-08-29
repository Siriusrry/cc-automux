package gateway

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestBearerAuthorizationIsStrict(t *testing.T) {
	for _, value := range []string{
		"",
		"Basic gateway-key",
		"Bearer",
		"Bearer ",
		"Bearer\tgateway-key",
		"Bearer  gateway-key",
		" Bearer gateway-key",
		"Bearer gateway-key ",
	} {
		request := httptest.NewRequest("POST", MessagesPath, nil)
		if value != "" {
			request.Header.Set("Authorization", value)
		}
		if authorized(request, "gateway-key") {
			t.Fatalf("authorized malformed value %q", value)
		}
	}
	request := httptest.NewRequest("POST", MessagesPath, nil)
	request.Header.Set("Authorization", "bEaReR gateway-key")
	if !authorized(request, "gateway-key") {
		t.Fatal("case-insensitive Bearer scheme was rejected")
	}
	request.Header.Add("Authorization", "Bearer gateway-key")
	if authorized(request, "gateway-key") {
		t.Fatal("multiple Authorization values were accepted")
	}
}

func TestReplayBodyIsUnlinkedAndRepeatable(t *testing.T) {
	directory := t.TempDir()
	original := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	replay, err := spoolRequestBody(bytes.NewReader(original), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replay path remained visible: %v", entries)
	}
	if replay.Size() != int64(len(original)) {
		t.Fatalf("size = %d, want %d", replay.Size(), len(original))
	}
	for attempt := 0; attempt < 3; attempt++ {
		reader, err := replay.Reader()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("attempt %d replay changed", attempt+1)
		}
	}
	info, err := replay.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replay mode = %o", info.Mode().Perm())
	}
}

func TestMessagesParserExtractsOnlyModel(t *testing.T) {
	input := `{
		"messages":[{"role":"user","content":[{"type":"text","text":"large-independent-value"}]}],
		"metad\u0061ta":{"ignored":{"nested":[true,false,null,-12.5e+2]},"user\u005fid":" session-raw "},
		"mo\u0064el":"Model-X",
		"unknown":"preserved upstream"
	}`
	fields, err := parseMessagesRequest(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if fields.model != "Model-X" {
		t.Fatalf("fields = %#v", fields)
	}
}

func TestMessagesParserRejectsInvalidModelAndJSON(t *testing.T) {
	tests := []string{
		``,
		`[]`,
		`{}`,
		`{"model":""}`,
		`{"model":1}`,
		`{"model":"a","model":"b"}`,
		`{"model":"a"} {"model":"b"}`,
		`{"model":"a","x":[1,]}`,
		`{"model":"a","x":"\q"}`,
	}
	for _, input := range tests {
		if _, err := parseMessagesRequest(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid request %q", input)
		}
	}
}

func TestMessagesParserSkipsLargeUnknownStringWithoutRetainingIt(t *testing.T) {
	const payloadSize = 8 << 20
	reader := io.MultiReader(
		strings.NewReader(`{"model":"m","messages":[{"content":"`),
		&fixedReader{remaining: payloadSize, value: 'x'},
		strings.NewReader(`"}]}`),
	)
	fields, err := parseMessagesRequest(reader)
	if err != nil {
		t.Fatal(err)
	}
	if fields.model != "m" {
		t.Fatalf("model = %q", fields.model)
	}
}

type fixedReader struct {
	remaining int
	value     byte
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = r.value
	}
	r.remaining -= len(p)
	return len(p), nil
}
