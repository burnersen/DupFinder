<div align="center">

# 🎞️ DupFinder

### Find duplicate videos by what they look like, not just by name or size.

**A content-aware duplicate video finder with a local web UI, for Windows. One EXE, nothing to install.**

*Powered by FFmpeg, which does the frame decoding. DupFinder is the detection, comparison and review layer around it.*

[![Windows x64](https://img.shields.io/badge/Windows-10%2F11%20x64-0078D6?logo=windows)](#requirements)
[![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?logo=go)](#building-from-source)
[![License](https://img.shields.io/badge/License-PolyForm%20Noncommercial-blue)](LICENSE.md)

**[⬇️ Download the latest release](https://github.com/burnersen/DupFinder/releases/latest)**

<!-- Optional: drop a screenshot at .github/screenshot.png and uncomment:
<img src=".github/screenshot.png" alt="DupFinder web UI showing a duplicate group" width="860">
-->

</div>

---

## ⚡ 30 seconds, no manual

1. Download `DupFinder.exe`, a single file with nothing to install.
2. Run it with one or more folders:
   ```
   DupFinder.exe "D:\Movies" "E:\Backup\Clips"
   ```
3. Your browser opens automatically. Review the duplicate groups and decide what to keep.

On first run DupFinder fetches FFmpeg automatically: no setup, no PATH fiddling, no dependencies.

---

## ✨ What DupFinder does

- 🧠 **Content-based, not name-based.** It samples a handful of frames per video and compares them with a perceptual hash (pHash). That catches duplicates even across **different containers, resolutions or re-encodes** — not just byte-identical copies (though those are detected too, without decoding).
- 🪟 **Local web UI.** Side-by-side comparison of each group: thumbnails, size, bitrate, resolution, codec and duration. The best value per column is highlighted, and the largest file is pre-selected as "keep".
- ⌨️ **Fast keyboard review.** Walk through groups and decide in seconds (see shortcuts below).
- 🛡️ **Safe with your files.** "Delete" moves a file to the **Windows recycle bin** (recoverable), never a hard delete. Decisions are remembered between runs and can be undone.
- 💾 **Cached.** Results are stored, so a second scan of the same files is near-instant — only new or changed files are re-examined.
- 🔒 **Stays on your machine.** The UI is served on `localhost` only; nothing is uploaded anywhere.

---

## 🚀 Usage

```
DupFinder.exe <folder> [more folders ...]
```

The web UI runs at `http://localhost:8765` (the next free port is used if that one is busy) and opens by itself.

### Keyboard shortcuts

| Key | Action |
|---|---|
| `1`…`9` | Toggle file N between **Keep** and **Delete** |
| `Shift`+`1…9` | Mark file N as **Ignore** |
| `Enter` | Confirm the focused group (carries out the actions) |
| `S` | Skip the group (keep everything) |
| `U` | Undo the last decision |
| `↑` / `↓` | Move between groups |
| `?` | Show / hide the help panel |

**Actions:** *Keep* leaves the file. *Delete* moves it to the recycle bin. *Ignore* excludes the file from future scans but leaves it where it is.

---

## 🎛️ Tuning (optional environment variables)

DupFinder runs fine with no configuration. For large libraries you can tweak:

| Variable | Default | Effect |
|---|---|---|
| `DUPFINDER_FAST_SEEK` | on | Jump straight to the nearest keyframe when sampling frames (fastest). Set to `0` for slower, frame-accurate seeking. |
| `DUPFINDER_HWACCEL` | off | Decode on the GPU (NVDEC). Often **slower** than CPU for this short-sample workload; off by default. |
| `DUPFINDER_FFMPEG_CONCURRENCY` | CPU cores | Number of parallel FFmpeg processes. Raise on fast SSD/NVMe, lower for a single mechanical disk. |

---

## 💻 Requirements

- Windows 10/11 x64
- FFmpeg: downloaded automatically on first run into a `bin\` folder next to the EXE (or drop your own `ffmpeg.exe` / `ffprobe.exe` there)
- An internet connection on first run (for that one-time FFmpeg download)

---

## 🛡️ Windows SmartScreen / antivirus warnings

Windows or your antivirus may warn you the first time you run `DupFinder.exe`. The honest reason: **the EXE is not code-signed** (signing certificates cost hundreds of euros per year, and this is a free hobby project). Unsigned Go binaries are common false-positive targets.

You don't have to trust it blindly:

- **Scan it:** upload the EXE to [VirusTotal](https://www.virustotal.com) before running.
- **Read it:** the complete source code is in this repository.
- **Build it yourself:** see below.

If SmartScreen blocks the start: click **"More info" → "Run anyway"**.

---

## 🔨 Building from source

```
go build -o DupFinder.exe .
```

Requires Go 1.22+. FFmpeg is **not** needed to build — it is downloaded at runtime, never embedded.

---

## 📜 License

**DupFinder is source-available under the [PolyForm Noncommercial License 1.0.0](LICENSE.md).**

Free to use, study, modify and share for any **noncommercial** purpose: personal use, hobby, education, research. **Commercial use, resale or bundling into paid products is not permitted** without a separate license from the author.

### FFmpeg

DupFinder does **not** bundle FFmpeg. On first run it downloads an official static build from the [BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) project onto your machine, or you provide your own copy. FFmpeg is a separate work by the [FFmpeg project](https://ffmpeg.org) under its own license; DupFinder invokes it as an external program.

---

## ⚠️ Disclaimer

DupFinder is free hobby software, provided **"as is", without any warranty or condition of any kind**. It was built and tested with care — deleted files go to the recycle bin, not a permanent delete — but you use it **at your own risk**. As far as the applicable law allows, the author is not liable for any damages or data loss arising from the use of this software. See the *No Liability* clause of the [license](LICENSE.md).
