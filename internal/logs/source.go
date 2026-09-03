package logs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

type ReadFile interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}

type Snapshot struct {
	Active      ReadFile
	ActiveSize  int64
	Archive     ReadFile
	ArchiveSize int64
}

func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.Active != nil {
		errs = append(errs, s.Active.Close())
		s.Active = nil
	}
	if s.Archive != nil {
		errs = append(errs, s.Archive.Close())
		s.Archive = nil
	}
	return errors.Join(errs...)
}

type SnapshotSource interface {
	OpenSnapshot() (Snapshot, error)
}

type SnapshotSourceFunc func() (Snapshot, error)

func (f SnapshotSourceFunc) OpenSnapshot() (Snapshot, error) {
	if f == nil {
		return Snapshot{}, errors.New("log snapshot source is nil")
	}
	return f()
}

type DirectorySource struct {
	dir string
}

func NewDirectorySource(dir string) (*DirectorySource, error) {
	if dir == "" {
		return nil, errors.New("log directory must not be empty")
	}
	return &DirectorySource{dir: dir}, nil
}

func (s *DirectorySource) OpenSnapshot() (Snapshot, error) {
	if s == nil || s.dir == "" {
		return Snapshot{}, errors.New("log directory source is not initialized")
	}
	active, activeSize, err := openOptional(filepath.Join(s.dir, ActiveFileName))
	if err != nil {
		return Snapshot{}, err
	}
	archive, archiveSize, err := openOptional(filepath.Join(s.dir, ArchiveFileName))
	if err != nil {
		if active != nil {
			_ = active.Close()
		}
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if active != nil {
		snapshot.Active = active
		snapshot.ActiveSize = activeSize
	}
	if archive != nil {
		snapshot.Archive = archive
		snapshot.ArchiveSize = archiveSize
	}
	return snapshot, nil
}
