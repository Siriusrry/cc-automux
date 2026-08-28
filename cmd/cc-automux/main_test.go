package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleArgsVersion(t *testing.T) {
	temp := t.TempDir()
	configPath := filepath.Join(temp, "config.json")
	t.Setenv("HOME", temp)
	t.Setenv("CC_AUTOMUX_CONFIG", configPath)

	var stdout, stderr bytes.Buffer
	handled, exitCode := handleArgs([]string{"--version"}, &stdout, &stderr)
	if !handled || exitCode != 0 {
		t.Fatalf("handleArgs(--version) = handled %v, exit %d", handled, exitCode)
	}
	if got, want := stdout.String(), "CC AutoMux v1.0.0-dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("--version touched config path: %v", err)
	}
}

func TestHandleArgsNoArgumentsStartsService(t *testing.T) {
	handled, exitCode := handleArgs(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if handled || exitCode != 0 {
		t.Fatalf("handleArgs(nil) = handled %v, exit %d", handled, exitCode)
	}
}

func TestHandleArgsRejectsOtherArguments(t *testing.T) {
	for _, args := range [][]string{{"-v"}, {"--help"}, {"--version", "extra"}} {
		var stdout, stderr bytes.Buffer
		handled, exitCode := handleArgs(args, &stdout, &stderr)
		if !handled || exitCode != 2 {
			t.Fatalf("handleArgs(%q) = handled %v, exit %d", args, handled, exitCode)
		}
		if stdout.Len() != 0 {
			t.Fatalf("handleArgs(%q) stdout = %q, want empty", args, stdout.String())
		}
		if got, want := stderr.String(), "Usage: cc-automux [--version]\n"; got != want {
			t.Fatalf("handleArgs(%q) stderr = %q, want %q", args, got, want)
		}
	}
}
