// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// recycle moves a file to the recycle bin. A variable so tests can see what
// would be moved without touching the recycle bin.
var recycle = moveToRecycleBin

type server struct {
	state      *appState
	cacheFile  string
	ignoreFile string
	guard      *webGuard
	httpSrv    *http.Server
	out        *output // console; nil in tests
}

func newServer(state *appState, cacheFile, ignoreFile string, guard *webGuard) *server {
	return &server{state: state, cacheFile: cacheFile, ignoreFile: ignoreFile, guard: guard}
}

func (s *server) run() error {
	s.httpSrv = &http.Server{
		Addr:         listenAddr(s.guard.port),
		Handler:      s.handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	return s.httpSrv.ListenAndServe()
}

// handler is the API behind the key and Host checks of webguard.go.
func (s *server) handler() http.Handler { return s.guard.wrap(s.routes()) }

// routes is the whole HTTP API; tests call it without a listening server.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	webFS, _ := fs.Sub(webAssets, "web") // ignore: "web" is embedded at build time, Sub cannot fail
	mux.Handle("/", http.FileServer(http.FS(webFS)))

	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/progress", s.handleProgress)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/decide", s.handleDecide)
	mux.HandleFunc("/api/undo", s.handleUndo)
	mux.HandleFunc("/api/add-root", s.handleAddRoot)
	mux.HandleFunc("/api/rescan", s.handleRescan)
	mux.HandleFunc("/api/thumb", s.handleThumb)
	mux.HandleFunc("/api/shutdown", s.handleShutdown)
	mux.HandleFunc("/api/play", s.handlePlay)
	mux.HandleFunc("/api/reveal", s.handleReveal)
	return mux
}

// ============================================================================
// /api/state
// ============================================================================

type stateResponse struct {
	Roots        []string     `json:"roots"`
	Clusters     []clusterDTO `json:"clusters"`
	TotalSavings int64        `json:"totalSavings"`
	PendingCount int          `json:"pendingCount"`
	DecidedCount int          `json:"decidedCount"`
	IgnoredCount int          `json:"ignoredCount"`
	ReadErrors   int          `json:"readErrors"`
	Scanning     bool         `json:"scanning"`
}

type clusterDTO struct {
	ID        string     `json:"id"`
	Videos    []videoDTO `json:"videos"`
	Decided   bool       `json:"decided"`
	Decisions []decision `json:"decisions"`
	TotalSize int64      `json:"totalSize"`
	Savings   int64      `json:"savings"`
}

