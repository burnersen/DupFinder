//go:build windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ============================================================================
// "Show in folder": select the file in Explorer and bring the window to the front
//
// Main way: SHOpenFolderAndSelectItems (shell API), the same way Explorer
// uses internally for "/select": it selects the file reliably and scrolls
// it into view.
//
// Foreground: Windows normally does not let a background program (here the
// local server, triggered by the browser) push a window in front of the
// active one ("focus protection"). The reliable way is to find the newly
// opened Explorer window and force it to the front with the
// AttachThreadInput trick: attach our own input to that of the active
// program for a moment, so SetForegroundWindow really works, and detach
// again at once. Works in nearly every case, but not guaranteed 100 %
// (e.g. not against a full-screen program in front). Only window commands
// are used: no risk for data.
//
// Fallback: the proven "explorer /select,", used if the shell API is not
// available or fails, like the recycle bin code falls back from
// IFileOperation to SHFileOperation.
//
// COM initialisation (procCoInitializeEx/procCoUninit, COINIT_*) and the
// shell32 handle come from recycle_windows.go (same package main).
// ============================================================================

var (
	procILCreateFromPath           = shell32.NewProc("ILCreateFromPathW")
	procSHOpenFolderAndSelectItems = shell32.NewProc("SHOpenFolderAndSelectItems")
	procILFree                     = shell32.NewProc("ILFree")
	procSHGetFileInfo              = shell32.NewProc("SHGetFileInfoW")

	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetClassName             = user32.NewProc("GetClassNameW")
	procGetWindowText            = user32.NewProc("GetWindowTextW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")

	// kernel32 is declared in console_windows.go.
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

const (
	shgfiDisplayName = 0x00000200
	swRestore        = 9
)

// revealInExplorer opens the folder of the file and selects it there.
func revealInExplorer(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	if err := revealViaShell(absPath); err == nil {
		return nil
	}
	return revealViaExplorerSelect(absPath)
}

// revealViaShell is the main way, through SHOpenFolderAndSelectItems.
func revealViaShell(absPath string) error {
	// If the APIs do not exist on this system: fail cleanly, so the caller
	// takes the fallback (instead of panicking).
	if err := procILCreateFromPath.Find(); err != nil {
		return err
	}
	if err := procSHOpenFolderAndSelectItems.Find(); err != nil {
		return err
	}

	// COM needs a fixed OS thread (apartment model).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED)
	// S_OK (0) and S_FALSE (1) are fine. RPC_E_CHANGED_MODE: another mode is
	// active, then do NOT call CoUninitialize.
	const rpcChangedMode = 0x80010106
	if uint32(hr) != rpcChangedMode {
		defer procCoUninit.Call()
	}

	pathPtr, err := syscall.UTF16PtrFromString(absPath)
	if err != nil {
		return err
	}
	// ILCreateFromPath returns the full item ID list (PIDL) of the file.
	pidl, _, _ := procILCreateFromPath.Call(uintptr(unsafe.Pointer(pathPtr)))
	runtime.KeepAlive(pathPtr)
	if pidl == 0 {
		return fmt.Errorf("ILCreateFromPath returned NULL for %q", absPath)
	}
	defer procILFree.Call(pidl)

	// pidl is the path of the FILE; with cidl=0 and apidl=NULL the shell opens
	// the parent folder and selects the file in it.
	hr, _, _ = procSHOpenFolderAndSelectItems.Call(pidl, 0, 0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("SHOpenFolderAndSelectItems hr=0x%x", uint32(hr))
	}

	// Bring the window to the front asynchronously, so the HTTP answer does not
	// wait for the short window search.
	dir := filepath.Dir(absPath)
	displayName := folderDisplayName(dir)
	go bringFolderWindowToFront(displayName, dir)
	return nil
}

// folderDisplayName returns the name Explorer shows in the window title
// (e.g. "Downloads" or "Local Disk (C:)"). That finds the right window more
// reliably than the bare folder name.
func folderDisplayName(dir string) string {
	pathPtr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return ""
	}
	var sfi shFileInfoW
	r, _, _ := procSHGetFileInfo.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		0,
		uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi),
		shgfiDisplayName,
	)
	runtime.KeepAlive(pathPtr)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(sfi.szDisplayName[:])
}

