// DupFinder — Required Notice: Copyright (c) 2026 burnersen — DupFinder
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0

package main

// Automatischer FFmpeg-Download beim ersten Start.
//
// FFmpeg/FFprobe werden aus Lizenzgründen NICHT mitgeliefert, sondern bei Bedarf
// vom offiziellen BtbN-Build-Repository geladen. Bewusst nur Standard-Bibliothek
// (keine Fremd-Abhängigkeiten), damit DupFinder abhängigkeitsfrei bleibt.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BtbN hat den festen "latest"-Release-Tag entfernt (der alte feste Download-Link
// liefert jetzt 404). Daher wird der neueste Autobuild zur Laufzeit über die
// GitHub-API ermittelt.
const ffmpegReleasesAPI = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases?per_page=5"

const bytesPerMiB = 1 << 20

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// newDownloadClient liefert einen HTTP-Client mit großzügigen, aber endlichen
// Zeitlimits: ein hängender Verbindungsaufbau blockiert so nicht ewig, langsame
// (aber fortschreitende) Downloads bleiben jedoch möglich.
func newDownloadClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// resolveFFmpegDownloadURL liefert die Download-URL des neuesten statischen
// win64-GPL-Builds (genau die Variante, auf die früher der "latest"-Tag zeigte).
// Fallback: irgendein nicht-shared win64-GPL-ZIP aus den neuesten Releases.
func resolveFFmpegDownloadURL(client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(appCtx, http.MethodGet, ffmpegReleasesAPI, nil)
	if err != nil {
		return "", err
	}
	// Die GitHub-API weist Anfragen ohne User-Agent ab.
	req.Header.Set("User-Agent", "DupFinder")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub-API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub-API lieferte HTTP %d", resp.StatusCode)
	}

	var releases []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&releases); err != nil {
		return "", fmt.Errorf("GitHub-API (JSON lesen): %w", err)
	}

	var fallback string
	for _, rel := range releases {
		for _, a := range rel.Assets {
			n := strings.ToLower(a.Name)
			if !strings.HasSuffix(n, ".zip") || !strings.Contains(n, "win64-gpl") ||
				strings.Contains(n, "shared") {
				continue
			}
			// Master-Build, z. B. "ffmpeg-N-124941-g54749da98a-win64-gpl.zip".
			if strings.HasPrefix(n, "ffmpeg-n-") && strings.HasSuffix(n, "-win64-gpl.zip") {
				return a.BrowserDownloadURL, nil
			}
			if fallback == "" {
				fallback = a.BrowserDownloadURL
			}
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("keine passende win64-gpl-ZIP in den neuesten Releases gefunden")
}

// downloadFFmpeg lädt das aktuelle win64-GPL-ZIP herunter und entpackt daraus nur
// ffmpeg.exe und ffprobe.exe nach targetDir. Das ZIP wird in eine Temp-Datei
// gestreamt (konstanter Speicherbedarf, unabhängig von der Archivgröße).
func downloadFFmpeg(targetDir string) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("Zielordner anlegen: %w", err)
	}

	client := newDownloadClient()
	url, err := resolveFFmpegDownloadURL(client)
	if err != nil {
		return err
	}

	// ── Schritt 1: ZIP in eine Temp-Datei streamen ───────────────────────────
	tmp, err := os.CreateTemp("", "ffmpeg-download-*.zip")
	if err != nil {
		return fmt.Errorf("Temp-Datei anlegen: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // immer aufräumen, auch im Erfolgsfall

	req, err := http.NewRequestWithContext(appCtx, http.MethodGet, url, nil)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	req.Header.Set("User-Agent", "DupFinder")

	resp, err := client.Do(req)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("Download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = tmp.Close()
		return fmt.Errorf("Server lieferte HTTP %d", resp.StatusCode)
	}

	pr := &progressReader{reader: resp.Body, total: resp.ContentLength}
	written, copyErr := io.Copy(tmp, pr)
	fmt.Println() // Zeilenumbruch nach der \r-Fortschrittszeile
	if copyErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("Download speichern: %w", copyErr)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("Temp-Datei schließen: %w", err)
	}

	fmt.Printf("Download fertig (%.1f MB). Entpacke ffmpeg.exe + ffprobe.exe…\n",
		float64(written)/bytesPerMiB)

	// ── Schritt 2: ZIP öffnen und nur die zwei Programme entpacken ────────────
	return extractFFmpegBinaries(tmpPath, targetDir)
}

// extractFFmpegBinaries entpackt aus dem ZIP genau ffmpeg.exe und ffprobe.exe.
// Das Archiv hat einen Wurzelordner, z. B. "ffmpeg-…-win64-gpl/bin/ffmpeg.exe";
// gematcht wird über den Pfad NACH dem ersten Verzeichnis.
func extractFFmpegBinaries(zipPath, targetDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("ZIP öffnen: %w", err)
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
		dest, ok := targets[parts[1]] // parts[1] z. B. "bin/ffmpeg.exe"
		if !ok {
			continue
		}
		if err := extractZipEntry(f, dest); err != nil {
			return err
		}
		extracted++
	}

	if extracted < 2 {
		return fmt.Errorf("nur %d von 2 Programmen (ffmpeg/ffprobe) im Archiv gefunden", extracted)
	}
	return nil
}

// extractZipEntry schreibt einen ZIP-Eintrag atomar nach dest: erst in eine
// .tmp-Datei, dann umbenennen. So bleibt bei einem Abbruch nie ein halb
// geschriebenes Programm liegen.
func extractZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("Archiv-Eintrag %q öffnen: %w", f.Name, err)
	}
	defer rc.Close()

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return fmt.Errorf("%q anlegen: %w", filepath.Base(dest), err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("%q entpacken: %w", filepath.Base(dest), err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%q schließen: %w", filepath.Base(dest), err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%q ablegen: %w", filepath.Base(dest), err)
	}
	return nil
}

// progressReader zählt die gelesenen Bytes und zeigt höchstens alle 200 ms eine
// Fortschrittszeile in der Konsole (mit \r überschrieben), damit ein langer
// Download nicht wie ein Hänger aussieht.
type progressReader struct {
	reader    io.Reader
	total     int64 // -1, falls der Server keine Größe meldet
	read      int64
	lastPrint time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.read += int64(n)
	if time.Since(p.lastPrint) >= 200*time.Millisecond {
		p.print()
		p.lastPrint = time.Now()
	}
	return n, err
}

func (p *progressReader) print() {
	readMiB := float64(p.read) / bytesPerMiB
	if p.total > 0 {
		pct := float64(p.read) / float64(p.total) * 100
		fmt.Printf("\rLade FFmpeg… %5.1f %%  (%.1f / %.1f MB)",
			pct, readMiB, float64(p.total)/bytesPerMiB)
		return
	}
	fmt.Printf("\rLade FFmpeg… %.1f MB", readMiB)
}
