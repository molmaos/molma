package sshaccess

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a temp file in the same directory and
// a rename, so a reader (sshd, or a later readDropIn) never sees a half-written
// file. uid/gid of -1 leaves ownership to the calling process.
//
// The temp file is created in the destination directory rather than /tmp so the
// rename stays within one filesystem, which is what makes it atomic.
func writeFileAtomic(path string, data []byte, perm os.FileMode, uid, gid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Any failure past this point leaves the temp file behind, so remove it on
	// every path that does not reach the rename.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 {
		if err := os.Chown(tmpName, uid, gid); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
