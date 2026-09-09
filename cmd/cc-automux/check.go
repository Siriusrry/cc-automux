package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/runtime"
	productversion "github.com/Siriusrry/cc-automux/internal/version"
)

// check performs read-only installation preflight. Service registration and
// process ownership remain with the platform scripts.
func runCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "absolute configuration path")
	listen := flags.String("listen-addr", "", "check whether a loopback address can be bound")
	ready := flags.Bool("ready", false, "check authenticated status and web console readiness")
	wait := flags.Duration("timeout", 30*time.Second, "bounded readiness wait (up to 2m)")
	expected := flags.String("expect-version", productversion.Current(), "expected running version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *listen != "" {
		if *path != "" || *ready {
			fmt.Fprintln(stderr, "check: --listen-addr cannot be combined with config/readiness")
			return 2
		}
		cfg := config.Default()
		cfg.Auth.ManagementKey = "port-check-placeholder"
		cfg.Service.ListenAddr = *listen
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(stderr, "check: %v\n", err)
			return 1
		}
		listener, err := net.Listen("tcp4", *listen)
		if err != nil {
			fmt.Fprintf(stderr, "check: port unavailable: %v\n", err)
			return 1
		}
		listener.Close()
		return 0
	}
	if *path == "" || *wait <= 0 || *wait > 2*time.Minute {
		fmt.Fprintln(stderr, "check: --config and a timeout in (0, 2m] are required")
		return 2
	}
	cfg, err := checkConfiguration(*path)
	if err == nil && *ready {
		ctx, cancel := context.WithTimeout(context.Background(), *wait)
		defer cancel()
		err = waitReady(ctx, cfg, *expected)
	}
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "http://%s/management\n", cfg.Service.ListenAddr)
	return 0
}

func checkConfiguration(path string) (config.Config, error) {
	store, err := config.NewStore(path)
	if err != nil {
		return config.Config{}, err
	}
	pending, err := store.PendingExists()
	if err != nil {
		return config.Config{}, err
	}
	if pending {
		return config.Config{}, errors.New("unfinished configuration restart; resolve the pending file before installing")
	}
	cfg, err := store.Load()
	if err != nil {
		return config.Config{}, err
	}
	registry, err := patch.NewDefaultRegistry(patch.Services{AliasStore: patch.NewAliasStore()})
	if err != nil {
		return config.Config{}, err
	}
	resources, err := provider.NewRuntimeContext(registry)
	if err != nil {
		return config.Config{}, err
	}
	// Prepare the same provider/fixed-target configuration as startup, without
	// opening logs, binding the service listener, reading a harness or writing files.
	_, err = runtime.NewManager(store, cfg, runtime.Options{RuntimeContext: resources})
	return cfg, err
}

func waitReady(ctx context.Context, cfg config.Config, expected string) error {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var last error
	for {
		if last = probeReady(ctx, client, cfg, expected); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service not ready: %w", last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func probeReady(ctx context.Context, client *http.Client, cfg config.Config, expected string) error {
	base := "http://" + cfg.Service.ListenAddr
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/status", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticated status returned HTTP %d", response.StatusCode)
	}
	var status struct {
		Product, Version  string
		ListenAddr        string `json:"listen_addr"`
		Pending           bool
		RestartInProgress bool `json:"restart_in_progress"`
		Restart           runtime.RestartStatus
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return fmt.Errorf("invalid status response: %w", err)
	}
	if status.Product != "CC AutoMux" || status.Version != expected || status.ListenAddr != cfg.Service.ListenAddr {
		return errors.New("status product, version or listener differs from the selected installation")
	}
	if status.Pending || status.RestartInProgress || status.Restart.Pending || status.Restart.InProgress {
		return errors.New("configuration restart is still in progress")
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/management", nil)
	page, err := client.Do(request)
	if err != nil {
		return err
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.HasPrefix(page.Header.Get("Content-Type"), "text/html") {
		return errors.New("management web console is unavailable")
	}
	return nil
}
