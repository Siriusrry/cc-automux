package version

import (
	"regexp"
	"testing"
)

func TestCurrentVersion(t *testing.T) {
	if got, want := embedded, "v1.0.1\n"; got != want {
		t.Fatalf("embedded VERSION bytes = %q, want %q", got, want)
	}
	if got, want := Current(), "v1.0.1"; got != want {
		t.Fatalf("Current() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`).MatchString(Current()) {
		t.Fatalf("Current() = %q, want a semantic version", Current())
	}
	if got, want := Display(), "CC AutoMux v1.0.1"; got != want {
		t.Fatalf("Display() = %q, want %q", got, want)
	}
}
