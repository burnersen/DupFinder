// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Log rotation: the log never grows past logMaxSize, and logKeep older files
// (name.log.1 newest ... name.log.N oldest) are kept. A tool that runs for
// months as a service or by schedule stays small this way.
const (
	logMaxSize = 2 << 20 // 2 MiB
	logKeep    = 3
)

// openLog opens the log file for structured lines (log/slog, text format).
// The returned function closes it. Secrets never go into the log: no
// passwords, tokens, keys, full URLs with query strings.
func openLog(path string, level slog.Level) (*slog.Logger, func() error, error) {
	w, err := newRotatingFile(path, logMaxSize, logKeep)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
	return logger, w.Close, nil
}

// rotatingFile is an io.Writer that checks the size before every write, so
// rotation also happens in a process that never restarts.
type rotatingFile struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	f       *os.File
	size    int64
}

func newRotatingFile(path string, maxSize int64, keep int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log folder: %w", err)
	}
	r := &rotatingFile{path: path, maxSize: maxSize, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("read log file size: %w", err)
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		// A failed rotation (another process holds an old file open on
		// Windows) must not cost the log line: keep writing to this file.
		_ = r.rotate() // ignore: see comment above, writing goes on either way
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate moves name.log to name.log.1, .1 to .2 and so on, and drops the
// oldest. os.Rename replaces an existing target on Windows too.
func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep)) // ignore: may not exist yet
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1)) // ignore: gaps are fine
	}
	renameErr := os.Rename(r.path, r.path+".1")
	if err := r.open(); err != nil {
		return err
	}
	return renameErr
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
