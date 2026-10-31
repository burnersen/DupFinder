// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

// DupFinder - Web-UI-basierter Video-Duplikat-Finder
package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web/index.html web/style.css web/app.js
var webAssets embed.FS

// appVersion ist die zentrale Versionsnummer (semantisch). Bei jedem Release nur
// hier erhöhen.
const appVersion = "1.0.1"

var shutdownRequested atomic.Bool

// appCtx wird bei Shutdown (Signal oder Browser-Beenden) gecancelt. Alle
// externen ffmpeg/ffprobe-Aufrufe sind daran gekoppelt, damit in-flight
// Kindprozesse beim Beenden sofort sterben statt verwaist weiterzulaufen.
var appCtx, appCancel = context.WithCancel(context.Background())

// globalSrv erlaubt dem Signal-Handler einen sauberen httpSrv.Shutdown.
var globalSrv *server

func isShutdownRequested() bool { return shutdownRequested.Load() }

type appState struct {
	mu           sync.RWMutex
	roots        []string
	clusters     []*clusterState
	dataDir      string
	binaries     *binaryPaths
	cache        *cache
	ignored      *ignoreList
	scanning     atomic.Bool
	progress     scanProgress
	readErrors   atomic.Int64 // Anzahl nicht lesbarer/defekter Dateien im letzten Scan
	stateVersion atomic.Int64 // erhöht sich bei jeder sichtbaren State-Änderung (für SSE)
}

// bumpVersion signalisiert SSE-Clients, dass sich der sichtbare State geändert
// hat (Cluster/Entscheidungen). Clients holen daraufhin /api/state neu.
func (st *appState) bumpVersion() { st.stateVersion.Add(1) }

type scanProgress struct {
	mu      sync.Mutex
	Phase   string `json:"phase"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Message string `json:"message"`
}

type clusterState struct {
	ID        string       `json:"id"`
	Videos    []*videoInfo `json:"videos"`
	Decided   bool         `json:"decided"`
	Decisions []decision   `json:"decisions"`
	TotalSize int64        `json:"totalSize"`
	Savings   int64        `json:"savings"`
}

type decision struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Printf("DupFinder %s - Web-UI-basierter Video-Duplikat-Finder\n", appVersion)
		fmt.Println("Aufruf: DupFinder.exe <Verzeichnis> [weitere ...]")
		os.Exit(1)
	}

	fmt.Printf("DupFinder %s\n", appVersion)

	setupSignalHandler()

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FEHLER: %v\n", err)
		os.Exit(2)
	}
	dataDir := filepath.Join(filepath.Dir(exePath), ".DupFinder.data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "FEHLER: %v\n", err)
		os.Exit(2)
	}
	thumbDir := filepath.Join(dataDir, "thumbs")
	_ = os.MkdirAll(thumbDir, 0755)

	fmt.Printf("Datenverzeichnis: %s\n", dataDir)
	setupLogging(dataDir)

	binDir := filepath.Join(filepath.Dir(exePath), "bin")
	bp, err := ensureBinaries(binDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FEHLER: %v\n", err)
		os.Exit(2)
	}
	defer bp.cleanup()

	cacheFile := filepath.Join(dataDir, "cache.json")
	c := loadCache(cacheFile)
	fmt.Printf("Cache: %d Einträge.\n", len(c.entries))

	ignoreFile := filepath.Join(dataDir, "ignored.json")
	ig := loadIgnoreList(ignoreFile)
	fmt.Printf("Ignorier-Liste: %d Einträge.\n", ig.size())

	state := &appState{
		roots:    args,
		dataDir:  dataDir,
		binaries: bp,
		cache:    c,
		ignored:  ig,
	}

	go func() {
		if err := runFullScan(state, cacheFile); err != nil {
			fmt.Fprintf(os.Stderr, "Scan-Fehler: %v\n", err)
		}
		// Persistierte Session-Entscheidungen wiederherstellen
		applySession(state)
	}()

	port := findFreePort()
	if port == 0 {
		fmt.Fprintln(os.Stderr, "FEHLER: Kein freier Port")
		os.Exit(2)
	}

	url := fmt.Sprintf("http://localhost:%d", port)
	fmt.Printf("\n========================================\n")
	fmt.Printf(" Web-UI bereit unter:  %s\n", url)
	fmt.Printf("========================================\n\n")
	fmt.Println("Browser öffnet sich automatisch.")
	fmt.Println("Strg+C im Terminal zum Beenden.")

	go openBrowser(url)

	srv := newServer(state, cacheFile, ignoreFile)
	globalSrv = srv
	if err := srv.run(port); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "Server-Fehler: %v\n", err)
		os.Exit(2)
	}
}

func setupSignalHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Fprintln(os.Stderr, "\n[!] Shutdown…")
		slog.Info("shutdown angefordert (signal)")
		shutdownRequested.Store(true)
		appCancel()                                          // laufende ffmpeg/ffprobe-Kindprozesse sofort abbrechen
		time.AfterFunc(4*time.Second, func() { os.Exit(0) }) // harte Reißleine, falls Shutdown hängt
		if s := globalSrv; s != nil && s.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = s.httpSrv.Shutdown(ctx) // ListenAndServe kehrt zurück → defers (Cleanup) laufen
		}
	}()
}

func findFreePort() int {
	for _, p := range []int{8765, 8766, 8767, 8768, 8769} {
		l, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", p))
		if err == nil {
			_ = l.Close()
			return p
		}
	}
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func openBrowser(url string) {
	time.Sleep(700 * time.Millisecond)
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	_ = cmd.Start()
	_ = context.Background()
}

// setupLogging schreibt strukturierte Diagnose-Logs in dataDir/dupfinder.log.
// Die hübsche Konsolen-Ausgabe (fmt.Print…) bleibt davon unberührt. Schlägt das
// Öffnen fehl, bleibt der slog-Default (stderr) aktiv.
func setupLogging(dataDir string) {
	f, err := os.OpenFile(filepath.Join(dataDir, "dupfinder.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("=== DupFinder gestartet ===")
}

// applySession liest session.json und markiert passende Cluster als entschieden.
func applySession(state *appState) {
	sess := loadSession(state.dataDir)
	state.mu.Lock()
	changed := false
	for _, cl := range state.clusters {
		if entry, ok := sess.Clusters[cl.ID]; ok {
			cl.Decided = entry.Decided
			cl.Decisions = entry.Decisions
			changed = true
		}
	}
	state.mu.Unlock()
	if changed {
		state.bumpVersion()
	}
}
