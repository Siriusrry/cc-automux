//go:build darwin

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// LogDir resolves the platform-owned directory for structured process logs.
func LogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory for logs: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", appConfigDir), nil
}
