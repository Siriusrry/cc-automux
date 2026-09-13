// Package textlimit bounds diagnostic text without splitting UTF-8 characters.
package textlimit

import (
	"strings"
	"unicode/utf8"
)

const DiagnosticBytes = 8 * 1024

func Prefix(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	end := limit
	for i := end - 1; i >= 0 && i >= end-utf8.UTFMax; i-- {
		if utf8.RuneStart(value[i]) {
			if !utf8.FullRuneInString(value[i:end]) {
				end = i
			}
			break
		}
	}
	return strings.Clone(value[:end]), true
}
