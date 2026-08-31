package harnessconfig

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileHandle is the small file surface needed by the safe target writer. It
// is intentionally injectable so tests can exercise every failure boundary
// without touching a real user configuration.
type FileHandle interface {
	io.Reader
	io.Writer
	io.Closer
	Name() string
	Stat() (fs.FileInfo, error)
	Sync() error
	Chmod(fs.FileMode) error
}

// FileOps is the filesystem seam used by FileStore. Production uses
// OSFileOps; tests may wrap it to inject failures or record calls.
type FileOps interface {
	Lstat(string) (fs.FileInfo, error)
	Open(string) (FileHandle, error)
	OpenFile(string, int, fs.FileMode) (FileHandle, error)
	CreateTemp(string, string) (FileHandle, error)
	MkdirAll(string, fs.FileMode) error
	Rename(string, string) error
	Remove(string) error
	SyncDirectory(string) error
}

// FileSystem is a descriptive alias for the injected filesystem boundary.
type FileSystem = FileOps

// OSFileOps is the production FileOps implementation.
type OSFileOps struct{}

func (OSFileOps) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func (OSFileOps) Open(path string) (FileHandle, error) { return os.Open(path) }

func (OSFileOps) OpenFile(path string, flag int, perm fs.FileMode) (FileHandle, error) {
	return os.OpenFile(path, flag, perm)
}

func (OSFileOps) CreateTemp(directory, pattern string) (FileHandle, error) {
	return os.CreateTemp(directory, pattern)
}

func (OSFileOps) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (OSFileOps) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (OSFileOps) Remove(path string) error { return os.Remove(path) }

func (OSFileOps) SyncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// Clock is kept at the file-core boundary so temporary-name generation and
// later transaction metadata can be deterministic in tests.
type Clock interface {
	Now() time.Time
}

type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time {
	if f == nil {
		return time.Time{}
	}
	return f()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// FileStoreOptions supplies injectable dependencies to FileStore.
type FileStoreOptions struct {
	FS    FileOps
	Clock Clock
}

// FileStore owns the safe read/backup/merge/atomic-replace workflow for one
// target operation. It has no persistent configuration or active-profile
// state and is safe to use concurrently when its injected FileOps is safe.
type FileStore struct {
	fs    FileOps
	clock Clock
}

func NewFileStore(options ...FileStoreOptions) *FileStore {
	var option FileStoreOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.FS == nil {
		option.FS = OSFileOps{}
	}
	if option.Clock == nil {
		option.Clock = systemClock{}
	}
	return &FileStore{fs: option.FS, clock: option.Clock}
}

func NewFileStoreWithDependencies(files FileOps, clock Clock) *FileStore {
	return NewFileStore(FileStoreOptions{FS: files, Clock: clock})
}

func NewTargetStore(options ...FileStoreOptions) *FileStore {
	return NewFileStore(options...)
}

type targetState struct {
	exists bool
	info   fs.FileInfo
}

// Read reads an existing target under the same ordinary-file and 8 MiB
// checks used by Apply. A missing target returns an error matching
// os.ErrNotExist.
func (s *FileStore) Read(path string) ([]byte, error) {
	if s == nil || s.fs == nil {
		return nil, ErrNilFileSystem
	}
	if err := validateTargetPath(path); err != nil {
		return nil, err
	}
	data, exists, err := s.readExisting(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	return data, nil
}

// Apply rereads the target, merges the complete managed projection, creates a
// one-time sibling backup when required, atomically replaces the target, and
// rereads/verifies the committed bytes. It returns the verified final bytes.
func (s *FileStore) Apply(path string, adapter Adapter, projection ManagedProjection) ([]byte, error) {
	if s == nil || s.fs == nil {
		return nil, ErrNilFileSystem
	}
	if isNilAdapter(adapter) {
		return nil, ErrNilAdapter
	}
	if err := validateTargetPath(path); err != nil {
		return nil, err
	}
	backupPath := BackupPath(path)
	if samePath(path, backupPath) {
		return nil, ErrPathConflict
	}
	directory := filepath.Dir(path)
	if err := s.fs.MkdirAll(directory, 0o700); err != nil {
		return nil, fileError("create target directory", directory, ErrTargetIO, err)
	}

	original, targetExists, err := s.readExisting(path)
	if err != nil {
		return nil, err
	}
	state := targetState{exists: targetExists}
	if targetExists {
		state.info, err = s.fs.Lstat(path)
		if err != nil {
			return nil, fileError("inspect target", path, ErrTargetIO, err)
		}
	}
	if !targetExists {
		original = []byte("{}")
	}
	backupExists, err := s.inspectBackup(backupPath)
	if err != nil {
		return nil, err
	}
	merged, err := adapter.Merge(original, projection)
	if err != nil {
		return nil, fmt.Errorf("merge target: %w", err)
	}
	if err := adapter.Verify(merged, projection); err != nil {
		return nil, fmt.Errorf("%w: pre-write verification: %w", ErrVerification, err)
	}

	if targetExists && !backupExists {
		if _, err := s.createBackup(backupPath, original); err != nil {
			return nil, err
		}
	}
	temporaryPath, err := s.writeTemporary(directory, merged)
	if err != nil {
		return nil, err
	}
	temporaryOwned := true
	defer func() {
		if temporaryOwned {
			_ = s.fs.Remove(temporaryPath)
		}
	}()

	if err := s.checkTargetBeforeReplace(path, state); err != nil {
		return nil, err
	}
	if err := s.fs.Rename(temporaryPath, path); err != nil {
		return nil, fileError("atomically replace target", path, ErrAtomicReplace, err)
	}
	// Rename is the committed state transition. Directory synchronization is
	// best effort and must not turn a successful replacement into a false error.
	temporaryOwned = false
	_ = s.fs.SyncDirectory(directory)

	final, exists, err := s.readExisting(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fileError("verify replaced target", path, ErrTargetIO, os.ErrNotExist)
	}
	if err := adapter.Verify(final, projection); err != nil {
		return nil, fmt.Errorf("%w: committed target: %w", ErrVerification, err)
	}
	return final, nil
}

// Write is the error-only form used by callers that do not need the final
// verified bytes.
func (s *FileStore) Write(path string, adapter Adapter, projection ManagedProjection) error {
	_, err := s.Apply(path, adapter, projection)
	return err
}

func (s *FileStore) Update(path string, adapter Adapter, projection ManagedProjection) ([]byte, error) {
	return s.Apply(path, adapter, projection)
}

func ApplyTarget(path string, adapter Adapter, projection ManagedProjection) ([]byte, error) {
	return NewFileStore().Apply(path, adapter, projection)
}

// BackupPath returns the fixed sibling backup path for a target. It does not
// clean, resolve, or otherwise rewrite the target path.
func BackupPath(path string) string {
	return filepath.Join(filepath.Dir(path), BackupFileName)
}

func validateTargetPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return ErrInvalidTargetPath
	}
	return nil
}

