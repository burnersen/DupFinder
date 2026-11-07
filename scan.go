// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
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
	// PERF: 6 sample frames instead of 8. Each video decodes 6 frames now
	// (about 25 % less decoding in the hash hot path), with the same
	// selectivity after grouping by duration.
	sampleCount       = 6
	durationTolerance = 500 * time.Millisecond
	maxHashDistance   = 10
	// minMatchingFrames: 4 of 6. Keeps the same absolute slack as before
	// (6 of 8): up to 2 sample frames may differ (black frames, letterbox,
	// credits). For a stricter filter: 5.
	minMatchingFrames = 4
	maxProbeWorkers   = 64
	maxHashWorkers    = 32

	thumbWidth  = 240
	thumbHeight = 135

	videoExtensions = ".mkv,.mp4,.m4v,.avi,.mov,.webm,.ts,.m2ts,.wmv,.flv,.mpg,.mpeg,.vob,.3gp,.divx"

	// Threshold of the hybrid strategy:
	//   < bulkDurationLimit  bulk extract (1 ffmpeg call, decodes LINEARLY without seeking)
	//   >= bulkDurationLimit single calls with two-stage seek (jump to keyframe)
	//
	// PERF FIX: was 5*time.Minute. The bulk path uses the select filter, which
	// CANNOT seek: ffmpeg decodes the video linearly from t=0 to the last
	// sample at 95 % and throws almost every frame away. For videos of several
	// minutes (1080p/4K) that is the main cost of the first hashing run.
	// With 20 s practically every real video takes the single-seek path (jumps
	// straight to the keyframe per sample, decodes about 1 GOP instead of half the file).
	// Only clips of a few seconds stay bulk, where a linear decode is trivial.
	bulkDurationLimit = 20 * time.Second

	// Timeouts for external processes: generous, so that legitimately slow
	// extractions (4K, HDD, high parallelism) are NOT killed by mistake. They
	// only apply when something really hangs (broken file, stalled network
	// drive, GPU driver lock) and then stop what used to block forever.
	ffprobeTimeout = 90 * time.Second
	ffmpegTimeout  = 180 * time.Second

	// cacheSaveInterval limits how often cache.json is saved during the hash
	// phase. Instead of rewriting the WHOLE file after every 5 groups (very
	// many expensive full writes for large libraries), it is saved at most
	// every few seconds. If the scan stops, only the last time window is lost;
	// the final save at the end of the scan is not affected.
	cacheSaveInterval = 15 * time.Second
)

// useHwAccel chooses the decoding of the HASH phase on the long-video path
// and can be switched with DUPFINDER_HWACCEL WITHOUT rebuilding:
//
//	DEFAULT (not set / =0): CPU single call (multiInputExtractHashes, useGPU=false):
//	    ONE ffmpeg process per video fetches all sample frames through several
//	    -ss ... -i inputs mapped to PGM outputs. Pure CPU decoding: saturates the
//	    CPU (fastest path on 8C/16T + NVMe) and saves about sampleCount-1 ffmpeg
//	    starts PER VIDEO compared with the old path (starting a process is expensive on Windows).
//
//	DUPFINDER_HWACCEL=1: GPU single call (useGPU=true):
//	    The same single call, but HEVC decoding on NVDEC via -init_hw_device
//	    cuda:0=gpu (once) + -hwaccel_device gpu (per input, one shared CUDA
//	    context). Measured SLOWER on the developer's hardware (the GPU idles
//	    between the seeks), so not the default. Kept as an option.
//
//	GPU on:  set DUPFINDER_HWACCEL=1   (=true / =on / =ja / =an)
//	GPU off: leave the variable out (default)
var useHwAccel = hwAccelEnabled()

func hwAccelEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUPFINDER_HWACCEL"))) {
	case "1", "true", "on", "yes", "ja", "an":
		return true
	}
	return false // default: CPU single call (fastest on the developer's hardware)
}

// fastSeek chooses the seek strategy of the HASH phase (not the thumbnails).
// ON by default: per sample, an input seek jumps to the nearest keyframe and
// takes that frame; the expensive precise decode from the keyframe to the
// exact target frame is skipped. That is by far the biggest speed lever of
// the first hashing run. A DCT pHash does not need frame accuracy (only
// minMatchingFrames of sampleCount have to match anyway).
// Trade-off: for copies encoded differently (another keyframe grid, mostly
// at hard cuts) a slightly lower hit rate. To get the exact two-stage seek
// (slower) back, switch fast seek off:
//
//	exact (slower):  set DUPFINDER_FAST_SEEK=0   (=off / =nein / =aus)
//	fast (default):  leave the variable out
var fastSeek = fastSeekEnabled()

func fastSeekEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUPFINDER_FAST_SEEK"))) {
	case "0", "false", "off", "no", "nein", "aus":
		return false // switched off on purpose: exact seeking
	}
	return true // default: fast seek on (fastest path)
}

// ffmpegSem caps the TOTAL number of ffmpeg processes running at the same
// time across all group and frame goroutines. The real bottleneck is not
// the number of workers but how many ffmpeg decodes (GPU NVDEC, disk) run
// in parallel. Default NumCPU; DUPFINDER_FFMPEG_CONCURRENCY overrides it.
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

// probeWorkers limits the parallel ffprobe calls (light, but on a single
// HDD or SATA disk they would thrash I/O otherwise). 2 x NumCPU, capped.
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

// runHiddenCmd runs an external command without a window, tied to the
// global appCtx (cancelled on shutdown) AND with a hard deadline, so one
// hanging file can no longer block a worker for good.
func runHiddenCmd(path string, timeout time.Duration, args ...string) error {
	// Global process limit: take a slot first, or leave at once on shutdown
	// without blocking.
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
	if err := startTracked(cmd); err != nil {
		return err
	}
	return cmd.Wait()
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
	// FullHashed=true: every sample frame was decoded (a complete set, even if
	// single frames are legitimately 0, e.g. a black frame). false: only the
	// probe frames are there (progressive hashing, no partner). A second run can
	// reuse cached probe hashes this way without decoding again, and still be
	// right if a partner turns up later.
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
// Discovery
// ============================================================================

// skipDirNames: folder names skipped while collecting (the Windows recycle
// bin of every partition, System Volume Information). Compared in lower
// case.
var skipDirNames = map[string]bool{
	"$recycle.bin":              true,
	"$recycle.bin~":             true,
	"recycler":                  true, // old recycle bin name (before Vista)
	"system volume information": true,
}

func collectVideoFiles(roots []string) ([]string, error) {
	extSet := map[string]bool{}
	for _, e := range strings.Split(videoExtensions, ",") {
		extSet[strings.ToLower(strings.TrimSpace(e))] = true
	}

	// visited is shared by all roots (protects against loops and doubles with
	// overlapping or nested roots), hence the mutex.
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

	// walk collects into a LOCAL slice (one per root goroutine), which avoids
	// lock contention when appending. They are merged at the end.
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

			// PERF: no os.Stat per entry: ReadDir already says the type. Only
			// symlinks and junctions (reparse points) are resolved, to keep the old
			// behaviour (follow a symlink to a folder).
			isDir := e.IsDir()
			if !isDir && e.Type()&os.ModeSymlink != 0 {
				if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
					isDir = info.IsDir()
				}
			}

			full := filepath.Join(dir, name)
			if isDir {
				// Skip Windows system and recycle bin folders: Explorer hides them
				// (hidden + system), and deleted files land there. Showing them as
				// duplicate candidates is confusing and could bring back matches that
				// were just deleted on the next scan.
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

	// Walk the roots IN PARALLEL: a real speedup for separate disks (each
	// disk has its own controller and head, they do not block each other).
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
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = startTracked(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	out := stdout.Bytes()
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
		info.ErrorMessage = "no video metadata"
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

// filenameDurWindow: duration window for file name bridge pairs. Changing
// the container shifts the reported duration a little only; more than 5 s means other content.
const filenameDurWindow = 5 * time.Second

// filenameNoiseTokens: tags that say nothing about identity and are
// ignored when tokenizing, so that "1080p" does not bridge two videos by chance.
var filenameNoiseTokens = map[string]bool{
	"1080p": true, "720p": true, "2160p": true, "480p": true, "4k": true, "uhd": true,
	"h264": true, "h265": true, "x264": true, "x265": true, "hevc": true, "avc": true,
	"web": true, "webrip": true, "webdl": true, "bluray": true, "bdrip": true, "brrip": true,
	"hdtv": true, "dvdrip": true, "xvid": true, "divx": true, "ts": true, "hdr": true,
	"aac": true, "ac3": true, "dts": true, "mp3": true, "10bit": true, "8bit": true,
	"proper": true, "repack": true, "internal": true, "remux": true, "extended": true,
	"the": true, "and": true, "von": true, "der": true, "die": true, "das": true, "und": true, // English and German filler words
}

// tokenizeName splits a file name (without extension) into normalised
// tokens: lower case, split at anything but letters and digits, noise and short tokens dropped.
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

// computeFilenameBridges returns video pairs that (a) are NOT in the same
// duration group already (those are compared anyway), (b) share at least 2
// meaningful title tokens and (c) differ in duration by at most
// filenameDurWindow. These pairs are checked by pHash later: the name only
// triggers the comparison, it never proves a match.
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

	// Token index: token to videos. Very common tokens (in more than
	// maxTokenSpread videos) say nothing (e.g. a studio name everywhere) and are
	// ignored, which also caps the number of pairs.
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
	for _, idxs := range tokenIndex {
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
	}

	var pairs [][2]*videoInfo
	for k, n := range shared {
		if n < 2 { // at least 2 shared meaningful tokens
			continue
		}
		a, b := valid[k.a], valid[k.b]
		d := a.Duration - b.Duration
		if d < 0 {
			d = -d
		}
		if d > filenameDurWindow {
			continue // durations too different: other content
		}
		if d <= durationTolerance {
			continue // same duration group: compared anyway
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

// saveCache stores the cache. A cache that cannot be saved costs only time on
// the next scan, so this goes to the log and not to the screen.
func saveCache(state *appState, path string) {
	if err := state.cache.save(path); err != nil {
		slog.Warn("cache not saved", "err", err)
	}
}

// ============================================================================
// Full scan
// ============================================================================

func runFullScan(state *appState, cacheFile string) error {
	if !state.scanning.CompareAndSwap(false, true) {
		return errors.New("a scan is running already")
	}
	defer state.scanning.Store(false)

	state.mu.RLock()
	roots := append([]string(nil), state.roots...)
	state.mu.RUnlock()

	updateProgress(state, "Discovery", 0, 0, "Collecting files...")
	files, err := collectVideoFiles(roots)
	if err != nil && !isShutdownRequested() {
		return err
	}
	updateProgress(state, "Discovery", len(files), len(files),
		fmt.Sprintf("%d files found", len(files)))

	// Clean up orphaned cache entries: only files that are gone for sure and
	// were under the scanned roots. Keeps cache.json small when many files are
	// moved, renamed or deleted.
	if n := state.cache.pruneMissing(roots); n > 0 {
		slog.Info("cache pruned", "removed", n)
	}

	if len(files) < 2 {
		state.mu.Lock()
		state.clusters = nil
		state.mu.Unlock()
		state.bumpVersion()
		updateProgress(state, "Done", 0, 0, "Too few files")
		return nil
	}

	infos := indexFiles(state, files)
	saveCache(state, cacheFile)

	var rerr int64
	for i := range infos {
		if infos[i].HasError {
			rerr++
		}
	}
	state.readErrors.Store(rerr)
	slog.Info("indexed", "files", len(files), "readErrors", rerr)

	updateProgress(state, "Clustering", 0, 0, "Grouping...")
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
		updateProgress(state, "Done", 0, 0, "No duplicates")
		return nil
	}

	// File name bridge: the same content in different containers can report
	// durations more than 500 ms apart, land in neighbouring groups and never
	// be compared. Videos with clearly similar file names (shared title tokens)
	// within a looser duration window become ADDITIONAL comparison pairs. The
	// pHash still decides (at least minMatchingFrames), so equal names alone
	// never make a false match. These videos are hashed in full (forceFull),
	// so progressive hashing does not cut them down to the probe frames.
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
			fmt.Sprintf("Group %d/%d", ci+1, len(clusters)))
		// Save on a timer instead of after a fixed number of groups: avoids
		// rewriting all of cache.json again and again in large runs.
		if time.Since(lastCacheSave) >= cacheSaveInterval {
			saveCache(state, cacheFile)
			lastCacheSave = time.Now()
		}
	}
	saveCache(state, cacheFile)

	// Confirm bridge candidates by pHash (same threshold as within a group).
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
		updateProgress(state, "Done", 0, 0, "No duplicates")
		return nil
	}

	thumbDir := filepath.Join(state.dataDir, "thumbs")
	totalThumbVideos := 0
	for _, cs := range confirmed {
		totalThumbVideos += len(cs.Videos)
	}
	var thumbCounter atomic.Int64
	updateProgress(state, "Thumbnails", 0, totalThumbVideos, "Making previews...")

	for _, cs := range confirmed {
		if isShutdownRequested() {
			break
		}
		computeThumbsForVideos(state, cs.Videos, thumbDir, &thumbCounter, totalThumbVideos)
	}

	updateProgress(state, "Done", len(confirmed), len(confirmed),
		fmt.Sprintf("%d duplicate group(s)", len(confirmed)))
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
	updateProgress(state, "Indexing", 0, int(total), "Reading metadata...")

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
					updateProgress(state, "Indexing", int(d), int(total), "Reading metadata...")
				}
			}
		}()
	}
	wg.Wait()
	updateProgress(state, "Indexing", int(total), int(total), "Indexing done")
	return results
}

