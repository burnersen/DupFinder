//go:build windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	rbKernel32                   = syscall.NewLazyDLL("kernel32.dll")
	rbGetVolumePathName          = rbKernel32.NewProc("GetVolumePathNameW")
	rbGetVolumeNameForMountPoint = rbKernel32.NewProc("GetVolumeNameForVolumeMountPointW")
	rbGetDriveType               = rbKernel32.NewProc("GetDriveTypeW")

	rbShell32         = syscall.NewLazyDLL("shell32.dll")
	rbSHFileOperation = rbShell32.NewProc("SHFileOperationW")
	rbSHQueryBin      = rbShell32.NewProc("SHQueryRecycleBinW")
	rbSHCreateItem    = rbShell32.NewProc("SHCreateItemFromParsingName")

	rbOle32          = syscall.NewLazyDLL("ole32.dll")
	rbCoInitializeEx = rbOle32.NewProc("CoInitializeEx")
	rbCoUninitialize = rbOle32.NewProc("CoUninitialize")
	rbCoCreate       = rbOle32.NewProc("CoCreateInstance")
)

const (
	rbDriveRemovable = 2
	rbDriveRemote    = 4
	rbDriveCDROM     = 5

	// The settings of the bin are per drive under HKEY_CURRENT_USER. Without
	// an entry Windows uses its default, and nothing is judged.
	rbVolumeKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\BitBucket\Volume\`

	rbFODelete          = 0x0003
	rbFOFSilent         = 0x0004
	rbFOFNoConfirmation = 0x0010
	rbFOFAllowUndo      = 0x0040
	rbFOFNoConfirmMkdir = 0x0200
	rbFOFNoErrorUI      = 0x0400
	rbFOFXRecycle       = 0x00080000 // IFileOperation: to the bin, not for good (Windows 8 and newer)
	rbFOFXEarlyFailure  = 0x00100000

	rbCoinitApartment = 0x2
	rbClsctxInproc    = 0x1
	rbRPCChangedMode  = 0x80010106
)

// rbFileOp is SHFILEOPSTRUCTW of 64 bit Windows (8 byte alignment, 56
// bytes; TestRecycleStructSizes).
type rbFileOp struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// rbQueryInfo is SHQUERYRBINFO (24 bytes): how much is in the bin.
type rbQueryInfo struct {
	cbSize   uint32
	bytes    int64
	numItems int64
}

type rbGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	rbCLSIDFileOperation = rbGUID{0x3ad05575, 0x8857, 0x4850, [8]byte{0x92, 0x77, 0x11, 0xb8, 0x5b, 0xdb, 0x8e, 0x09}}
	rbIIDFileOperation   = rbGUID{0x947aab5f, 0x0a5c, 0x4c13, [8]byte{0xb4, 0xd6, 0x4b, 0xf7, 0x83, 0x6f, 0xc9, 0xf8}}
	rbIIDShellItem       = rbGUID{0x43826d1e, 0xe718, 0x42ee, [8]byte{0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}}
)

// Places in the vtable of IFileOperation (after IUnknown: 0 to 2).
const (
	rbIdxRelease           = 2
	rbIdxSetOperationFlags = 5
	rbIdxDeleteItem        = 18
	rbIdxPerformOperations = 21
)

// moveToRecycleBin moves a file or folder to the recycle bin, but only when
// the bin can take it. Otherwise it stays, and the error (errBinRefused)
// says why. After the move it checks that the bin grew; if not, the error
// is errRecycleNotVerified: the file is gone and not in the bin.
func moveToRecycleBin(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if isUNC(abs) {
		return fmt.Errorf("%s: a network drive has no recycle bin: %w", abs, errBinRefused)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	size := info.Size()
	if info.IsDir() {
		size = -1 // the size of a folder is not judged; the check afterwards still is
	}
	facts := binFactsOf(abs)
	if reason := binRefuses(facts, size); reason != "" {
		return fmt.Errorf("%s: %s: %w", abs, reason, errBinRefused)
	}
	beforeItems, beforeBytes, haveBefore := binContents(facts.root)

	if err := recycleViaIFileOperation(abs); err != nil {
		// The old way, for systems without the COM interface. It knows
		// paths up to 260 characters only.
		if err2 := recycleViaSHFileOperation(abs); err2 != nil {
			return fmt.Errorf("move %s to the recycle bin: %w", abs, errors.Join(err, err2))
		}
	}
	if _, err := os.Lstat(abs); err == nil {
		return fmt.Errorf("move %s to the recycle bin: Windows left it where it is", abs)
	}
	if haveBefore {
		if afterItems, afterBytes, ok := binContents(facts.root); ok && !binGrew(beforeItems, beforeBytes, afterItems, afterBytes) {
			return fmt.Errorf("%s: %w", abs, errRecycleNotVerified)
		}
	}
	return nil
}

// binFactsOf asks Windows about the bin of the drive that holds path. What
// Windows does not answer stays unknown and is not judged.
func binFactsOf(path string) binFacts {
	var f binFacts
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return f
	}
	buf := make([]uint16, 1024)
	if r, _, _ := rbGetVolumePathName.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return f
	}
	f.root = syscall.UTF16ToString(buf)
	root, err := syscall.UTF16PtrFromString(f.root)
	if err != nil {
		return f
	}
	switch r, _, _ := rbGetDriveType.Call(uintptr(unsafe.Pointer(root))); r {
	case rbDriveRemote:
		f.network = true
	case rbDriveRemovable:
		f.removable = true
	case rbDriveCDROM:
		f.cdrom = true
	}
	name := make([]uint16, 64) // \\?\Volume{GUID}\ has 49 characters
	if r, _, _ := rbGetVolumeNameForMountPoint.Call(uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name))); r == 0 {
		return f
	}
	guid := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(syscall.UTF16ToString(name), `\\?\`), `\`), "Volume")
	key := rbVolumeKey + guid
	if v, ok := rbRegDWORD(key, "NukeOnDelete"); ok && v == 1 {
		f.off = true
	}
	f.capMB, f.capKnown = rbRegDWORD(key, "MaxCapacity")
	return f
}

// binContents is the number of items and bytes in the bin of a drive.
func binContents(root string) (items, bytes int64, ok bool) {
	if root == "" {
		return 0, 0, false
	}
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, false
	}
	info := rbQueryInfo{cbSize: uint32(unsafe.Sizeof(rbQueryInfo{}))}
	if hr, _, _ := rbSHQueryBin.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&info))); hr != 0 {
		return 0, 0, false
	}
	return info.numItems, info.bytes, true
}

// rbRegDWORD reads a DWORD under HKEY_CURRENT_USER. A missing value is no
// error, only "not set".
func rbRegDWORD(subKey, name string) (uint32, bool) {
	k, err := syscall.UTF16PtrFromString(subKey)
	if err != nil {
		return 0, false
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, k, 0, syscall.KEY_READ, &h); err != nil {
		return 0, false
	}
	defer syscall.RegCloseKey(h) // ignore: closing a key that was only read
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, false
	}
	var typ, data uint32
	size := uint32(unsafe.Sizeof(data))
	if err := syscall.RegQueryValueEx(h, n, nil, &typ, (*byte)(unsafe.Pointer(&data)), &size); err != nil || typ != syscall.REG_DWORD {
		return 0, false
	}
	return data, true
}

// recycleViaIFileOperation is the way Microsoft recommends since Vista; it
// handles paths far beyond 260 characters.
func recycleViaIFileOperation(abs string) error {
	runtime.LockOSThread() // COM belongs to one thread
	defer runtime.UnlockOSThread()
	hr, _, _ := rbCoInitializeEx.Call(0, rbCoinitApartment)
	if uint32(hr) != rbRPCChangedMode { // another mode is active: then do not uninitialise
		defer rbCoUninitialize.Call() // ignore: has no result
	}
	var op unsafe.Pointer
	hr, _, _ = rbCoCreate.Call(uintptr(unsafe.Pointer(&rbCLSIDFileOperation)), 0, rbClsctxInproc,
		uintptr(unsafe.Pointer(&rbIIDFileOperation)), uintptr(unsafe.Pointer(&op)))
	if int32(hr) < 0 || op == nil {
		return fmt.Errorf("CoCreateInstance(IFileOperation): hr=0x%x", uint32(hr))
	}
	defer rbRelease(op)
	flags := uintptr(rbFOFSilent | rbFOFNoConfirmation | rbFOFNoErrorUI | rbFOFNoConfirmMkdir | rbFOFAllowUndo | rbFOFXRecycle | rbFOFXEarlyFailure)
	if hr := rbCall(op, rbIdxSetOperationFlags, flags); int32(hr) < 0 {
		return fmt.Errorf("IFileOperation.SetOperationFlags: hr=0x%x", uint32(hr))
	}
	item, err := rbShellItem(abs)
	if err != nil {
		return err
	}
	defer rbRelease(item)
	if hr := rbCall(op, rbIdxDeleteItem, uintptr(item), 0); int32(hr) < 0 {
		return fmt.Errorf("IFileOperation.DeleteItem: hr=0x%x", uint32(hr))
	}
	if hr := rbCall(op, rbIdxPerformOperations); int32(hr) < 0 {
		return fmt.Errorf("IFileOperation.PerformOperations: hr=0x%x", uint32(hr))
	}
	return nil
}

// rbShellItem makes an IShellItem from a path; long paths get the \\?\
// prefix, which gets around the 260 character limit of the parser.
func rbShellItem(abs string) (unsafe.Pointer, error) {
	parse := abs
	if len(abs) >= 240 && !strings.HasPrefix(abs, `\\?\`) {
		parse = `\\?\` + abs
	}
	p, err := syscall.UTF16PtrFromString(parse)
	if err != nil {
		return nil, err
	}
	var item unsafe.Pointer
	hr, _, _ := rbSHCreateItem.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&rbIIDShellItem)), uintptr(unsafe.Pointer(&item)))
	runtime.KeepAlive(p)
	if int32(hr) < 0 || item == nil {
		return nil, fmt.Errorf("SHCreateItemFromParsingName: hr=0x%x", uint32(hr))
	}
	return item, nil
}

// rbCall calls the method at place index of the vtable of a COM object.
// this stays an unsafe.Pointer, so go vet sees no misuse of unsafe.Pointer.
func rbCall(this unsafe.Pointer, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(this)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(this)}, args...)...)
	return r
}

func rbRelease(this unsafe.Pointer) {
	if this != nil {
		rbCall(this, rbIdxRelease)
	}
}

// recycleViaSHFileOperation is the old way (paths up to 260 characters,
// without the \\?\ prefix, which it does not understand).
func recycleViaSHFileOperation(abs string) error {
	from, err := syscall.UTF16FromString(strings.TrimPrefix(abs, `\\?\`))
	if err != nil {
		return err
	}
	from = append(from, 0) // the list ends with two zeros
	op := rbFileOp{
		wFunc:  rbFODelete,
		pFrom:  &from[0],
		fFlags: rbFOFAllowUndo | rbFOFNoConfirmation | rbFOFSilent | rbFOFNoErrorUI,
	}
	r, _, _ := rbSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(from)
	if r != 0 {
		return fmt.Errorf("SHFileOperation: code %d", r)
	}
	if op.fAnyOperationsAborted != 0 {
		return errors.New("SHFileOperation: aborted")
	}
	return nil
}
