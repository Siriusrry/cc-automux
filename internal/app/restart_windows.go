//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const restartParentEnv = "CC_AUTOMUX_RESTART_PARENT_PID"

func waitForRestartParent() error {
	value := strings.TrimSpace(os.Getenv(restartParentEnv))
	if value == "" {
		return nil
	}
	_ = os.Unsetenv(restartParentEnv)
	pid, err := strconv.ParseUint(value, 10, 32)
	if err != nil || pid == 0 {
		return fmt.Errorf("invalid restart parent pid %q", value)
	}
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, syscall.Errno(87)) {
		// The parent exited before the child opened its handle, so there is
		// nothing left to wait for.
		return nil
	}
	if err != nil {
		return fmt.Errorf("open restart parent process: %w", err)
	}
	defer syscall.CloseHandle(handle)
	result, err := syscall.WaitForSingleObject(handle, syscall.INFINITE)
	if err != nil {
		return fmt.Errorf("wait for restart parent process: %w", err)
	}
	if result != syscall.WAIT_OBJECT_0 {
		return fmt.Errorf("wait for restart parent process returned %#x", result)
	}
	return nil
}

func restartProcessEnvironment(configPath string, parentPID int) []string {
	environ := processEnvironment(configPath)
	entry := restartParentEnv + "=" + strconv.Itoa(parentPID)
	for index, value := range environ {
		name, _, ok := strings.Cut(value, "=")
		if ok && strings.EqualFold(name, restartParentEnv) {
			environ[index] = entry
			return environ
		}
	}
	return append(environ, entry)
}

func (a *App) restartProcess() error {
	if err := a.beginRestart(); err != nil {
		return err
	}
	if a.exec != nil {
		err := a.exec()
		if err == nil {
			err = errors.New("self-restart callback returned unexpectedly")
		}
		return a.recoverRestartFailure(err)
	}
	executable, err := os.Executable()
	if err != nil {
		return a.recoverRestartFailure(err)
	}
	command := exec.Command(executable, os.Args[1:]...)
	command.Env = restartProcessEnvironment(a.configPath, os.Getpid())
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	if err := command.Start(); err != nil {
		return a.recoverRestartFailure(err)
	}
	// The child starts from the pending candidate after this process released
	// its listener. It promotes only after its logs and listener are ready; on a
	// candidate failure it rolls back and serves the old active configuration.
	os.Exit(0)
	return nil
}
