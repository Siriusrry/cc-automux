package textlimit

import "testing"

func TestPrefixPreservesUTF8Boundaries(t *testing.T) {
	for _, test := range []struct {
		value     string
		limit     int
		want      string
		truncated bool
	}{
		{"a界b", 2, "a", true}, {"a界b", 4, "a界", true}, {"a界", 4, "a界", false},
		{"a\xe7\x95", 3, "a", true}, {"abc", 0, "", true}, {"", 0, "", false},
	} {
		value, truncated := Prefix(test.value, test.limit)
		if value != test.want || truncated != test.truncated {
			t.Fatalf("Prefix(%q,%d)=%q,%v", test.value, test.limit, value, truncated)
		}
	}
}
