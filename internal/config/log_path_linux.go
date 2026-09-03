//go:build !darwin && !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LogDir resolves the XDG state directory used for structured process logs.
func LogDir() (string, error) {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path, got %q", stateHome)
		}
		return filepath.Join(stateHome, appConfigDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory for logs: %w", err)
	}
	return filepath.Join(home, ".local", "state", appConfigDir), nil
}
