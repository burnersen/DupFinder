//go:build !windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
