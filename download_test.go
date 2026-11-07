// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// makeZip writes an archive shaped like the FFmpeg builds: one root folder.
func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractTakesOnlyTheTwoPrograms(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "ffmpeg.zip")
	makeZip(t, zipPath, map[string]string{
		"ffmpeg-n8-win64-gpl/bin/ffmpeg.exe":  "ffmpeg",
		"ffmpeg-n8-win64-gpl/bin/ffprobe.exe": "ffprobe",
		"ffmpeg-n8-win64-gpl/bin/ffplay.exe":  "ffplay",
		"ffmpeg-n8-win64-gpl/LICENSE.txt":     "license",
	})
	target := filepath.Join(dir, "bin")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractFFmpegBinaries(zipPath, target); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 2 {
		t.Fatalf("got %d files in bin, want 2 (no .tmp left behind)", len(entries))
	}
	if b, _ := os.ReadFile(filepath.Join(target, "ffprobe.exe")); string(b) != "ffprobe" {
		t.Fatalf("ffprobe.exe = %q", b)
	}
}

func TestExtractFailsWhenAProgramIsMissing(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "ffmpeg.zip")
	makeZip(t, zipPath, map[string]string{"root/bin/ffmpeg.exe": "ffmpeg"})
	if err := extractFFmpegBinaries(zipPath, dir); err == nil {
		t.Fatal("expected an error with ffprobe.exe missing")
	}
}
