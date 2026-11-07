// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

// DupFinder finds duplicate videos by what they look like and lets the user
// decide in a local web page which copies go to the recycle bin.
//
// Where things are:
//
//	main.go       start, arguments, exit codes; the only place with os.Exit
//	scan.go       finding videos, probing, frame hashes, grouping
//	hash.go       perceptual hash (DCT) of 32x32 grey frames
//	state.go      cache, ignore list and decisions between runs
//	server.go     the HTTP API the page talks to
//	webguard.go   key and Host checks: only DupFinder's own tab gets in
//	web/          the page (HTML, CSS, JavaScript, no build step)
//	binaries.go   where ffmpeg.exe and ffprobe.exe come from
//	download.go   the pinned FFmpeg download (runtime.json)
//	errors.go     UserError: what the user sees, what only goes to the log
//	output.go     console output: [OK] [INFO] [WARN] [ERROR]
//	logging.go    log file with rotation
//	files.go      atomic writes
//	*_windows.go  recycle bin, Explorer, console, job object (with *_other.go)
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web/index.html web/style.css web/app.js
var webAssets embed.FS

// version is set by the release build with
//
//	go build -ldflags "-X main.version=1.2.3"
//
// Local builds say "dev". Never edit it by hand: the tag is the only source.
var version = "dev"

const appName = "DupFinder"

// Exit codes. They are part of the interface and kept as DupFinder 1.0 had
// them, which differs from the dev-starterkit (1 failed, 2 wrong call).
// Ending with Ctrl+C or "Quit" in the page is the normal way out: 0.
const (
	exitOK    = 0 // ended normally
	exitUsage = 1 // no folder given
	exitError = 2 // could not start: data folder, FFmpeg, web server
)

var shutdownRequested atomic.Bool

// appCtx is cancelled when DupFinder stops (Ctrl+C or "Quit" in the page).
// Every ffmpeg and ffprobe call hangs on it, so running child processes end
// at once instead of running on without a parent.
var appCtx, appCancel = context.WithCancel(context.Background())

// globalSrv lets stopApp shut the web server down.
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
	readErrors   atomic.Int64 // files that could not be read in the last scan
	stateVersion atomic.Int64 // goes up with every visible change (for the page's event stream)
}

// bumpVersion tells the page that groups or decisions changed; it then
// fetches /api/state again.
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
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	// Started by double-click, the console window would close at once and
	// take the message with it. Only when something went wrong: after a
	// normal end there is nothing left to read. The wait ends by itself
	// after a minute.
	if code != exitOK && launchedByDoubleClick() {
		waitForEnter(os.Stdin, os.Stdout, time.Minute)
	}
	os.Exit(code)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%s %s: finds duplicate videos by what they look like

Usage:
  DupFinder.exe <folder> [more folders ...]
  DupFinder.exe -version

Or drag one or more folders onto DupFinder.exe. The page opens in your
browser; end DupFinder with "Quit" there or Ctrl+C here.

Exit codes:
  0 ended normally
  1 no folder given
  2 could not start (the message says why)
