// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func runDupFinder(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestNoFolderShowsUsageAndExitCode1(t *testing.T) {
	code, stdout, _ := runDupFinder(t)
	if code != exitUsage || exitUsage != 1 || !strings.Contains(stdout, "Usage:") {
		t.Fatalf("code %d, output %q", code, stdout)
	}
}

func TestVersionAndHelp(t *testing.T) {
	if code, stdout, _ := runDupFinder(t, "-version"); code != exitOK || stdout != "DupFinder dev\n" {
		t.Fatalf("-version: %d %q", code, stdout)
	}
	if code, stdout, _ := runDupFinder(t, "--help"); code != exitOK || !strings.Contains(stdout, "Exit codes:") {
		t.Fatalf("--help: %d %q", code, stdout)
	}
}

func TestUnwritableDataFolderExplainsAndExitCode2(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeFile(t, home, []byte("a file where the folder should be"))
	t.Setenv("DUPFINDER_HOME", home)
	code, _, stderr := runDupFinder(t, t.TempDir())
	if code != exitError || exitError != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(stderr, "[ERROR] Cannot create the data folder") || !strings.Contains(stderr, "folder you may write to") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestCrashBecomesExitCode2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := func() (code int) {
		out := newOutput(&stdout, &stderr)
		logger := testLogger()
		defer catchCrash(out, &logger, &code)
		panic("boom")
	}()
	if code != exitError || !strings.Contains(stderr.String(), "internal error: boom") {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
