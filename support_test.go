// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "x.log")
	w, err := newRotatingFile(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 59) + "\n")
	for range 10 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Size() > 100 {
			t.Errorf("%s has %d bytes, limit 100", name, info.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("more old files kept than asked for")
	}
}

func TestOpenLogWritesLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	logger, closeLog, err := openLog(path, slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello", "k", "v")
	logger.Debug("hidden")
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "msg=hello k=v") || strings.Contains(string(data), "hidden") {
		t.Fatalf("log: %q", data)
	}
}

func TestWriteFileAtomicLeavesNoPartFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	for _, content := range []string{"one", "two"} {
		if err := writeFileAtomic(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "two" {
		t.Fatalf("content %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestFileErrorExplains(t *testing.T) {
	_, err := os.Open(filepath.Join(t.TempDir(), "nope"))
	got := fileError(err, "open", "nope")
	var ue *UserError
	if !errors.As(got, &ue) || !strings.Contains(ue.Msg, "does not exist") || !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("%v", got)
	}
	if fmt.Sprint(ue.Hint) == "" {
		t.Fatal("no hint")
	}
}

func withSHA(f pinnedFile, s string) pinnedFile { f.SHA256 = s; return f }
func withSize(f pinnedFile, n int64) pinnedFile { f.Size = n; return f }
func withURL(f pinnedFile, u string) pinnedFile { f.URL = u; return f }
