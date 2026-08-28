package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path resolves the v1 configuration path. The only supported override is the
// absolute CC_AUTOMUX_CONFIG path; no v0 environment names are consulted.
func Path() (string, error) {
	if override := strings.TrimSpace(os.Getenv(ConfigPathEnv)); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", ConfigPathEnv, override)
		}
		return override, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(configDir, appConfigDir, configFileName), nil
}

// PendingPath returns the sibling transaction file used for restart-required
// updates (for example config.json.pending).
func PendingPath(path string) string { return path + ".pending" }
