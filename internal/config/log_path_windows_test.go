//go:build windows

package config

import (
	"path/filepath"
	"testing"
)

func TestLogDirWindows(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	got, err := LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "cc-automux", "logs"); got != want {
		t.Fatalf("LogDir() = %q, want %q", got, want)
	}

	t.Setenv("LOCALAPPDATA", "")
	if _, err := LogDir(); err == nil {
		t.Fatal("missing LOCALAPPDATA was accepted")
	}
}
