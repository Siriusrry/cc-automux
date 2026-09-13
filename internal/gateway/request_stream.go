package gateway

import (
	"errors"
	"io"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

// requestStream reads only the final top-level stream value from the ingress
// index. Other types remain untouched for the upstream to validate.
func requestStream(body bodyfile.Body, index bodyfile.JSONIndex) (stream bool, err error) {
	fields := index.Find("/stream")
	if len(fields) == 0 {
		return false, nil
	}
	last := fields[0]
	for _, field := range fields[1:] {
		if field.ValueRange.Start > last.ValueRange.Start {
			last = field
		}
	}
	if last.Type != bodyfile.JSONBool || last.ValueRange.Len() != 4 {
		return false, nil
	}
	reader, err := body.OpenReader()
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	if _, err := io.CopyN(io.Discard, reader, last.ValueRange.Start); err != nil {
		return false, err
	}
	var value [4]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return false, err
	}
	return string(value[:]) == "true", nil
}
