// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectVideoFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"a.mkv", "b.MP4", "sub/c.avi", "sub/deeper/d.webm",
		"notes.txt", "cover.jpg",
		"$RECYCLE.BIN/deleted.mkv", "System Volume Information/x.mp4",
	} {
		writeFile(t, filepath.Join(root, f), []byte("x"))
	}
	got, err := collectVideoFiles([]string{root, root}) // the same root twice
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, f := range got {
		r, _ := filepath.Rel(root, f)
		rel = append(rel, filepath.ToSlash(r))
	}
	want := []string{"a.mkv", "b.MP4", "sub/c.avi", "sub/deeper/d.webm"}
	if !reflect.DeepEqual(rel, want) {
		t.Fatalf("got %v, want %v", rel, want)
	}
}

func TestTokenizeNameDropsNoiseAndShortTokens(t *testing.T) {
	got := tokenizeName(filepath.Join("films", "The.Big.Holiday.2019.1080p.x265-ab.mkv"))
	want := map[string]bool{"big": true, "holiday": true, "2019": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func vid(path string, sec float64) videoInfo {
	return videoInfo{Path: path, Size: int64(sec * 1000), Duration: time.Duration(sec * float64(time.Second)), Width: 1920}
}

func TestBuildDurationClusters(t *testing.T) {
	infos := []videoInfo{
		vid("a", 100), vid("b", 100.4), // within 500 ms of each other
		vid("c", 200), // alone
		vid("d", 300), vid("e", 300.2), vid("f", 300.5),
		{Path: "broken", Duration: 100 * time.Second, HasError: true},
	}
	var got [][]string
	for _, cl := range buildDurationClusters(infos) {
		var names []string
		for _, v := range cl.videos {
			names = append(names, v.Path)
		}
		got = append(got, names)
	}
	want := [][]string{{"a", "b"}, {"d", "e", "f"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilenameBridges(t *testing.T) {
	infos := []videoInfo{
		vid("Great Escape Movie.mkv", 100),
		vid("great.escape.movie.1080p.mp4", 103), // same title, 3 s longer
		vid("great escape movie extra.avi", 120), // same title, far too long
		vid("Something Else Entirely.mkv", 101),
	}
	pairs := computeFilenameBridges(infos)
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}
	names := []string{pairs[0][0].Path, pairs[0][1].Path}
	sort.Strings(names)
	if names[0] != "Great Escape Movie.mkv" || names[1] != "great.escape.movie.1080p.mp4" {
		t.Fatalf("got %v", names)
	}
}

func hashes(v uint64) []uint64 {
	h := make([]uint64, sampleCount)
	for i := range h {
		h[i] = v ^ uint64(i)<<40
	}
	return h
}

func TestBuildConfirmedClusters(t *testing.T) {
	a := &videoInfo{Path: "a.mkv", Size: 300, Hashes: hashes(0xF0F0)}
	b := &videoInfo{Path: "b.mkv", Size: 100, Hashes: hashes(0xF0F0)}
	c := &videoInfo{Path: "c.mkv", Size: 200, Hashes: hashes(^uint64(0xF0F0))} // other content
	d := &videoInfo{Path: "d.mkv", Size: 50, Hashes: hashes(0xF0F0)}
	ignored := &ignoreList{paths: map[string]bool{}}

	got := buildConfirmedClusters([]cluster{{videos: []*videoInfo{a, b, c}}}, [][2]*videoInfo{{b, d}}, ignored)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	cl := got[0]
	var names []string
	for _, v := range cl.Videos {
		names = append(names, v.Path)
	}
	// Largest first; the bridge pair joins d into the group of a and b.
	if !reflect.DeepEqual(names, []string{"a.mkv", "b.mkv", "d.mkv"}) {
		t.Fatalf("videos %v", names)
	}
	if cl.TotalSize != 450 || cl.Savings != 150 {
		t.Fatalf("total %d savings %d", cl.TotalSize, cl.Savings)
	}
	again := buildConfirmedClusters([]cluster{{videos: []*videoInfo{a, b, c}}}, [][2]*videoInfo{{b, d}}, ignored)
	if again[0].ID != cl.ID {
		t.Fatal("cluster ID must not change between scans (decisions are stored by ID)")
	}

	// Ignored files drop out; a group with one file left is no group.
	ignored.add("b.mkv")
	ignored.add("d.mkv")
	if got := buildConfirmedClusters([]cluster{{videos: []*videoInfo{a, b, c}}}, [][2]*videoInfo{{b, d}}, ignored); len(got) != 0 {
		t.Fatalf("got %d clusters, want 0", len(got))
	}
}

func TestExactDupeReps(t *testing.T) {
	dir := t.TempDir()
	same := []byte("identical content")
	for _, n := range []string{"a.mkv", "b.mkv"} {
		writeFile(t, filepath.Join(dir, n), same)
	}
	writeFile(t, filepath.Join(dir, "c.mkv"), []byte("different content")) // same size, other bytes
	a := &videoInfo{Path: filepath.Join(dir, "a.mkv"), Size: int64(len(same))}
	b := &videoInfo{Path: filepath.Join(dir, "b.mkv"), Size: int64(len(same))}
	c := &videoInfo{Path: filepath.Join(dir, "c.mkv"), Size: int64(len(same))}

	copyOf := exactDupeReps([]*videoInfo{a, b, c})
	if len(copyOf) != 1 || copyOf[b] != a {
		t.Fatalf("got %v", copyOf)
	}
	if contentFingerprint(filepath.Join(dir, "missing.mkv"), 1) != "" {
		t.Fatal("a missing file must give an empty fingerprint")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := loadCache(path) // missing file: empty cache
	c.set(videoInfo{Path: "x.mkv", Size: 42, ModTime: mtime, Duration: time.Minute, Hashes: hashes(1)})
	if err := c.save(path); err != nil {
		t.Fatal(err)
	}
	got, ok := loadCache(path).get("x.mkv", 42, mtime)
	if !ok || got.Duration != time.Minute || len(got.Hashes) != sampleCount {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if _, ok := loadCache(path).get("x.mkv", 43, mtime); ok {
		t.Fatal("a changed size must miss the cache")
	}
}

func TestCachePruneOnlyUnderScannedRoots(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	kept := filepath.Join(root, "kept.mkv")
	writeFile(t, kept, []byte("x"))
	c := &cache{entries: map[string]videoInfo{
		"1": {Path: kept},
		"2": {Path: filepath.Join(root, "gone.mkv")},
		"3": {Path: filepath.Join(dir, "offline-disk", "gone.mkv")}, // not scanned
	}}
	if n := c.pruneMissing([]string{root}); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, ok := c.entries["3"]; !ok {
		t.Fatal("entries outside the scanned roots must stay")
	}
}

func TestIgnoreListRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignored.json")
	il := loadIgnoreList(path)
	il.add(`D:\b.mkv`)
	il.add(`D:\a.mkv`)
	if err := il.save(path); err != nil {
		t.Fatal(err)
	}
	got := loadIgnoreList(path)
	if got.size() != 2 || !got.has(`D:\a.mkv`) {
		t.Fatalf("got %v", got.paths)
	}
	got.remove(`D:\a.mkv`)
	if got.has(`D:\a.mkv`) {
		t.Fatal("remove did not remove")
	}
}

func TestSplitSeek(t *testing.T) {
	if pre, post := splitSeek(60); pre != 58 || post != 2 {
		t.Fatalf("60 s: %v %v", pre, post)
	}
	if pre, post := splitSeek(3); pre != 0 || post != 3 {
		t.Fatalf("3 s: %v %v", pre, post)
	}
}
