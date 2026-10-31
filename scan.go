// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// PERF: 8 → 6 Sample-Frames. Pro Video werden jetzt nur noch 6 statt 8
	// Frames dekodiert (≈25 % weniger Decode-Arbeit im Hash-Hotpath), bei
	// gleichbleibender Trennschärfe nach dem Duration-Clustering.
	sampleCount       = 6
	durationTolerance = 500 * time.Millisecond
	maxHashDistance   = 10
	// minMatchingFrames: 4 von 6. Behält denselben absoluten Spielraum wie
	// früher (6 von 8) — es dürfen weiterhin bis zu 2 Sample-Frames abweichen
	// (Schwarzbilder, Letterbox, Abspann). Wer strenger filtern will: auf 5.
	minMatchingFrames = 4
	maxProbeWorkers   = 64
	maxHashWorkers    = 32

	thumbWidth  = 240
	thumbHeight = 135

	videoExtensions = ".mkv,.mp4,.m4v,.avi,.mov,.webm,.ts,.m2ts,.wmv,.flv,.mpg,.mpeg,.vob,.3gp,.divx"

	// Schwelle für Hybrid-Strategie:
	//   < bulkDurationLimit  → Bulk-Extract (1 ffmpeg-Call, dekodiert LINEAR ohne Seek!)
	//   >= bulkDurationLimit → Single-Calls mit Two-Stage-Seek (Keyframe-Sprung)
	//
	// PERF-FIX: Früher 5*time.Minute. Der Bulk-Pfad nutzt den select-Filter, der
	// NICHT seeken kann — ffmpeg dekodiert das Video linear von t=0 bis zum
	// letzten Sample bei 95 % und wirft fast alle Frames weg. Bei mehrminütigen
	// Videos (1080p/4K) ist genau das der Hauptkostentreiber beim ersten Hashen.
	// Mit 20s landen praktisch alle realen Videos im Single-Seek-Pfad (springt
	// pro Sample direkt zum Keyframe, dekodiert nur ~1 GOP statt das halbe File).
	// Nur echte Sekunden-Clips bleiben Bulk, wo linearer Decode trivial ist.
	bulkDurationLimit = 20 * time.Second

	// Timeouts für externe Prozesse: großzügig, damit legitime langsame
	// Extraktionen (4K/HDD/hohe Parallelität) NICHT fälschlich gekillt werden.
	// Greifen nur im echten Hänger-Fall (kaputte Datei, Netzlaufwerk-Stall,
	// GPU-Treiber-Lock) und verhindern dann den bisherigen Endlos-Block.
	ffprobeTimeout = 90 * time.Second
	ffmpegTimeout  = 180 * time.Second

	// cacheSaveInterval drosselt das Zwischenspeichern der cache.json während der
	// Hash-Phase. Statt nach jeweils 5 Clustern die KOMPLETTE Datei neu zu
	// schreiben (bei großen Bibliotheken sehr viele teure Vollschreibungen),
	// wird höchstens alle paar Sekunden gespeichert. Bricht der Scan ab, geht
	// nur das letzte Zeitfenster verloren; die finale Speicherung am Scan-Ende
	// bleibt davon unberührt.
	cacheSaveInterval = 15 * time.Second
)

// useHwAccel steuert die Decode-Strategie der HASH-Phase im Long-Video-Pfad und
// ist per Env DUPFINDER_HWACCEL OHNE Neukompilieren schaltbar:
//
//	DEFAULT (kein Env / =0) → CPU-Single-Call (multiInputExtractHashes, useGPU=false):
//	    EIN ffmpeg-Prozess pro Video holt alle Sample-Frames über mehrere -ss…-i-
//	    Inputs und mappt sie auf PGM-Outputs. Reiner CPU-Decode — sättigt die CPU
//	    (schnellster Pfad auf 8C/16T + NVMe) und spart gegenüber dem alten Pfad
//	    ~sampleCount−1 ffmpeg-Starts PRO VIDEO (Prozess-Spawn ist auf Windows teuer).
//
//	DUPFINDER_HWACCEL=1 → GPU-Single-Call (useGPU=true):
//	    Gleicher Single-Call, aber HEVC-Decode auf NVDEC via -init_hw_device
//	    cuda:0=gpu (einmal) + -hwaccel_device gpu (je Input → ein gemeinsamer
//	    CUDA-Context). Auf dieser Hardware gemessen LANGSAMER (GPU läuft zwischen
//	    den Seeks leer), daher nicht Default. Bleibt als Option erhalten.
//
//	GPU an :  set DUPFINDER_HWACCEL=1   (=true / =on / =ja / =an)
//	GPU aus:  Variable weglassen (Default)
var useHwAccel = hwAccelEnabled()

func hwAccelEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUPFINDER_HWACCEL"))) {
	case "1", "true", "on", "yes", "ja", "an":
		return true
	}
	return false // Default: CPU-Single-Call (auf dieser Hardware am schnellsten)
}

// fastSeek steuert die Seek-Strategie der HASH-Phase (nicht der Thumbnails).
// STANDARD jetzt AN: pro Sample wird nur per Input-Seek zum nächstgelegenen
// Keyframe gesprungen und dieser Frame genommen — der teure Präzisions-Decode
// vom Keyframe bis zum exakten Ziel-Frame entfällt. Das ist mit Abstand der
// größte Tempo-Hebel beim ersten Hashen. Für einen DCT-pHash ist Frame-Genauigkeit
// unnötig (es müssen ohnehin nur minMatchingFrames von sampleCount passen).
// Trade-off: bei unterschiedlich enkodierten Kopien (abweichendes Keyframe-Raster,
// v. a. an harten Schnitten) eine minimal geringere Treffgenauigkeit. Wer das
// genaue Two-Stage-Seek (langsamer) zurückwill, schaltet Fast-Seek per Env ab:
//
//	genau (langsamer): set DUPFINDER_FAST_SEEK=0   (=off / =nein / =aus)
//	schnell (Default): Variable weglassen
var fastSeek = fastSeekEnabled()

func fastSeekEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUPFINDER_FAST_SEEK"))) {
	case "0", "false", "off", "no", "nein", "aus":
		return false // ausdrücklich abgeschaltet → genaues Seeking
	}
	return true // Default: Fast-Seek an (schnellster Pfad)
}

// ffmpegSem deckelt die GESAMTzahl gleichzeitiger ffmpeg-Prozesse über alle
// Cluster- und Frame-Goroutinen hinweg. Der reale Engpass ist nicht die
// Worker-Zahl, sondern wie viele ffmpeg-Decodes (GPU-NVDEC/Disk) parallel
// laufen. Default = NumCPU; per Env DUPFINDER_FFMPEG_CONCURRENCY überschreibbar.
var ffmpegSem = make(chan struct{}, ffmpegConcurrency())

