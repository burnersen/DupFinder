// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func guardedRequest(g *webGuard, h http.Handler, method, target, host string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	g.wrap(h).ServeHTTP(rec, req)
	return rec
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

func TestGuardKeyIsRandomAndLongEnough(t *testing.T) {
	a, _ := newWebGuard(8765)
	b, _ := newWebGuard(8765)
	if len(a.key) != 2*keyBytes || a.key == b.key {
		t.Fatalf("keys %q and %q", a.key, b.key)
	}
}

func TestGuardListensOnLoopbackOnly(t *testing.T) {
	// README: "The UI is served on this computer only."
	if got := listenAddr(8765); got != "127.0.0.1:8765" {
		t.Fatalf("listen address %q", got)
	}
	g, _ := newWebGuard(8765)
	u, err := url.Parse(g.url())
	if err != nil || u.Host != "127.0.0.1:8765" || u.Query().Get("t") != g.key {
		t.Fatalf("url %q", g.url())
	}
}

func TestGuardRefusesForeignHostEvenWithKey(t *testing.T) {
	g, _ := newWebGuard(8765)
	good := &http.Cookie{Name: g.cookie, Value: g.key}
	for _, host := range []string{"evil.example.org:8765", "127.0.0.1:9999", "localhost", "[::1]:8765", ""} {
		if rec := guardedRequest(g, okHandler, http.MethodGet, "/api/state", host, good); rec.Code != http.StatusForbidden {
			t.Errorf("host %q: %d, want 403", host, rec.Code)
		}
	}
}

func TestGuardRefusesWithoutKey(t *testing.T) {
	g, _ := newWebGuard(8765)
	for name, c := range map[string]*http.Cookie{
		"no cookie":    nil,
		"wrong key":    {Name: g.cookie, Value: strings.Repeat("0", 2*keyBytes)},
		"other port":   {Name: "dupfinder_8766", Value: g.key},
		"empty cookie": {Name: g.cookie, Value: ""},
	} {
		if rec := guardedRequest(g, okHandler, http.MethodPost, "/api/decide", "127.0.0.1:8765", c); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
	if rec := guardedRequest(g, okHandler, http.MethodGet, "/?t=wrong", "127.0.0.1:8765", nil); rec.Code != http.StatusForbidden {
		t.Errorf("wrong key in the address: %d, want 403", rec.Code)
	}
}

func TestGuardKeyInAddressBecomesCookie(t *testing.T) {
	g, _ := newWebGuard(8765)
	rec := guardedRequest(g, okHandler, http.MethodGet, "/?t="+g.key, "127.0.0.1:8765", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("first visit: %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != g.key || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookies %+v", cookies)
	}
	for _, host := range []string{"127.0.0.1:8765", "localhost:8765"} {
		if rec := guardedRequest(g, okHandler, http.MethodPost, "/api/decide", host, cookies[0]); rec.Code != http.StatusTeapot {
			t.Errorf("with cookie via %s: %d, want the handler", host, rec.Code)
		}
	}
}

func TestEveryRouteIsGuarded(t *testing.T) {
	s, _ := testServer(t)
	g, _ := newWebGuard(8765)
	s.guard = g
	for _, p := range []string{"/", "/app.js", "/api/state", "/api/progress", "/api/events", "/api/decide", "/api/undo",
		"/api/add-root", "/api/rescan", "/api/thumb?h=ab&i=1", "/api/shutdown", "/api/play", "/api/reveal"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		req.Host = "127.0.0.1:8765"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without key: %d, want 403", p, rec.Code)
		}
	}
}
