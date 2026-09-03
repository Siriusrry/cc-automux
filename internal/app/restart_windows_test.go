//go:build windows

package app

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const restartWaitHelperEnv = "CC_AUTOMUX_RESTART_WAIT_HELPER"

func TestRestartWaitHelperProcess(t *testing.T) {
	if os.Getenv(restartWaitHelperEnv) != "1" {
		return
	}
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

func TestWaitForRestartParent(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRestartWaitHelperProcess$")
	command.Env = append(os.Environ(), restartWaitHelperEnv+"=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	t.Setenv(restartParentEnv, strconv.Itoa(command.Process.Pid))
	started := time.Now()
	if err := waitForRestartParent(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("wait returned before parent exit: %v", elapsed)
	}
	if os.Getenv(restartParentEnv) != "" {
		t.Fatal("restart parent environment was not cleared")
	}
}

func TestRestartProcessEnvironmentReplacesParentPID(t *testing.T) {
	t.Setenv(restartParentEnv, "old")
	environ := restartProcessEnvironment(`C:\config.json`, 1234)
	count := 0
	for _, value := range environ {
		name, payload, ok := strings.Cut(value, "=")
		if ok && strings.EqualFold(name, restartParentEnv) {
			count++
			if payload != "1234" {
				t.Fatalf("restart parent value = %q", payload)
			}
		}
	}
	if count != 1 {
		t.Fatalf("restart parent entries = %d", count)
	}
}

func TestWaitForRestartParentRejectsInvalidPID(t *testing.T) {
	t.Setenv(restartParentEnv, "invalid")
	if err := waitForRestartParent(); err == nil {
		t.Fatal("invalid restart parent pid was accepted")
	}
}
