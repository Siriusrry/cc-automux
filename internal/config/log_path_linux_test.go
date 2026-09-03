//go:build !darwin && !windows

package config

import (
	"path/filepath"
	"testing"
)

func TestLogDirLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "cc-automux"); got != want {
		t.Fatalf("LogDir() = %q, want %q", got, want)
	}

	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	got, err = LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "cc-automux"); got != want {
		t.Fatalf("LogDir() with XDG_STATE_HOME = %q, want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "relative")
	if _, err := LogDir(); err == nil {
		t.Fatal("relative XDG_STATE_HOME was accepted")
	}
}
