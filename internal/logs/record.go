package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	ActiveFileName  = "cc-automux.log"
	ArchiveFileName = "cc-automux.log.1"
)

var (
	validLevels = map[string]struct{}{
		"INFO":  {},
		"WARN":  {},
		"ERROR": {},
	}
	validMessages = map[string]struct{}{
		"gateway": {},
		"service": {},
	}
)

// Record keeps the original JSON object alongside the fields needed for
// filtering, ordering and cursor generation. Unknown event fields remain in
// Raw and are returned to management clients unchanged in meaning.
type Record struct {
	Time       time.Time
	Seq        uint64
	Raw        json.RawMessage
	stringData map[string]string
	status     int
	hasStatus  bool
}

func ParseRecord(line []byte) (Record, error) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	trimmed := bytes.TrimSpace(line)
	var fields map[string]json.RawMessage
	if len(trimmed) == 0 || !json.Valid(trimmed) || trimmed[0] != '{' {
		return Record{}, errors.New("log line is not a JSON object")
	}
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return Record{}, errors.New("log line is not a JSON object")
	}

	var timestamp string
	if err := json.Unmarshal(fields["time"], &timestamp); err != nil || timestamp == "" {
		return Record{}, errors.New("log time is missing or invalid")
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return Record{}, fmt.Errorf("parse log time: %w", err)
	}
	var seq uint64
	if err := json.Unmarshal(fields["seq"], &seq); err != nil {
		return Record{}, errors.New("log seq is missing or invalid")
	}
	var level, message string
	if err := json.Unmarshal(fields["level"], &level); err != nil {
		return Record{}, errors.New("log level is missing or invalid")
	}
	if _, ok := validLevels[level]; !ok {
		return Record{}, errors.New("log level is invalid")
	}
	if err := json.Unmarshal(fields["msg"], &message); err != nil {
		return Record{}, errors.New("log msg is missing or invalid")
	}
	if _, ok := validMessages[message]; !ok {
		return Record{}, errors.New("log msg is invalid")
	}

	record := Record{
		Time:       parsedTime,
		Seq:        seq,
		Raw:        append(json.RawMessage(nil), line...),
		stringData: make(map[string]string),
	}
	for name := range stringFilterNames {
		value, ok := fields[name]
		if !ok {
			continue
		}
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return Record{}, fmt.Errorf("log %s is invalid", name)
		}
		record.stringData[name] = decoded
	}
	if value, ok := fields["http_status"]; ok {
		if err := json.Unmarshal(value, &record.status); err != nil {
			return Record{}, errors.New("log http_status is invalid")
		}
		record.hasStatus = true
	}
	return record, nil
}

func (r Record) MarshalJSON() ([]byte, error) {
	if !json.Valid(r.Raw) {
		return nil, errors.New("invalid raw log record")
	}
	return append([]byte(nil), r.Raw...), nil
}

// Bytes returns a defensive copy of the JSON line without its line terminator.
func (r Record) Bytes() []byte { return append([]byte(nil), r.Raw...) }

func comparePosition(leftTime time.Time, leftSeq uint64, rightTime time.Time, rightSeq uint64) int {
	if leftTime.Before(rightTime) {
		return -1
	}
	if leftTime.After(rightTime) {
		return 1
	}
	if leftSeq < rightSeq {
		return -1
	}
	if leftSeq > rightSeq {
		return 1
	}
	return 0
}