func samePath(first, second string) bool {
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	if first == second {
		return true
	}
	if os.PathSeparator == '\\' {
		return strings.EqualFold(first, second)
	}
	return false
}

func (s *FileStore) readExisting(path string) ([]byte, bool, error) {
	info, err := s.fs.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fileError("inspect target", path, ErrTargetIO, err)
	}
	if info == nil {
		return nil, false, fileError("inspect target", path, ErrTargetIO, errors.New("filesystem returned nil target info"))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fileError("inspect target", path, ErrTargetSymlink, nil)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fileError("inspect target", path, ErrTargetNotRegular, nil)
	}
	if info.Size() > MaxExistingTargetBytes {
		return nil, false, fileError("read target", path, ErrTargetTooLarge, nil)
	}
	file, err := s.fs.Open(path)
	if err != nil {
		return nil, false, fileError("open target", path, ErrTargetIO, err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		closeErr := file.Close()
		return nil, false, joinFileErrors("stat target", path, ErrTargetIO, statErr, closeErr)
	}
	if openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.Mode().IsRegular() {
		closeErr := file.Close()
		return nil, false, joinFileErrors("inspect opened target", path, ErrTargetNotRegular, nil, closeErr)
	}
	if openedInfo.Size() > MaxExistingTargetBytes {
		closeErr := file.Close()
		return nil, false, joinFileErrors("read target", path, ErrTargetTooLarge, nil, closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxExistingTargetBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, false, joinFileErrors("read target", path, ErrTargetIO, readErr, closeErr)
	}
	if closeErr != nil {
		return nil, false, fileError("close target", path, ErrTargetIO, closeErr)
	}
	if int64(len(data)) > MaxExistingTargetBytes {
		return nil, false, fileError("read target", path, ErrTargetTooLarge, nil)
	}
	if info.Size() >= 0 && int64(len(data)) != info.Size() {
		return nil, false, fileError("read target", path, ErrTargetChanged, nil)
	}
	return data, true, nil
}

func (s *FileStore) inspectBackup(path string) (bool, error) {
	info, err := s.fs.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fileError("inspect backup", path, ErrBackupIO, err)
	}
	if info == nil {
		return false, fileError("inspect backup", path, ErrBackupIO, errors.New("filesystem returned nil backup info"))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fileError("inspect backup", path, ErrBackupSymlink, nil)
	}
	if !info.Mode().IsRegular() {
		return false, fileError("inspect backup", path, ErrBackupNotRegular, nil)
	}
	return true, nil
}

