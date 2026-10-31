# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io),
certificate by [SignPath Foundation](https://signpath.org).

> **Status:** DupFinder has applied to the SignPath Foundation. Until the
> signing is active, `DupFinder.exe` is not signed. Every release says in
> its text whether its exe is signed.

## What is signed

Only `DupFinder.exe`, built from the source code in this repository by the
[release workflow](.github/workflows/release.yml) on GitHub-hosted runners.
Nothing that was built elsewhere is signed with this certificate. Every
signing request is approved by hand, release by release.

FFmpeg is not part of DupFinder and is never signed by this project: on
first run DupFinder downloads an official build of the
[BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) project into a
`bin` folder next to the exe, or uses the `ffmpeg.exe` / `ffprobe.exe` you
put there.

The file properties of the signed exe name the product `DupFinder` and the
version of the release; SignPath refuses to sign a file that does not match.

## Team roles

| Role | Who |
| ---- | --- |
| Committers and reviewers | [burnersen](https://github.com/burnersen) |
| Approvers | [burnersen](https://github.com/burnersen) |

Changes from anybody else (pull requests) are reviewed by a committer before
they are merged. All team members use multi-factor authentication for GitHub
and for SignPath.

## Privacy policy

This program will not transfer any information to other networked systems
unless specifically requested by the user or the person installing or
operating it.

What DupFinder does on the network, and only for that:

- **FFmpeg download:** only if there is no FFmpeg in the `bin` folder next
  to the exe, DupFinder asks the GitHub API for the newest release of
  [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) and downloads
  it from GitHub. Nothing else is sent.
- **The web interface** is served on `localhost` only: other computers in
  your network cannot reach it.

Like any network request, the download reveals your IP address to GitHub.
Your videos are compared on your own PC and never leave it; the list of
files and your decisions are kept in the `.DupFinder.data` folder next to
the exe. There is no telemetry, no update check, no account and no
advertising.

## Changes to your files

"Delete" in DupFinder moves a file to the Windows **recycle bin**, never a
permanent delete, and only when you choose it.

## Installing and uninstalling

There is no installer: DupFinder is one exe. It keeps FFmpeg (`bin`) and its
data (`.DupFinder.data`, hidden) next to that exe and writes nothing to the
registry. To uninstall, delete the folder.

## Verifying a download

- **Signature:** right-click `DupFinder.exe` > *Properties* > *Digital
  Signatures*. A signed release shows *SignPath Foundation*.
- **Build it yourself:** see [Building from source](README.md#-building-from-source).

## Reporting a problem

If you think a signed file of this project does something it should not,
please open an [issue](https://github.com/burnersen/DupFinder/issues).
Signed files that break these rules can also be reported to the
[SignPath Foundation](https://signpath.org).