type shFileInfoW struct {
	hIcon         uintptr
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

// State of the window search. EnumWindows calls its callback synchronously
// in the same thread; the state is guarded by a mutex and NOT passed in
// through lParam. That avoids a uintptr to unsafe.Pointer conversion (which
// go vet rightly complains about) and still shares the state safely.
type findWindowCtx struct {
	wantDisplay string // display name of the folder (default window title)
	wantDir     string // full path (if "show the full path in the title" is on)
	found       uintptr
}

var (
	enumSearchMu sync.Mutex
	enumSearch   findWindowCtx
)

// enumWindowsCallback looks for a visible Explorer window whose title fits
// the folder. Returns 1 = go on searching, 0 = found, stop.
var enumWindowsCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
	if r, _, _ := procIsWindowVisible.Call(hwnd); r == 0 {
		return 1
	}
	class := getWindowText(hwnd, procGetClassName)
	if class != "CabinetWClass" && class != "ExploreWClass" {
		return 1
	}
	title := getWindowText(hwnd, procGetWindowText)
	if strings.EqualFold(title, enumSearch.wantDisplay) || strings.EqualFold(title, enumSearch.wantDir) {
		enumSearch.found = hwnd
		return 0
	}
	return 1
})

// getWindowText reads the class name or the window title (GetClassNameW
// and GetWindowTextW are called the same way).
func getWindowText(hwnd uintptr, proc *syscall.LazyProc) string {
	var buf [512]uint16
	n, _, _ := proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// bringFolderWindowToFront looks for the right Explorer window (polls
// briefly, a newly opened window needs a moment) and forces it to the
// front. If it finds nothing, nothing bad happens: the window is open
// anyway, perhaps behind others.
func bringFolderWindowToFront(displayName, dir string) {
	if displayName == "" {
		displayName = filepath.Base(dir)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		if hwnd := findExplorerWindow(displayName, dir); hwnd != 0 {
			forceForeground(hwnd)
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// findExplorerWindow searches all top-level windows synchronously for a
// fitting Explorer window and returns its handle (0 = not found).
func findExplorerWindow(displayName, dir string) uintptr {
	enumSearchMu.Lock()
	defer enumSearchMu.Unlock()
	enumSearch = findWindowCtx{wantDisplay: displayName, wantDir: dir}
	procEnumWindows.Call(enumWindowsCallback, 0)
	return enumSearch.found
}

// forceForeground brings hwnd to the front. Trick: attach our own input to
// that of the active program for a moment, so SetForegroundWindow works.
func forceForeground(hwnd uintptr) {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == hwnd {
		return
	}

	var fgThread uintptr
	if fg != 0 {
		fgThread, _, _ = procGetWindowThreadProcessId.Call(fg, 0)
	}
	myThread, _, _ := procGetCurrentThreadID.Call()

	attached := false
	if fgThread != 0 && fgThread != myThread {
		if r, _, _ := procAttachThreadInput.Call(myThread, fgThread, 1); r != 0 {
			attached = true
		}
	}

	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)

	if attached {
		procAttachThreadInput.Call(myThread, fgThread, 0)
	}
}

// revealViaExplorerSelect is the fallback through "explorer /select,".
//
// The command line is built by hand: Explorer recognises /select only when
// the quotes are ONLY around the path (explorer /select,"C:\..."). With
// normal argument passing Go would quote the whole token for paths with
// spaces and the switch would be ignored. A " is not allowed in Windows
// paths, so embedding it in "..." is safe; the caller has checked the path
// with pathIsKnown as well.
func revealViaExplorerSelect(path string) error {
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `explorer /select,"` + path + `"`,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaper goroutine: avoids zombie processes. explorer.exe often returns
	// exit code 1 even on success: the Wait error is ignored on purpose.
	go func() { _ = cmd.Wait() }() // ignore: see the comment above
	return nil
}
