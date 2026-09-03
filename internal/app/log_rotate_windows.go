//go:build windows

package app

import (
	"syscall"
	"unsafe"
)

const moveFileReplaceExisting = 0x00000001

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

// replaceLogArchive uses replacement-enabled MoveFileExW. The active and
// archive paths always share a directory, so the namespace update is atomic.
func replaceLogArchive(active, archive string) error {
	from, err := syscall.UTF16PtrFromString(active)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(archive)
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
