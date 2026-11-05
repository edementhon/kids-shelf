# Kids Shelf

Kids Shelf is a small "local internet" for kids. One computer on the
home Wi-Fi runs it; any other device with a web browser opens it. There are
no accounts, no cloud and no ads. Parents manage everything on a parent
page (`/admin`); kids get their own pages with their own content.

What it does:

- **Downloads from YouTube**: paste a video, playlist or channel link on
  the parent page and Kids Shelf downloads the videos (with its bundled
  yt-dlp and ffmpeg) and keeps its own copy, on any drive you choose. Kids
  never see YouTube itself. No separate downloader, Docker or server setup.
  A followed playlist or channel brings in its new videos every 6 hours,
  straight to the shelf or after the parent says yes.
- **Plays your own videos**: folders on the computer, an external drive or a
  NAS share, in place; files browsers can't play get a converted copy.
- **Daily minutes per kid**, math and spelling practice that earns extra
  minutes, and optional weekly game minutes for screen time outside the app.
- **Offline libraries** (Wikipedia, Vikidia, PhET, Project Gutenberg and
  more, from Kiwix), downloaded or used from `.zim` files you already have.
- No algorithm and no autoplay: a video ends and the next plays only when
  the kid presses Next.

It is built for non-technical families on whatever spare hardware they
have, and it updates itself in the background (every release is signed,
and updates are verified before they install).

- What it is, who it is for, and how to install it on Windows, macOS or
  Linux: [docs/parent-guide.md](docs/parent-guide.md)
- Releases: https://github.com/edementhon/kids-shelf/releases
- Website: https://kidsshelf.org (the page is `site/`; `.github/workflows/site.yml`
  publishes it to GitHub Pages on every push to main that changes it)

## Building

Requirements: Go 1.27 or newer, curl, tar, python3. Release binaries
also bundle helper programs (ffmpeg/ffprobe, QuickJS, kiwix-serve); see
build/helpers/build.sh for their exact pinned builds. The release
tooling keeps the pinned Go toolchain and the helper builds in a
kids-shelf-build folder next to the repo (override with KS_BUILD).

```bash
git clone https://github.com/edementhon/kids-shelf
cd kids-shelf
go build ./cmd/kids-shelf
go test ./...
```

Cutting a signed release is documented in
[docs/RELEASING.md](docs/RELEASING.md).

## License

MIT ([LICENSE](LICENSE)), from v0.3.1 on (v0.2.0 was released under
GPL-3.0-or-later). The helper programs bundled inside each release
(ffmpeg, ffprobe, kiwix-serve and qjs) keep their own licenses, several
of them GPL; Kids Shelf runs them as separate programs.
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) lists every bundled
component with its version, license and upstream URL, and the exact
pinned sources ship with each release as
`kids-shelf-helper-sources-<version>.tar.gz`.
