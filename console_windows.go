// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procGetConsoleMode        = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode        = kernel32.NewProc("SetConsoleMode")
)

// launchedByDoubleClick: the console belongs to this process alone, so it was
// opened for it (Explorer, drag and drop) and closes when the program ends.
func launchedByDoubleClick() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

func waitForEnter(in io.Reader, out io.Writer, limit time.Duration) {
	fmt.Fprint(out, "\nPress Enter to close this window (closes by itself in one minute)...")
	done := make(chan struct{})
	go func() {
		bufio.NewReader(in).ReadString('\n') // any input or end of input ends the wait
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// enableColorConsole switches on ANSI colours in the classic Windows console.
// Windows Terminal has them on already; if switching fails, no colours.
func enableColorConsole() bool {
	const enableVirtualTerminalProcessing = 0x0004
	h := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
