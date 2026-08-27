// Package atomicfile writes files in a way that a crash cannot leave
// half-written.
//
// dk uses it for the files it owns: the config and the token cache, which hold
// credentials and would strand the user if a truncated write survived, and the
// response cache, where a truncated entry would be served as a whole response
// for the rest of its TTL.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write creates path with the given contents and permissions, replacing any
// existing file. It writes to a temporary file in the same directory and
// renames it into place, so a reader either sees the old contents or the new
// ones, never a partial write.
//
// The parent directory must already exist; callers own its permissions, which
// differ (0700 for credential directories, 0755 for user-chosen output).
func Write(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Harmless once the rename has succeeded, and the only cleanup on failure.
	defer os.Remove(tmpName)

	// Chmod before writing: the temp file is created 0600, but an explicit mode
	// keeps the result independent of that detail.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}
