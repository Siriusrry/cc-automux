//go:build windows

package harnessconfig

import (
	"syscall"
	"unsafe"
)

const moveFileReplaceExisting = 0x00000001

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func atomicReplace(files FileOps, oldPath, newPath string) error {
	if replacer, ok := files.(atomicFileReplacer); ok {
		return replacer.AtomicReplace(oldPath, newPath)
	}
	// Test and alternate filesystems that do not expose a platform replacement
	// primitive retain their injected Rename behavior.
	return files.Rename(oldPath, newPath)
}

// AtomicReplace uses MoveFileExW with replacement enabled. The source and
// destination are created in the same directory by FileStore, so the Windows
// filesystem can commit the namespace change without a delete-then-create gap.
func (OSFileOps) AtomicReplace(oldPath, newPath string) error {
	from, err := syscall.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		uintptr(moveFileReplaceExisting),
	)
	if result == 0 {
		return callErr
	}
	return nil
}
