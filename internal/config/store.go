package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrNotFound            = os.ErrNotExist
	ErrInsecurePermissions = errors.New("configuration file must have permissions 0600")
	ErrSymlink             = errors.New("configuration file must not be a symbolic link")
	ErrNotRegular          = errors.New("configuration file must be a regular file")
	ErrAlreadyExists       = os.ErrExist
)

// Store owns one active configuration path and its sibling pending path.
type Store struct {
	path        string
	pendingPath string
}

func NewStore(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("configuration path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("configuration path must be absolute, got %q", path)
	}
	return &Store{path: path, pendingPath: PendingPath(path)}, nil
}

func (s *Store) Path() string        { return s.path }
func (s *Store) PendingPath() string { return s.pendingPath }

func (s *Store) Load() (Config, error) {
	return loadFile(s.path)
}

func (s *Store) LoadPending() (Config, error) {
	return loadFile(s.pendingPath)
}

func (s *Store) PendingExists() (bool, error) {
	info, err := os.Lstat(s.pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, ErrSymlink
	}
	return true, nil
}

func (s *Store) Save(cfg Config) error {
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicWrite(s.path, data, false)
}

func (s *Store) SavePending(cfg Config) error {
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicWrite(s.pendingPath, data, false)
}

// Create writes the first active configuration without replacing a file that
// appeared concurrently. It is used by the shared initialization path.
func (s *Store) Create(cfg Config) error {
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicWrite(s.path, data, true)
}

// PromotePending atomically makes the already validated pending configuration
// active. It never removes or truncates the active file before the rename.
func (s *Store) PromotePending() error {
	if _, err := s.LoadPending(); err != nil {
		return err
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrSymlink
	}
	if !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	if err := validateConfigFileMode(info); err != nil {
		return err
	}
	if err := os.Rename(s.pendingPath, s.path); err != nil {
		return err
	}
	// The rename is already the committed state transition. Directory fsync is
	// best effort; reporting an error after the rename would make callers think
	// promotion failed even though the active file has changed.
	_ = syncDirectory(filepath.Dir(s.path))
	return nil
}

func (s *Store) RemovePending() error {
	err := os.Remove(s.pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = syncDirectory(filepath.Dir(s.path))
	return nil
}

func loadFile(path string) (Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Config{}, ErrSymlink
	}
	if !info.Mode().IsRegular() {
		return Config{}, ErrNotRegular
	}
	if err := validateConfigFileMode(info); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Decode(data)
	if err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return cfg, nil
}

func atomicWrite(path string, data []byte, exclusive bool) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirectoryMode()); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	f := tmpFile
	tmp := f.Name()
	if err := setConfigFileMode(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	removeTemp := true
	closed := false
	defer func() {
		if !closed {
			if closeErr := f.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = setConfigFileMode(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	closed = true
	if exclusive {
		if err = os.Link(tmp, path); err != nil {
			return err
		}
		// The hard link is the exclusive commit. The temporary directory entry is
		// only a second name for the already committed inode; failure to unlink
		// that cleanup name must not be reported as a failed initialization.
		if removeErr := os.Remove(tmp); removeErr == nil {
			removeTemp = false
		}
	} else {
		if err = os.Rename(tmp, path); err != nil {
			return err
		}
		removeTemp = false
	}
	// The rename/link is the atomic state transition. Directory fsync is best
	// effort on filesystems that do not support it; never report a post-rename
	// durability warning as if the configuration write had not happened.
	_ = syncDirectory(dir)
	return nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		// Some filesystems (notably older macOS configurations) reject directory
		// fsync. The rename is still atomic, so this is best effort.
		return nil
	}
	return nil
}
