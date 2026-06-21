//go:build windows

// DupFinder — Required Notice: Copyright (c) 2026 burnersen — DupFinder
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

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
// "Ordner öffnen" – Datei im Explorer markieren und das Fenster nach vorne holen
//
// Primärweg: SHOpenFolderAndSelectItems (Shell-API). Das ist derselbe Weg, den
// der Explorer intern für "/select" nutzt – er markiert die Datei zuverlässig
// und scrollt sie ins Bild.
//
// Vordergrund: Windows erlaubt einem Hintergrund-Programm (hier: der lokale
// Server, vom Browser angestoßen) normalerweise nicht, ein Fenster vor das
// aktive zu schieben ("Fokus-Schutz"). Der zuverlässige Weg ist, das frisch
// geöffnete Explorer-Fenster zu suchen und es mit dem AttachThreadInput-Trick
// nach vorne zu zwingen: Man hängt die eigene Eingabe kurz an die des aktuell
// aktiven Programms an – dadurch greift SetForegroundWindow wirklich durch – und
// löst die Verbindung sofort wieder. Klappt in den allermeisten Fällen, ist aber
// nicht zu 100 % garantiert (z. B. nicht gegen ein Vollbild-Programm im
// Vordergrund). Es werden nur Fenster-Befehle genutzt – kein Risiko für Daten.
//
// Fallback: das bewährte "explorer /select,". Greift, falls die Shell-API nicht
// verfügbar ist oder fehlschlägt – analog zum Papierkorb-Code, der von
// IFileOperation auf SHFileOperation zurückfällt.
//
// COM-Initialisierung (procCoInitializeEx/procCoUninit, COINIT_*) und der
// shell32-Handle stammen aus recycle_windows.go (gleiches package main).
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

	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

const (
	shgfiDisplayName = 0x00000200
	swRestore        = 9
)

// revealInExplorer öffnet den Ordner der Datei und markiert sie darin.
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

// revealViaShell ist der Primärweg über SHOpenFolderAndSelectItems.
func revealViaShell(absPath string) error {
	// Falls die APIs auf diesem System nicht existieren: sauber scheitern,
	// damit der Aufrufer auf den Fallback ausweicht (statt zu panicken).
	if err := procILCreateFromPath.Find(); err != nil {
		return err
	}
	if err := procSHOpenFolderAndSelectItems.Find(); err != nil {
		return err
	}

	// COM braucht einen festen OS-Thread (Apartment-Modell).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED)
	// S_OK(0)/S_FALSE(1) = ok. RPC_E_CHANGED_MODE = anderer Modus aktiv, dann
	// KEIN CoUninitialize aufrufen.
	const rpcChangedMode = 0x80010106
	if uint32(hr) != rpcChangedMode {
		defer procCoUninit.Call()
	}

	pathPtr, err := syscall.UTF16PtrFromString(absPath)
	if err != nil {
		return err
	}
	// ILCreateFromPath liefert die vollständige Item-ID-Liste (PIDL) der Datei.
	pidl, _, _ := procILCreateFromPath.Call(uintptr(unsafe.Pointer(pathPtr)))
	runtime.KeepAlive(pathPtr)
	if pidl == 0 {
		return fmt.Errorf("ILCreateFromPath lieferte NULL für %q", absPath)
	}
	defer procILFree.Call(pidl)

	// pidl ist der Pfad der DATEI; mit cidl=0 und apidl=NULL öffnet die Shell den
	// Elternordner und markiert die Datei darin.
	hr, _, _ = procSHOpenFolderAndSelectItems.Call(pidl, 0, 0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("SHOpenFolderAndSelectItems hr=0x%x", uint32(hr))
	}

	// Fenster nach vorne holen – asynchron, damit die HTTP-Antwort nicht auf die
	// kurze Fenstersuche wartet.
	dir := filepath.Dir(absPath)
	displayName := folderDisplayName(dir)
	go bringFolderWindowToFront(displayName, dir)
	return nil
}

// folderDisplayName liefert den Namen, den der Explorer im Fenstertitel anzeigt
// (z. B. "Downloads" oder "Lokaler Datenträger (C:)"). Darüber finden wir das
// richtige Fenster zuverlässiger als über den reinen Ordnernamen.
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

// Suchzustand für die Fenstersuche. EnumWindows ruft seinen Callback synchron im
// selben Thread auf; wir schützen den Zustand per Mutex und reichen ihn NICHT
// über lParam herein. So vermeiden wir eine uintptr→unsafe.Pointer-Wandlung (die
// go vet zu Recht anmeckert) und teilen den Zustand trotzdem sicher.
type findWindowCtx struct {
	wantDisplay string // Anzeigename des Ordners (Fenstertitel-Standard)
	wantDir     string // voller Pfad (falls "vollständigen Pfad im Titel" aktiv)
	found       uintptr
}

var (
	enumSearchMu sync.Mutex
	enumSearch   findWindowCtx
)

// enumWindowsCallback sucht ein sichtbares Explorer-Fenster, dessen Titel zum
// gesuchten Ordner passt. Rückgabe 1 = weitersuchen, 0 = gefunden/stoppen.
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

// getWindowText liest Klassennamen oder Fenstertitel (gleiche Aufrufform für
// GetClassNameW und GetWindowTextW).
func getWindowText(hwnd uintptr, proc *syscall.LazyProc) string {
	var buf [512]uint16
	n, _, _ := proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// bringFolderWindowToFront sucht das passende Explorer-Fenster (kurzes Pollen,
// weil ein frisch geöffnetes Fenster einen Moment braucht) und zwingt es nach
// vorne. Findet es nichts, passiert nichts Schlimmes – das Fenster ist trotzdem
// offen, nur evtl. im Hintergrund.
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

// findExplorerWindow durchsucht alle Top-Level-Fenster synchron nach einem
// passenden Explorer-Fenster und liefert dessen Handle (0 = nicht gefunden).
func findExplorerWindow(displayName, dir string) uintptr {
	enumSearchMu.Lock()
	defer enumSearchMu.Unlock()
	enumSearch = findWindowCtx{wantDisplay: displayName, wantDir: dir}
	procEnumWindows.Call(enumWindowsCallback, 0)
	return enumSearch.found
}

// forceForeground holt hwnd nach vorne. Trick: kurz die eigene Eingabe an die des
// aktuell aktiven Programms anhängen, damit SetForegroundWindow durchgreift.
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

// revealViaExplorerSelect ist der Fallback über "explorer /select,".
//
// Die Kommandozeile wird von Hand gebaut: Der Explorer erkennt /select nur, wenn
// die Anführungszeichen NUR um den Pfad stehen (explorer /select,"C:\..."). Bei
// normaler Argumentübergabe würde Go bei Pfaden mit Leerzeichen das ganze Token
// einklammern und der Schalter würde ignoriert. Ein " ist unter Windows ein
// verbotenes Pfadzeichen, daher ist das Einbetten in "..." sicher; der Pfad ist
// zudem vom Aufrufer über pathIsKnown geprüft.
func revealViaExplorerSelect(path string) error {
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `explorer /select,"` + path + `"`,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaper-Goroutine: vermeidet Zombie-Prozesse. explorer.exe liefert oft
	// Exit-Code 1 selbst bei Erfolg – der Wait-Fehler wird bewusst ignoriert.
	go func() { _ = cmd.Wait() }()
	return nil
}
