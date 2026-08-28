package shim

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func Main() {
	if err := configureLoggingFromEnv(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "CC AutoMux logging setup failed: %v\n", err)
		os.Exit(1)
	}
	if err := Run(); err != nil {
		errorf("%v", err)
		os.Exit(1)
	}
}

func Run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// The persisted log_max_bytes is authoritative once the config file exists, so
	// rebind the log writers to it now — demoting CC_AUTOMUX_LOG_MAX_BYTES to a
	// first-run seed only, exactly as CC_AUTOMUX_LISTEN seeds listen_addr
	// (seedRuntimeConfigFromEnv). Main already configured the writers from the env
	// (or the default) for the bootstrap window, so the loadConfig error above was
	// still logged; this only retunes the cap (same fd, no reopen). cfg.runtime is
	// the normalized config, so the cap is always positive here.
	if err := configureLogging(cfg.runtime.LogMaxBytes); err != nil {
		return err
	}

	server := newProxyServer(cfg)
	httpServer := &http.Server{
		Addr:              cfg.runtime.ListenAddr,
		Handler:           server,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	infof("CC AutoMux listening on http://%s (%s -> %s, %s -> %s)",
		cfg.runtime.ListenAddr,
		anyRouterPrefix,
		strings.Join(cfg.runtime.AnyRouter.Entrances, ","),
		cliproxyPrefix,
		cfg.runtime.CPA.Upstream,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
