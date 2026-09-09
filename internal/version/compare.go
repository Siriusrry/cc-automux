package version

import (
	"fmt"
	"regexp"
	"strings"
)

var semanticVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// Compare returns -1, 0 or 1 according to semantic version precedence.
// Build metadata has no precedence; arbitrarily large numeric parts are safe.
func Compare(a, b string) (int, error) {
	left, right := semanticVersion.FindStringSubmatch(a), semanticVersion.FindStringSubmatch(b)
	for i, parts := range [][]string{left, right} {
		if parts == nil {
			return 0, fmt.Errorf("unrecognized version %q", []string{a, b}[i])
		}
		for _, part := range strings.Split(parts[4], ".") {
			if numeric(part) && len(part) > 1 && part[0] == '0' {
				return 0, fmt.Errorf("invalid numeric prerelease %q", part)
			}
		}
	}
	for i := 1; i <= 3; i++ {
		if compared := compareNumber(left[i], right[i]); compared != 0 {
			return compared, nil
		}
	}
	if left[4] == right[4] {
		return 0, nil
	}
	if left[4] == "" {
		return 1, nil
	}
	if right[4] == "" {
		return -1, nil
	}
	x, y := strings.Split(left[4], "."), strings.Split(right[4], ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		xnum, ynum := numeric(x[i]), numeric(y[i])
		if xnum && ynum {
			return compareNumber(x[i], y[i]), nil
		}
		if xnum {
			return -1, nil
		}
		if ynum {
			return 1, nil
		}
		return strings.Compare(x[i], y[i]), nil
	}
	if len(x) < len(y) {
		return -1, nil
	}
	return 1, nil
}

func numeric(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func compareNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
