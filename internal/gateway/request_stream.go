package gateway

import "github.com/Siriusrry/cc-automux/internal/bodyfile"

// requestStream reads only the final top-level stream value from the ingress
// index. Other types remain untouched for the upstream to validate.
func requestStream(index bodyfile.JSONIndex) bool {
	fields := index.Find("/stream")
	if len(fields) == 0 {
		return false
	}
	last := fields[0]
	for _, field := range fields[1:] {
		if field.ValueRange.Start > last.ValueRange.Start {
			last = field
		}
	}
	value, _ := last.BoolValue()
	return value
}
