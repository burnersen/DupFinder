// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestRuntimeJSONPinsFFmpeg(t *testing.T) {
	rt, err := loadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	f := rt.FFmpeg
	if !strings.HasPrefix(f.URLs[0], "https://github.com/burnersen/DupFinder/releases/download/runtime-v1/") {
		t.Errorf("first source must be the own mirror, got %s", f.URLs[0])
	}
	// An exact FFmpeg release that stays (dev-starterkit standard), never a
	// daily, "latest" or git build: those change or disappear.
	if len(f.URLs) < 2 || !strings.HasPrefix(f.URLs[1], "https://github.com/GyanD/codexffmpeg/releases/download/") ||
		strings.Contains(f.URLs[1], "latest") || strings.Contains(f.URLs[1], "-git-") || strings.Contains(f.URLs[1], "autobuild") {
		t.Errorf("second source must be an exact release build, never latest or daily: %v", f.URLs)
	}
	if f.License == "" || f.Source == "" || f.Build == "" {
		t.Error("license, source and build must be named (GPL: the mirror links the source)")
	}
}

func pinnedServer(t *testing.T, body []byte) (*httptest.Server, runtimeEntry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/good/f.zip" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	return srv, runtimeEntry{Name: "Test", File: "f.zip", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func TestDownloadTriesTheNextSource(t *testing.T) {
	body := []byte("the pinned file")
	srv, e := pinnedServer(t, body)
	e.URLs = []string{srv.URL + "/missing/f.zip", srv.URL + "/good/f.zip"}
	dest := filepath.Join(t.TempDir(), "f.zip")
	var shown int64
	if err := fetchPinned(context.Background(), srv.Client(), e, dest, func(done, total int64) { shown = done }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(body) || shown != int64(len(body)) {
		t.Fatalf("got %q, progress %d", got, shown)
	}
}

func TestDownloadRejectsAnyOtherFile(t *testing.T) {
	srv, e := pinnedServer(t, []byte("the pinned file"))
	for name, change := range map[string]func(*runtimeEntry){
		"other checksum": func(e *runtimeEntry) { e.SHA256 = strings.Repeat("0", 64) },
		"larger file":    func(e *runtimeEntry) { e.Size-- },
		"smaller file":   func(e *runtimeEntry) { e.Size++ },
		"not found":      func(e *runtimeEntry) { e.URLs = []string{srv.URL + "/missing/f.zip"} },
	} {
		e := e
		e.URLs = []string{srv.URL + "/good/f.zip"}
		change(&e)
		dir := t.TempDir()
		err := fetchPinned(context.Background(), srv.Client(), e, filepath.Join(dir, "f.zip"), nil)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Hint == "" {
			t.Errorf("%s: got %v, want a UserError with a hint", name, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: left %d file(s) behind", name, len(entries))
		}
	}
}
