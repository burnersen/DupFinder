// DupFinder (https://github.com/burnersen/DupFinder)
// Copyright (C) 2026 burnersen
// SPDX-License-Identifier: GPL-3.0-only

package main

// FFmpeg download on first start (dev-starterkit 08-sicherheit.md S4).
//
// FFmpeg is not shipped with DupFinder. On first start exactly one build is
// downloaded: the one pinned in runtime.json by name, size and SHA-256. The
// sources are tried in order, first DupFinder's own mirror (release
// runtime-v1, filled by the workflow "Mirror FFmpeg"), then the BtbN release
// it was copied from. Which source answers does not matter: only a file with
// exactly this size and checksum is unpacked, anything else is thrown away.
//
// A newer FFmpeg: change runtime.json, run "Mirror FFmpeg" (it checks the
// build with DupFinder's own tests before publishing it), then release.

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed runtime.json
var runtimeJSON []byte

// runtimeEntry is one pinned download as runtime.json describes it.
type runtimeEntry struct {
	Name    string   `json:"name"`
	File    string   `json:"file"`
	Size    int64    `json:"size"`
	SHA256  string   `json:"sha256"`
	License string   `json:"license"`
	Build   string   `json:"build"`
	Source  string   `json:"source"`
	URLs    []string `json:"urls"`
}

