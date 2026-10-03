package harnessconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedFileRejectsReplacementWithIdenticalContents(t *testing.T) {
	target := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"before":true}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := NewFileStore().Prepare(target, NewClaudeCodeAdapter(), testProjection(t, true, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, target+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Commit(); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("replacement with identical contents was not rejected: %v", err)
	}
}

func TestPreparedFileRollbackTracksRenamedFileIdentity(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, replaced := range []bool{false, true} {
			t.Run(fmt.Sprintf("exists=%t/replaced=%t", exists, replaced), func(t *testing.T) {
				target := filepath.Join(t.TempDir(), "settings.json")
				original := []byte(`{"keep":"original"}`)
				if exists {
					if err := os.WriteFile(target, original, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				prepared, err := NewFileStore().Prepare(target, NewClaudeCodeAdapter(), testProjection(t, true, false))
				if err != nil {
					t.Fatal(err)
				}
				written, err := prepared.Commit()
				if err != nil {
					t.Fatal(err)
				}
				if replaced {
					if err := os.Rename(target, target+".previous"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, written, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				err = prepared.Rollback()
				got, readErr := os.ReadFile(target)
				if replaced {
					if !errors.Is(err, ErrTargetChanged) || readErr != nil || !bytes.Equal(got, written) {
						t.Fatalf("external replacement must survive: rollback=%v read=%v", err, readErr)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if exists && (readErr != nil || !bytes.Equal(got, original)) {
					t.Fatalf("original not restored: %q, %v", got, readErr)
				} else if !exists && !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("new target not removed: %v", readErr)
				}
			})
		}
	}
}

type mutateTargetOps struct {
	OSFileOps
	target string
}

func (o *mutateTargetOps) CreateTemp(directory, pattern string) (FileHandle, error) {
	file, err := o.OSFileOps.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	// Rewrite through the same inode. An identity-only pre-replace check would
	// miss this mutation and overwrite a concurrent user's update.
	targetFile, openErr := os.OpenFile(o.target, os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		_ = file.Close()
		_ = o.OSFileOps.Remove(file.Name())
		return nil, openErr
	}
	_, writeErr := targetFile.WriteString(`{"concurrent":true}`)
	closeErr := targetFile.Close()
	if writeErr != nil {
		_ = file.Close()
		_ = o.OSFileOps.Remove(file.Name())
		return nil, writeErr
	}
	if closeErr != nil {
		_ = file.Close()
		_ = o.OSFileOps.Remove(file.Name())
		return nil, closeErr
	}
	return file, nil
}

func TestFileStoreRejectsSameInodeTargetMutationBeforeReplace(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "settings.json")
	if err := os.WriteFile(target, []byte(`{"before":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(FileStoreOptions{FS: &mutateTargetOps{target: target}})
	_, err := store.Apply(target, NewClaudeCodeAdapter(), testProjection(t, true, false))
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("same-inode mutation error = %v", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != `{"concurrent":true}` {
		t.Fatalf("concurrent target after rejection = %q, err %v", got, readErr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(target) && entry.Name() != BackupFileName {
			t.Errorf("temporary file remains: %s", entry.Name())
		}
	}
}
