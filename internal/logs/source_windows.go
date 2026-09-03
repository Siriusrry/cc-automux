//go:build windows

package logs

import (
	"os"
	"syscall"
)

// openOptional explicitly allows delete sharing. Windows rename/replacement
// otherwise fails while a history request holds a read handle to either log
// generation.
func openOptional(path string) (*os.File, int64, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, 0, err
	}
	handle, err := syscall.CreateFile(
		pathPointer,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		pathErr := &os.PathError{Op: "open", Path: path, Err: err}
		if os.IsNotExist(pathErr) {
			return nil, 0, nil
		}
		return nil, 0, pathErr
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, 0, &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}
