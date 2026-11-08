//go:build !windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import "errors"

// moveToRecycleBin has no recycle bin to move to outside Windows: the file
// stays where it is.
func moveToRecycleBin(path string) error {
	return errors.Join(errors.New(path+": the recycle bin is only available on Windows"), errBinRefused)
}
