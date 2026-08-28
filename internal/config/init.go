package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Initialize creates the first configuration through the shared initialization
// core used by installation frontends. Existing configuration is loaded and returned
// unchanged; its keys are never overwritten by reinstall.
func Initialize(path, managementKey string) (cfg Config, created bool, err error) {
	return InitializeWithService(path, managementKey, ServiceConfig{})
}

// InitializeWithService is the shared first-run initialization primitive. The
// optional service value is normalized and validated together with the key
// before one atomic file creation; an existing file is always returned intact.
func InitializeWithService(path, managementKey string, service ServiceConfig) (cfg Config, created bool, err error) {
	store, err := NewStore(path)
	if err != nil {
		return Config{}, false, err
	}
	if cfg, loadErr := store.Load(); loadErr == nil {
		return cfg, false, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return Config{}, false, loadErr
	}
	if strings.TrimSpace(managementKey) == "" {
		return Config{}, false, errors.New("management key must not be empty")
	}
	cfg = Default()
	if service.ListenAddr != "" {
		cfg.Service.ListenAddr = service.ListenAddr
	}
	if service.LogMaxBytes != 0 {
		cfg.Service.LogMaxBytes = service.LogMaxBytes
	}
	cfg.Auth.ManagementKey = managementKey
	if err := cfg.Validate(); err != nil {
		return Config{}, false, err
	}
	if err := store.Create(cfg); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			cfg, loadErr := store.Load()
			return cfg, false, loadErr
		}
		return Config{}, false, fmt.Errorf("create initial configuration: %w", err)
	}
	return cfg, true, nil
}

// GenerateManagementKey returns a high-entropy URL-safe key suitable for the
// first-run prompt. It never uses a pseudo-random fallback.
func GenerateManagementKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate management key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// GenerateUUID returns a random RFC 4122 version-4 UUID in canonical text
// form. It is used when a Provider is created without a client-supplied ID.
func GenerateUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}
