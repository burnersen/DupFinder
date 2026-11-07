// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// Where ffmpeg.exe and ffprobe.exe come from, and the scratch folder for the
// frames DupFinder hashes.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type binaryPaths struct {
	ffprobe string // ffprobe.exe in the bin folder next to DupFinder.exe
	ffmpeg  string // ffmpeg.exe, same place
	tempDir string // scratch folder for hash frames, removed by cleanup
}

// ensureBinaries makes sure ffmpeg.exe and ffprobe.exe are in binDir (the
// folder "bin" next to the exe) and nowhere else. If one is missing, the
// pinned FFmpeg build is downloaded (download.go): it is not shipped with
// DupFinder. A copy the user put there is used as it is.
func ensureBinaries(binDir string) (*binaryPaths, error) {
	ffmpegPath := filepath.Join(binDir, "ffmpeg.exe")
	ffprobePath := filepath.Join(binDir, "ffprobe.exe")

	if fileExists(ffmpegPath) && fileExists(ffprobePath) {
		fmt.Println("[OK] FFmpeg found.")
	} else {
		fmt.Println("[...] FFmpeg is missing, downloading it once (about 190 MB)...")
		if err := downloadFFmpeg(appCtx, binDir); err != nil {
			var ue *UserError
			if errors.As(err, &ue) {
				ue.Hint += " Or put ffmpeg.exe and ffprobe.exe (Windows build from https://github.com/BtbN/FFmpeg-Builds) into " + binDir
				return nil, ue
			}
			return nil, userError(err, "Cannot set up FFmpeg",
				"Put ffmpeg.exe and ffprobe.exe (Windows build from https://github.com/BtbN/FFmpeg-Builds) into "+binDir)
		}
		if !fileExists(ffmpegPath) || !fileExists(ffprobePath) {
			return nil, fmt.Errorf("ffmpeg.exe or ffprobe.exe still missing after the download")
		}
	}

	tempDir, err := os.MkdirTemp("", "dupfinder-scratch-*")
	if err != nil {
		return nil, fmt.Errorf("create a scratch folder: %w", err)
	}
	return &binaryPaths{ffprobe: ffprobePath, ffmpeg: ffmpegPath, tempDir: tempDir}, nil
}

// fileExists reports whether p is an existing, non-empty file (no folder).
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func (bp *binaryPaths) cleanup() {
	if bp != nil && bp.tempDir != "" {
		os.RemoveAll(bp.tempDir) // ignore: only scratch frames, the system cleans its temp folder too
	}
}
