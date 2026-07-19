//go:build !windows

package timeshadedb

import (
	"os"
	"path/filepath"
)

func replaceFileAtomically(source, destination string) error {
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	// The replacement is already committed, so a sync error cannot be rolled back.
	_ = dir.Sync()
	return nil
}
