// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testServer has one undecided group of two videos that exist on disk.
func testServer(t *testing.T) (*server, []string) {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	var videos []*videoInfo
	for _, n := range []string{"big.mkv", "small.mkv"} {
		p := filepath.Join(dir, n)
		writeFile(t, p, []byte("video"))
		paths = append(paths, p)
		videos = append(videos, &videoInfo{Path: p, Size: 5})
	}
	state := &appState{
		dataDir:  dir,
		ignored:  &ignoreList{paths: map[string]bool{}},
		clusters: []*clusterState{{ID: "c1", Videos: videos}},
	}
	return newServer(state, filepath.Join(dir, "cache.json"), filepath.Join(dir, "ignored.json")), paths
}

func do(t *testing.T, s *server, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestChangingRequestsNeedPost(t *testing.T) {
	s, _ := testServer(t)
	for _, p := range []string{"/api/decide", "/api/undo", "/api/add-root", "/api/rescan", "/api/shutdown", "/api/play", "/api/reveal"} {
		if rec := do(t, s, http.MethodGet, p, "", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d, want 405", p, rec.Code)
		}
	}
}

// A form on a foreign page can only send "simple" content types without a
// CORS preflight, so requiring JSON keeps other sites from posting here.
func TestChangingRequestsNeedJSON(t *testing.T) {
	s, _ := testServer(t)
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		if rec := do(t, s, http.MethodPost, "/api/decide", ct, `{}`); rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q: %d, want 415", ct, rec.Code)
		}
	}
	if rec := do(t, s, http.MethodPost, "/api/undo", "application/json; charset=utf-8", `{"clusterId":"none"}`); rec.Code != http.StatusNotFound {
		t.Errorf("JSON with charset: %d, want 404 (accepted, unknown group)", rec.Code)
	}
}

func TestPlayAndRevealOnlyKnownVideos(t *testing.T) {
	s, _ := testServer(t)
	other := filepath.Join(t.TempDir(), "other.exe")
	writeFile(t, other, []byte("x"))
	for _, p := range []string{"/api/play", "/api/reveal"} {
		body, _ := json.Marshal(map[string]string{"path": other})
		if rec := do(t, s, http.MethodPost, p, "application/json", string(body)); rec.Code != http.StatusForbidden {
			t.Errorf("%s with an unknown file: %d, want 403", p, rec.Code)
		}
	}
}

func TestThumbRejectsOddNames(t *testing.T) {
	s, _ := testServer(t)
	for _, q := range []string{"h=../../x&i=1", "h=abc&i=1/../2", "h=ABC&i=1", "h=&i=1"} {
		if rec := do(t, s, http.MethodGet, "/api/thumb?"+q, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", q, rec.Code)
		}
	}
}

func TestIgnoreDecisionIsStoredAndUndone(t *testing.T) {
	s, paths := testServer(t)
	body, _ := json.Marshal(map[string]any{"clusterId": "c1", "decisions": []decision{
		{Path: paths[0], Action: "keep"}, {Path: paths[1], Action: "ignore"},
	}})
	if rec := do(t, s, http.MethodPost, "/api/decide", "application/json", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	if !loadIgnoreList(s.ignoreFile).has(paths[1]) {
		t.Fatal("ignored file is not in ignored.json")
	}
	if sess := loadSession(s.state.dataDir); !sess.Clusters["c1"].Decided {
		t.Fatal("decision is not in session.json")
	}
	if rec := do(t, s, http.MethodPost, "/api/decide", "application/json", string(body)); rec.Code != http.StatusConflict {
		t.Fatalf("second decide: %d, want 409", rec.Code)
	}

	if rec := do(t, s, http.MethodPost, "/api/undo", "application/json", `{"clusterId":"c1"}`); rec.Code != http.StatusOK {
		t.Fatalf("undo: %d", rec.Code)
	}
	if loadIgnoreList(s.ignoreFile).has(paths[1]) {
		t.Fatal("undo left the file in ignored.json")
	}
}

func TestStateListsGroups(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, http.MethodGet, "/api/state", "", "")
	var st stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Clusters) != 1 || len(st.Clusters[0].Videos) != 2 || st.PendingCount != 1 {
		t.Fatalf("got %+v", st)
	}
}

func TestPageIsServed(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") {
		t.Fatalf("index: %d", rec.Code)
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if rec := do(t, s, http.MethodGet, f, "", ""); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", f, rec.Code)
		}
	}
}

func TestAddRootNeedsAnExistingFolder(t *testing.T) {
	s, paths := testServer(t)
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing"), paths[0]} {
		body, _ := json.Marshal(map[string]string{"path": p})
		if rec := do(t, s, http.MethodPost, "/api/add-root", "application/json", string(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", p, rec.Code)
		}
	}
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]string{"path": dir})
	if rec := do(t, s, http.MethodPost, "/api/add-root", "application/json", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("existing folder: %d", rec.Code)
	}
	if len(s.state.roots) != 1 || s.state.roots[0] != dir {
		t.Fatalf("roots %v", s.state.roots)
	}
}

func TestMain(m *testing.M) {
	// Nothing in the tests may start a browser or ffmpeg; this makes sure a
	// stray call fails instead of opening windows on a developer machine.
	os.Setenv("PATH", "")
	os.Exit(m.Run())
}
