// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// The local web server answers only the browser tab DupFinder opened
// itself (dev-starterkit 08-sicherheit.md S6). Two checks on every request:
//
//   - Host header: 127.0.0.1:<port> or localhost:<port>. A foreign web page
//     can make its own name point at 127.0.0.1 (DNS rebinding); its requests
//     then still carry the foreign name and are refused.
//   - Key: a random value from crypto/rand, new on every start. The browser
//     gets it once in the address DupFinder opens (?t=...), DupFinder turns
//     it into a cookie and sends the browser on to the bare address, so the
//     key does not stay in the address bar. Page, API, events and thumbnails
//     then carry the cookie by themselves.
//
// Changing requests additionally need POST with Content-Type
// application/json (requireJSON in server.go).

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
)

// keyBytes: 32 random bytes, as the dev-starterkit asks for (S6).
const keyBytes = 32

type webGuard struct {
	port   int
	key    string
	cookie string
}

func newWebGuard(port int) (*webGuard, error) {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("make the key for the web page: %w", err)
	}
	return &webGuard{
		port: port,
		key:  hex.EncodeToString(b),
		// The port is part of the name: cookies do not tell ports apart,
		// and two running copies must not overwrite each other's key.
		cookie: fmt.Sprintf("dupfinder_%d", port),
	}, nil
}

// listenAddr is where the server listens: the loopback address only, never
// all networks.
func listenAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// url is the address to open in the browser. It carries the key, so it is
// shown in the console but never written to the log.
func (g *webGuard) url() string {
	return fmt.Sprintf("http://%s/?t=%s", listenAddr(g.port), g.key)
}

func (g *webGuard) hostOK(host string) bool {
	return host == listenAddr(g.port) || host == fmt.Sprintf("localhost:%d", g.port)
}

func (g *webGuard) keyOK(k string) bool {
	return subtle.ConstantTimeCompare([]byte(k), []byte(g.key)) == 1
}

func (g *webGuard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostOK(r.Host) {
			http.Error(w, "Forbidden: unknown host name.", http.StatusForbidden)
			return
		}
		if k := r.URL.Query().Get("t"); k != "" && g.keyOK(k) {
			http.SetCookie(w, &http.Cookie{
				Name:     g.cookie,
				Value:    g.key,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(g.cookie); err == nil && g.keyOK(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Forbidden: open DupFinder with the link it shows in its window.", http.StatusForbidden)
	})
}
