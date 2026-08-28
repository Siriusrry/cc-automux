//go:build windows

package app

import (
	"errors"
	"os"
	"os/exec"
)

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
	command.Env = processEnvironment(a.configPath)
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
