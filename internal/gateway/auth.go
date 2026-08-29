package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"unicode"
)

func authorized(r *http.Request, key string) bool {
	values := headerValuesFold(r.Header, "Authorization")
	if len(values) != 1 {
		return false
	}
	token, ok := parseBearerAuthorization(values[0])
	if !ok {
		return false
	}
	want := sha256.Sum256([]byte(key))
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1 && token == key
}

func headerValuesFold(headers http.Header, target string) []string {
	var values []string
	for name, entries := range headers {
		if strings.EqualFold(name, target) {
			values = append(values, entries...)
		}
	}
	return values
}

func parseBearerAuthorization(value string) (string, bool) {
	const scheme = "Bearer"
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) || value[len(scheme)] != ' ' {
		return "", false
	}
	token := value[len(scheme)+1:]
	if token == "" || strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", false
	}
	return token, true
}
