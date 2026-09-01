//go:build windows

package harnessconfig

import "io/fs"

// Windows FileInfo values contain synthetic Unix mode bits rather than the
// effective ACL. Harness files use the operating system's normal per-user
// defaults and do not impose a separate Unix permission policy.
func harnessDirectoryMode() fs.FileMode { return 0 }

func setPrivateFileMode(FileHandle) error { return nil }

func verifyPrivateFileMode(fs.FileInfo) error { return nil }

func fileModeChanged(fs.FileInfo, fs.FileInfo) bool { return false }