type videoDTO struct {
	Path        string  `json:"path"`
	FileName    string  `json:"fileName"`
	Size        int64   `json:"size"`
	Duration    float64 `json:"durationSec"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Codec       string  `json:"codec"`
	BitrateKbps int     `json:"bitrateKbps"`
	ThumbIDs    []int   `json:"thumbIds"`
	PathHash    string  `json:"pathHash"`
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()

	resp := stateResponse{
		// Empty lists, never null: the page reads .length on both.
		Roots:    append([]string{}, s.state.roots...),
		Clusters: []clusterDTO{},
		Scanning: s.state.scanning.Load(),
	}
	for _, cl := range s.state.clusters {
		dto := clusterDTO{
			ID:        cl.ID,
			Decided:   cl.Decided,
			Decisions: cl.Decisions,
			TotalSize: cl.TotalSize,
			Savings:   cl.Savings,
		}
		for _, v := range cl.Videos {
			dto.Videos = append(dto.Videos, videoDTO{
				Path:        v.Path,
				FileName:    filepath.Base(v.Path),
				Size:        v.Size,
				Duration:    v.Duration.Seconds(),
				Width:       v.Width,
				Height:      v.Height,
				Codec:       v.Codec,
				BitrateKbps: v.BitrateKbps,
				ThumbIDs:    thumbIndices,
				PathHash:    pathHash(v.Path),
			})
		}
		resp.Clusters = append(resp.Clusters, dto)

		if cl.Decided {
			resp.DecidedCount++
		} else {
			resp.PendingCount++
			resp.TotalSavings += cl.Savings
		}
	}
	resp.IgnoredCount = s.state.ignored.size()
	resp.ReadErrors = int(s.state.readErrors.Load())
	writeJSON(w, resp)
}

// ============================================================================
// /api/progress
// ============================================================================

func (s *server) handleProgress(w http.ResponseWriter, r *http.Request) {
	s.state.progress.mu.Lock()
	defer s.state.progress.mu.Unlock()
	writeJSON(w, map[string]any{
		"phase":    s.state.progress.Phase,
		"current":  s.state.progress.Current,
		"total":    s.state.progress.Total,
		"message":  s.state.progress.Message,
		"scanning": s.state.scanning.Load(),
	})
}

// ============================================================================
// /api/events: Server-Sent Events (push instead of polling)
// ============================================================================

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// Lift the server's WriteTimeout (60 s) for THIS long-running connection,
	// or the stream would break off after 60 s.
	_ = rc.SetWriteDeadline(time.Time{}) // ignore: not every writer supports it; the stream works without

	write := func(event, data string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second)) // ignore: as above
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	lastVer := int64(-1)
	lastProg := ""
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		s.state.progress.mu.Lock()
		pb, _ := json.Marshal(map[string]any{ // ignore: strings and numbers always encode
			"phase":    s.state.progress.Phase,
			"current":  s.state.progress.Current,
			"total":    s.state.progress.Total,
			"message":  s.state.progress.Message,
			"scanning": s.state.scanning.Load(),
		})
		s.state.progress.mu.Unlock()
		if prog := string(pb); prog != lastProg {
			if !write("progress", prog) {
				return
			}
			lastProg = prog
		}
		if v := s.state.stateVersion.Load(); v != lastVer {
			if !write("state", fmt.Sprintf("%d", v)) {
				return
			}
			lastVer = v
		}

		select {
		case <-r.Context().Done(): // Client getrennt
			return
		case <-appCtx.Done(): // shutdown: does not block httpSrv.Shutdown
			return
		case <-ticker.C:
		case <-heartbeat.C:
			if !write("ping", "1") {
				return
			}
		}
	}
}

// ============================================================================
// /api/decide
// ============================================================================

func (s *server) handleDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		ClusterID string     `json:"clusterId"`
		Decisions []decision `json:"decisions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.state.mu.Lock()
	var cl *clusterState
	for _, c := range s.state.clusters {
		if c.ID == req.ClusterID {
			cl = c
			break
		}
	}
	var decided bool
	var badDecision error
	if cl != nil {
		decided = cl.Decided
		badDecision = checkDecisions(cl, req.Decisions)
	}
	s.state.mu.Unlock()

	if cl == nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	// Checked before anything happens: a decision may only name files of
	// this group, so no request can send any other file to the recycle bin.
	if badDecision != nil {
		http.Error(w, badDecision.Error(), http.StatusBadRequest)
		return
	}
	if decided {
		http.Error(w, "Cluster bereits entschieden", http.StatusConflict)
		return
	}

	var deleteErrors []string
	for _, d := range req.Decisions {
		switch d.Action {
		case "delete":
			if err := recycle(d.Path); err != nil {
				deleteErrors = append(deleteErrors, fmt.Sprintf("%s: %v", d.Path, err))
			}
		case "ignore":
			s.state.ignored.add(d.Path)
		}
	}

	s.state.mu.Lock()
	cl.Decided = true
	cl.Decisions = req.Decisions
	sessErr := saveSessionLocked(s.state)
	s.state.mu.Unlock()
	s.state.bumpVersion()

	problems := deleteErrors
	problems = s.notSaved(problems, "the decision", sessErr)
	problems = s.notSaved(problems, "the ignore list", s.state.ignored.save(s.ignoreFile))

	resp := map[string]any{"ok": true}
	if len(problems) > 0 {
		resp["errors"] = problems
	}
	writeJSON(w, resp)
}

