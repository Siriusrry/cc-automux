//go:build !windows

package gateway

import "os"

func createReplayFile(directory string) (*os.File, error) {
	file, err := os.CreateTemp(directory, "cc-automux-request-*")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	// An open-but-unlinked file is removed by the kernel even if the process
	// exits without running request cleanup.
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}
