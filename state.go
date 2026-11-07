// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// The files DupFinder keeps between runs, all JSON in .DupFinder.data:
//
//	cache.json    probe results and frame hashes per file (saves time only)
//	ignored.json  files the user chose to ignore (the user's decisions)
//	session.json  groups already decided, so they stay decided
//
// All are written atomically (files.go). A file that cannot be read is
// never overwritten silently: it is moved aside and the user is told, since
// it may hold decisions that took hours.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// readState fills v from the JSON file at path. A missing file is fine
// (first start). A file that is not valid JSON is renamed to
// <name>.broken-<time> and a warning for the user comes back; v stays empty.
func readState(path string, v any) (warning string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fileError(err, "read", path)
	}
	if err := json.Unmarshal(data, v); err == nil {
		return "", nil
	}
	aside := fmt.Sprintf("%s.broken-%s", path, time.Now().Format("20060102-150405"))
	if err := os.Rename(path, aside); err != nil {
		return "", userError(err, "Cannot read "+path+", and cannot move it aside either",
			"Close other programs that may have it open, or move the file away yourself, then start again.")
	}
	return fmt.Sprintf("%s could not be read and was kept as %s. DupFinder starts without it.",
		filepath.Base(path), filepath.Base(aside)), nil
}

func writeState(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return writeFileAtomic(path, data, 0o644)
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

func loadCache(path string) (*cache, string, error) {
	c := &cache{entries: map[string]videoInfo{}}
	var entries []cacheEntry
	warning, err := readState(path, &entries)
	for _, e := range entries {
		c.entries[e.Key] = e.Info
	}
	return c, warning, err
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
	if err := writeState(path, entries); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// pruneMissing removes entries whose file is gone for sure (os.Stat says it
// does not exist) AND whose path lies under one of the roots being scanned.
// Moved, deleted and renamed files are cleaned up this way, while entries of
// disks that are not scanned right now (or offline) stay. Returns the number
// of entries removed.
func (c *cache) pruneMissing(roots []string) int {
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
			continue // outside the scanned roots: leave alone
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
// Ignore list
// ============================================================================

type ignoreList struct {
	mu    sync.Mutex
	paths map[string]bool
}

func loadIgnoreList(path string) (*ignoreList, string, error) {
	il := &ignoreList{paths: map[string]bool{}}
	var paths []string
	warning, err := readState(path, &paths)
	for _, p := range paths {
		il.paths[p] = true
	}
	return il, warning, err
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
	return writeState(path, paths)
}

// ============================================================================
// Session: decided groups
// ============================================================================

type sessionEntry struct {
	Decided   bool       `json:"decided"`
	Decisions []decision `json:"decisions"`
}

type sessionData struct {
	Clusters map[string]sessionEntry `json:"clusters"`
}

func sessionPath(dataDir string) string { return filepath.Join(dataDir, "session.json") }

func loadSession(dataDir string) (sessionData, string, error) {
	sess := sessionData{}
	warning, err := readState(sessionPath(dataDir), &sess)
	if sess.Clusters == nil {
		sess.Clusters = map[string]sessionEntry{}
	}
	return sess, warning, err
}

// saveSessionLocked writes the decided groups. The caller holds state.mu.
func saveSessionLocked(state *appState) error {
	sess := sessionData{Clusters: map[string]sessionEntry{}}
	for _, cl := range state.clusters {
		if cl.Decided {
			sess.Clusters[cl.ID] = sessionEntry{Decided: cl.Decided, Decisions: cl.Decisions}
		}
	}
	return writeState(sessionPath(state.dataDir), sess)
}
