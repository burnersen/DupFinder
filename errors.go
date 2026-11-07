// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// What the user sees when something fails, and what only goes to the log
// (dev-starterkit 03-fehlerbehandlung.md).

import (
	"errors"
	"fmt"
	"io/fs"
)

// UserError is an error the user can act on.
//
//	Msg  what happened, in plain words ("Cannot read photo.jpg")
//	Hint what the user can do about it ("Check that the file exists.")
//	Err  the technical cause: goes to the log and stays reachable for
//	     errors.Is and errors.As, but is never the headline on screen
type UserError struct {
	Msg  string
	Hint string
	Err  error
}

func (e *UserError) Error() string {
	if e.Err == nil {
		return e.Msg
	}
	return e.Msg + ": " + e.Err.Error()
}

func (e *UserError) Unwrap() error { return e.Err }

// userError wraps err with a message and a hint for the user.
func userError(err error, msg, hint string) error {
	return &UserError{Msg: msg, Hint: hint, Err: err}
}

// fileError explains the usual file problems in words a user can act on.
// Everything else keeps its technical text, but still gets a hint.
func fileError(err error, action, path string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return userError(err, fmt.Sprintf("Cannot %s %s: it does not exist", action, path),
			"Check the name and the folder. Paths with spaces need quotes.")
	case errors.Is(err, fs.ErrPermission):
		return userError(err, fmt.Sprintf("Cannot %s %s: no permission", action, path),
			"Check that you may access it, or that no other program has it open.")
	default:
		return userError(err, fmt.Sprintf("Cannot %s %s", action, path),
			"Try again. If it keeps failing, the log file has the details.")
	}
}
