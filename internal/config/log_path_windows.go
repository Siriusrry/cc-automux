//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LogDir resolves the per-user Windows directory for structured process logs.
func LogDir() (string, error) {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		return "", errors.New("LOCALAPPDATA is not set")
	}
	if !filepath.IsAbs(localAppData) {
		return "", errors.New("LOCALAPPDATA must be an absolute path")
	}
	return filepath.Join(localAppData, appConfigDir, "logs"), nil
}
