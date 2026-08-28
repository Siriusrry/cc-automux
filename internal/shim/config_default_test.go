package shim

import (
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultsAutoShimListen(t *testing.T) {
	t.Setenv(configPathEnv, filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("CC_AUTO_SHIM_LISTEN", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig should default CC_AUTO_SHIM_LISTEN: %v", err)
	}
	if cfg.runtime.ListenAddr != defaultListenAddr {
		t.Fatalf("listenAddr = %q, want %q", cfg.runtime.ListenAddr, defaultListenAddr)
	}
}