func ffmpegConcurrency() int {
	if v := os.Getenv("DUPFINDER_FFMPEG_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 1
}

// probeWorkers begrenzt die parallelen ffprobe-Aufrufe (leichtgewichtig, aber
// auf einer einzelnen HDD/SATA sonst I/O-Thrashing). 2×NumCPU, gedeckelt.
func probeWorkers() int {
	n := 2 * runtime.NumCPU()
	if n > maxProbeWorkers {
		n = maxProbeWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// runHiddenCmd startet einen externen Befehl ohne Fenster, gekoppelt an den
// globalen appCtx (Abbruch bei Shutdown) UND mit einer harten Deadline. So
// blockiert eine einzige hängende Datei keinen Worker mehr dauerhaft.
func runHiddenCmd(path string, timeout time.Duration, args ...string) error {
	// Globales Prozess-Limit: erst einen Slot holen — oder bei Shutdown sofort
	// raus, ohne zu blockieren.
	select {
	case ffmpegSem <- struct{}{}:
		defer func() { <-ffmpegSem }()
	case <-appCtx.Done():
		return appCtx.Err()
	}
	ctx, cancel := context.WithTimeout(appCtx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	hideWindow(cmd)
	return cmd.Run()
}

var samplePositions = []float64{0.10, 0.25, 0.40, 0.55, 0.70, 0.85}
var thumbIndices = []int{1, 2, 3, 4} // 25%, 40%, 55%, 70%

type videoInfo struct {
	Path         string        `json:"path"`
	Size         int64         `json:"size"`
	ModTime      time.Time     `json:"mtime"`
	Duration     time.Duration `json:"duration_ns"`
	Width        int           `json:"width"`
	Height       int           `json:"height"`
	Codec        string        `json:"codec"`
	BitrateKbps  int           `json:"bitrate_kbps"`
	HasError     bool          `json:"error,omitempty"`
	ErrorMessage string        `json:"error_message,omitempty"`
	Hashes       []uint64      `json:"hashes,omitempty"`
	// FullHashed=true: alle Sample-Frames wurden dekodiert (vollständiger Satz,
	// auch wenn einzelne Frames legitim 0 sind, z. B. Schwarzbild). false:
	// nur die Probe-Frames liegen vor (progressives Hashing, nicht-lebend). So
	// kann ein zweiter Lauf gecachte Probe-Hashes wiederverwenden, ohne neu zu
	// dekodieren, und trotzdem korrekt bleiben, falls später ein Partner dazukommt.
	FullHashed bool `json:"full,omitempty"`
}

func makeCacheKey(path string, size int64, mtime time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d", path, size, mtime.UnixNano())
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func pathHash(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])[:16]
}

// ============================================================================
// Binaries
// ============================================================================

type binaryPaths struct {
	ffprobe string // dauerhafter Pfad zu ffprobe.exe (im bin-Ordner neben der EXE)
	ffmpeg  string // dauerhafter Pfad zu ffmpeg.exe
	tempDir string // kurzlebiger Scratch-Ordner für Hash-Bilder, via cleanup gelöscht
}

// ensureBinaries stellt sicher, dass ffmpeg.exe und ffprobe.exe verfügbar sind.
// Gesucht wird AUSSCHLIESSLICH in binDir (Unterordner "bin" neben der EXE). Fehlt
// eine der beiden Dateien, werden beide aus dem offiziellen BtbN-Build geladen
// (siehe download.go) — aus Lizenzgründen werden sie NICHT mit dem Programm
// ausgeliefert. Zusätzlich wird ein kurzlebiger Scratch-Ordner für die beim
// Hashen anfallenden Zwischenbilder angelegt (beim Beenden via cleanup gelöscht).
func ensureBinaries(binDir string) (*binaryPaths, error) {
	ffmpegPath := filepath.Join(binDir, "ffmpeg.exe")
	ffprobePath := filepath.Join(binDir, "ffprobe.exe")

	if fileExists(ffmpegPath) && fileExists(ffprobePath) {
		fmt.Println("FFmpeg/FFprobe gefunden.")
	} else {
		fmt.Println("FFmpeg/FFprobe fehlt – lade die offizielle Version (einmalig)…")
		if err := downloadFFmpeg(binDir); err != nil {
			return nil, fmt.Errorf("FFmpeg-Download fehlgeschlagen: %w\n"+
				"Tipp: ffmpeg.exe und ffprobe.exe von https://github.com/BtbN/FFmpeg-Builds "+
				"manuell in den Ordner %q legen.", err, binDir)
		}
		if !fileExists(ffmpegPath) || !fileExists(ffprobePath) {
			return nil, fmt.Errorf("ffmpeg.exe/ffprobe.exe fehlen weiterhin nach dem Download")
		}
	}

	tempDir, err := os.MkdirTemp("", "dupfinder-scratch-*")
	if err != nil {
		return nil, err
	}
	return &binaryPaths{ffprobe: ffprobePath, ffmpeg: ffmpegPath, tempDir: tempDir}, nil
}

// fileExists meldet, ob p eine vorhandene, nicht leere Datei ist (kein Ordner).
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func (bp *binaryPaths) cleanup() {
	if bp != nil && bp.tempDir != "" {
		_ = os.RemoveAll(bp.tempDir)
	}
}

// ============================================================================
// Discovery
// ============================================================================

// skipDirNames – Verzeichnisnamen, die beim Sammeln übersprungen werden
// (Windows-Papierkorb je Partition + System Volume Information). Vergleich
// case-insensitiv über strings.ToLower.
var skipDirNames = map[string]bool{
	"$recycle.bin":              true,
	"$recycle.bin~":             true,
	"recycler":                  true, // alter Papierkorb-Name (vor Vista)
	"system volume information": true,
}

func collectVideoFiles(roots []string) ([]string, error) {
	extSet := map[string]bool{}
	for _, e := range strings.Split(videoExtensions, ",") {
		extSet[strings.ToLower(strings.TrimSpace(e))] = true
	}

	// visited wird über alle Roots geteilt (Schleifen-/Doppel-Schutz bei
	// überlappenden oder verschachtelten Wurzeln) → Mutex.
	var mu sync.Mutex
	visited := map[string]bool{}
	markVisited := func(key string) bool {
		mu.Lock()
		defer mu.Unlock()
		if visited[key] {
			return false
		}
		visited[key] = true
		return true
	}

	// walk sammelt in eine LOKALE Slice (pro Root-Goroutine), das vermeidet
	// Lock-Contention beim Anhängen. Erst am Ende wird zusammengeführt.
	var walk func(dir string, out *[]string) error
	walk = func(dir string, out *[]string) error {
		if isShutdownRequested() {
			return errors.New("abgebrochen")
		}
		key, err := filepath.EvalSymlinks(dir)
		if err != nil || key == "" {
			key = dir
		}
		key = strings.ToLower(filepath.Clean(key))
		if !markVisited(key) {
			return nil
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if isShutdownRequested() {
				return errors.New("abgebrochen")
			}
			name := e.Name()

			// PERF: kein os.Stat pro Eintrag mehr — ReadDir liefert den Typ schon
			// mit. Nur bei Symlinks/Junctions (Reparse-Points) wird aufgelöst, um
			// das bisherige Verhalten (Symlink-zu-Ordner folgen) zu erhalten.
			isDir := e.IsDir()
			if !isDir && e.Type()&os.ModeSymlink != 0 {
				if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
					isDir = info.IsDir()
				}
			}

			full := filepath.Join(dir, name)
			if isDir {
				// Windows-System-/Papierkorb-Ordner überspringen: der Explorer
				// blendet sie aus (versteckt+System), und gelöschte Dateien landen
				// dort — sie als Duplikat-Kandidaten zu zeigen ist verwirrend und
				// kann gerade gelöschte Treffer beim nächsten Scan wieder einblenden.
				if skipDirNames[strings.ToLower(name)] {
					continue
				}
				if err := walk(full, out); err != nil {
					return err
				}
				continue
			}
			if extSet[strings.ToLower(filepath.Ext(name))] {
				*out = append(*out, full)
			}
		}
		return nil
	}

	// Roots PARALLEL durchlaufen — bei eigenständigen Platten echter Speedup
	// (jede Platte hat eigenen Controller/Kopf, blockiert die anderen nicht).
	var wg sync.WaitGroup
	results := make([][]string, len(roots))
	errs := make([]error, len(roots))
	for i, root := range roots {
		wg.Add(1)
		go func(i int, root string) {
			defer wg.Done()
			var local []string
			errs[i] = walk(root, &local)
			results[i] = local
		}(i, root)
	}
	wg.Wait()

	var files []string
	for i := range results {
		if errs[i] != nil && !isShutdownRequested() {
			return nil, errs[i]
		}
		files = append(files, results[i]...)
	}
	sort.Strings(files)
	return files, nil
}

// ============================================================================
// ffprobe
// ============================================================================

type ffprobeOutput struct {
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  string `json:"duration"`
	} `json:"streams"`
}

func probeVideo(ctx context.Context, ffprobePath, videoPath string) (videoInfo, error) {
	st, err := os.Stat(videoPath)
	if err != nil {
		return videoInfo{}, err
	}
	info := videoInfo{Path: videoPath, Size: st.Size(), ModTime: st.ModTime()}

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", videoPath)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		info.HasError = true
		info.ErrorMessage = fmt.Sprintf("ffprobe: %v", err)
		return info, nil
	}

	var data ffprobeOutput
	if err := json.Unmarshal(out, &data); err != nil {
		info.HasError = true
		return info, nil
	}

	if d, err := strconv.ParseFloat(data.Format.Duration, 64); err == nil && d > 0 {
		info.Duration = time.Duration(d * float64(time.Second))
	}
	if br, err := strconv.Atoi(data.Format.BitRate); err == nil {
		info.BitrateKbps = br / 1000
	}
	for _, s := range data.Streams {
		if s.CodecType == "video" && s.Width > 0 && s.Height > 0 {
			info.Width = s.Width
			info.Height = s.Height
			info.Codec = s.CodecName
			if info.Duration == 0 {
				if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && d > 0 {
					info.Duration = time.Duration(d * float64(time.Second))
				}
			}
			break
		}
	}
	if info.Duration == 0 || info.Width == 0 {
		info.HasError = true
		info.ErrorMessage = "keine Video-Metadaten"
	}
	return info, nil
}

