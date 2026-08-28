package shim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// getLogsTail drives GET /admin/logs/tail through the full ServeHTTP path and
// decodes the response. after=="" omits the query (cursor 0); otherwise it is
// sent verbatim. It returns the decoded payload and the HTTP status so callers can
// assert both the contract and the status code.
func getLogsTail(t *testing.T, server *proxyServer, after string) (adminLogsTail, int) {
	t.Helper()
	path := "/admin/logs/tail"
	if after != "" {
		path += "?after=" + after
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var got adminLogsTail
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode /admin/logs/tail: %v", err)
		}
	}
	return got, rec.Code
}

// TestAdminLogsTailIncrementalCursor drives GET /admin/logs/tail through the full
// ServeHTTP path and asserts the {entries:[{seq,stream,ts,text}],head} contract:
// the ?after=<seq> cursor returns only newer entries (oldest-first), head advances
// as lines are appended, and the per-stream label rides through. It captures a
// baseline head first and appends known markers directly to the process log ring,
// so it is robust to lines other tests left in the shared ring (the ring backs the
// global loggers; package tests run sequentially, so no concurrent appends race
// this between the baseline and the asserts).
func TestAdminLogsTailIncrementalCursor(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")

	// Baseline: the shared ring may already hold lines from earlier tests, so
	// anchor every assertion to the current head rather than absolute seq 1.
	base, code := getLogsTail(t, server, "")
	if code != http.StatusOK {
		t.Fatalf("baseline tail status = %d, want 200", code)
	}

	logBuffer.append(logStreamStdout, []byte("tail-marker-alpha\n"))
	logBuffer.append(logStreamStderr, []byte("tail-marker-bravo\n"))

	// after=baseHead returns exactly the two new lines, in order, with the right
	// streams and consecutive seqs, and head advanced by 2.
	got, code := getLogsTail(t, server, strconv.FormatInt(base.Head, 10))
	if code != http.StatusOK {
		t.Fatalf("tail status = %d, want 200", code)
	}
	if got.Head != base.Head+2 {
		t.Fatalf("head = %d, want %d", got.Head, base.Head+2)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2 (only lines newer than the cursor): %+v", len(got.Entries), got.Entries)
	}
	if got.Entries[0].Seq != base.Head+1 || got.Entries[1].Seq != base.Head+2 {
		t.Fatalf("seqs = %d,%d, want %d,%d", got.Entries[0].Seq, got.Entries[1].Seq, base.Head+1, base.Head+2)
	}
	if got.Entries[0].Stream != logStreamStdout || got.Entries[0].Text != "tail-marker-alpha" {
		t.Fatalf("entries[0] = {stream:%q text:%q}, want stdout/alpha", got.Entries[0].Stream, got.Entries[0].Text)
	}
	if got.Entries[1].Stream != logStreamStderr || got.Entries[1].Text != "tail-marker-bravo" {
		t.Fatalf("entries[1] = {stream:%q text:%q}, want stderr/bravo", got.Entries[1].Stream, got.Entries[1].Text)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.Entries[0].TS); err != nil {
		t.Fatalf("entry ts %q not RFC3339Nano: %v", got.Entries[0].TS, err)
	}

	// Polling again at the new head returns no entries and the same head: the
	// cursor already delivered everything.
	drained, code := getLogsTail(t, server, strconv.FormatInt(got.Head, 10))
	if code != http.StatusOK {
		t.Fatalf("drained tail status = %d, want 200", code)
	}
	if drained.Head != got.Head || len(drained.Entries) != 0 {
		t.Fatalf("drained = {head:%d entries:%d}, want {head:%d entries:0}", drained.Head, len(drained.Entries), got.Head)
	}

	// A new line shows up incrementally at the previous head.
	logBuffer.append(logStreamStdout, []byte("tail-marker-charlie\n"))
	inc, _ := getLogsTail(t, server, strconv.FormatInt(got.Head, 10))
	if inc.Head != got.Head+1 || len(inc.Entries) != 1 || inc.Entries[0].Text != "tail-marker-charlie" {
		t.Fatalf("incremental = {head:%d entries:%+v}, want exactly one new charlie line", inc.Head, inc.Entries)
	}
}

// TestAdminLogsTailWireContract asserts the JSON wire shape: top-level entries +
// head keys, entry-level seq/stream/ts/text keys, and the null/[] contract —
// entries serializes as a JSON array, including [] (never null) when the cursor
// has drained the ring.
func TestAdminLogsTailWireContract(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")
	logBuffer.append(logStreamStdout, []byte("wire-contract-line\n"))

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/logs/tail", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, k := range []string{"entries", "head"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("tail JSON missing top-level key %q", k)
		}
	}
	if s := strings.TrimSpace(string(raw["entries"])); !strings.HasPrefix(s, "[") {
		t.Fatalf("entries JSON = %s, want an array (never null)", s)
	}

	var nested struct {
		Entries []map[string]json.RawMessage `json:"entries"`
		Head    int64                        `json:"head"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nested); err != nil {
		t.Fatalf("decode nested: %v", err)
	}
	if len(nested.Entries) == 0 {
		t.Fatal("entries empty, want at least the appended line")
	}
	for _, k := range []string{"seq", "stream", "ts", "text"} {
		if _, ok := nested.Entries[0][k]; !ok {
			t.Fatalf("entry JSON missing key %q", k)
		}
	}

	// Drained cursor still serializes entries as [], never null.
	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/admin/logs/tail?after="+strconv.FormatInt(nested.Head, 10), nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("drained status = %d, want 200", rec2.Code)
	}
	var raw2 map[string]json.RawMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &raw2); err != nil {
		t.Fatalf("decode drained: %v", err)
	}
	if got := strings.TrimSpace(string(raw2["entries"])); got != "[]" {
		t.Fatalf("drained entries JSON = %s, want [] (never null)", got)
	}
}

// TestAdminLogsTailRejectsNonGetAndBadCursor pins the desk contract: only GET is
// allowed (405 + Allow: GET otherwise), and a present-but-unparseable cursor is a
// 400 while a missing/empty cursor is accepted as 0.
func TestAdminLogsTailRejectsNonGetAndBadCursor(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(method, "/admin/logs/tail", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s Allow = %q, want %q", method, allow, http.MethodGet)
		}
	}

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/logs/tail?after=not-an-int", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("after=not-an-int status = %d, want 400", rec.Code)
	}

	// A missing cursor is accepted (treated as 0).
	if _, code := getLogsTail(t, server, ""); code != http.StatusOK {
		t.Fatalf("missing cursor status = %d, want 200", code)
	}
}
