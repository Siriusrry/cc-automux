//go:build darwin

package config

import (
	"path/filepath"
	"testing"
)

func TestLogDirDarwin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Library", "Logs", "cc-automux"); got != want {
		t.Fatalf("LogDir() = %q, want %q", got, want)
	}
}
