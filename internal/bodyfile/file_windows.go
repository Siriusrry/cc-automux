//go:build windows

package bodyfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const (
	fileAttributeTemporary = 0x00000100
	fileFlagDeleteOnClose  = 0x04000000
)

func createBodyFile(directory string) (*os.File, error) {
	if directory == "" {
		directory = os.TempDir()
	}
	for attempts := 0; attempts < 100; attempts++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(directory, "cc-automux-body-"+hex.EncodeToString(random[:]))
		name16, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		handle, err := syscall.CreateFile(
			name16,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil,
			syscall.CREATE_NEW,
			fileAttributeTemporary|fileFlagDeleteOnClose,
			0,
		)
		if errors.Is(err, syscall.ERROR_FILE_EXISTS) || errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(handle), name)
		if file == nil {
			_ = syscall.CloseHandle(handle)
			return nil, errors.New("could not wrap body file handle")
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	return nil, errors.New("could not create a unique body file")
}
