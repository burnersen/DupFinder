<div align="center">

# 🎞️ DupFinder

### Find duplicate videos by what they look like, not just by name or size.

**A content-aware duplicate video finder with a local web UI, for Windows. One EXE, nothing to install.**

*Powered by FFmpeg, which does the frame decoding. DupFinder is the detection, comparison and review layer around it.*

[![Windows x64](https://img.shields.io/badge/Windows-10%2F11%20x64-0078D6?logo=windows)](#requirements)
[![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?logo=go)](#building-from-source)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue)](LICENSE)
[![Ko-fi](https://img.shields.io/badge/Ko--fi-Support%20me-FF5E5B?logo=kofi&logoColor=white)](https://ko-fi.com/burnersen)

**[⬇️ Download the latest release](https://github.com/burnersen/DupFinder/releases/latest)** · **[🌐 Website](https://tech.burnersen.de/en/tools/dupfinder-en/)** · **[🔏 Code signing policy](CODE_SIGNING.md)** · **[☕ Buy me a coffee](https://ko-fi.com/burnersen)**

<sub>🇩🇪 Auf Deutsch: [tech.burnersen.de/tools/dupfinder](https://tech.burnersen.de/tools/dupfinder/)</sub>

<!-- Optional: drop a screenshot at .github/screenshot.png and uncomment:
<img src=".github/screenshot.png" alt="DupFinder web UI showing a duplicate group" width="860">
-->

</div>

> 💜 **Using DupFinder and happy with it?** A ⭐ on GitHub is the easiest way to let me know,
> and it does more than you'd think: stars are how small projects like this one qualify for
> free code signing, which makes the Windows warning go away for everyone. Or
> [tell me what you use it for](https://github.com/burnersen/DupFinder/issues/new), I read every message.
> Thanks for being here!

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

Windows or your antivirus may warn you the first time you run `DupFinder.exe`. The honest reason: **the EXE is not code-signed yet** (signing certificates cost hundreds of euros per year, and this is a free hobby project). Free code signing through the [SignPath Foundation](https://signpath.org) is applied for and already prepared; see the [code signing policy](CODE_SIGNING.md). Until then, unsigned Go binaries are common false-positive targets.

Every release is built by GitHub Actions from this repository ([release workflow](.github/workflows/release.yml)), never on a private PC.

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

Releases are built by the [release workflow](.github/workflows/release.yml) on GitHub: push a tag `vX.Y.Z` equal to `appVersion` in `main.go`. It also writes name and version into the file properties (from [`winres/winres.json`](winres/winres.json)) and, once code signing is switched on, has the exe signed: see [`.signpath/README.md`](.signpath/README.md). "Run workflow" with "dry run" ticked builds and checks everything without publishing.

---

## 📜 License

DupFinder is free software under the [GNU General Public License, version 3](LICENSE) — that version only (`GPL-3.0-only`).

- **Use it:** Anyone may use DupFinder free of charge, at home or at work, and read, change and share its source code.
- **The one condition:** Whoever passes on DupFinder or parts of it — unchanged, changed or built into a program of their own — must include the complete source code and pass everything on under the same license. A closed, paid product with DupFinder inside is therefore not possible.
- **Free of charge:** DupFinder is and stays free. The official source is [github.com/burnersen/DupFinder](https://github.com/burnersen/DupFinder) — anyone asking money for DupFinder is selling you what you can get here for free.

Versions up to and including 1.0.1 were released under the PolyForm Noncommercial License 1.0.0; every later version is under the GPLv3.

### Code signing and privacy

What is signed, who approves it and what DupFinder sends over the network (only the FFmpeg download): [code signing policy](CODE_SIGNING.md). To uninstall, delete the folder; nothing is written to the registry.

### FFmpeg

DupFinder does **not** bundle FFmpeg. On first run it downloads an official static build from the [BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) project onto your machine, or you provide your own copy. FFmpeg is a separate work by the [FFmpeg project](https://ffmpeg.org) under its own license; DupFinder invokes it as an external program.

---

## 💬 Feedback & contributions

Found a bug or have a feature wish? Please open an issue on the [GitHub repository](https://github.com/burnersen/DupFinder/issues) — feedback is genuinely welcome. Forks and pull requests are too.

---

## ☕ Support

DupFinder is free and made in my spare time. If it saved you some disk space and you'd like to say thanks, you can [buy me a coffee on Ko-fi](https://ko-fi.com/burnersen). Completely optional — and thank you!

---

## ⚠️ Disclaimer

DupFinder is free hobby software, provided **"as is", without any warranty or condition of any kind**. It was built and tested with care — deleted files go to the recycle bin, not a permanent delete — but you use it **at your own risk**. As far as the applicable law allows, the author is not liable for any damages or data loss arising from the use of this software. See sections 15 and 16 of the [license](LICENSE).
