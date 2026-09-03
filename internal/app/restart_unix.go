//go:build !windows

package app

import (
	"errors"
	"os"
	"syscall"
)

func waitForRestartParent() error { return nil }

func execCurrentProcessWithConfig(configPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(executable, os.Args, processEnvironment(configPath))
}

func (a *App) restartProcess() error {
	if err := a.beginRestart(); err != nil {
		return err
	}
	exec := a.exec
	if exec == nil {
		configPath := a.configPath
		exec = func() error { return execCurrentProcessWithConfig(configPath) }
	}
	err := exec()
	if err == nil {
		err = errors.New("self-reexec returned unexpectedly")
	}
	return a.recoverRestartFailure(err)
}
