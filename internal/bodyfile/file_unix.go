//go:build !windows

package bodyfile

import "os"

func createBodyFile(directory string) (*os.File, error) {
	file, err := os.CreateTemp(directory, "cc-automux-body-*")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	// Keep the descriptor alive only for this request.  This also ensures a
	// process crash cannot leave a body file in the temporary directory.
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}
