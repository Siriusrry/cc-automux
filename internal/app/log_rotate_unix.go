//go:build !windows

package app

import "os"

// replaceLogArchive is a same-directory atomic namespace replacement.
func replaceLogArchive(active, archive string) error { return os.Rename(active, archive) }
