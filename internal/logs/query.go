package logs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultLimit = 200
	MaximumLimit = 1000
)

var stringFilterNames = map[string]struct{}{
	"level":         {},
	"msg":           {},
	"kind":          {},
	"event":         {},
	"request_type":  {},
	"provider_id":   {},
	"provider_name": {},
	"model":         {},
	"session_id":    {},
	"patch_id":      {},
	"error_code":    {},
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

type Cursor struct {
	Time time.Time
	Seq  uint64
}

func EncodeCursor(timestamp time.Time, seq uint64) string {
	return encodePosition(timestamp, seq)
}

func DecodeCursor(value string) (Cursor, error) {
	return decodePosition(value, "cursor")
}

// EncodeReference identifies one record for later complete retrieval. It shares
// the cursor encoding because both name a position by time and sequence, and it
// stays opaque so the identity can gain another component without a client
// change.
func EncodeReference(timestamp time.Time, seq uint64) string {
	return encodePosition(timestamp, seq)
}

func DecodeReference(value string) (Cursor, error) {
	return decodePosition(value, "ref")
}

func encodePosition(timestamp time.Time, seq uint64) string {
	value := timestamp.Format(time.RFC3339Nano) + "|" + strconv.FormatUint(seq, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodePosition(value, field string) (Cursor, error) {
	invalid := &ValidationError{Field: field, Message: "must be an unmodified " + field + " returned by the server"}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, invalid
	}
	text := string(decoded)
	if strings.Count(text, "|") != 1 {
		return Cursor{}, invalid
	}
	timeText, seqText, _ := strings.Cut(text, "|")
	timestamp, err := time.Parse(time.RFC3339Nano, timeText)
	if err != nil {
		return Cursor{}, invalid
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil {
		return Cursor{}, invalid
	}
	return Cursor{Time: timestamp, Seq: seq}, nil
}

type Filter struct {
	strings  map[string]map[string]struct{}
	statuses map[int]struct{}
	since    *time.Time
	until    *time.Time
}

func (f Filter) Match(record Record) bool {
	if f.since != nil && record.Time.Before(*f.since) {
		return false
	}
	if f.until != nil && record.Time.After(*f.until) {
		return false
	}
	for name, allowed := range f.strings {
		value, ok := record.stringData[name]
		if !ok {
			return false
		}
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	if len(f.statuses) > 0 {
		if !record.hasStatus {
			return false
		}
		if _, ok := f.statuses[record.status]; !ok {
			return false
		}
	}
	return true
}

type Parameters struct {
	Filter Filter
	Limit  int
	Cursor *Cursor
}

type ParseOptions struct {
	History bool
}

// ParseParameters is the single parser used by history and stream endpoints.
// Stream parsing sets History=false, which rejects pagination-only controls.
func ParseParameters(values url.Values, options ParseOptions) (Parameters, error) {
	params := Parameters{
		Filter: Filter{
			strings:  make(map[string]map[string]struct{}),
			statuses: make(map[int]struct{}),
		},
		Limit: DefaultLimit,
	}
	for name, entries := range values {
		if len(entries) == 0 {
			return Parameters{}, validation(name, "must not be empty")
		}
		for _, entry := range entries {
			if entry == "" {
				return Parameters{}, validation(name, "must not be empty")
			}
		}
		switch {
		case isStringFilter(name):
			allowed := make(map[string]struct{}, len(entries))
			for _, entry := range entries {
				allowed[entry] = struct{}{}
			}
			params.Filter.strings[name] = allowed
		case name == "http_status":
			for _, entry := range entries {
				status, err := strconv.Atoi(entry)
				if err != nil {
					return Parameters{}, validation(name, "must contain integers")
				}
				params.Filter.statuses[status] = struct{}{}
			}
		case name == "since" || name == "until":
			if len(entries) != 1 {
				return Parameters{}, validation(name, "must appear exactly once")
			}
			timestamp, err := time.Parse(time.RFC3339Nano, entries[0])
			if err != nil {
				return Parameters{}, validation(name, "must be an RFC3339 timestamp")
			}
			if name == "since" {
				params.Filter.since = &timestamp
			} else {
				params.Filter.until = &timestamp
			}
		case name == "limit" && options.History:
			if len(entries) != 1 {
				return Parameters{}, validation(name, "must appear exactly once")
			}
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit <= 0 || limit > MaximumLimit {
				return Parameters{}, validation(name, fmt.Sprintf("must be an integer from 1 to %d", MaximumLimit))
			}
			params.Limit = limit
		case name == "cursor" && options.History:
			if len(entries) != 1 {
				return Parameters{}, validation(name, "must appear exactly once")
			}
			cursor, err := DecodeCursor(entries[0])
			if err != nil {
				return Parameters{}, err
			}
			params.Cursor = &cursor
		default:
			return Parameters{}, validation(name, "is not a supported parameter")
		}
	}
	if params.Filter.since != nil && params.Filter.until != nil && params.Filter.since.After(*params.Filter.until) {
		return Parameters{}, validation("since", "must not be later than until")
	}
	return params, nil
}

func validation(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

func isStringFilter(name string) bool {
	_, ok := stringFilterNames[name]
	return ok
}

func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}
