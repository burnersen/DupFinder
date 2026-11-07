// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

//go:build !windows

package main

import "os/exec"

// startTracked: outside Windows there is no job object; the context of the
// command still ends FFmpeg when DupFinder stops.
func startTracked(cmd *exec.Cmd) error { return cmd.Start() }
