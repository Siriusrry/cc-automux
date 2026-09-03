package logs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func summarizeLine(t *testing.T, line string) Record {
	t.Helper()
	record, err := ParseRecord([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Summarize(record)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func decodeFields(t *testing.T, record Record) map[string]json.RawMessage {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(record.Bytes(), &fields); err != nil {
		t.Fatalf("summary is not a JSON object: %v", err)
	}
	return fields
}

func decodeString(t *testing.T, value json.RawMessage) string {
	t.Helper()
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func referenceOf(t *testing.T, record Record) string {
	t.Helper()
	fields := decodeFields(t, record)
	value, ok := fields[referenceFieldName]
	if !ok {
		t.Fatal("summary is missing its reference")
	}
	return decodeString(t, value)
}

func TestSummarizeLeavesOrdinaryRecordsByteIdentical(t *testing.T) {
	for name, line := range map[string]string{
		"forward": `{"time":"2026-09-03T12:00:00Z","level":"INFO","msg":"gateway","seq":1,"kind":"forward","model":"m","attempt":1}`,
		"success": `{"time":"2026-09-03T12:00:00Z","level":"INFO","msg":"gateway","seq":2,"kind":"success","http_status":200}`,
		"failure": `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":3,"kind":"failure","raw_error":"rate limited"}`,
		"service": `{"time":"2026-09-03T12:00:00Z","level":"WARN","msg":"service","seq":4,"event":"restart_failed","error":"exec failed"}`,
		"at limit": `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":5,"kind":"failure","raw_error":"` +
			strings.Repeat("x", MaximumFieldBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			original, err := ParseRecord([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			summary, err := Summarize(original)
			if err != nil {
				t.Fatal(err)
			}
			if string(summary.Bytes()) != line {
				t.Fatalf("record was rewritten:\n got %s\nwant %s", summary.Bytes(), line)
			}
			if strings.Contains(string(summary.Bytes()), truncatedFieldName) {
				t.Fatal("untruncated record carries a marker")
			}
		})
	}
}

func TestSummarizeBoundsOversizedFieldsAndAddsMarker(t *testing.T) {
	body := strings.Repeat("x", MaximumFieldBytes+500)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	summary := summarizeLine(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+
		`","level":"ERROR","msg":"gateway","seq":9,"kind":"failure","http_status":503,"raw_error":`+string(encoded)+`}`)

	fields := decodeFields(t, summary)
	if got := decodeString(t, fields["raw_error"]); len(got) != MaximumFieldBytes {
		t.Fatalf("bounded field is %d bytes, want %d", len(got), MaximumFieldBytes)
	}
	// Fields that were already bounded keep their exact values.
	if decodeString(t, fields["kind"]) != "failure" || string(fields["http_status"]) != "503" {
		t.Fatalf("bounded fields changed: %s", summary.Bytes())
	}
	var limits map[string]int
	if err := json.Unmarshal(fields[truncatedFieldName], &limits); err != nil {
		t.Fatal(err)
	}
	if limits["raw_error"] != MaximumFieldBytes+500 {
		t.Fatalf("original length = %d", limits["raw_error"])
	}
	if len(limits) != 1 {
		t.Fatalf("marker names %d fields", len(limits))
	}
	if reference := referenceOf(t, summary); reference != EncodeReference(timestamp, 9) {
		t.Fatalf("reference = %q", reference)
	}
	// Ordering and identity fields survive so the summary still filters, sorts
	// and paginates exactly like the persisted record.
	if summary.Seq != 9 || !summary.Time.Equal(timestamp) {
		t.Fatalf("identity changed: seq %d, time %s", summary.Seq, summary.Time)
	}
}

func TestSummarizeBoundsEveryOversizedFieldIncludingServiceErrors(t *testing.T) {
	long, err := json.Marshal(strings.Repeat("z", MaximumFieldBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeLine(t, `{"time":"2026-09-03T12:00:00Z","level":"WARN","msg":"service","seq":4,`+
		`"event":"pending_rejected","error":`+string(long)+`}`)
	var limits map[string]int
	if err := json.Unmarshal(decodeFields(t, summary)[truncatedFieldName], &limits); err != nil {
		t.Fatal(err)
	}
	if limits["error"] != MaximumFieldBytes+1 {
		t.Fatalf("service error was not bounded: %v", limits)
	}

	// Two oversized fields on one record are both named by the single marker.
	both := summarizeLine(t, `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":5,`+
		`"kind":"failure","raw_error":`+string(long)+`,"upstream_url":`+string(long)+`}`)
	limits = nil
	if err := json.Unmarshal(decodeFields(t, both)[truncatedFieldName], &limits); err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits["raw_error"] == 0 || limits["upstream_url"] == 0 {
		t.Fatalf("marker = %v", limits)
	}
}

func TestSummarizeKeepsCharacterBoundariesAndReplacesInheritedMarkers(t *testing.T) {
	// A multi-byte character straddles the limit, so the boundary must move back
	// rather than emit half a character.
	body := strings.Repeat("a", MaximumFieldBytes-1) + "世" + strings.Repeat("b", 100)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeLine(t, `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":6,`+
		`"kind":"failure","raw_error":`+string(encoded)+`}`)
	kept := decodeString(t, decodeFields(t, summary)["raw_error"])
	if len(kept) != MaximumFieldBytes-1 {
		t.Fatalf("kept %d bytes, want %d", len(kept), MaximumFieldBytes-1)
	}
	if strings.ContainsRune(kept, '�') || !strings.HasSuffix(kept, "a") {
		t.Fatal("truncation split a character")
	}

	// An upstream body is not required to be valid UTF-8; a partial sequence at
	// the boundary is dropped and the rest is preserved as received.
	invalid := strings.Repeat("c", MaximumFieldBytes-1) + "\xe4\xb8\x96" + "d"
	encoded, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	summary = summarizeLine(t, `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":7,`+
		`"kind":"failure","raw_error":`+string(encoded)+`}`)
	if got := len(decodeString(t, decodeFields(t, summary)["raw_error"])); got != MaximumFieldBytes-1 {
		t.Fatalf("invalid UTF-8 kept %d bytes", got)
	}

	// A record that already contains these names must not end up with two
	// copies; the server-produced values win.
	inherited := summarizeLine(t, `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":8,`+
		`"kind":"failure","truncated":{"spoofed":1},"ref":"spoofed","raw_error":`+string(encoded)+`}`)
	fields := decodeFields(t, inherited)
	var limits map[string]int
	if err := json.Unmarshal(fields[truncatedFieldName], &limits); err != nil {
		t.Fatal(err)
	}
	if _, spoofed := limits["spoofed"]; spoofed || len(limits) != 1 {
		t.Fatalf("inherited marker survived: %v", limits)
	}
	if referenceOf(t, inherited) == "spoofed" {
		t.Fatal("inherited reference survived")
	}
	if strings.Count(string(inherited.Bytes()), `"ref":`) != 1 ||
		strings.Count(string(inherited.Bytes()), `"truncated":`) != 1 {
		t.Fatalf("duplicate marker: %s", inherited.Bytes())
	}
}

func TestSummarizeRejectsNonObjectRecords(t *testing.T) {
	oversized := strings.Repeat("x", MaximumFieldBytes+1)
	record := Record{Raw: []byte(`["` + oversized + `"]`)}
	if _, err := Summarize(record); err == nil {
		t.Fatal("non-object record was summarized")
	}
}