`, appName, version)
}

// run is main without os.Exit: deferred cleanup always runs, and tests can
// call it with their own arguments and writers.
func run(args []string, stdout, stderr io.Writer) (code int) {
	out := newOutput(stdout, stderr)
	logger := slog.New(slog.DiscardHandler)
	// First defer: a panic anywhere below ends here, with a message instead
	// of a stack trace and an exit code instead of a crash.
	defer catchCrash(out, &logger, &code)

	if len(args) == 1 {
		switch args[0] {
		case "-version", "--version":
			fmt.Fprintf(stdout, "%s %s\n", appName, version)
			return exitOK
		case "-h", "-help", "--help", "/?":
			usage(stdout)
			return exitOK
		}
	}
	if len(args) == 0 {
		usage(stdout)
		return exitUsage
	}
	fmt.Fprintf(stdout, "%s %s\n", appName, version)

	home, err := homeDir()
	if err != nil {
		return finish(out, logger, "", err)
	}
	dataDir := filepath.Join(home, ".DupFinder.data")
	if err := os.MkdirAll(filepath.Join(dataDir, "thumbs"), 0o755); err != nil {
		return finish(out, logger, "", userError(err, "Cannot create the data folder "+dataDir,
			"DupFinder keeps its data next to DupFinder.exe. Put DupFinder.exe into a folder you may write to (not C:\\Program Files)."))
	}
	out.Info("Data folder: " + dataDir)

	logFile := filepath.Join(dataDir, "dupfinder.log")
	l, closeLog, err := openLog(logFile, slog.LevelInfo)
	if err != nil {
		// Without a log DupFinder still works; the user hears about it once.
		out.Warn("No log file (" + err.Error() + "). DupFinder continues without one.")
		logFile = ""
	} else {
		logger = l
		defer closeLog()
	}
	slog.SetDefault(logger)
	logger.Info("start", "version", version, "folders", args)

	for _, f := range args {
		if st, err := os.Stat(f); err != nil || !st.IsDir() {
			out.Warn("Not a folder, skipped: " + f)
			logger.Warn("not a folder", "path", f)
		}
	}

	setupSignalHandler(out)

	bp, err := ensureBinaries(out, filepath.Join(home, "bin"))
	if err != nil {
		return finish(out, logger, logFile, err)
	}
	defer bp.cleanup()

	cacheFile := filepath.Join(dataDir, "cache.json")
	ignoreFile := filepath.Join(dataDir, "ignored.json")
	c, warning, err := loadCache(cacheFile)
	if err != nil {
		return finish(out, logger, logFile, err)
	}
	report(out, logger, warning)
	ig, warning, err := loadIgnoreList(ignoreFile)
	if err != nil {
		return finish(out, logger, logFile, err)
	}
	report(out, logger, warning)
	sess, warning, err := loadSession(dataDir)
	if err != nil {
		return finish(out, logger, logFile, err)
	}
	report(out, logger, warning)
	out.Info(fmt.Sprintf("Cache: %d files. Ignore list: %d files.", len(c.entries), ig.size()))

	state := &appState{
		roots:    args,
		dataDir:  dataDir,
		binaries: bp,
		cache:    c,
		ignored:  ig,
	}
	srv := newServer(state, cacheFile, ignoreFile, nil)
	srv.out = out

	go func() {
		if err := runFullScan(state, cacheFile); err != nil {
			srv.scanFailed(err)
		}
		applySession(state, sess)
	}()

	port := findFreePort()
	if port == 0 {
		return finish(out, logger, logFile, userError(nil, "No free network port on this computer for the page",
			"Close other programs that run local servers, then start DupFinder again."))
	}
	guard, err := newWebGuard(port)
	if err != nil {
		return finish(out, logger, logFile, err)
	}
	srv.guard = guard
	globalSrv = srv

	url := guard.url()
	fmt.Fprintf(stdout, "\n========================================\n")
	fmt.Fprintf(stdout, " Web UI ready at:  %s\n", url)
	fmt.Fprintf(stdout, "========================================\n\n")
	out.Info("Your browser opens by itself. If not, open the address above.")
	out.Info("End DupFinder with \"Quit\" in the page or Ctrl+C here.")
	logger.Info("listening", "port", port) // the address carries the key: never logged

	go openBrowser(url)

	if err := srv.run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return finish(out, logger, logFile, userError(err, "The web server stopped",
			"Start DupFinder again. If it keeps happening, the log file has the details."))
	}
	out.OK("DupFinder stopped.")
	return finish(out, logger, logFile, nil)
}

// homeDir is the folder next to DupFinder.exe, where bin and .DupFinder.data
// live (DupFinder 1.0 put them there; it stays portable this way).
// DUPFINDER_HOME moves both elsewhere, for tests and special setups.
func homeDir() (string, error) {
	if h := os.Getenv("DUPFINDER_HOME"); h != "" {
		return h, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find DupFinder.exe: %w", err)
	}
	return filepath.Dir(exe), nil
}

// report shows a warning from loading a file, if there is one.
func report(out *output, logger *slog.Logger, warning string) {
	if warning != "" {
		out.Warn(warning)
		logger.Warn("state file", "problem", warning)
	}
}

// finish turns the result of a run into the exit code and tells the user
// what happened. It is the one place where errors are shown: below it,
// errors are only returned (with context), never printed and returned.
func finish(out *output, logger *slog.Logger, logFile string, err error) int {
	if err == nil {
		logger.Info("done")
		return exitOK
	}
	var ue *UserError
	if errors.As(err, &ue) {
		out.Error(ue.Msg)
		if ue.Hint != "" {
			out.Hint(ue.Hint)
		}
		logger.Error(ue.Msg, causeAttr(ue.Err)...)
		return exitError
	}
	// Not explained yet: show it anyway (never stay silent), point to the log.
	out.Error("Something went wrong: " + err.Error())
	if logFile != "" {
		out.Hint("Details are in the log file: " + logFile)
	}
	logger.Error("unexpected error", "err", err)
	return exitError
}

// causeAttr is the technical cause as a log attribute, or nothing.
func causeAttr(err error) []any {
	if err == nil {
		return nil
	}
	return []any{"cause", err.Error()}
}

// catchCrash turns a panic into a message and exit code 2. It must be the
// first deferred call in run, so it runs last and sees every panic.
func catchCrash(out *output, logger **slog.Logger, code *int) {
	r := recover()
	if r == nil {
		return
	}
	(*logger).Error("crash", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	out.Error(fmt.Sprintf("%s stopped because of an internal error: %v", appName, r))
	out.Hint("Please report this at https://github.com/burnersen/" + appName + "/issues with the log file attached.")
	*code = exitError
}

// setupSignalHandler: the first Ctrl+C stops DupFinder in order, a second
// one ends it at once (the default handling is back by then).
func setupSignalHandler(out *output) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		signal.Reset(os.Interrupt, syscall.SIGTERM)
		out.Info("Stopping... (Ctrl+C again ends at once)")
		stopApp("signal")
	}()
}

var stopOnce sync.Once

// stopApp ends DupFinder in order: running FFmpeg processes are cancelled,
// the web server stops, run returns and its deferred cleanup (scratch
// folder, log) runs.
func stopApp(reason string) {
	stopOnce.Do(func() {
		slog.Info("stop", "reason", reason)
		shutdownRequested.Store(true)
		appCancel()
		if s := globalSrv; s != nil && s.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = s.httpSrv.Shutdown(ctx) // ignore: the server ends either way; a timeout only cuts open connections
		}
	})
}

// findFreePort prefers the ports DupFinder always used, so the address stays
// familiar, and falls back to any free one.
func findFreePort() int {
	for _, p := range []int{8765, 8766, 8767, 8768, 8769} {
		l, err := net.Listen("tcp", listenAddr(p))
		if err == nil {
			_ = l.Close() // ignore: only a probe, the server opens the port again
			return p
		}
	}
	l, err := net.Listen("tcp", listenAddr(0))
	if err != nil {
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // ignore: only a probe, as above
	return port
}

func openBrowser(url string) {
	time.Sleep(700 * time.Millisecond) // the server is listening by then
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		// The console shows the address and says to open it by hand.
		slog.Warn("browser not opened", "err", err)
	}
}

// applySession marks the groups decided in an earlier run as decided.
func applySession(state *appState, sess sessionData) {
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
