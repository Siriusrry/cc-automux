package harnessconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
