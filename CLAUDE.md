# Anleitung für KI in diesem Projekt

Gilt für jede KI, die hier arbeitet (`AGENTS.md` zeigt hierher). Diese Datei ist öffentlich: nur Dinge hineinschreiben, die jeder lesen darf. Private Notizen gehören nach `_notizen/` (wird nicht eingecheckt).

## Grundlage

- Dieses Projekt folgt dem **dev-starterkit** (`burnersen/dev-starterkit`, privat). **Kit-Stand: 1.0** (abgeglichen am 07.11.2026 nach `10-bestehendes-projekt.md`, Vorlage `go-cli` mit lokaler Weboberfläche). Vor größerer Arbeit das Kit holen und die passenden Leitlinien lesen (`03` bis `09`). Ist das Kit neuer als der Stand oben, kurz sagen, was sich laut `AENDERUNGEN-*.md` geändert hat.
- Antworte auf Deutsch, locker, ohne Gedankenstriche. Erst sagen, wie du den Auftrag verstanden hast, bei allem Größeren auf Okay warten.
- Befehle, die der Nutzer selbst ausführt: als ganzer Block zum Kopieren, mit Sicherung vorher und dem Befehl zum Zurückholen (`01-arbeitsweise.md`).

## Steckbrief

| | |
|---|---|
| Zweck | Findet doppelte Videos am Bildinhalt (pHash über Stichproben-Bilder), Entscheidung in einer lokalen Webseite, Löschen nur in den Papierkorb |
| Sichtbarkeit | öffentlich |
| Sprache Kommentare, Commits, Meldungen, Oberfläche | Englisch |
| Plattformen | Windows x64 (Logik plattformneutral, Tests laufen unter Linux) |
| Lizenz | GPL-3.0-only |
| Fremdes zur Laufzeit | FFmpeg, festgenagelt in `runtime.json` (Version, Größe, SHA-256, Lizenz, Quellen) |

## Befehle

```
gofmt -l .                      # muss leer bleiben
go vet ./...
GOOS=windows go vet ./...
go test -race -count=1 ./...
go build -o DupFinder.exe .
```

Live-Test mit echtem FFmpeg: `DUPFINDER_TEST_FFMPEG=<Ordner mit ffmpeg.exe und ffprobe.exe> go test -run Live ./...` (läuft im Workflow "Mirror FFmpeg" für jeden neuen Build).

Release: `.github/release-notes/1.2.3.md` schreiben, dann Tag `v1.2.3` pushen (`07-build-und-release.md`). Die Version steht nur im Tag, im Code steht `dev`.

Neues FFmpeg: `runtime.json` ändern (Datei, Größe, SHA-256, URLs, Quellen), dann den Workflow "Mirror FFmpeg" von Hand starten. Er prüft Größe und Prüfsumme, lässt die Live-Tests laufen und legt die Datei in den Release `runtime-v1` (Vorabversion, nie "latest").

## Aufbau

Siehe Kopfkommentar in `main.go`. `os.Exit` steht nur in `main()`, Fehler beim Start zeigt nur `finish()`, die Weboberfläche läuft nur über `webguard.go` (Schlüssel und Host-Prüfung), Zustandsdateien nur über `state.go` (atomar, kaputte Dateien werden beiseitegelegt).

## Bewusste Abweichungen vom Kit

| Kit | DupFinder | Grund |
|---|---|---|
| Exit-Codes 1 Fehler, 2 falscher Aufruf, 3 Abbruch (F6, F12) | 1 kein Ordner, 2 Start gescheitert, 0 auch bei Strg+C | so seit 1.0 (U2); Strg+C und "Quit" sind das normale Ende |
| Daten unter `%LOCALAPPDATA%` (K8, L4) | `bin\` und `.DupFinder.data\` neben der exe, `DUPFINDER_HOME` verschiebt beides | so seit 1.0 (U2), DupFinder ist portabel |
| Einstellungen als INI (K1, K2) | nur drei Umgebungsvariablen `DUPFINDER_*` (README) | Feineinstellungen für große Sammlungen, keine Datei nötig |
| Format-Version in Zustandsdateien (K10) | keine | ältere Versionen sollen die Dateien weiter lesen; kaputte oder fremde Dateien werden nie überschrieben, sondern beiseitegelegt |
| Release-Ziele Windows und Linux (B4) | nur `DupFinder.exe` (x64) | Papierkorb und Explorer gibt es nur unter Windows |

## Offen

- `scan.go` hat über 1000 Zeilen (P3): beim nächsten inhaltlichen Anfassen den Hash- und den Vorschau-Teil in eigene Dateien ziehen (U5).
- Prüfung der Seite ohne Browser (`web/app_check.js`, T6) fehlt noch.
- Nach dem Zusammenführen einmal "Mirror FFmpeg" starten, damit der Spiegel `runtime-v1` gefüllt ist.

## Vor jedem Commit

Die Checklisten aus `03` bis `09` des Kits durchgehen, mindestens: gofmt leer, vet und Tests grün, kein neues `_ =` ohne `// ignore: <Grund>`, neue Endpunkte laufen durch `webguard.go`, keine Geheimnisse und privaten Pfade, README und Release-Notes nachgezogen, wenn sich für Nutzer etwas ändert. Danach `bash <kit>/werkzeuge/projekt-pruefen.sh .`

## Das Kit lernt mit

Ergibt sich etwas, das auch anderen Projekten hilft (Fehler, der wiederkommen kann, gute Lösung, Bibliothek mit gutem oder schlechtem Ergebnis, Build-Trick, bessere Regel), am Ende des Arbeitsschritts in einem Satz fragen: "Das wäre was fürs Kit: <worum es geht>. Übernehmen?" Ablauf in `21-kit-lernt-mit.md` des Kits.