// ============================================================================
// Cluster
// ============================================================================

type cluster struct {
	videos []*videoInfo
}

func buildDurationClusters(infos []videoInfo) []cluster {
	var valid []*videoInfo
	for i := range infos {
		if !infos[i].HasError && infos[i].Duration > 0 {
			valid = append(valid, &infos[i])
		}
	}
	sort.Slice(valid, func(i, j int) bool {
		return valid[i].Duration < valid[j].Duration
	})

	var clusters []cluster
	var cur []*videoInfo
	var anchor time.Duration
	for _, v := range valid {
		if len(cur) == 0 {
			cur = append(cur, v)
			anchor = v.Duration
			continue
		}
		if v.Duration-anchor <= durationTolerance {
			cur = append(cur, v)
		} else {
			if len(cur) >= 2 {
				clusters = append(clusters, cluster{videos: cur})
			}
			cur = []*videoInfo{v}
			anchor = v.Duration
		}
	}
	if len(cur) >= 2 {
		clusters = append(clusters, cluster{videos: cur})
	}
	return clusters
}

// filenameDurWindow – Dauerfenster für Dateiname-Bridge-Paare. Container-Wechsel
// driften die gemeldete Dauer nur wenig; >5 s deutet auf anderen Inhalt hin.
const filenameDurWindow = 5 * time.Second

// filenameNoiseTokens – Tags, die nichts über die Identität aussagen und beim
// Tokenisieren ignoriert werden, damit nicht zufällig "1080p" zwei Videos brückt.
var filenameNoiseTokens = map[string]bool{
	"1080p": true, "720p": true, "2160p": true, "480p": true, "4k": true, "uhd": true,
	"h264": true, "h265": true, "x264": true, "x265": true, "hevc": true, "avc": true,
	"web": true, "webrip": true, "webdl": true, "bluray": true, "bdrip": true, "brrip": true,
	"hdtv": true, "dvdrip": true, "xvid": true, "divx": true, "ts": true, "hdr": true,
	"aac": true, "ac3": true, "dts": true, "mp3": true, "10bit": true, "8bit": true,
	"proper": true, "repack": true, "internal": true, "remux": true, "extended": true,
	"the": true, "and": true, "von": true, "der": true, "die": true, "das": true, "und": true,
}

