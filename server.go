package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type server struct {
	state      *appState
	cacheFile  string
	ignoreFile string
	httpSrv    *http.Server
}

func newServer(state *appState, cacheFile, ignoreFile string) *server {
	return &server{state: state, cacheFile: cacheFile, ignoreFile: ignoreFile}
}

func (s *server) run(port int) error {
	mux := http.NewServeMux()

	webFS, _ := fs.Sub(webAssets, "web")
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

	s.httpSrv = &http.Server{
		Addr:         fmt.Sprintf("localhost:%d", port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	return s.httpSrv.ListenAndServe()
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
		Roots:    append([]string(nil), s.state.roots...),
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
// /api/events  – Server-Sent Events (Push statt Polling)
// ============================================================================

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// WriteTimeout des Servers (60s) für DIESE Langläufer-Verbindung aufheben,
	// sonst würde der Stream nach 60s abreißen.
	_ = rc.SetWriteDeadline(time.Time{})

	write := func(event, data string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
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
		pb, _ := json.Marshal(map[string]any{
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
		case <-appCtx.Done(): // Shutdown – blockiert httpSrv.Shutdown nicht
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
	s.state.mu.Unlock()

	if cl == nil {
		http.Error(w, "Cluster nicht gefunden", http.StatusNotFound)
		return
	}
	if cl.Decided {
		http.Error(w, "Cluster bereits entschieden", http.StatusConflict)
		return
	}

	var deleteErrors []string
	for _, d := range req.Decisions {
		switch d.Action {
		case "delete":
			if err := moveToRecycleBin(d.Path); err != nil {
				deleteErrors = append(deleteErrors, fmt.Sprintf("%s: %v", d.Path, err))
			}
		case "ignore":
			s.state.ignored.add(d.Path)
		}
	}

	s.state.mu.Lock()
	cl.Decided = true
	cl.Decisions = req.Decisions
	s.state.mu.Unlock()
	s.state.bumpVersion()

	_ = s.state.ignored.save(s.ignoreFile)
	s.saveSession()

	resp := map[string]any{"ok": true}
	if len(deleteErrors) > 0 {
		resp["errors"] = deleteErrors
	}
	writeJSON(w, resp)
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
			s.saveSessionLocked()
			_ = s.state.ignored.save(s.ignoreFile)
			s.state.bumpVersion()
			writeJSON(w, map[string]any{
				"ok":   true,
				"note": "Bereits gelöschte Dateien liegen im Windows-Papierkorb.",
			})
			return
		}
	}
	http.Error(w, "Cluster nicht gefunden", http.StatusNotFound)
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
		http.Error(w, "Pfad leer", http.StatusBadRequest)
		return
	}
	st, err := os.Stat(req.Path)
	if err != nil || !st.IsDir() {
		http.Error(w, "Verzeichnis nicht gefunden", http.StatusBadRequest)
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
		http.Error(w, "Scan läuft bereits", http.StatusConflict)
		return
	}
	go func() {
		_ = runFullScan(s.state, s.cacheFile)
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
		time.Sleep(200 * time.Millisecond)
		fmt.Println("\nShutdown via Browser.")
		shutdownRequested.Store(true)
		appCancel() // laufende ffmpeg/ffprobe-Kindprozesse sofort abbrechen
		// Harte Reißleine analog zum Signal-Handler, falls Shutdown/Cleanup hängt.
		time.AfterFunc(4*time.Second, func() { os.Exit(0) })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// KEIN os.Exit(0): ListenAndServe kehrt mit ErrServerClosed zurück →
		// main() läuft sauber zu Ende und die defers (u.a. bp.cleanup) löschen
		// das Temp-Verzeichnis. Sonst bliebe pro UI-Beenden ein ffmpeg/ffprobe-
		// Tempordner (~100-150 MB) liegen.
		_ = s.httpSrv.Shutdown(ctx)
	}()
}

// ============================================================================
// /api/play, /api/reveal
// ============================================================================

// requireJSON erzwingt Content-Type: application/json bei mutierenden Requests.
// Schutz gegen CSRF: Eine fremde Website kann per HTML-Formular nur "simple"
// Content-Types (text/plain, form-urlencoded) ohne CORS-Preflight senden.
// Durch das Verlangen von application/json wird ein Preflight erzwungen, den
// der Browser für cross-origin Requests blockiert. Liefert true, wenn ok.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	// Optionale Parameter wie "; charset=utf-8" zulassen.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(ct) != "application/json" {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// pathIsKnown prüft, ob der angegebene Pfad zu einem Video gehört, das aktuell
// im State bekannt ist. Damit verhindern wir Command-Injection / das Öffnen
// beliebiger Pfade über das HTTP-API.
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
		http.Error(w, "Pfad leer", http.StatusBadRequest)
		return
	}
	if !s.pathIsKnown(req.Path) {
		http.Error(w, "Pfad nicht im aktuellen State", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		http.Error(w, "Datei nicht gefunden", http.StatusNotFound)
		return
	}

	// Kein cmd.exe: rundll32 FileProtocolHandler öffnet die Datei mit dem im
	// Windows-Default registrierten Programm und umgeht das fehleranfällige
	// cmd-Argument-Parsing (Zeichen wie & oder " können dort missinterpretiert
	// werden). Pfad ist bereits über pathIsKnown abgesichert.
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", req.Path)
	if err := cmd.Start(); err != nil {
		http.Error(w, "Konnte Player nicht starten: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Reaper-Goroutine: vermeidet Zombie-Prozesse und gibt Handles frei.
	go func() { _ = cmd.Wait() }()

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
		http.Error(w, "Pfad leer", http.StatusBadRequest)
		return
	}
	if !s.pathIsKnown(req.Path) {
		http.Error(w, "Pfad nicht im aktuellen State", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		http.Error(w, "Datei nicht gefunden", http.StatusNotFound)
		return
	}

	// explorer /select,"pfad"  – öffnet den enthaltenden Ordner und markiert
	// die Datei. Achtung: Argument muss als EIN String "/select,<pfad>" übergeben
	// werden, sonst trennt Explorer falsch.
	cmd := exec.Command("explorer", "/select,"+req.Path)
	if err := cmd.Start(); err != nil {
		http.Error(w, "Konnte Explorer nicht starten: "+err.Error(), http.StatusInternalServerError)
		return
	}
	go func() { _ = cmd.Wait() }()

	writeJSON(w, map[string]any{"ok": true})
}

// ============================================================================
// Session
// ============================================================================

type sessionEntry struct {
	Decided   bool       `json:"decided"`
	Decisions []decision `json:"decisions"`
}

type sessionData struct {
	Clusters map[string]sessionEntry `json:"clusters"`
}

func (s *server) saveSession() {
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	s.saveSessionLocked()
}

func (s *server) saveSessionLocked() {
	sess := sessionData{Clusters: map[string]sessionEntry{}}
	for _, cl := range s.state.clusters {
		if cl.Decided {
			sess.Clusters[cl.ID] = sessionEntry{Decided: cl.Decided, Decisions: cl.Decisions}
		}
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return
	}
	sessionFile := filepath.Join(s.state.dataDir, "session.json")
	tmp := sessionFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		_ = os.Rename(tmp, sessionFile)
	}
}

func loadSession(dataDir string) sessionData {
	sess := sessionData{Clusters: map[string]sessionEntry{}}
	data, err := os.ReadFile(filepath.Join(dataDir, "session.json"))
	if err != nil {
		return sess
	}
	_ = json.Unmarshal(data, &sess)
	if sess.Clusters == nil {
		sess.Clusters = map[string]sessionEntry{}
	}
	return sess
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}