func (s *FileStore) createBackup(path string, original []byte) (bool, error) {
	file, err := s.fs.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			exists, inspectErr := s.inspectBackup(path)
			if inspectErr != nil {
				return false, inspectErr
			}
			if exists {
				return false, nil
			}
		}
		return false, fileError("create backup", path, ErrBackupIO, err)
	}
	if file == nil {
		return false, fileError("create backup", path, ErrBackupIO, errors.New("filesystem returned a nil file"))
	}
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !committed {
			_ = s.fs.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return false, fileError("chmod backup", path, ErrBackupIO, err)
	}
	if err := writeAll(file, original); err != nil {
		return false, fileError("write backup", path, ErrBackupIO, err)
	}
	if err := file.Sync(); err != nil {
		return false, fileError("sync backup", path, ErrBackupIO, err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return false, fileError("close backup", path, ErrBackupIO, err)
	}
	closed = true
	info, err := s.fs.Lstat(path)
	if err != nil {
		return false, fileError("verify backup", path, ErrBackupIO, err)
	}
	if info == nil {
		return false, fileError("verify backup", path, ErrBackupIO, errors.New("filesystem returned nil backup info"))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fileError("verify backup", path, ErrBackupSymlink, nil)
	}
	if !info.Mode().IsRegular() {
		return false, fileError("verify backup", path, ErrBackupNotRegular, nil)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fileError("verify backup", path, ErrBackupIO, errors.New("backup permissions are not private"))
	}
	committed = true
	return true, nil
}

func (s *FileStore) writeTemporary(directory string, data []byte) (string, error) {
	stamp := int64(0)
	if s.clock != nil {
		stamp = s.clock.Now().UnixNano()
	}
	pattern := ".cc-automux-" + strconv.FormatInt(stamp, 10) + "-tmp-*"
	file, err := s.fs.CreateTemp(directory, pattern)
	if err != nil {
		return "", fileError("create temporary target", directory, ErrTemporaryIO, err)
	}
	if file == nil {
		return "", fileError("create temporary target", directory, ErrTemporaryIO, errors.New("filesystem returned a nil file"))
	}
	path := file.Name()
	if path == "" {
		_ = file.Close()
		return "", fileError("create temporary target", directory, ErrTemporaryIO, errors.New("temporary file has no name"))
	}
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !committed {
			_ = s.fs.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fileError("chmod temporary target", path, ErrTemporaryIO, err)
	}
	if err := writeAll(file, data); err != nil {
		return "", fileError("write temporary target", path, ErrTemporaryIO, err)
	}
	if err := file.Sync(); err != nil {
		return "", fileError("sync temporary target", path, ErrTemporaryIO, err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return "", fileError("close temporary target", path, ErrTemporaryIO, err)
	}
	closed = true
	info, err := s.fs.Lstat(path)
	if err != nil {
		return "", fileError("verify temporary target", path, ErrTemporaryIO, err)
	}
	if info == nil {
		return "", fileError("verify temporary target", path, ErrTemporaryIO, errors.New("filesystem returned nil temporary info"))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fileError("verify temporary target", path, ErrTempNotRegular, nil)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fileError("verify temporary target", path, ErrTemporaryIO, errors.New("temporary permissions are not private"))
	}
	committed = true
	return path, nil
}

func (s *FileStore) checkTargetBeforeReplace(path string, expected targetState) error {
	info, err := s.fs.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if expected.exists {
			return fileError("check target", path, ErrTargetChanged, os.ErrNotExist)
		}
		return nil
	}
	if err != nil {
		return fileError("check target", path, ErrTargetIO, err)
	}
	if info == nil {
		return fileError("check target", path, ErrTargetIO, errors.New("filesystem returned nil target info"))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fileError("check target", path, ErrTargetSymlink, nil)
	}
	if !info.Mode().IsRegular() {
		return fileError("check target", path, ErrTargetNotRegular, nil)
	}
	if !expected.exists {
		return fileError("check target", path, ErrTargetChanged, errors.New("target appeared during write"))
	}
	if expected.info != nil && !os.SameFile(expected.info, info) {
		return fileError("check target", path, ErrTargetChanged, errors.New("target inode changed during write"))
	}
	return nil
}

func writeAll(file io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func fileError(operation, path string, category, cause error) error {
	if cause == nil {
		cause = category
	}
	return &FileError{Operation: operation, Path: path, Err: errors.Join(category, cause)}
}

func joinFileErrors(operation, path string, category, primary, secondary error) error {
	if primary == nil {
		primary = category
	}
	if secondary != nil {
		primary = errors.Join(primary, secondary)
	}
	return fileError(operation, path, category, primary)
}
