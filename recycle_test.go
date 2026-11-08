// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBinRefusesWhatWindowsWouldDeleteForGood(t *testing.T) {
	const gb = 1024 * binBytesPerMB
	cases := []struct {
		name  string
		facts binFacts
		size  int64
		want  string // part of the reason, "" = the bin takes it
	}{
		{"normal drive", binFacts{root: `C:\`}, 5 * gb, ""},
		{"fits the limit", binFacts{root: `C:\`, capMB: 10 * 1024, capKnown: true}, 5 * gb, ""},
		{"exactly the limit", binFacts{root: `C:\`, capMB: 1, capKnown: true}, binBytesPerMB, ""},
		{"too big for the bin", binFacts{root: `D:\`, capMB: 2048, capKnown: true}, 5 * gb, "holds only 2.0 GB"},
		{"bin without room", binFacts{root: `D:\`, capMB: 0, capKnown: true}, 1, "holds only 0 MB"},
		{"limit unknown", binFacts{root: `D:\`}, 500 * gb, ""},
		{"size unknown", binFacts{root: `D:\`, capMB: 1, capKnown: true}, -1, ""},
		{"network drive", binFacts{root: `Z:\`, network: true}, 1, "network drive"},
		{"USB stick", binFacts{root: `E:\`, removable: true}, 1, `removable drive E:\`},
		{"DVD", binFacts{root: `F:\`, cdrom: true}, 1, `drive F:\ has no recycle bin`},
		{"switched off", binFacts{root: `C:\`, off: true}, 1, "switched off"},
	}
	for _, c := range cases {
		got := binRefuses(c.facts, c.size)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBinGrew(t *testing.T) {
	if !binGrew(3, 100, 4, 150) || !binGrew(3, 100, 2, 900) || binGrew(3, 100, 3, 100) || binGrew(3, 100, 2, 50) {
		t.Fatal("binGrew: more items or more bytes must count, nothing else")
	}
}

func TestIsUNC(t *testing.T) {
	for path, want := range map[string]bool{
		`\\server\share\a.mkv`:     true,
		`\\?\UNC\server\share\a`:   true,
		`\\?\unc\server\share\a`:   true,
		`\\?\C:\very\long\path`:    false,
		`\\.\PhysicalDrive0`:       false,
		`C:\Videos\a.mkv`:          false,
		`relative\path\to\a.mkv`:   false,
		`/home/user/videos/a.mkv`:  false,
		`\\server-without-a-share`: true,
	} {
		if got := isUNC(path); got != want {
			t.Errorf("isUNC(%q) = %v", path, got)
		}
	}
}

func TestBinSizeText(t *testing.T) {
	if got := binSizeText(500 * binBytesPerMB); got != "500 MB" {
		t.Fatal(got)
	}
	if got := binSizeText(5 * 1024 * binBytesPerMB); got != "5.0 GB" {
		t.Fatal(got)
	}
}

func TestRecycleKeepsTheFileOutsideWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has a recycle bin: recycle_windows_test.go")
	}
	path := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveToRecycleBin(path); !errors.Is(err, errBinRefused) {
		t.Fatalf("want errBinRefused, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file must stay: %v", err)
	}
}