// ============================================================================
// HASH PHASE: hybrid strategy by video length
//   < bulkDurationLimit  bulk extract (sequential reading, fast for short clips)
//   >= bulkDurationLimit single calls with two-stage seek (jump to keyframe)
// ============================================================================

// probeIndices are the frames decoded first. With sampleCount=6 and
// minMatchingFrames=4, at most 2 of the 6 frames may differ, so a real
// duplicate has at least 1 match among ANY 3 frames. With 3 probe frames, a
// video whose probes match nobody cannot be a duplicate, and the remaining
// frames are saved (provably without losing a match).
var probeIndices, restIndices = splitProbeRest()

func splitProbeRest() (probe, rest []int) {
	// budget = allowed misses; the probe needs budget+1 frames, so that "all
	// probes miss" rules out a match for sure.
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
	// Exact duplicate prefilter: byte-identical copies are recognised BEFORE a
	// single frame is decoded (content fingerprint instead of decoding).
	copyOf := exactDupeReps(cl.videos)
	hasCopy := map[*videoInfo]bool{}
	for _, rep := range copyOf {
		hasCopy[rep] = true // representative with copies: known partner, hash in full
	}

	// 1) Fill from the cache and sort into:
	//    fullCached: every frame decoded, done (no decoding).
	//    probeDone:  only probe hashes from the cache, NO probe decoding needed,
	//                but goes through the alive check (a partner may have come).
	//    toProbe:    nothing yet, decode the probe frames.
	var toProbe, probeDone []*videoInfo
	for _, v := range cl.videos {
		if len(v.Hashes) != sampleCount {
			if cached, ok := state.cache.get(v.Path, v.Size, v.ModTime); ok && len(cached.Hashes) == sampleCount {
				v.Hashes = cached.Hashes
				v.FullHashed = cached.FullHashed
			}
		}
		if _, isCopy := copyOf[v]; isCopy {
			// Byte copy: handled AND counted in step 6 only. Must come BEFORE the
			// FullHashed count, or a copy that is fully cached already would be
			// counted twice (here and in step 6) and the progress would go past
			// 100 %.
			continue
		}
		if len(v.Hashes) == sampleCount && v.FullHashed {
			counter.Add(1)
			continue // fully cached: nothing to do
		}
		if len(v.Hashes) == sampleCount && hasProbe(v) {
			probeDone = append(probeDone, v) // probe hashes from the cache
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

	// 2) STAGE A: decode probe frames only for candidates that are really new
	//    (probeDone has its probes from the cache already, no decoding).
	hashSubset(toProbe, probeIndices)

	// 3) Alive check: does the candidate need the rest? Only if its probe frames
	//    match at least one other video of the group (or it is forced to full:
	//    byte representative, file name bridge, incomplete probes).
	isLive := func(v *videoInfo) bool {
		if hasCopy[v] || (forceFull != nil && forceFull[v]) {
			return true
		}
		if !allProbesSet(v) {
			return true // probe frame missing: hash in full to be safe
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

	// 4) STAGE B: decode the remaining frames for alive candidates only.
	hashSubset(needRest, restIndices)
	for _, v := range needRest {
		v.FullHashed = true
	}

	// 5) Cache EVERYTHING that was decoded (probe only OR full), so a second
	//    run decodes nothing again. Candidates that are not alive keep
	//    FullHashed=false (probe hashes only); if a partner comes later, the
	//    rest is decoded then. probeDone that stays not alive is cached
	//    correctly already: not written again.
	for _, v := range toProbe {
		state.cache.set(*v) // probe only (FullHashed=false) or full (true)
		counter.Add(1)
	}
	for _, v := range probeDone {
		if v.FullHashed {
			state.cache.set(*v) // was probe only, now full: update
		}
		counter.Add(1)
	}

	// 6) Byte copies take over the pHashes of their (fully hashed) representative.
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

// hasProbe reports whether at least one probe frame is set.
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

// allProbesSet reports whether ALL probe frames were decoded.
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

// probeMatch: do a and b share at least one matching probe frame (Hamming
// distance at most maxHashDistance)? Only then can the pair be a duplicate.
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

// exactDupeReps groups the videos of a group by byte identity and returns
// the representative of every byte-identical copy (copy to representative).
// Only videos that share their EXACT size with at least one other are
// fingerprinted at all (no other can be a byte copy), which keeps the
// extra I/O minimal. The fingerprint covers size plus the first and last
// 8 MB; for real video files a wrong match is practically impossible.
func exactDupeReps(videos []*videoInfo) map[*videoInfo]*videoInfo {
	copyOf := map[*videoInfo]*videoInfo{}
	if len(videos) < 2 {
		return copyOf
	}
	// 1) group by exact size first (free, uses the metadata we have)
	bySize := map[int64][]*videoInfo{}
	for _, v := range videos {
		if v.Size > 0 {
			bySize[v.Size] = append(bySize[v.Size], v)
		}
	}
	// 2) fingerprint only where sizes collide, then group by fingerprint
	for _, group := range bySize {
		if len(group) < 2 {
			continue
		}
		byFP := map[string]*videoInfo{} // fingerprint to the representative seen first
		for _, v := range group {
			fp := contentFingerprint(v.Path, v.Size)
			if fp == "" {
				continue // read error: to be safe, go the normal pHash way
			}
			if rep, ok := byFP[fp]; ok {
				copyOf[v] = rep // byte-identical to rep
			} else {
				byFP[fp] = v // first with this fingerprint = representative
			}
		}
	}
	return copyOf
}

// contentFingerprint makes a strong byte identity key without decoding:
// SHA-256 over the file size plus the first and last fpEdgeBytes. Files
// smaller than 2 x fpEdgeBytes are hashed completely. On a read error "" (the
// caller then falls back to the normal pHash path).
func contentFingerprint(path string, size int64) string {
	const fpEdgeBytes = 8 << 20 // 8 MiB per end
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

// processVideoHashes decodes the sample frames named in `indices` into
// vi.Hashes[idx]. indices==nil means ALL frames. Hashes already set stay
// (important for the progressive 2-stage hashing: probe frames first, then
// the rest if needed, without overwriting the probe results).
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

	// Hybrid: short videos go bulk (one call decodes all frames anyway, partial
	// progressive decoding does not pay off), long ones a single call.
	if vi.Duration < bulkDurationLimit {
		if bulkExtractHashes(ffmpegPath, vi.Path, durSec, hashDir, vi, useHwAccel) {
			return
		}
		if useHwAccel && bulkExtractHashes(ffmpegPath, vi.Path, durSec, hashDir, vi, false) {
			return
		}
		// Bulk failed: fall back to the long-video path anyway
	}

	// Long videos OR bulk fallback: ONE ffmpeg process per video (single call).
	if useHwAccel {
		// Optional (DUPFINDER_HWACCEL=1): HEVC decoding on NVDEC.
		if multiInputExtractHashes(ffmpegPath, vi, durSec, hashDir, true, indices) {
			return
		}
		// GPU failed (no CUDA or driver): CPU single call.
	}
	// DEFAULT: CPU single call (1 process, saturates the CPU, saves process starts).
	if multiInputExtractHashes(ffmpegPath, vi, durSec, hashDir, false, indices) {
		return
	}
	// Single call failed (broken or unusual file): the most robust path,
	// frame by frame (fills only the hashes still 0).
	singleExtractHashes(ffmpegPath, vi, durSec, hashDir)
}

var allIndices = func() []int {
	idx := make([]int, sampleCount)
	for i := range idx {
		idx[i] = i
	}
	return idx
}()

// bulkExtractHashes: a single ffmpeg call extracts all sample frames with
// the select filter. Only sensible for short videos.
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
		"-fps_mode", "passthrough", // was -vsync 0: no longer in the FFmpeg 9.0 manual, -fps_mode exists since 5.1
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

// multiInputExtractHashes: ONE ffmpeg process per video fetches ALL sample
// frames through one input per frame (each an input seek -ss to the keyframe
// near the target time) mapped to one PGM output each. Replaces the separate
// ffmpeg process per frame on the long-video path.
//
// useGPU=true: -init_hw_device cuda:0=gpu is set ONCE and every input refers
// to it with -hwaccel_device gpu, so all inputs share ONE CUDA context and
// CUDA starts once per video instead of once per frame. HEVC decoding runs
// on NVDEC (GPU), the CPU is saturated. Without -hwaccel_output_format,
// ffmpeg copies the frames back into system memory by itself, so the trivial
// 32x32 grey scale runs on the CPU.
//
// useGPU=false: the same single call with pure CPU decoding (fallback, and
// the "CPU single call" to compare with).
//
// TUNING (important when measuring): one process now drives one NVDEC decode
// per frame in parallel. With DUPFINDER_FFMPEG_CONCURRENCY=16 that is up to 16
// times as many decoder contexts on the GPU at once, which can oversaturate
// NVDEC sessions or VRAM and lower the throughput again. For the GPU single
// call, test with LOWER values (e.g. set DUPFINDER_FFMPEG_CONCURRENCY=4 to 8) and go up.
func multiInputExtractHashes(ffmpegPath string, vi *videoInfo, durSec float64,
	hashDir string, useGPU bool, indices []int) bool {

	args := []string{"-hide_banner", "-loglevel", "error"}
	if useGPU {
		// Initialise the CUDA device ONCE; every input refers to it.
		args = append(args, "-init_hw_device", "cuda:0=gpu")
	}

	// One input per requested sample index (input seek to the target time).
	for _, idx := range indices {
		ts := durSec * samplePositions[idx]
		if useGPU {
			args = append(args, "-hwaccel", "cuda", "-hwaccel_device", "gpu")
		}
		if fastSeek {
			// Jump to the nearest keyframe only (no precise decoding).
			args = append(args, "-noaccurate_seek")
		}
		args = append(args, "-ss", fmt.Sprintf("%.3f", ts), "-i", vi.Path)
	}

	// One output per input: 1 frame each, 32x32 grey, as PGM.
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
	// Accepted when at least half of the REQUESTED frames are there.
	return gotAny && count >= (len(indices)+1)/2
}

// singleExtractHashes: two-stage seek per frame, in parallel. For long
// videos far faster than bulk, since every call jumps straight to the keyframe.
func singleExtractHashes(ffmpegPath string, vi *videoInfo, durSec float64, hashDir string) {
	var wg sync.WaitGroup
	for i, p := range samplePositions {
		if vi.Hashes[i] != 0 {
			continue // set by the bulk attempt already
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
			// Fast: input seek to the keyframe near ts only, NO precise decoding to
			// the exact frame. Saves the decode of about postSeek per sample.
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
			// Exact: two-stage seek, decodes about postSeek seconds up to the exact
			// target frame. More robust for copies encoded differently.
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
// THUMBNAIL PHASE: the same hybrid strategy
// ============================================================================

func computeThumbsForVideos(state *appState, videos []*videoInfo, thumbDir string,
	counter *atomic.Int64, total int) {

	sem := make(chan struct{}, maxHashWorkers)
	var wg sync.WaitGroup
	for _, v := range videos {
		if allThumbsExist(thumbDir, v.Path) {
			counter.Add(1)
			updateProgress(state, "Thumbnails", int(counter.Load()), total, "Making previews...")
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
				updateProgress(state, "Thumbnails", int(cur), total, "Making previews...")
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

	// Long videos OR bulk fallback: single calls
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
		"-fps_mode", "passthrough", // was -vsync 0: no longer in the FFmpeg 9.0 manual, -fps_mode exists since 5.1
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
			continue // there already
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
		_ = os.Remove(tmpThumb) // ignore: cleanup after a failed thumbnail
		err = runOnce(false)
	}
	if err != nil {
		_ = os.Remove(tmpThumb) // ignore: cleanup after a failed thumbnail
		return
	}
	if st, err := os.Stat(tmpThumb); err != nil || st.Size() == 0 {
		_ = os.Remove(tmpThumb) // ignore: cleanup after a failed thumbnail
		return
	}
	if err := os.Rename(tmpThumb, finalThumb); err != nil {
		// The page shows an empty frame for it; the next scan tries again.
		slog.Warn("thumbnail not saved", "file", finalThumb, "err", err)
	}
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
// Confirmed groups
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
	// File name bridge pairs already confirmed by pHash (across groups).
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
		// 1. Drop every ignored video from the list
		var validVs []*videoInfo
		for _, v := range vs {
			if !ignored.has(v.Path) {
				validVs = append(validVs, v)
			}
		}

		// 2. With fewer than 2 videos left after that, it is no duplicate any more
		if len(validVs) < 2 {
			continue
		}

		// 3. Go on with the remaining videos
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
