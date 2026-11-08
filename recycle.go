// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// Building block "recycle bin" (kit 03-error-handling.md, F16). Files:
// recycle.go, recycle_windows.go, recycle_other.go, recycle_test.go,
// recycle_windows_test.go.
//
// Windows deletes a file for good, without a word, when the recycle bin
// cannot take it: the file is bigger than the limit of the bin, the drive
// has no bin (network drive, USB stick, \\server\share) or the bin is
// switched off for the drive. The flag that keeps the shell quiet
// (FOF_NOCONFIRMATION) answers its question "delete it for good?" with yes.
// So moveToRecycleBin checks before (binRefuses) and after (binGrew), and
// keeps the file with an error when the bin cannot take it.
//
// Adapted from NVENCForge (the checks) and DupFinder (IFileOperation for
// long paths).

import (
	"errors"
	"fmt"
	"strings"
)

// errBinRefused: the bin cannot take the file, it was left where it is.
var errBinRefused = errors.New("kept where it is")

// errRecycleNotVerified: Windows deleted the file, but the bin did not grow,
// so the file is most likely gone for good. Say that plainly, never "kept".
var errRecycleNotVerified = errors.New("the file was deleted, but it did not arrive in the recycle bin")

// binFacts is what Windows tells about the recycle bin of one drive.
type binFacts struct {
	root      string // mount point, e.g. C:\
	network   bool   // network drive or \\server\share
	removable bool   // USB stick, memory card
	cdrom     bool   // CD or DVD
	off       bool   // NukeOnDelete=1: the bin is switched off for this drive
	capMB     uint32 // MaxCapacity in MB
	capKnown  bool   // capMB was read; without it Windows decides and we do not judge
}

const binBytesPerMB = 1024 * 1024

// binRefuses says why the bin cannot take a file of size bytes, or "" when
// it can. size < 0 means unknown: then the limit is not judged, and the
// check afterwards (binGrew) is the net.
func binRefuses(f binFacts, size int64) string {
	switch {
	case f.network:
		return "a network drive has no recycle bin"
	case f.removable:
		return "the removable drive " + f.root + " has no recycle bin"
	case f.cdrom:
		return "the drive " + f.root + " has no recycle bin"
	case f.off:
		return "the recycle bin is switched off for " + f.root
	case f.capKnown && size >= 0 && (f.capMB == 0 || size > int64(f.capMB)*binBytesPerMB):
		return fmt.Sprintf("the file is %s, but the recycle bin of %s holds only %s",
			binSizeText(size), f.root, binSizeText(int64(f.capMB)*binBytesPerMB))
	}
	return ""
}

// binGrew compares the bin before and after. More items or more bytes are
// enough: when the bin is full, Windows throws older items out at the same
// time, so the count can drop while the size grows. Someone emptying the bin
// at the same moment can fool it; the check before is the real protection.
func binGrew(beforeItems, beforeBytes, afterItems, afterBytes int64) bool {
	return afterItems > beforeItems || afterBytes > beforeBytes
}

// isUNC reports network paths: \\server\share and \\?\UNC\server\share, but
// not the long path form of a local drive (\\?\C:\...).
func isUNC(path string) bool {
	upper := strings.ToUpper(path)
	return strings.HasPrefix(upper, `\\?\UNC\`) ||
		(strings.HasPrefix(path, `\\`) && !strings.HasPrefix(path, `\\?\`) && !strings.HasPrefix(path, `\\.\`))
}

// binSizeText writes sizes the way people read them: MB below 1 GB, then GB.
func binSizeText(bytes int64) string {
	mb := float64(bytes) / binBytesPerMB
	if mb < 1024 {
		return fmt.Sprintf("%.0f MB", mb)
	}
	return fmt.Sprintf("%.1f GB", mb/1024)
}
