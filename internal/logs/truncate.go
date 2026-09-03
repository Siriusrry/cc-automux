package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

// MaximumFieldBytes bounds any single string field in an interface response.
// The file keeps every record complete; both the history and the stream return
// summaries so one upstream error body cannot dominate a page or a connection.
const MaximumFieldBytes = 8 * 1024

// A summarized record carries "truncated" (field name to original byte count)
// so a client can show how much is withheld, and "ref" so it can fetch the
// complete record. Neither name is ever emitted by a log call site.
const (
	truncatedFieldName = "truncated"
	referenceFieldName = "ref"
)

// Summarize bounds every oversized string field and adds the truncation marker
// plus the reference needed to fetch the complete record. Records with no
// oversized field are returned untouched, so the common path stays byte
// identical to the persisted line and costs nothing.
func Summarize(record Record) (Record, error) {
	if len(record.Raw) <= MaximumFieldBytes {
		return record, nil
	}
	summary, truncated, err := summarizeFields(record)
	if err != nil {
		return record, err
	}
	if !truncated {
		return record, nil
	}
	record.Raw = summary
	return record, nil
}

func summarizeFields(record Record) ([]byte, bool, error) {
	var fields [][]byte
	decoder := json.NewDecoder(bytes.NewReader(record.Raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, false, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, false, errors.New("log record is not a JSON object")
	}

	limits := make([][]byte, 0, 2)
	truncated := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, errors.New("log record has a non-string field name")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, err
		}
		// A record produced by this process never carries these names. Dropping
		// any inherited copy keeps the emitted marker unambiguous.
		if key == truncatedFieldName || key == referenceFieldName {
			continue
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, false, err
		}
		shortened, original, ok, err := shortenString(value)
		if err != nil {
			return nil, false, err
		}
		if ok {
			truncated = true
			value = shortened
			limits = append(limits, encodeField(encodedKey, []byte(strconv.Itoa(original))))
		}
		fields = append(fields, encodeField(encodedKey, value))
	}
	if !truncated {
		return nil, false, nil
	}

	marker := append([]byte{'{'}, bytes.Join(limits, []byte{','})...)
	marker = append(marker, '}')
	fields = append(fields, encodeField([]byte(`"`+truncatedFieldName+`"`), marker))
	fields = append(fields, encodeField(
		[]byte(`"`+referenceFieldName+`"`),
		[]byte(strconv.Quote(EncodeReference(record.Time, record.Seq))),
	))

	summary := append([]byte{'{'}, bytes.Join(fields, []byte{','})...)
	summary = append(summary, '}')
	if !json.Valid(summary) {
		return nil, false, errors.New("log record summary is not valid JSON")
	}
	return summary, true, nil
}

func encodeField(name, value []byte) []byte {
	field := make([]byte, 0, len(name)+1+len(value))
	field = append(field, name...)
	field = append(field, ':')
	return append(field, value...)
}

// shortenString reports the bounded replacement for one oversized JSON string
// value. Non-string values and short strings are left alone.
func shortenString(value json.RawMessage) (json.RawMessage, int, bool, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil, 0, false, nil
	}
	var decoded string
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil, 0, false, err
	}
	if len(decoded) <= MaximumFieldBytes {
		return nil, 0, false, nil
	}
	kept := truncateUTF8(decoded, MaximumFieldBytes)
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, 0, false, err
	}
	return encoded, len(decoded), true, nil
}

// truncateUTF8 keeps the longest prefix within limit that does not split a
// multi-byte character. Only continuation bytes are walked back, so an upstream
// body that is not valid UTF-8 loses at most the final partial sequence.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for i := 0; i < utf8.UTFMax-1 && cut > 0; i++ {
		if value[cut]&0xC0 != 0x80 {
			break
		}
		cut--
	}
	return value[:cut]
}