// tokenizeName zerlegt einen Dateinamen (ohne Endung) in normalisierte Tokens:
// kleingeschrieben, an Nicht-Buchstaben/Ziffern getrennt, Noise/Kurz-Tokens raus.
func tokenizeName(path string) map[string]bool {
	base := strings.ToLower(filepath.Base(path))
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	toks := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		t := cur.String()
		cur.Reset()
		if len(t) < 3 || filenameNoiseTokens[t] {
			return
		}
		toks[t] = true
	}
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r >= 0x80 {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// computeFilenameBridges liefert Video-Paare, die (a) NICHT schon im selben
// Dauer-Cluster liegen (sonst werden sie ohnehin verglichen), (b) sich ≥2
// aussagekräftige Titel-Tokens teilen und (c) deren Dauer ≤ filenameDurWindow
// auseinanderliegt. Diese Paare werden später per pHash geprüft — der Name ist
// nur der Auslöser fürs Vergleichen, nicht der Treffer-Beweis.
func computeFilenameBridges(infos []videoInfo) [][2]*videoInfo {
	var valid []*videoInfo
	for i := range infos {
		if !infos[i].HasError && infos[i].Duration > 0 {
			valid = append(valid, &infos[i])
		}
	}
	if len(valid) < 2 {
		return nil
	}

	// Token-Index: Token → Videos. Sehr häufige Tokens (>maxTokenSpread Videos)
	// sind unspezifisch (z. B. ein überall vorkommender Studio-Name) → ignorieren,
	// das deckelt zugleich die Paar-Anzahl.
	const maxTokenSpread = 40
	toks := make([]map[string]bool, len(valid))
	tokenIndex := map[string][]int{}
	for i, v := range valid {
		toks[i] = tokenizeName(v.Path)
		for t := range toks[i] {
			tokenIndex[t] = append(tokenIndex[t], i)
		}
	}

	type ab struct{ a, b int }
	shared := map[ab]int{}
	for t, idxs := range tokenIndex {
		if len(idxs) < 2 || len(idxs) > maxTokenSpread {
			continue
		}
		for x := 0; x < len(idxs); x++ {
			for y := x + 1; y < len(idxs); y++ {
				a, b := idxs[x], idxs[y]
				if a > b {
					a, b = b, a
				}
				shared[ab{a, b}]++
			}
		}
		_ = t
	}

	var pairs [][2]*videoInfo
	for k, n := range shared {
		if n < 2 { // mindestens 2 gemeinsame aussagekräftige Tokens
			continue
		}
		a, b := valid[k.a], valid[k.b]
		d := a.Duration - b.Duration
		if d < 0 {
			d = -d
		}
		if d > filenameDurWindow {
			continue // zu unterschiedliche Länge → anderer Inhalt
		}
		if d <= durationTolerance {
			continue // gleiches Dauer-Cluster → wird ohnehin verglichen
		}
		pairs = append(pairs, [2]*videoInfo{a, b})
	}
	return pairs
}

func verifyClusterMatches(cl cluster) [][2]int {
	var pairs [][2]int
	for i := 0; i < len(cl.videos); i++ {
		for j := i + 1; j < len(cl.videos); j++ {
			a, b := cl.videos[i], cl.videos[j]
			if len(a.Hashes) != sampleCount || len(b.Hashes) != sampleCount {
				continue
			}
			matches, _ := compareHashSets(a.Hashes, b.Hashes)
			if matches >= minMatchingFrames {
				pairs = append(pairs, [2]int{i, j})
			}
		}
	}
	return pairs
}

// ============================================================================
// Cache
// ============================================================================

type cacheEntry struct {
	Key  string    `json:"key"`
	Info videoInfo `json:"info"`
}

type cache struct {
	mu      sync.Mutex
	entries map[string]videoInfo
	dirty   bool
}

func loadCache(path string) *cache {
	c := &cache{entries: map[string]videoInfo{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var entries []cacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return c
	}
	for _, e := range entries {
		c.entries[e.Key] = e.Info
	}
	return c
}

func (c *cache) get(path string, size int64, mtime time.Time) (videoInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.entries[makeCacheKey(path, size, mtime)]
	return info, ok
}

func (c *cache) set(info videoInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[makeCacheKey(info.Path, info.Size, info.ModTime)] = info
	c.dirty = true
}

func (c *cache) save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	entries := make([]cacheEntry, 0, len(c.entries))
	for k, v := range c.entries {
		entries = append(entries, cacheEntry{Key: k, Info: v})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// pruneMissing entfernt verwaiste Cache-Einträge (Variante A): nur solche, deren
// Datei NACHWEISLICH nicht mehr existiert (os.Stat → IsNotExist) UND deren Pfad
// unter einer der aktuell gescannten Wurzeln liegt. So werden verschobene/
// gelöschte/umbenannte Dateien aufgeräumt, ABER Einträge von gerade nicht
// gescannten oder offline Platten bleiben unangetastet (kein Datenverlust an
// gültigen Hashes). Rückgabe: Anzahl entfernter Einträge.
func (c *cache) pruneMissing(roots []string) int {
	// Wurzeln normalisieren (kleingeschrieben, sauber) für den Präfix-Vergleich.
	norm := make([]string, 0, len(roots))
	for _, r := range roots {
		norm = append(norm, strings.ToLower(filepath.Clean(r)))
	}
	underRoot := func(p string) bool {
		lp := strings.ToLower(filepath.Clean(p))
		for _, r := range norm {
			if lp == r || strings.HasPrefix(lp, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, v := range c.entries {
		if v.Path == "" || !underRoot(v.Path) {
			continue // außerhalb der gescannten Wurzeln → nicht anfassen
		}
		if _, err := os.Stat(v.Path); err != nil && os.IsNotExist(err) {
			delete(c.entries, k)
			removed++
		}
	}
	if removed > 0 {
		c.dirty = true
	}
	return removed
}

// ============================================================================
// Ignore-Liste
// ============================================================================

type ignoreList struct {
	mu    sync.Mutex
	paths map[string]bool
}

func loadIgnoreList(path string) *ignoreList {
	il := &ignoreList{paths: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return il
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err == nil {
		for _, p := range paths {
			il.paths[p] = true
		}
	}
	return il
}

func (il *ignoreList) has(p string) bool {
	il.mu.Lock()
	defer il.mu.Unlock()
	return il.paths[p]
}

func (il *ignoreList) add(p string) {
	il.mu.Lock()
	defer il.mu.Unlock()
	il.paths[p] = true
}

func (il *ignoreList) remove(p string) {
	il.mu.Lock()
	defer il.mu.Unlock()
	delete(il.paths, p)
}

func (il *ignoreList) size() int {
	il.mu.Lock()
	defer il.mu.Unlock()
	return len(il.paths)
}

func (il *ignoreList) save(path string) error {
	il.mu.Lock()
	paths := make([]string, 0, len(il.paths))
	for p := range il.paths {
		paths = append(paths, p)
	}
	il.mu.Unlock()
	sort.Strings(paths)
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ============================================================================
// Voller Scan
// ============================================================================

func runFullScan(state *appState, cacheFile string) error {
	if !state.scanning.CompareAndSwap(false, true) {
		return errors.New("scan läuft bereits")
	}
	defer state.scanning.Store(false)

	state.mu.RLock()
	roots := append([]string(nil), state.roots...)
	state.mu.RUnlock()

	updateProgress(state, "Discovery", 0, 0, "Sammle Dateien…")
	files, err := collectVideoFiles(roots)
	if err != nil && !isShutdownRequested() {
		return err
	}
	updateProgress(state, "Discovery", len(files), len(files),
		fmt.Sprintf("%d Dateien gefunden", len(files)))

	// V10 — verwaiste Cache-Einträge aufräumen (Variante A): nur Dateien, die
	// nachweislich weg sind und unter den gescannten Wurzeln lagen. Hält die
	// cache.json schlank, wenn (typisch bei Usenet-Workflow) viel verschoben/
	// umbenannt/gelöscht wird.
	if n := state.cache.pruneMissing(roots); n > 0 {
		slog.Info("Cache aufgeräumt", "entfernt", n)
	}

	if len(files) < 2 {
		state.mu.Lock()
		state.clusters = nil
		state.mu.Unlock()
		state.bumpVersion()
		updateProgress(state, "Done", 0, 0, "Zu wenig Dateien")
		return nil
	}

	infos := indexFiles(state, files)
	_ = state.cache.save(cacheFile)

	var rerr int64
	for i := range infos {
		if infos[i].HasError {
			rerr++
		}
	}
	state.readErrors.Store(rerr)
	slog.Info("indexing fertig", "files", len(files), "readErrors", rerr)

	updateProgress(state, "Clustering", 0, 0, "Bilde Cluster…")
	clusters := buildDurationClusters(infos)
	totalCandidates := 0
	for _, cl := range clusters {
		totalCandidates += len(cl.videos)
	}
	if len(clusters) == 0 {
		state.mu.Lock()
		state.clusters = nil
		state.mu.Unlock()
		state.bumpVersion()
		updateProgress(state, "Done", 0, 0, "Keine Duplikate")
		return nil
	}

	// V6 — Dateiname-Bridge: gleicher Inhalt in verschiedenen Containern kann
	// eine um >500 ms abweichende gemeldete Dauer haben → landet in benachbarten
	// Clustern und würde sonst nie verglichen. Videos mit deutlich ähnlichen
	// Dateinamen (gemeinsame Titel-Tokens) innerhalb eines lockeren Dauerfensters
	// werden als ZUSÄTZLICHE Vergleichspaare aufgenommen. Der pHash entscheidet
	// weiterhin (≥minMatchingFrames) → reine Namensgleichheit erzeugt KEINE
	// Falschtreffer. Die beteiligten Videos werden voll gehasht (forceFull),
	// damit das progressive Hashing sie nicht auf Probe-Frames kürzt.
	bridgeCands := computeFilenameBridges(infos)
	forceFull := map[*videoInfo]bool{}
	for _, bp := range bridgeCands {
		forceFull[bp[0]] = true
		forceFull[bp[1]] = true
	}

	var hashedCounter atomic.Int64
	lastCacheSave := time.Now()
	for ci, cl := range clusters {
		if isShutdownRequested() {
			break
		}
		computeHashesOnly(state, cl, &hashedCounter, forceFull)
		updateProgress(state, "Hashing", int(hashedCounter.Load()), totalCandidates,
			fmt.Sprintf("Cluster %d/%d", ci+1, len(clusters)))
		// Zeitlich gedrosselt zwischenspeichern statt nach festen Cluster-Schritten:
		// vermeidet die wiederholte Vollschreibung der cache.json bei großen Läufen.
		if time.Since(lastCacheSave) >= cacheSaveInterval {
			_ = state.cache.save(cacheFile)
			lastCacheSave = time.Now()
		}
	}
	_ = state.cache.save(cacheFile)

	// Bridge-Kandidaten per pHash bestätigen (gleiche Schwelle wie im Cluster).
	var bridgePairs [][2]*videoInfo
	for _, bp := range bridgeCands {
		a, b := bp[0], bp[1]
		if len(a.Hashes) != sampleCount || len(b.Hashes) != sampleCount {
			continue
		}
		if m, _ := compareHashSets(a.Hashes, b.Hashes); m >= minMatchingFrames {
			bridgePairs = append(bridgePairs, bp)
		}
	}

	confirmed := buildConfirmedClusters(clusters, bridgePairs, state.ignored)

	state.mu.Lock()
	state.clusters = confirmed
	state.mu.Unlock()
	state.bumpVersion()

	if len(confirmed) == 0 {
		updateProgress(state, "Done", 0, 0, "Keine Duplikate")
		return nil
	}

	thumbDir := filepath.Join(state.dataDir, "thumbs")
	totalThumbVideos := 0
	for _, cs := range confirmed {
		totalThumbVideos += len(cs.Videos)
	}
	var thumbCounter atomic.Int64
	updateProgress(state, "Thumbnails", 0, totalThumbVideos, "Erstelle Vorschauen…")

	for _, cs := range confirmed {
		if isShutdownRequested() {
			break
		}
		computeThumbsForVideos(state, cs.Videos, thumbDir, &thumbCounter, totalThumbVideos)
	}

	updateProgress(state, "Done", len(confirmed), len(confirmed),
		fmt.Sprintf("%d Duplikat-Gruppe(n)", len(confirmed)))
	return nil
}

func updateProgress(state *appState, phase string, cur, total int, msg string) {
	state.progress.mu.Lock()
	state.progress.Phase = phase
	state.progress.Current = cur
	state.progress.Total = total
	state.progress.Message = msg
	state.progress.mu.Unlock()
}

func indexFiles(state *appState, files []string) []videoInfo {
	results := make([]videoInfo, len(files))
	jobs := make(chan int, len(files))
	for i := range files {
		jobs <- i
	}
	close(jobs)

	var done atomic.Int64
	total := int64(len(files))
	updateProgress(state, "Indexing", 0, int(total), "Lese Metadaten…")

	var wg sync.WaitGroup
	pw := probeWorkers()
	for w := 0; w < pw; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if isShutdownRequested() {
					return
				}
				path := files[idx]
				st, err := os.Stat(path)
				if err != nil {
					results[idx] = videoInfo{Path: path, HasError: true}
					done.Add(1)
					continue
				}
				if cached, ok := state.cache.get(path, st.Size(), st.ModTime()); ok {
					results[idx] = cached
				} else {
					pctx, pcancel := context.WithTimeout(appCtx, ffprobeTimeout)
					info, _ := probeVideo(pctx, state.binaries.ffprobe, path)
					pcancel()
					state.cache.set(info)
					results[idx] = info
				}
				d := done.Add(1)
				if d%50 == 0 {
					updateProgress(state, "Indexing", int(d), int(total), "Lese Metadaten…")
				}
			}
		}()
	}
	wg.Wait()
	updateProgress(state, "Indexing", int(total), int(total), "Indexing fertig")
	return results
}

// ============================================================================
// HASH-PHASE: Hybrid-Strategie nach Video-Länge
//   < bulkDurationLimit  → Bulk-Extract (sequenzielles Lesen, schnell bei kurzen)
//   >= bulkDurationLimit → Single-Calls mit Two-Stage-Seek (Keyframe-Sprung)
// ============================================================================

// probeIndices = die zuerst dekodierten Frames. Bei sampleCount=6 und
// minMatchingFrames=4 dürfen über alle 6 Frames höchstens 2 abweichen; ein
// echtes Duplikat hat daher unter JEDEN 3 Frames mindestens 1 Treffer. Mit 3
// Probe-Frames kann ein Video, dessen Probes mit niemandem matchen, also kein
// Duplikat sein — die restlichen Frames spart man dann (beweisbar trefferneutral).
var probeIndices, restIndices = splitProbeRest()

func splitProbeRest() (probe, rest []int) {
	// budget = erlaubte Abweichungen; Probe muss budget+1 Frames umfassen, damit
	// „alle Probes daneben" einen Treffer sicher ausschließt.
	budget := sampleCount - minMatchingFrames
	probeN := budget + 1
	if probeN < 1 {
		probeN = 1
	}
	if probeN > sampleCount {
		probeN = sampleCount
	}
	for i := 0; i < sampleCount; i++ {
		if i < probeN {
			probe = append(probe, i)
		} else {
			rest = append(rest, i)
		}
	}
	return
}

func computeHashesOnly(state *appState, cl cluster, counter *atomic.Int64, forceFull map[*videoInfo]bool) {
	// V5 — Exakt-Duplikat-Vorfilter (Variante B): Byte-identische Kopien werden
	// erkannt, BEVOR ein Frame dekodiert wird (Content-Fingerprint statt Decode).
	copyOf := exactDupeReps(cl.videos)
	hasCopy := map[*videoInfo]bool{}
	for _, rep := range copyOf {
		hasCopy[rep] = true // Repräsentant mit Kopien → bekannter Partner → voll hashen
	}

	// 1) Cache füllen + klassifizieren.
	//    fullCached  : alle Frames dekodiert → fertig (kein Decode).
	//    probeDone   : nur Probe-Hashes aus Cache → KEIN Probe-Decode mehr nötig,
	//                  geht aber in die Lebend-Prüfung (evtl. kam ein Partner dazu).
	//    toProbe     : noch nichts → Probe-Frames dekodieren.
	var toProbe, probeDone []*videoInfo
	for _, v := range cl.videos {
		if len(v.Hashes) != sampleCount {
			if cached, ok := state.cache.get(v.Path, v.Size, v.ModTime); ok && len(cached.Hashes) == sampleCount {
				v.Hashes = cached.Hashes
				v.FullHashed = cached.FullHashed
			}
		}
		if _, isCopy := copyOf[v]; isCopy {
			// Byte-Kopie → wird ausschließlich in Schritt 6 behandelt UND gezählt.
			// Muss VOR dem FullHashed-Zähler stehen, sonst würde eine bereits voll
			// gecachte Kopie doppelt gezählt (hier + Schritt 6) und der Fortschritt
			// über 100 % springen.
			continue
		}
		if len(v.Hashes) == sampleCount && v.FullHashed {
			counter.Add(1)
			continue // vollständig gecacht → nichts zu tun
		}
		if len(v.Hashes) == sampleCount && hasProbe(v) {
			probeDone = append(probeDone, v) // Probe-Hashes aus Cache vorhanden
		} else {
			toProbe = append(toProbe, v)
		}
	}

	hashSubset := func(list []*videoInfo, indices []int) {
		sem := make(chan struct{}, maxHashWorkers)
		var wg sync.WaitGroup
		for _, v := range list {
			if isShutdownRequested() {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(vi *videoInfo) {
				defer wg.Done()
				defer func() { <-sem }()
				processVideoHashes(state.binaries.ffmpeg, vi, state.binaries.tempDir, indices)
			}(v)
		}
		wg.Wait()
	}

	// 2) STUFE A — Probe-Frames nur für die wirklich neuen Kandidaten dekodieren
	//    (probeDone hat seine Probes bereits aus dem Cache → kein Decode).
	hashSubset(toProbe, probeIndices)

	// 3) Lebend-Bestimmung: braucht der Kandidat den Rest? Nur wenn seine Probe-
	//    Frames mit mind. einem anderen Cluster-Video matchen (oder erzwungen voll:
	//    Byte-Repräsentant / Dateiname-Bridge / unvollständige Probes).
	isLive := func(v *videoInfo) bool {
		if hasCopy[v] || (forceFull != nil && forceFull[v]) {
			return true
		}
		if !allProbesSet(v) {
			return true // fehlender Probe-Frame → sicherheitshalber voll hashen
		}
		for _, w := range cl.videos {
			if w == v || !hasProbe(w) {
				continue
			}
			if probeMatch(v, w) {
				return true
			}
		}
		return false
	}
	candidates := append(append([]*videoInfo{}, toProbe...), probeDone...)
	var needRest []*videoInfo
	for _, v := range candidates {
		if !v.FullHashed && isLive(v) {
			needRest = append(needRest, v)
		}
	}

	// 4) STUFE B — restliche Frames nur für lebende Kandidaten dekodieren.
	hashSubset(needRest, restIndices)
	for _, v := range needRest {
		v.FullHashed = true
	}

	// 5) Cachen: ALLES, was wir dekodiert haben (Probe-only ODER voll) wird
	//    gespeichert — so dekodiert ein zweiter Lauf nichts neu. Nicht-lebende
	//    behalten FullHashed=false (nur Probe-Hashes); kommt später ein Partner
	//    dazu, wird der Rest dann nachgeholt. probeDone, das nicht-lebend bleibt,
	//    ist bereits korrekt gecacht → nicht erneut schreiben.
	for _, v := range toProbe {
		state.cache.set(*v) // Probe-only (FullHashed=false) oder voll (true)
		counter.Add(1)
	}
	for _, v := range probeDone {
		if v.FullHashed {
			state.cache.set(*v) // war Probe-only, jetzt voll → aktualisieren
		}
		counter.Add(1)
	}

	// 6) Byte-Kopien übernehmen die pHashes ihres (voll gehashten) Repräsentanten.
	for v, rep := range copyOf {
		if len(v.Hashes) == sampleCount && v.FullHashed {
			counter.Add(1)
			continue
		}
		if rep.FullHashed && len(rep.Hashes) == sampleCount {
			v.Hashes = append([]uint64(nil), rep.Hashes...)
			v.FullHashed = true
			state.cache.set(*v)
		}
		counter.Add(1)
	}
}

// hasProbe meldet, ob mindestens ein Probe-Frame gesetzt ist.
func hasProbe(v *videoInfo) bool {
	if len(v.Hashes) != sampleCount {
		return false
	}
	for _, i := range probeIndices {
		if v.Hashes[i] != 0 {
			return true
		}
	}
	return false
}

// allProbesSet meldet, ob ALLE Probe-Frames erfolgreich dekodiert wurden.
func allProbesSet(v *videoInfo) bool {
	if len(v.Hashes) != sampleCount {
		return false
	}
	for _, i := range probeIndices {
		if v.Hashes[i] == 0 {
			return false
		}
	}
	return true
}

// probeMatch: teilen a und b mindestens einen passenden Probe-Frame
// (Hamming ≤ maxHashDistance)? Nur dann kann das Paar ein Duplikat sein.
func probeMatch(a, b *videoInfo) bool {
	if len(a.Hashes) != sampleCount || len(b.Hashes) != sampleCount {
		return false
	}
	for _, i := range probeIndices {
		if a.Hashes[i] == 0 || b.Hashes[i] == 0 {
			continue
		}
		if hammingDistance(a.Hashes[i], b.Hashes[i]) <= maxHashDistance {
			return true
		}
	}
	return false
}

// exactDupeReps gruppiert die Cluster-Videos nach byte-Identität und liefert für
// jede byte-gleiche Kopie ihren Repräsentanten zurück (copy → representative).
// Nur Videos, die sich die EXAKTE Dateigröße mit mindestens einem anderen teilen,
// werden überhaupt fingerprintet (alle anderen können keine Byte-Kopie haben) —
// das hält den Zusatz-I/O minimal. Der Fingerprint deckt Größe + erste/letzte
// 8 MB ab; für reale Videodateien ist eine Fehlzuordnung praktisch ausgeschlossen.
func exactDupeReps(videos []*videoInfo) map[*videoInfo]*videoInfo {
	copyOf := map[*videoInfo]*videoInfo{}
	if len(videos) < 2 {
		return copyOf
	}
	// 1) nach exakter Größe vorgruppieren (gratis, nutzt vorhandene Metadaten)
	bySize := map[int64][]*videoInfo{}
	for _, v := range videos {
		if v.Size > 0 {
			bySize[v.Size] = append(bySize[v.Size], v)
		}
	}
	// 2) nur in Größen-Kollisionen fingerprinten, dann nach Fingerprint gruppieren
	for _, group := range bySize {
		if len(group) < 2 {
			continue
		}
		byFP := map[string]*videoInfo{} // fingerprint → bereits gesehener Repräsentant
		for _, v := range group {
			fp := contentFingerprint(v.Path, v.Size)
			if fp == "" {
				continue // Lesefehler → vorsichtshalber normal über pHash laufen lassen
			}
			if rep, ok := byFP[fp]; ok {
				copyOf[v] = rep // byte-gleich zu rep
			} else {
				byFP[fp] = v // erster mit diesem Fingerprint = Repräsentant
			}
		}
	}
	return copyOf
}

// contentFingerprint bildet einen starken Byte-Identitäts-Schlüssel ohne Decode:
// SHA-256 über Dateigröße + die ersten und letzten fpEdgeBytes. Kleiner als
// 2×fpEdgeBytes große Dateien werden komplett gehasht. Bei Lesefehler "" (der
// Aufrufer fällt dann auf den normalen pHash-Pfad zurück).
func contentFingerprint(path string, size int64) string {
	const fpEdgeBytes = 8 << 20 // 8 MiB pro Ende
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	fmt.Fprintf(h, "%d|", size)

	if size <= 2*fpEdgeBytes {
		if _, err := io.Copy(h, f); err != nil {
			return ""
		}
		return hex.EncodeToString(h.Sum(nil))
	}

	head := make([]byte, fpEdgeBytes)
	if _, err := io.ReadFull(f, head); err != nil {
		return ""
	}
	h.Write(head)

	if _, err := f.Seek(-fpEdgeBytes, io.SeekEnd); err != nil {
		return ""
	}
	tail := make([]byte, fpEdgeBytes)
	if _, err := io.ReadFull(f, tail); err != nil {
		return ""
	}
	h.Write(tail)

	return hex.EncodeToString(h.Sum(nil))
}

// processVideoHashes dekodiert die in `indices` angegebenen Sample-Frames in
// vi.Hashes[idx]. indices==nil bedeutet ALLE Frames. Bereits gesetzte Hashes
// bleiben erhalten (wichtig für das progressive 2-Stufen-Hashing: erst Probe-
// Frames, dann ggf. der Rest — ohne die Probe-Ergebnisse zu überschreiben).
func processVideoHashes(ffmpegPath string, vi *videoInfo, tempDir string, indices []int) {
	if vi.Duration <= 0 {
		return
	}
	if len(vi.Hashes) != sampleCount {
		vi.Hashes = make([]uint64, sampleCount)
	}
	if indices == nil {
		indices = allIndices
	}
	hashDir, err := os.MkdirTemp(tempDir, "hash-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(hashDir)

	durSec := vi.Duration.Seconds()

	// Hybrid: bei kurzen Videos Bulk (dekodiert ohnehin alle Frames in 1 Call,
	// progressives Teil-Decoding lohnt da nicht), bei langen ein Single-Call.
	if vi.Duration < bulkDurationLimit {
		if bulkExtractHashes(ffmpegPath, vi.Path, durSec, hashDir, vi, useHwAccel) {
			return
		}
		if useHwAccel && bulkExtractHashes(ffmpegPath, vi.Path, durSec, hashDir, vi, false) {
			return
		}
		// Bulk fehlgeschlagen → trotzdem Long-Video-Pfad als Fallback
	}

	// Lange Videos ODER Bulk-Fallback: EIN ffmpeg-Prozess pro Video (Single-Call).
	if useHwAccel {
		// Optional (DUPFINDER_HWACCEL=1): HEVC-Decode auf NVDEC.
		if multiInputExtractHashes(ffmpegPath, vi, durSec, hashDir, true, indices) {
			return
		}
		// GPU gescheitert (kein CUDA/Treiber) → CPU-Single-Call.
	}
	// DEFAULT: CPU-Single-Call (1 Prozess, sättigt die CPU, spart Prozessstarts).
	if multiInputExtractHashes(ffmpegPath, vi, durSec, hashDir, false, indices) {
		return
	}
	// Single-Call gescheitert (kaputte/exotische Datei) → robustester Pfad:
	// Frame-für-Frame (füllt nur noch die verbliebenen 0-Hashes).
	singleExtractHashes(ffmpegPath, vi, durSec, hashDir)
}

var allIndices = func() []int {
	idx := make([]int, sampleCount)
	for i := range idx {
		idx[i] = i
	}
	return idx
}()

// bulkExtractHashes – ein einziger ffmpeg-Call extrahiert mit dem select-Filter
// alle 8 Sample-Frames. Nur sinnvoll bei kurzen Videos.
func bulkExtractHashes(ffmpegPath, videoPath string, durSec float64,
	hashDir string, vi *videoInfo, useGPU bool) bool {

	var sb strings.Builder
	for i, p := range samplePositions {
		if i > 0 {
			sb.WriteByte('+')
		}
		ts := durSec * p
		fmt.Fprintf(&sb, "between(t,%.3f,%.3f)", ts, ts+0.5)
	}
	selectExpr := sb.String()

	outPattern := filepath.Join(hashDir, "h%d.pgm")

	args := []string{"-hide_banner", "-loglevel", "error"}
	if useGPU {
		args = append(args, "-hwaccel", "cuda")
	}
	args = append(args,
		"-i", videoPath,
		"-an", "-sn", "-dn",
		"-vf", fmt.Sprintf("select='%s',scale=32:32:flags=area,format=gray", selectExpr),
		"-vsync", "0",
		"-frames:v", fmt.Sprintf("%d", sampleCount),
		"-f", "image2",
		"-start_number", "1",
		"-y", outPattern,
	)
	if err := runHiddenCmd(ffmpegPath, ffmpegTimeout, args...); err != nil {
		return false
	}

	gotAny := false
	for i := 0; i < sampleCount; i++ {
		fp := filepath.Join(hashDir, fmt.Sprintf("h%d.pgm", i+1))
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		px, err := parsePGM(data)
		if err != nil {
			continue
		}
		vi.Hashes[i] = computePHash(px)
		gotAny = true
	}
	count := 0
	for _, h := range vi.Hashes {
		if h != 0 {
			count++
		}
	}
	return gotAny && count >= sampleCount/2
}

// multiInputExtractHashes – EIN ffmpeg-Prozess pro Video holt ALLE 8 Sample-
// Frames über 8 Inputs (je ein Input-Seek -ss zum Keyframe nahe dem Ziel-
// Timestamp) und mappt sie auf 8 PGM-Outputs. Ersetzt im Long-Video-Pfad die
// bisherigen 8 separaten ffmpeg-Prozesse pro Video.
//
// useGPU=true (Sinn der Übung): -init_hw_device cuda:0=gpu wird EINMAL gesetzt,
// jeder Input referenziert es per -hwaccel_device gpu → alle 8 Inputs teilen
// EINEN CUDA-Context, CUDA-Init fällt also nur 1× statt 8× pro Video an. Der
// HEVC-Decode läuft auf NVDEC (GPU frei), die CPU ist saturiert. Da kein
// -hwaccel_output_format gesetzt ist, lädt ffmpeg die Frames automatisch in den
// System-RAM zurück, sodass der triviale 32x32-Graustufen-scale CPU-seitig läuft.
//
// useGPU=false: identischer Single-Call, aber reiner CPU-Decode (Fallback bzw.
// „CPU-Single-Call"-Vergleichswert).
//
// TUNING (wichtig beim Messen): Ein Prozess treibt jetzt 8 NVDEC-Decodes parallel.
// Bei DUPFINDER_FFMPEG_CONCURRENCY=16 laufen damit bis zu 16×8 Decoder-Kontexte
// gleichzeitig auf der GPU — das kann NVDEC-Sessions/VRAM übersättigen und den
// Durchsatz wieder drücken. Für den GPU-Single-Call daher mit NIEDRIGEREN Werten
// testen (z. B. set DUPFINDER_FFMPEG_CONCURRENCY=4 … 8) und hochtasten.
func multiInputExtractHashes(ffmpegPath string, vi *videoInfo, durSec float64,
	hashDir string, useGPU bool, indices []int) bool {

	args := []string{"-hide_banner", "-loglevel", "error"}
	if useGPU {
		// EINMALIGE CUDA-Device-Init; alle Inputs referenzieren dieses Device.
		args = append(args, "-init_hw_device", "cuda:0=gpu")
	}

	// Ein Input pro angefordertem Sample-Index (Input-Seek zum Ziel-Timestamp).
	for _, idx := range indices {
		ts := durSec * samplePositions[idx]
		if useGPU {
			args = append(args, "-hwaccel", "cuda", "-hwaccel_device", "gpu")
		}
		if fastSeek {
			// Nur zum nächstgelegenen Keyframe springen (kein Präzisions-Decode).
			args = append(args, "-noaccurate_seek")
		}
		args = append(args, "-ss", fmt.Sprintf("%.3f", ts), "-i", vi.Path)
	}

	// Ein Output pro Input: je 1 Frame, 32x32 Graustufen, als PGM.
	for n, idx := range indices {
		out := filepath.Join(hashDir, fmt.Sprintf("multi_h%d.pgm", idx))
		args = append(args,
			"-map", fmt.Sprintf("%d:v:0", n),
			"-frames:v", "1",
			"-vf", "scale=32:32:flags=area,format=gray",
			"-f", "image2",
			"-y", out,
		)
	}

	if err := runHiddenCmd(ffmpegPath, ffmpegTimeout, args...); err != nil {
		return false
	}

	gotAny := false
	count := 0
	for _, idx := range indices {
		out := filepath.Join(hashDir, fmt.Sprintf("multi_h%d.pgm", idx))
		data, err := os.ReadFile(out)
		if err != nil {
			continue
		}
		px, err := parsePGM(data)
		if err != nil {
			continue
		}
		vi.Hashes[idx] = computePHash(px)
		gotAny = true
		count++
	}
	// Akzeptiert, wenn mind. die Hälfte der ANGEFORDERTEN Frames da sind.
	return gotAny && count >= (len(indices)+1)/2
}

// singleExtractHashes – Two-Stage-Seek pro Frame, parallel. Bei langen Videos
// massiv schneller als Bulk, weil jeder Aufruf direkt zum Keyframe springt.
func singleExtractHashes(ffmpegPath string, vi *videoInfo, durSec float64, hashDir string) {
	var wg sync.WaitGroup
	for i, p := range samplePositions {
		if vi.Hashes[i] != 0 {
			continue // bereits durch Bulk-Versuch gesetzt
		}
		ts := durSec * p
		wg.Add(1)
		go func(idx int, t float64) {
			defer wg.Done()
			extractSingleHashFrame(ffmpegPath, vi.Path, t, idx, hashDir, vi)
		}(i, ts)
	}
	wg.Wait()
}

func extractSingleHashFrame(ffmpegPath, videoPath string, ts float64, idx int,
	hashDir string, vi *videoInfo) {

	preSeek, postSeek := splitSeek(ts)
	hashOut := filepath.Join(hashDir, fmt.Sprintf("single_h%d.pgm", idx))

	runOnce := func(useGPU bool) error {
		args := []string{"-hide_banner", "-loglevel", "error"}
		if useGPU {
			args = append(args, "-hwaccel", "cuda")
		}
		if fastSeek {
			// Schnell: nur Input-Seek zum Keyframe nahe ts, KEIN Praezisions-Decode
			// bis zum exakten Frame. Spart pro Sample den ~postSeek-langen Decode.
			args = append(args,
				"-ss", fmt.Sprintf("%.3f", ts),
				"-noaccurate_seek",
				"-i", videoPath,
				"-frames:v", "1",
				"-an", "-sn", "-dn",
				"-vf", "scale=32:32:flags=area,format=gray",
				"-f", "image2", "-y", hashOut,
			)
		} else {
			// Genau (Default): Two-Stage-Seek, dekodiert ~postSeek Sekunden bis
			// zum exakten Ziel-Frame. Robuster bei unterschiedlich enkodierten Kopien.
			if preSeek > 0 {
				args = append(args, "-ss", fmt.Sprintf("%.3f", preSeek))
			}
			args = append(args,
				"-i", videoPath,
				"-ss", fmt.Sprintf("%.3f", postSeek),
				"-frames:v", "1",
				"-an", "-sn", "-dn",
				"-vf", "scale=32:32:flags=area,format=gray",
				"-f", "image2", "-y", hashOut,
			)
		}
		return runHiddenCmd(ffmpegPath, ffmpegTimeout, args...)
	}

	err := runOnce(useHwAccel)
	if err != nil && useHwAccel {
		err = runOnce(false)
	}
	if err != nil {
		return
	}
	if data, err := os.ReadFile(hashOut); err == nil {
		if px, err := parsePGM(data); err == nil {
			vi.Hashes[idx] = computePHash(px)
		}
	}
}

// ============================================================================
// THUMB-PHASE: gleiche Hybrid-Strategie
// ============================================================================

func computeThumbsForVideos(state *appState, videos []*videoInfo, thumbDir string,
	counter *atomic.Int64, total int) {

	sem := make(chan struct{}, maxHashWorkers)
	var wg sync.WaitGroup
	for _, v := range videos {
		if allThumbsExist(thumbDir, v.Path) {
			counter.Add(1)
			updateProgress(state, "Thumbnails", int(counter.Load()), total, "Erstelle Vorschauen…")
			continue
		}
		if isShutdownRequested() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(vi *videoInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			processVideoThumbs(state.binaries.ffmpeg, vi, thumbDir)
			cur := counter.Add(1)
			if cur%5 == 0 || int(cur) == total {
				updateProgress(state, "Thumbnails", int(cur), total, "Erstelle Vorschauen…")
			}
		}(v)
	}
	wg.Wait()
}

func processVideoThumbs(ffmpegPath string, vi *videoInfo, thumbDir string) {
	if vi.Duration <= 0 {
		return
	}
	durSec := vi.Duration.Seconds()
	thumbPrefix := pathHash(vi.Path)

	if vi.Duration < bulkDurationLimit {
		if bulkExtractThumbs(ffmpegPath, vi.Path, durSec, thumbDir, thumbPrefix, useHwAccel) {
			return
		}
		if useHwAccel && bulkExtractThumbs(ffmpegPath, vi.Path, durSec, thumbDir, thumbPrefix, false) {
			return
		}
	}

	// Lange Videos ODER Bulk-Fallback: Single-Calls
	singleExtractThumbs(ffmpegPath, vi, durSec, thumbDir, thumbPrefix)
}

func bulkExtractThumbs(ffmpegPath, videoPath string, durSec float64,
	thumbDir, thumbPrefix string, useGPU bool) bool {

	thumbSet := map[int]bool{}
	for _, idx := range thumbIndices {
		thumbSet[idx] = true
	}

	var sb strings.Builder
	first := true
	for i, p := range samplePositions {
		if !thumbSet[i] {
			continue
		}
		if !first {
			sb.WriteByte('+')
		}
		first = false
		ts := durSec * p
		fmt.Fprintf(&sb, "between(t,%.3f,%.3f)", ts, ts+0.5)
	}
	selectExpr := sb.String()

	tmpDir, err := os.MkdirTemp(thumbDir, "tmp-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmpDir)
	outPattern := filepath.Join(tmpDir, "t%d.jpg")

	vfilter := fmt.Sprintf(
		"select='%s',format=yuv420p,scale=%d:%d:flags=lanczos:force_original_aspect_ratio=decrease",
		selectExpr, thumbWidth, thumbHeight)

	args := []string{"-hide_banner", "-loglevel", "error"}
	if useGPU {
		args = append(args, "-hwaccel", "cuda")
	}
	args = append(args,
		"-i", videoPath,
		"-an", "-sn", "-dn",
		"-vf", vfilter,
		"-vsync", "0",
		"-frames:v", fmt.Sprintf("%d", len(thumbIndices)),
		"-q:v", "5",
		"-f", "image2",
		"-start_number", "1",
		"-y", outPattern,
	)
	if err := runHiddenCmd(ffmpegPath, ffmpegTimeout, args...); err != nil {
		return false
	}

	successCount := 0
	for n, idx := range thumbIndices {
		src := filepath.Join(tmpDir, fmt.Sprintf("t%d.jpg", n+1))
		st, err := os.Stat(src)
		if err != nil || st.Size() == 0 {
			continue
		}
		dst := filepath.Join(thumbDir, fmt.Sprintf("%s_%d.jpg", thumbPrefix, idx))
		if err := os.Rename(src, dst); err == nil {
			successCount++
		}
	}
	return successCount == len(thumbIndices)
}

func singleExtractThumbs(ffmpegPath string, vi *videoInfo, durSec float64,
	thumbDir, thumbPrefix string) {

	var wg sync.WaitGroup
	for _, idx := range thumbIndices {
		if idx >= len(samplePositions) {
			continue
		}
		dst := filepath.Join(thumbDir, fmt.Sprintf("%s_%d.jpg", thumbPrefix, idx))
		if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
			continue // schon da
		}
		ts := durSec * samplePositions[idx]
		wg.Add(1)
		go func(i int, t float64) {
			defer wg.Done()
			extractSingleThumbFrame(ffmpegPath, vi.Path, t, i, thumbDir, thumbPrefix)
		}(idx, ts)
	}
	wg.Wait()
}

func extractSingleThumbFrame(ffmpegPath, videoPath string, ts float64, idx int,
	thumbDir, thumbPrefix string) {

	preSeek, postSeek := splitSeek(ts)
	finalThumb := filepath.Join(thumbDir, fmt.Sprintf("%s_%d.jpg", thumbPrefix, idx))
	tmpThumb := finalThumb + ".tmp"

	vfilter := fmt.Sprintf(
		"format=yuv420p,scale=%d:%d:flags=lanczos:force_original_aspect_ratio=decrease",
		thumbWidth, thumbHeight)

	runOnce := func(useGPU bool) error {
		args := []string{"-hide_banner", "-loglevel", "error"}
		if useGPU {
			args = append(args, "-hwaccel", "cuda")
		}
		if preSeek > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", preSeek))
		}
		args = append(args,
			"-i", videoPath,
			"-ss", fmt.Sprintf("%.3f", postSeek),
			"-frames:v", "1",
			"-an", "-sn", "-dn",
			"-vf", vfilter,
			"-q:v", "5",
			"-f", "image2", "-y", tmpThumb,
		)
		return runHiddenCmd(ffmpegPath, ffmpegTimeout, args...)
	}

	err := runOnce(useHwAccel)
	if err != nil && useHwAccel {
		_ = os.Remove(tmpThumb)
		err = runOnce(false)
	}
	if err != nil {
		_ = os.Remove(tmpThumb)
		return
	}
	if st, err := os.Stat(tmpThumb); err != nil || st.Size() == 0 {
		_ = os.Remove(tmpThumb)
		return
	}
	_ = os.Rename(tmpThumb, finalThumb)
}

// ============================================================================
// Helpers
// ============================================================================

func splitSeek(ts float64) (preSeek, postSeek float64) {
	if ts > 5.0 {
		return ts - 2.0, 2.0
	}
	return 0, ts
}

func allThumbsExist(thumbDir, videoPath string) bool {
	prefix := pathHash(videoPath)
	for _, idx := range thumbIndices {
		p := filepath.Join(thumbDir, fmt.Sprintf("%s_%d.jpg", prefix, idx))
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// ============================================================================
// Confirmed Clusters
// ============================================================================

func buildConfirmedClusters(clusters []cluster, extraPairs [][2]*videoInfo, ignored *ignoreList) []*clusterState {
	type pair struct{ a, b *videoInfo }
	var pairs []pair
	for _, cl := range clusters {
		matches := verifyClusterMatches(cl)
		for _, m := range matches {
			pairs = append(pairs, pair{cl.videos[m[0]], cl.videos[m[1]]})
		}
	}
	// V6 — bereits pHash-bestätigte Dateiname-Bridge-Paare (cluster-übergreifend).
	for _, bp := range extraPairs {
		pairs = append(pairs, pair{bp[0], bp[1]})
	}

	parent := map[*videoInfo]*videoInfo{}
	var find func(v *videoInfo) *videoInfo
	find = func(v *videoInfo) *videoInfo {
		if parent[v] == nil {
			parent[v] = v
			return v
		}
		if parent[v] == v {
			return v
		}
		root := find(parent[v])
		parent[v] = root
		return root
	}
	for _, p := range pairs {
		ra, rb := find(p.a), find(p.b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	groups := map[*videoInfo][]*videoInfo{}
	for v := range parent {
		groups[find(v)] = append(groups[find(v)], v)
	}

	var result []*clusterState
	for _, vs := range groups {
		// 1. Alle ignorierten Videos aus der Liste werfen
		var validVs []*videoInfo
		for _, v := range vs {
			if !ignored.has(v.Path) {
				validVs = append(validVs, v)
			}
		}

		// 2. Wenn nach dem Rauswurf weniger als 2 Videos übrig sind, ist es kein Duplikat mehr
		if len(validVs) < 2 {
			continue
		}

		// 3. Mit den verbleibenden Videos weitermachen
		vs = validVs

		sort.Slice(vs, func(i, j int) bool {
			return vs[i].Size > vs[j].Size
		})

		paths := make([]string, len(vs))
		for i, v := range vs {
			paths[i] = v.Path
		}
		sort.Strings(paths)
		var sb strings.Builder
		for _, p := range paths {
			sb.WriteString(p)
			sb.WriteByte('|')
		}
		idHash := sha256.Sum256([]byte(sb.String()))
		clusterID := hex.EncodeToString(idHash[:])[:12]

		var totalSize, maxSize int64
		for _, v := range vs {
			totalSize += v.Size
			if v.Size > maxSize {
				maxSize = v.Size
			}
		}
		result = append(result, &clusterState{
			ID:        clusterID,
			Videos:    vs,
			TotalSize: totalSize,
			Savings:   totalSize - maxSize,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Savings > result[j].Savings
	})
	return result
}
