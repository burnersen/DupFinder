// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"io"
	"os"
)

// output is everything the user reads in the console. Results and progress
// go to stdout, warnings and errors to stderr. Colour only when stdout is a
// terminal and NO_COLOR is not set (https://no-color.org).
//
// The log file is separate (logging.go): the console says what the user can
// do, the log says what exactly happened.
type output struct {
	stdout, stderr io.Writer
	color          bool
}

func newOutput(stdout, stderr io.Writer) *output {
	return &output{stdout: stdout, stderr: stderr, color: wantColor(stdout)}
}

func wantColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return enableColorConsole()
}

const (
	colorReset  = "\x1b[0m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorRed    = "\x1b[31m"
	colorCyan   = "\x1b[36m"
)

func (o *output) line(w io.Writer, color, prefix, msg string) {
	if o.color {
		fmt.Fprintf(w, "%s%s%s %s\n", color, prefix, colorReset, msg)
		return
	}
	fmt.Fprintf(w, "%s %s\n", prefix, msg)
}

// Step announces something that takes a while: "[...] Reading 3 files".
func (o *output) Step(msg string) { o.line(o.stdout, colorCyan, "[...]", msg) }

// OK reports one finished thing.
func (o *output) OK(msg string) { o.line(o.stdout, colorGreen, "[OK]", msg) }

// Info is neutral information.
func (o *output) Info(msg string) { o.line(o.stdout, colorCyan, "[INFO]", msg) }

// Warn: something is not as it should be, the program carries on.
func (o *output) Warn(msg string) { o.line(o.stderr, colorYellow, "[WARN]", msg) }

// Error: something failed. Always followed by a Hint where one exists.
func (o *output) Error(msg string) { o.line(o.stderr, colorRed, "[ERROR]", msg) }

// Hint says what the user can do, indented under the error.
func (o *output) Hint(msg string) { fmt.Fprintf(o.stderr, "        %s\n", msg) }
