// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data so that path holds either the old or the new
// content, never half of it: first into a ".part" file in the same folder,
// flushed to disk, then renamed. If anything fails, the part file is removed
// and path is untouched.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create folder %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.part")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	part := f.Name()
	defer func() {
		if err != nil {
			f.Close()       // may already be closed; the error is not news
			os.Remove(part) // clean up, path itself was never touched
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", part, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", part, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", part, err)
	}
	if err = os.Chmod(part, perm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", part, err)
	}
	// Replaces an existing file on Windows too, unless another program has
	// it open: then this fails and the old file stays as it was.
	if err = os.Rename(part, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
