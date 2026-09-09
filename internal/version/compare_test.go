package version

import "testing"

func TestCompare(t *testing.T) {
	for _, pair := range [][2]string{
		{"v1.0.0-dev", "v1.0.0"}, {"v1.0.1", "v1.1.0-dev"},
		{"v1.9.0", "v1.10.0"}, {"v1.0.0-2", "v1.0.0-10"},
		{"v1.0.0-10", "v1.0.0-alpha"}, {"v1.0.0-alpha", "v1.0.0-alpha.1"},
		{"v1.0.0-alpha.2", "v1.0.0-alpha.10"}, {"v1.0.0-rc.1", "v1.0.0"},
		{"v999999999999999999999.0.0", "v1000000000000000000000.0.0"},
	} {
		if got, err := Compare(pair[0], pair[1]); err != nil || got != -1 {
			t.Fatalf("Compare(%v) = %d, %v", pair, got, err)
		}
		if got, err := Compare(pair[1], pair[0]); err != nil || got != 1 {
			t.Fatalf("reverse Compare(%v) = %d, %v", pair, got, err)
		}
	}
	if got, err := Compare("v1.0.0+build.1", "v1.0.0+build.2"); err != nil || got != 0 {
		t.Fatalf("metadata precedence: %d, %v", got, err)
	}
	for _, invalid := range []string{"1.0.0", "v01.0.0", "v1.0", "v1.0.0-01", "v1.0.0-alpha..2", "v1.0.0\n"} {
		if _, err := Compare(invalid, "v1.0.0"); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
