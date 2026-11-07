// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// Live test with a real FFmpeg (dev-starterkit 06-tests.md T5). It runs only
// when DUPFINDER_TEST_FFMPEG names a folder with ffmpeg.exe and ffprobe.exe;
// the workflow "Mirror FFmpeg" sets it for every build before mirroring it.
// Normal CI does not, so it skips there.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func liveFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	dir := os.Getenv("DUPFINDER_TEST_FFMPEG")
	if dir == "" {
		t.Skip("DUPFINDER_TEST_FFMPEG not set (folder with ffmpeg.exe and ffprobe.exe)")
	}
	return filepath.Join(dir, "ffmpeg.exe"), filepath.Join(dir, "ffprobe.exe")
}

// makeClip renders a test video with FFmpeg's own pattern generators.
func makeClip(t *testing.T, ffmpeg, dest string, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-loglevel", "error"}, args...)
	full = append(full, "-pix_fmt", "yuv420p", "-c:v", "libx264", "-y", dest)
	if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

func probed(t *testing.T, ffprobe, path string) videoInfo {
	t.Helper()
	info, err := probeVideo(context.Background(), ffprobe, path)
	if err != nil || info.HasError {
		t.Fatalf("probe %s: %v %s", path, err, info.ErrorMessage)
	}
	return info
}

func allHashed(v videoInfo) bool {
	for _, h := range v.Hashes {
		if h == 0 {
			return false
		}
	}
	return len(v.Hashes) == sampleCount
}

func TestLiveFindsReencodedCopies(t *testing.T) {
	ffmpeg, ffprobe := liveFFmpeg(t)
	dir := t.TempDir()
	clips := map[string][]string{
		// 12 s: below bulkDurationLimit, takes the bulk path (one ffmpeg call)
		"short.mp4": {"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=12"},
		// 40 s: the single-call path with seeking
		"long.mp4":  {"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=40"},
		"other.mp4": {"-f", "lavfi", "-i", "mandelbrot=size=640x360:rate=25", "-t", "12"},
	}
	for name, args := range clips {
		makeClip(t, ffmpeg, filepath.Join(dir, name), args...)
	}
	// Copies as people have them: smaller and worse, in another container.
	for _, name := range []string{"short", "long"} {
		makeClip(t, ffmpeg, filepath.Join(dir, name+"-copy.mkv"),
			"-i", filepath.Join(dir, name+".mp4"), "-vf", "scale=320:180", "-crf", "35")
	}

	scratch := t.TempDir()
	videos := map[string]*videoInfo{}
	for _, name := range []string{"short.mp4", "short-copy.mkv", "long.mp4", "long-copy.mkv", "other.mp4"} {
		v := probed(t, ffprobe, filepath.Join(dir, name))
		processVideoHashes(ffmpeg, &v, scratch, nil)
		if !allHashed(v) {
			t.Fatalf("%s: not every frame hashed: %x", name, v.Hashes)
		}
		videos[name] = &v
	}
	for _, pair := range [][2]string{{"short.mp4", "short-copy.mkv"}, {"long.mp4", "long-copy.mkv"}} {
		if m, _ := compareHashSets(videos[pair[0]].Hashes, videos[pair[1]].Hashes); m < minMatchingFrames {
			t.Errorf("%s and %s: %d matching frames, want at least %d", pair[0], pair[1], m, minMatchingFrames)
		}
	}
	if m, _ := compareHashSets(videos["short.mp4"].Hashes, videos["other.mp4"].Hashes); m >= minMatchingFrames {
		t.Errorf("different videos match on %d frames", m)
	}
}

// The bulk paths for short clips must work on their own: if they fail,
// DupFinder silently falls back to slower single calls. Until 07.11.2026
// they used -vsync, which the FFmpeg 9.0 manual no longer lists.
func TestLiveBulkPathsWork(t *testing.T) {
	ffmpeg, ffprobe := liveFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "short.mp4")
	makeClip(t, ffmpeg, clip, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=12")
	v := probed(t, ffprobe, clip)
	if v.Duration >= bulkDurationLimit || v.Duration < 11*time.Second {
		t.Fatalf("duration %v", v.Duration)
	}
	v.Hashes = make([]uint64, sampleCount)
	if !bulkExtractHashes(ffmpeg, clip, v.Duration.Seconds(), t.TempDir(), &v, false) {
		t.Fatal("bulk hash extraction failed")
	}
	thumbs := t.TempDir()
	if !bulkExtractThumbs(ffmpeg, clip, v.Duration.Seconds(), thumbs, pathHash(clip), false) {
		t.Fatal("bulk thumbnail extraction failed")
	}
	if !allThumbsExist(thumbs, clip) {
		t.Fatal("thumbnails missing")
	}
}
