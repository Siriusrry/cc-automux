//go:build !windows

package config

import (
	"io/fs"
	"os"
)

// Unix permissions are part of the configuration-file contract. Other
// platforms use their native per-user filesystem defaults instead.
func configDirectoryMode() fs.FileMode { return 0o700 }

func setConfigFileMode(file *os.File) error {
	return file.Chmod(0o600)
}

func validateConfigFileMode(info fs.FileInfo) error {
	if info.Mode().Perm() != 0o600 {
		return ErrInsecurePermissions
	}
	return nil
}