// checkDecisions accepts only the known actions, and only for files that
// belong to the group. The caller holds state.mu.
func checkDecisions(cl *clusterState, decisions []decision) error {
	for _, d := range decisions {
		switch d.Action {
		case "keep", "delete", "ignore":
		default:
			return fmt.Errorf("unknown action %q", d.Action)
		}
		known := false
		for _, v := range cl.Videos {
			if v.Path == d.Path {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("file is not part of this group: %s", d.Path)
		}
	}
	return nil
}

// ============================================================================
// /api/undo
// ============================================================================

func (s *server) handleUndo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		ClusterID string `json:"clusterId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.state.mu.Lock()
	defer s.state.mu.Unlock()

	for _, cl := range s.state.clusters {
		if cl.ID == req.ClusterID {
			for _, d := range cl.Decisions {
				if d.Action == "ignore" {
					s.state.ignored.remove(d.Path)
				}
			}
			cl.Decided = false
			cl.Decisions = nil
			problems := s.notSaved(nil, "the decision", saveSessionLocked(s.state))
			problems = s.notSaved(problems, "the ignore list", s.state.ignored.save(s.ignoreFile))
			s.state.bumpVersion()
			resp := map[string]any{
				"ok":   true,
				"note": "Files deleted already are in the Windows recycle bin.",
			}
			if len(problems) > 0 {
				resp["errors"] = problems
			}
			writeJSON(w, resp)
			return
		}
	}
	http.Error(w, "Group not found", http.StatusNotFound)
}

// ============================================================================
// /api/add-root, /api/rescan, /api/thumb, /api/shutdown
// ============================================================================

func (s *server) handleAddRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		http.Error(w, "No folder given", http.StatusBadRequest)
		return
	}
	st, err := os.Stat(req.Path)
	if err != nil || !st.IsDir() {
		http.Error(w, "Folder not found", http.StatusBadRequest)
		return
	}

	s.state.mu.Lock()
	for _, rt := range s.state.roots {
		if strings.EqualFold(rt, req.Path) {
			s.state.mu.Unlock()
			writeJSON(w, map[string]any{"ok": true, "note": "bereits vorhanden"})
			return
		}
	}
	s.state.roots = append(s.state.roots, req.Path)
	s.state.mu.Unlock()
	s.state.bumpVersion()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	if s.state.scanning.Load() {
		http.Error(w, "A scan is running already", http.StatusConflict)
		return
	}
	go func() {
		if err := runFullScan(s.state, s.cacheFile); err != nil {
			s.scanFailed(err)
		}
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleThumb(w http.ResponseWriter, r *http.Request) {
	h := r.URL.Query().Get("h")
	i := r.URL.Query().Get("i")
	if h == "" || i == "" {
		http.NotFound(w, r)
		return
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			http.NotFound(w, r)
			return
		}
	}
	for _, c := range i {
		if c < '0' || c > '9' {
			http.NotFound(w, r)
			return
		}
	}
	thumbDir := filepath.Join(s.state.dataDir, "thumbs")
	thumbPath := filepath.Join(thumbDir, fmt.Sprintf("%s_%s.jpg", h, i))
	st, err := os.Stat(thumbPath)
	if err != nil || st.Size() == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, thumbPath)
}

func (s *server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond) // the answer reaches the page first
		if s.out != nil {
			s.out.Info("Quit in the browser.")
		}
		stopApp("browser")
	}()
}

// scanFailed reports a scan that stopped with an error. The page shows the
// last progress message; the console and the log say why.
func (s *server) scanFailed(err error) {
	slog.Error("scan failed", "err", err)
	if s.out != nil {
		s.out.Error("The scan stopped: " + err.Error())
	}
}

// ============================================================================
// /api/play, /api/reveal
// ============================================================================

// requireJSON insists on Content-Type: application/json for changing requests.
// Against CSRF: a foreign web page can send only "simple" content types
// (text/plain, form-urlencoded) with an HTML form, without a CORS preflight.
// Requiring application/json forces a preflight, which the browser blocks
// for cross-origin requests. Returns true if all is well.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	// Allow parameters such as "; charset=utf-8".
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(ct) != "application/json" {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// pathIsKnown checks that the path belongs to a video in the current state.
// This keeps the HTTP API from opening arbitrary paths or being used for
// command injection.
func (s *server) pathIsKnown(p string) bool {
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	for _, cl := range s.state.clusters {
		for _, v := range cl.Videos {
			if strings.EqualFold(v.Path, p) {
				return true
			}
		}
	}
	return false
}

func (s *server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		http.Error(w, "No file given", http.StatusBadRequest)
		return
	}
	if !s.pathIsKnown(req.Path) {
		http.Error(w, "File is not part of the current result", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// No cmd.exe: rundll32 FileProtocolHandler opens the file with the program
	// registered in Windows and avoids cmd's error-prone argument parsing
	// (characters such as & or " can be misread there). The path is checked
	// by pathIsKnown already.
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", req.Path)
	if err := cmd.Start(); err != nil {
		http.Error(w, "Could not start the player: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Reaper goroutine: avoids zombie processes and frees handles.
	go func() { _ = cmd.Wait() }() // ignore: only reaps the process; the player's exit code means nothing here

	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		http.Error(w, "No file given", http.StatusBadRequest)
		return
	}
	if !s.pathIsKnown(req.Path) {
		http.Error(w, "File is not part of the current result", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Opens the containing folder and selects the file. The platform
	// specific part (including the right quoting for /select) is in
	// reveal_windows.go and reveal_other.go.
	if err := revealInExplorer(req.Path); err != nil {
		http.Error(w, "Could not start Explorer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// notSaved reports a file that could not be written: in the answer to the
// page (which shows it), in the console and in the log.
func (s *server) notSaved(problems []string, what string, err error) []string {
	if err == nil {
		return problems
	}
	msg := "Could not save " + what + ": " + err.Error()
	slog.Error("not saved", "what", what, "err", err)
	if s.out != nil {
		s.out.Error(msg)
	}
	return append(problems, msg)
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data) // ignore: the browser has gone away, nobody is left to tell
}
