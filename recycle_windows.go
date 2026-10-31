//go:build windows

// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// ============================================================================
// Papierkorb-Löschung
//
// Primärweg: IFileOperation (COM, ab Windows Vista). Microsofts empfohlener
// Ersatz für SHFileOperation; unterstützt Pfade weit über MAX_PATH (260 Zeichen)
// hinaus – wichtig für tief verschachtelte Videoordner.
//
// Fallback: das bewährte SHFileOperation. Greift, falls COM nicht verfügbar ist
// oder IFileOperation einen Fehler liefert. Für normale (kurze) Pfade voll
// funktionsfähig, sodass nie Funktionalität verloren geht.
// ============================================================================

const (
	FO_DELETE          = 0x0003
	FOF_SILENT         = 0x0004
	FOF_NOCONFIRMATION = 0x0010
	FOF_ALLOWUNDO      = 0x0040
	FOF_NOCONFIRMMKDIR = 0x0200
	FOF_NOERRORUI      = 0x0400

	// IFileOperation-spezifische Flags
	FOF_NO_UI            = FOF_SILENT | FOF_NOCONFIRMATION | FOF_NOERRORUI | FOF_NOCONFIRMMKDIR
	FOFX_RECYCLEONDELETE = 0x00080000 // DeleteItem → Papierkorb statt endgültig (Win8+)
	FOFX_EARLYFAILURE    = 0x00100000

	// CoInitializeEx / CoCreateInstance
	COINIT_APARTMENTTHREADED = 0x2
	CLSCTX_INPROC_SERVER     = 0x1
)

type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// CLSID_FileOperation  {3ad05575-8857-4850-9277-11b85bdb8e09}
var clsidFileOperation = guid{0x3ad05575, 0x8857, 0x4850, [8]byte{0x92, 0x77, 0x11, 0xb8, 0x5b, 0xdb, 0x8e, 0x09}}

// IID_IFileOperation   {947aab5f-0a5c-4c13-b4d6-4bf7836fc9f8}
var iidFileOperation = guid{0x947aab5f, 0x0a5c, 0x4c13, [8]byte{0xb4, 0xd6, 0x4b, 0xf7, 0x83, 0x6f, 0xc9, 0xf8}}

// IID_IShellItem       {43826d1e-e718-42ee-bc55-a1e261c37bfe}
var iidShellItem = guid{0x43826d1e, 0xe718, 0x42ee, [8]byte{0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}}

var (
	shell32             = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperation = shell32.NewProc("SHFileOperationW")
	procSHCreateItem    = shell32.NewProc("SHCreateItemFromParsingName")

	ole32              = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninit       = ole32.NewProc("CoUninitialize")
	procCoCreate       = ole32.NewProc("CoCreateInstance")
)

// moveToRecycleBin löscht path in den Papierkorb. Erst der moderne, long-path-
// fähige Weg; bei jedem Fehler automatischer Rückfall auf SHFileOperation.
func moveToRecycleBin(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	if err := recycleViaIFileOperation(absPath); err == nil {
		return nil
	}
	// Fallback – bewährter Legacy-Weg (MAX_PATH-begrenzt, aber stabil).
	return recycleViaSHFileOperation(absPath)
}

// ----------------------------------------------------------------------------
// Primärweg: IFileOperation (COM)
// ----------------------------------------------------------------------------

// vtable-Indizes von IFileOperation (erbt IUnknown: QI=0, AddRef=1, Release=2).
const (
	idxRelease           = 2  // IUnknown::Release
	idxSetOperationFlags = 5  // IFileOperation::SetOperationFlags
	idxDeleteItem        = 18 // IFileOperation::DeleteItem
	idxPerformOps        = 21 // IFileOperation::PerformOperations
)

