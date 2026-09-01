//go:build !windows

package harnessconfig

import (
	"errors"
	"io/fs"
)

func harnessDirectoryMode() fs.FileMode { return 0o700 }

func setPrivateFileMode(file FileHandle) error {
	return file.Chmod(0o600)
}

func verifyPrivateFileMode(info fs.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("file permissions are not private")
	}
	return nil
}

func fileModeChanged(expected, current fs.FileInfo) bool {
	return expected.Mode().Perm() != current.Mode().Perm()
}
