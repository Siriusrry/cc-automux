package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	configPathEnv     = "CC_AUTO_SHIM_CONFIG"
	appSupportDirName = "cc-auto-mode-shim"
	configFileName    = "config.json"
)

func loadConfig() (appConfig, error) {
	compiled, path, err := loadOrSeedRuntimeConfig()
	if err != nil {
		return appConfig{}, err
	}
	table, err := buildRoutingTable(compiled)
	if err != nil {
		return appConfig{}, err
	}
	return appConfig{configPath: path, runtime: compiled.runtime, table: table}, nil
}

func loadOrSeedRuntimeConfig() (*compiledRuntimeConfig, string, error) {
	path, err := runtimeConfigPath()
	if err != nil {
		return nil, "", err
	}

	cfg, err := readRuntimeConfig(path)
	if err == nil {
		compiled, err := compileRuntimeConfig(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("invalid runtime config %s: %w", path, err)
		}
		return compiled, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("read runtime config %s: %w", path, err)
	}

	cfg = seedRuntimeConfigFromEnv()
	compiled, err := compileRuntimeConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	if err := writeRuntimeConfigFile(path, compiled.runtime); err != nil {
		return nil, "", fmt.Errorf("write initial runtime config %s: %w", path, err)
	}
	return compiled, path, nil
}

func runtimeConfigPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(configPathEnv)); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", configPathEnv, override)
		}
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", appSupportDirName, configFileName), nil
}

func seedRuntimeConfigFromEnv() *runtimeConfig {
	// CC_AUTO_SHIM_LOG_MAX_BYTES seeds the cap only on first config creation, after
	// which the on-disk file is authoritative (mirrors CC_AUTO_SHIM_LISTEN →
	// listen_addr). An invalid value has already failed startup in
	// configureLoggingFromEnv (Main) before seeding runs, so the discarded error path
	// here returns 0 — which normalizeRuntimeConfig maps to defaultMaxLogBytes.
	logMaxBytes, _ := configuredMaxLogBytes()
	return &runtimeConfig{
		ListenAddr:  getenvDefault("CC_AUTO_SHIM_LISTEN", defaultListenAddr),
		LogMaxBytes: logMaxBytes,
		AnyRouter: anyRouterRuntimeConfig{
			Entrances: trimStringSlice(strings.Split(getenvDefault("CC_ANYROUTER_SHIM_UPSTREAM", defaultAnyRouterUpstreamURLs), ",")),
			Accounts:  []accountEntry{},
		},
		CPA: cpaRuntimeConfig{
			Upstream: getenvDefault("CC_CLIPROXY_SHIM_UPSTREAM", defaultCliproxyUpstreamURL),
			CAPath:   strings.TrimSpace(os.Getenv("CC_CLIPROXY_SHIM_CA")),
		},
	}
}

func readRuntimeConfig(path string) (*runtimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg runtimeConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected trailing JSON content")
	}
	return &cfg, nil
}

func saveRuntimeConfig(path string, cfg *runtimeConfig) error {
	compiled, err := compileRuntimeConfig(cfg)
	if err != nil {
		return err
	}
	return writeRuntimeConfigFile(path, compiled.runtime)
}

func writeRuntimeConfigFile(path string, cfg *runtimeConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, configFileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
