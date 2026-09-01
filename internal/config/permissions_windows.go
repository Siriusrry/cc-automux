//go:build windows

package config

import (
	"io/fs"
	"os"
)

// Windows FileInfo values expose synthetic Unix mode bits. They are not an
// ACL/security descriptor, so configuration files use the OS's normal
// per-user defaults without a Unix permission policy.
func configDirectoryMode() fs.FileMode { return 0 }

func setConfigFileMode(*os.File) error { return nil }

func validateConfigFileMode(fs.FileInfo) error { return nil }
