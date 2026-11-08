//go:build windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

// The Windows structs must have the size of the Windows headers (64 bit).
func TestRecycleStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(rbFileOp{}); got != 56 {
		t.Errorf("SHFILEOPSTRUCTW: %d bytes, want 56", got)
	}
	if got := unsafe.Sizeof(rbQueryInfo{}); got != 24 {
		t.Errorf("SHQUERYRBINFO: %d bytes, want 24", got)
	}
}

// A real file on the drive of the temp folder goes to the real bin, and the
// check afterwards sees it there.
func TestMoveToRecycleBinForReal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kit-recycle-test.txt")
	if err := os.WriteFile(path, []byte("dev-starterkit recycle bin test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	facts := binFactsOf(path)
	t.Logf("drive %s: %+v", facts.root, facts)
	err := moveToRecycleBin(path)
	if errors.Is(err, errBinRefused) {
		t.Skipf("this drive has no usable recycle bin: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
}

func TestNetworkPathIsRefusedBeforeAnything(t *testing.T) {
	err := moveToRecycleBin(`\\127.0.0.1\no-such-share\file.txt`)
	if !errors.Is(err, errBinRefused) {
		t.Fatalf("want errBinRefused, got %v", err)
	}
}