func recycleViaIFileOperation(absPath string) error {
	// COM braucht einen festen OS-Thread (Apartment-Modell).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED)
	// S_OK(0) und S_FALSE(1) = ok (S_FALSE: bereits initialisiert). RPC_E_
	// CHANGED_MODE(0x80010106) = anderer Modus aktiv, dann KEIN Uninit aufrufen.
	const rpcChangedMode = 0x80010106
	if uint32(hr) != rpcChangedMode {
		defer procCoUninit.Call()
	}

	// IFileOperation-Instanz erzeugen.
	var pfo unsafe.Pointer
	hr, _, _ = procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidFileOperation)),
		0,
		CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidFileOperation)),
		uintptr(unsafe.Pointer(&pfo)),
	)
	if int32(hr) < 0 || pfo == nil {
		return fmt.Errorf("CoCreateInstance(IFileOperation) hr=0x%x", uint32(hr))
	}
	defer release(pfo)

	// In den Papierkorb statt endgültig löschen, ohne UI/Rückfragen.
	if hr := callMethod(pfo, idxSetOperationFlags,
		uintptr(FOF_NO_UI|FOF_ALLOWUNDO|FOFX_RECYCLEONDELETE|FOFX_EARLYFAILURE)); int32(hr) < 0 {
		return fmt.Errorf("SetOperationFlags hr=0x%x", uint32(hr))
	}

	// IShellItem aus dem Pfad bauen (long-path-fähig).
	psi, err := shellItemFromPath(absPath)
	if err != nil {
		return err
	}
	defer release(psi)

	// DeleteItem(psi, NULL) – psi wird als reines Aufruf-Argument übergeben.
	if hr := callMethod(pfo, idxDeleteItem, uintptr(psi), 0); int32(hr) < 0 {
		return fmt.Errorf("DeleteItem hr=0x%x", uint32(hr))
	}
	// PerformOperations()
	if hr := callMethod(pfo, idxPerformOps); int32(hr) < 0 {
		return fmt.Errorf("PerformOperations hr=0x%x", uint32(hr))
	}
	return nil
}

// shellItemFromPath erzeugt ein IShellItem. Bei sehr langen Pfaden wird das
// \\?\-Präfix vorangestellt, damit die MAX_PATH-Parser-Grenze umgangen wird.
func shellItemFromPath(absPath string) (unsafe.Pointer, error) {
	parse := absPath
	if len(absPath) >= 240 && !strings.HasPrefix(absPath, `\\?\`) {
		if strings.HasPrefix(absPath, `\\`) {
			parse = `\\?\UNC\` + absPath[2:] // UNC-Netzwerkpfad
		} else {
			parse = `\\?\` + absPath
		}
	}
	p, err := syscall.UTF16PtrFromString(parse)
	if err != nil {
		return nil, err
	}
	var psi unsafe.Pointer
	hr, _, _ := procSHCreateItem.Call(
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(unsafe.Pointer(&iidShellItem)),
		uintptr(unsafe.Pointer(&psi)),
	)
	runtime.KeepAlive(p)
	if int32(hr) < 0 || psi == nil {
		return nil, fmt.Errorf("SHCreateItemFromParsingName hr=0x%x", uint32(hr))
	}
	return psi, nil
}

// callMethod ruft die Methode an vtable[index] eines COM-Objekts auf.
// this zeigt auf das COM-Objekt, args sind die Argumente NACH dem this-Zeiger.
// this wird als unsafe.Pointer geführt (nicht als uintptr), damit die Zeiger-
// Arithmetik über unsafe.Add läuft und `go vet` keine „misuse of unsafe.Pointer"
// meldet. Die Speicheroperationen sind identisch zu vorher.
func callMethod(this unsafe.Pointer, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(this) // erstes Feld = Zeiger auf die vtable
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*unsafe.Sizeof(uintptr(0))))
	call := append([]uintptr{uintptr(this)}, args...)
	r, _, _ := syscall.SyscallN(fn, call...)
	return r
}

func release(this unsafe.Pointer) {
	if this != nil {
		callMethod(this, idxRelease)
	}
}

// ----------------------------------------------------------------------------
// Fallback: SHFileOperation (Legacy, MAX_PATH-begrenzt)
// ----------------------------------------------------------------------------

func recycleViaSHFileOperation(absPath string) error {
	// Die alte Shell-API versteht das \\?\-Langpfad-Präfix NICHT.
	cleanPath := strings.TrimPrefix(absPath, `\\?\`)

	utf16Path, err := syscall.UTF16FromString(cleanPath)
	if err != nil {
		return err
	}
	utf16Path = append(utf16Path, 0) // double-null-terminated

	op := shFileOpStruct{
		wFunc:  FO_DELETE,
		pFrom:  &utf16Path[0],
		fFlags: FOF_ALLOWUNDO | FOF_NOCONFIRMATION | FOF_SILENT | FOF_NOERRORUI,
	}
	ret, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(utf16Path)
	if ret != 0 {
		return fmt.Errorf("SHFileOperation: code=%d (Pfad evtl. zu lang oder Laufwerk ohne Papierkorb-Support)", ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("operation aborted")
	}
	return nil
}