type runtimeFiles struct {
	Format int          `json:"format"`
	FFmpeg runtimeEntry `json:"ffmpeg"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// loadRuntime reads the embedded runtime.json. A broken file is a build
// mistake, so the tests check it too.
func loadRuntime() (runtimeFiles, error) {
	var rt runtimeFiles
	if err := json.Unmarshal(runtimeJSON, &rt); err != nil {
		return rt, fmt.Errorf("read runtime.json: %w", err)
	}
	f := rt.FFmpeg
	switch {
	case rt.Format != 1:
		return rt, fmt.Errorf("runtime.json: format %d, expected 1", rt.Format)
	case f.Size <= 0 || !sha256Hex.MatchString(f.SHA256) || len(f.URLs) == 0:
		return rt, errors.New("runtime.json: ffmpeg needs size, sha256 (lower case hex) and urls")
	}
	for _, u := range f.URLs {
		if !strings.HasPrefix(u, "https://") || !strings.HasSuffix(u, "/"+f.File) {
			return rt, fmt.Errorf("runtime.json: url %s must be https and end with %s", u, f.File)
		}
	}
	return rt, nil
}

// pinnedFile is one download that is accepted only if size and SHA-256 match.
type pinnedFile struct {
	Name   string // shown to the user, e.g. "FFmpeg n9.0.2"
	URL    string
	Size   int64
	SHA256 string // lower case hex
}

// downloadFFmpeg fetches the pinned build into targetDir and unpacks
// ffmpeg.exe and ffprobe.exe from it. The ZIP itself is removed afterwards.
func downloadFFmpeg(ctx context.Context, targetDir string) error {
	rt, err := loadRuntime()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fileError(err, "create the folder", targetDir)
	}
	zipPath := filepath.Join(targetDir, rt.FFmpeg.File)
	defer os.Remove(zipPath) // ignore: only the two programs are kept
	show := progressPrinter(rt.FFmpeg.Name)
	if err := fetchPinned(ctx, newHTTPClient(), rt.FFmpeg, zipPath, show); err != nil {
		return err
	}
	fmt.Printf("Download complete (%.1f MB). Unpacking ffmpeg.exe and ffprobe.exe...\n", float64(rt.FFmpeg.Size)/bytesPerMiB)
	return extractFFmpegBinaries(zipPath, targetDir)
}

// fetchPinned tries the sources of e in order until one delivers the pinned
// file. The error of the last source is returned, the others are logged.
func fetchPinned(ctx context.Context, client *http.Client, e runtimeEntry, dest string, show func(done, total int64)) error {
	var err error
	for _, u := range e.URLs {
		f := pinnedFile{Name: e.Name, URL: u, Size: e.Size, SHA256: e.SHA256}
		if err = downloadVerified(ctx, client, f, dest, show); err == nil {
			slog.Info("downloaded", "url", u) // no query part, nothing secret
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("download failed, trying the next source", "url", u, "err", err)
	}
	return err
}

// newHTTPClient never hangs forever: every phase has a limit. The body has no
// overall limit (large files on slow lines); cancel the context to stop.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// downloadVerified fetches f into dest. It writes to dest.part first and
// renames only after size and checksum matched: dest is either the complete,
// checked file or untouched. show, if not nil, is told about the progress.
func downloadVerified(ctx context.Context, client *http.Client, f pinnedFile, dest string, show func(done, total int64)) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Name, err)
	}
	req.Header.Set("User-Agent", "DupFinder")
	resp, err := client.Do(req)
	if err != nil {
		return netError(err, f.Name)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return userError(fmt.Errorf("HTTP %s from %s", resp.Status, hostOf(f.URL)),
			"Cannot download "+f.Name, "The server refused it right now. Try again later.")
	}

	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return fileError(err, "create", part)
	}
	defer func() {
		out.Close() // ignore: closed below on success; this only cleans up
		if err != nil {
			os.Remove(part) // ignore: a broken download never stays behind
		}
	}()

	var body io.Reader = resp.Body
	if show != nil {
		body = &progressReader{reader: resp.Body, total: f.Size, show: show}
	}
	h := sha256.New()
	// Read one byte more than expected, so a larger file is noticed.
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(body, f.Size+1))
	if show != nil {
		show(n, -1) // finished: ends the progress line
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return netError(err, f.Name)
	}
	if n != f.Size {
		return userError(fmt.Errorf("got %d bytes, expected %d", n, f.Size),
			"The download of "+f.Name+" is incomplete or not the expected file",
			"Nothing was installed. Try again; if it keeps failing, please report it.")
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		return userError(fmt.Errorf("SHA-256 %s, expected %s", got, f.SHA256),
			"The download of "+f.Name+" does not match its checksum",
			"Nothing was installed. The file was damaged on the way or changed on the server. Please report it.")
	}
	if err = out.Sync(); err != nil {
		return fileError(err, "write", part)
	}
	if err = out.Close(); err != nil {
		return fileError(err, "write", part)
	}
	if err = os.Rename(part, dest); err != nil {
		return fileError(err, "save", dest)
	}
	return nil
}

// netError explains the usual network problems. The technical text stays
// in the error for the log.
func netError(err error, what string) error {
	var dnsErr *net.DNSError
	var certErr *x509.UnknownAuthorityError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &dnsErr):
		return userError(err, "Cannot download "+what+": the server name was not found",
			"Check the internet connection.")
	case errors.As(err, &certErr):
		return userError(err, "Cannot download "+what+": the connection is not trusted",
			"A virus scanner or company proxy may be intercepting it. Try another network.")
	case errors.As(err, &netErr) && netErr.Timeout():
		return userError(err, "Cannot download "+what+": the server does not answer",
			"Check the internet connection and try again.")
	default:
		return userError(err, "Cannot download "+what,
			"Check the internet connection and try again.")
	}
}

func hostOf(url string) string {
	rest := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// extractFFmpegBinaries unpacks exactly ffmpeg.exe and ffprobe.exe. The
// archive has one root folder, e.g. "ffmpeg-n9.0.2-...-win64-gpl-9.0/bin/
// ffmpeg.exe"; the match is on the path after that folder.
func extractFFmpegBinaries(zipPath, targetDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fileError(err, "open the archive", zipPath)
	}
	defer zr.Close()

	targets := map[string]string{
		"bin/ffmpeg.exe":  filepath.Join(targetDir, "ffmpeg.exe"),
		"bin/ffprobe.exe": filepath.Join(targetDir, "ffprobe.exe"),
	}

	extracted := 0
	for _, f := range zr.File {
		parts := strings.SplitN(filepath.ToSlash(f.Name), "/", 2)
		if len(parts) != 2 {
			continue
		}
		dest, ok := targets[parts[1]]
		if !ok {
			continue
		}
		if err := extractZipEntry(f, dest); err != nil {
			return err
		}
		extracted++
	}

	if extracted < 2 {
		return fmt.Errorf("only %d of 2 programs (ffmpeg, ffprobe) found in the archive", extracted)
	}
	return nil
}

// extractZipEntry writes one archive entry to dest: into dest.part first,
// then renamed, so an interrupted start never leaves half a program behind.
func extractZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s in the archive: %w", f.Name, err)
	}
	defer rc.Close()

	part := dest + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fileError(err, "create", part)
	}
	_, err = io.Copy(out, rc)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, dest)
	}
	if err != nil {
		os.Remove(part) // ignore: cleanup after the error that is returned
		return fileError(err, "unpack", dest)
	}
	return nil
}

const bytesPerMiB = 1 << 20

// progressReader counts the bytes read and reports them at most every 200 ms,
// so a long download does not look like a hang.
type progressReader struct {
	reader    io.Reader
	total     int64
	read      int64
	lastPrint time.Time
	show      func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.read += int64(n)
	if time.Since(p.lastPrint) >= 200*time.Millisecond {
		p.show(p.read, p.total)
		p.lastPrint = time.Now()
	}
	return n, err
}

// progressPrinter shows the download on one console line (rewritten with \r).
// total -1 means finished.
func progressPrinter(name string) func(done, total int64) {
	return func(done, total int64) {
		if total < 0 {
			fmt.Println()
			return
		}
		fmt.Printf("\r[...] Downloading %s: %5.1f %%  (%.1f / %.1f MB)", name,
			float64(done)/float64(total)*100, float64(done)/bytesPerMiB, float64(total)/bytesPerMiB)
	}
}
