//go:build !windows

package harnessconfig

import "os"

// atomicReplace uses same-directory rename on Unix. rename(2) replaces the
// destination as one atomic namespace operation.
func atomicReplace(files FileOps, oldPath, newPath string) error {
	if replacer, ok := files.(atomicFileReplacer); ok {
		return replacer.AtomicReplace(oldPath, newPath)
	}
	return files.Rename(oldPath, newPath)
}

// Keep the platform primitive visible to production callers that inspect the
// concrete OS file operations; injected FileOps continue to use Rename above.
func (OSFileOps) AtomicReplace(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
