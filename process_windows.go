// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

//go:build windows

package main

// FFmpeg and FFprobe end together with DupFinder, even if DupFinder itself is
// killed or crashes (dev-starterkit 08-sicherheit.md S7): each one is put
// into a job object that Windows closes with DupFinder's last handle, and
// "kill on close" then ends every process still in it. Only the processes
// started through startTracked are in the job, never the browser.

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

// JOBOBJECT_EXTENDED_LIMIT_INFORMATION, laid out as in winnt.h.
type jobExtendedLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
	IoCounters              [6]uint64
	ProcessMemoryLimit      uintptr
	JobMemoryLimit          uintptr
	PeakProcessMemoryUsed   uintptr
	PeakJobMemoryUsed       uintptr
}

var (
	jobOnce   sync.Once
	jobHandle uintptr
)

// childJob creates the job once. Its handle stays open on purpose: it closes
// when DupFinder ends, however it ends. 0 means no job (logged once).
func childJob() uintptr {
	jobOnce.Do(func() {
		h, _, err := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			slog.Warn("no job object, FFmpeg may outlive a crash", "err", err)
			return
		}
		var info jobExtendedLimit
		info.LimitFlags = jobObjectLimitKillOnJobClose
		if r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
			syscall.CloseHandle(syscall.Handle(h)) // ignore: unused handle
			slog.Warn("no job object, FFmpeg may outlive a crash", "err", err)
			return
		}
		jobHandle = h
	})
	return jobHandle
}

// startTracked starts cmd and puts the new process into the job.
func startTracked(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	job := childJob()
	if job == 0 {
		return nil
	}
	if err := assignToJob(job, cmd.Process.Pid); err != nil {
		// The process runs anyway; it is only not tied to DupFinder.
		slog.Debug("process not in job object", "pid", cmd.Process.Pid, "err", err)
	}
	return nil
}

func assignToJob(job uintptr, pid int) error {
	p, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	defer syscall.CloseHandle(p) // ignore: the job keeps its own reference
	if r, _, err := procAssignProcessToJobObject.Call(job, uintptr(p)); r == 0 {
		return fmt.Errorf("assign to job: %w", err)
	}
	return nil
}
