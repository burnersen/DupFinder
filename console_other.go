// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

//go:build !windows

package main

import (
	"io"
	"time"
)

// Outside Windows a program is started from a terminal that stays open.
func launchedByDoubleClick() bool { return false }

func waitForEnter(io.Reader, io.Writer, time.Duration) {}

func enableColorConsole() bool { return true }
