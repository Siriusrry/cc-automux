package logs

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func mustRecord(t *testing.T, line string) Record {
	t.Helper()
	record, err := ParseRecord([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestParseParametersAndFilterMatching(t *testing.T) {
	timestamp := time.Date(2026, 9, 3, 12, 0, 0, 123, time.FixedZone("offset", 8*60*60))
	cursorText := EncodeCursor(timestamp, 42)
	values := url.Values{
		"level":         {"ERROR", "WARN"},
		"msg":           {"gateway"},
		"kind":          {"failure"},
		"request_type":  {"normal"},
		"provider_id":   {"provider-id"},
		"provider_name": {"Provider"},
		"model":         {"model-a"},
		"session_id":    {"session-a"},
		"patch_id":      {"patch-a"},
		"http_status":   {"429", "503"},
		"since":         {timestamp.Format(time.RFC3339Nano)},
		"until":         {timestamp.Format(time.RFC3339Nano)},
		"limit":         {"17"},
		"cursor":        {cursorText},
	}
	parameters, err := ParseParameters(values, ParseOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	if parameters.Limit != 17 || parameters.Cursor == nil || !parameters.Cursor.Time.Equal(timestamp) || parameters.Cursor.Seq != 42 {
		t.Fatalf("parameters = %#v", parameters)
	}
	record := mustRecord(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+`","level":"ERROR","msg":"gateway","seq":7,"kind":"failure","request_type":"normal","provider_id":"provider-id","provider_name":"Provider","model":"model-a","session_id":"session-a","patch_id":"patch-a","http_status":503}`)
	if !parameters.Filter.Match(record) {
		t.Fatal("matching record was filtered out")
	}
	wrong := mustRecord(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+`","level":"INFO","msg":"gateway","seq":8,"kind":"failure","request_type":"normal","provider_id":"provider-id","provider_name":"Provider","model":"model-a","session_id":"session-a","patch_id":"patch-a","http_status":503}`)
	if parameters.Filter.Match(wrong) {
		t.Fatal("cross-field AND was not applied")
	}

	defaults, err := ParseParameters(nil, ParseOptions{History: true})
	if err != nil || defaults.Limit != DefaultLimit || !defaults.Filter.Match(record) {
		t.Fatalf("default parameters = %#v, %v", defaults, err)
	}
}

func TestTimeFilterIsInclusive(t *testing.T) {
	timestamp := time.Date(2026, 9, 3, 12, 0, 0, 123456789, time.UTC)
	values := url.Values{"since": {timestamp.Format(time.RFC3339Nano)}, "until": {timestamp.Format(time.RFC3339Nano)}}
	parameters, err := ParseParameters(values, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	record := mustRecord(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+`","level":"INFO","msg":"service","seq":1,"event":"listening"}`)
	if !parameters.Filter.Match(record) {
		t.Fatal("record at inclusive time endpoints was filtered out")
	}
}

func TestServiceEventFilter(t *testing.T) {
	timestamp := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	parameters, err := ParseParameters(url.Values{"event": {"listening"}, "msg": {"service"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	matching := mustRecord(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+`","level":"INFO","msg":"service","seq":1,"event":"listening"}`)
	other := mustRecord(t, `{"time":"`+timestamp.Format(time.RFC3339Nano)+`","level":"WARN","msg":"service","seq":2,"event":"restart_failed"}`)
	if !parameters.Filter.Match(matching) || parameters.Filter.Match(other) {
		t.Fatal("service event filter did not match exactly")
	}
}

func TestEachEqualityFilter(t *testing.T) {
	timestamp := "2026-09-03T12:00:00Z"
	gateway := mustRecord(t, `{"time":"`+timestamp+`","level":"ERROR","msg":"gateway","seq":1,"kind":"failure","request_type":"normal","provider_id":"provider-id","provider_name":"Provider","model":"model-a","session_id":"session-a","patch_id":"patch-a","http_status":503}`)
	service := mustRecord(t, `{"time":"`+timestamp+`","level":"WARN","msg":"service","seq":2,"event":"restart_failed"}`)
	tests := []struct {
		name   string
		value  string
		record Record
	}{
		{"level", "ERROR", gateway},
		{"msg", "gateway", gateway},
		{"kind", "failure", gateway},
		{"event", "restart_failed", service},
		{"request_type", "normal", gateway},
		{"provider_id", "provider-id", gateway},
		{"provider_name", "Provider", gateway},
		{"model", "model-a", gateway},
		{"session_id", "session-a", gateway},
		{"patch_id", "patch-a", gateway},
		{"http_status", "503", gateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters, err := ParseParameters(url.Values{test.name: {test.value}}, ParseOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !parameters.Filter.Match(test.record) {
				t.Fatalf("%s=%q did not match", test.name, test.value)
			}
			different := test.value + "-different"
			if test.name == "http_status" {
				different = "504"
			}
			parameters, err = ParseParameters(url.Values{test.name: {different}}, ParseOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if parameters.Filter.Match(test.record) {
				t.Fatalf("different %s matched", test.name)
			}
		})
	}
}

func TestParameterValidation(t *testing.T) {
	validCursor := EncodeCursor(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), 1)
	tests := []url.Values{
		{"unknown": {"value"}},
		{"level": {""}},
		{"http_status": {"not-an-integer"}},
		{"since": {"2026-09-03"}},
		{"until": {"yesterday"}},
		{"since": {"2026-09-03T00:00:01Z"}, "until": {"2026-09-03T00:00:00Z"}},
		{"since": {"2026-09-03T00:00:00Z", "2026-09-03T00:00:01Z"}},
		{"limit": {"0"}},
		{"limit": {strconv.Itoa(MaximumLimit + 1)}},
		{"limit": {"invalid"}},
		{"limit": {"1", "2"}},
		{"cursor": {"not-base64!"}},
		{"cursor": {base64.URLEncoding.EncodeToString([]byte("2026-09-03T00:00:00Z|1"))}},
		{"cursor": {base64.RawURLEncoding.EncodeToString([]byte("bad-time|1"))}},
		{"cursor": {base64.RawURLEncoding.EncodeToString([]byte("2026-09-03T00:00:00Z|bad-seq"))}},
		{"cursor": {base64.RawURLEncoding.EncodeToString([]byte("2026-09-03T00:00:00Z|1|extra"))}},
		{"cursor": {validCursor, validCursor}},
	}
	for _, values := range tests {
		if _, err := ParseParameters(values, ParseOptions{History: true}); !IsValidationError(err) {
			t.Errorf("ParseParameters(%v) error = %v", values, err)
		}
	}
	for _, values := range []url.Values{{"limit": {"1"}}, {"cursor": {validCursor}}} {
		if _, err := ParseParameters(values, ParseOptions{History: false}); !IsValidationError(err) {
			t.Errorf("stream ParseParameters(%v) error = %v", values, err)
		}
	}
}

func TestCursorEncodingIsUnpaddedBase64URL(t *testing.T) {
	timestamp := time.Date(2026, 9, 3, 12, 34, 56, 789, time.FixedZone("offset", -7*60*60))
	encoded := EncodeCursor(timestamp, 123)
	if _, err := base64.RawURLEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("cursor is not raw base64url: %q", encoded)
	}
	if decoded, err := DecodeCursor(encoded); err != nil || !decoded.Time.Equal(timestamp) || decoded.Seq != 123 {
		t.Fatalf("DecodeCursor() = %#v, %v", decoded, err)
	}
}

func TestParseRecordRejectsInvalidShape(t *testing.T) {
	for _, line := range []string{
		``,
		`[]`,
		`{"time":"bad","level":"INFO","msg":"service","seq":1}`,
		`{"time":"2026-09-03T00:00:00Z","level":"DEBUG","msg":"service","seq":1}`,
		`{"time":"2026-09-03T00:00:00Z","level":"INFO","msg":"other","seq":1}`,
		`{"time":"2026-09-03T00:00:00Z","level":"INFO","msg":"service","seq":1.5}`,
		`{"time":"2026-09-03T00:00:00Z","level":"INFO","msg":"service","seq":1,"event":3}`,
		`{"time":"2026-09-03T00:00:00Z","level":"INFO","msg":"gateway","seq":1,"http_status":"503"}`,
	} {
		if _, err := ParseRecord([]byte(line)); err == nil {
			t.Errorf("ParseRecord(%q) succeeded", line)
		}
	}
}
