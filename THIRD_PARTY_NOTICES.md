# Third Party Notices

kids-shelf itself is licensed under GPL-3.0-or-later (see LICENSE). It
also bundles third-party programs and libraries; this file lists every
bundled component with its version, licence and upstream URL, so nothing
ships without its provenance.

The exact pinned sources of the bundled helpers ship with every release
as a separate asset, `kids-shelf-helper-sources-<version>.tar.gz` (built
by scripts/release.sh from the build/helpers download cache;
build/helpers/versions.env pins every version and SHA-256, and the
archive carries build/helpers itself plus x264 at its pinned commit).
The Go module sources are the ones go.sum pins.

## Bundled helper programs (inside every release zip)

| Component | Version | Licence | Upstream |
| --- | --- | --- | --- |
| FFmpeg (ffmpeg, ffprobe) | 9.0.2 | GPL-2.0-or-later as built: configured with `--enable-gpl` and libx264 (stock FFmpeg without the GPL parts is LGPL-2.1-or-later; LICENSE.md in the sources explains the split) | https://ffmpeg.org/ |
| x264 | b35605ace3ddf7c1a5d67a2eb553f034aef41d55 | GPL-2.0-or-later | https://code.videolan.org/videolan/x264 |
| QuickJS-NG (qjs) | 0.17.0 | MIT | https://github.com/quickjs-ng/quickjs |
| kiwix-tools (kiwix-serve) | 3.8.2 | GPL-3.0-or-later | https://github.com/kiwix/kiwix-tools |
| libkiwix | 14.2.1 | GPL-3.0-or-later | https://github.com/kiwix/libkiwix |
| libzim | 9.8.2 | GPL-3.0-or-later | https://github.com/openzim/libzim |
| xapian-core | 1.4.23 | GPL-2.0-or-later | https://xapian.org/ |
| ICU4C | 73.2 | Unicode-DFS-2016 (the Unicode licence in its LICENSE file) | https://github.com/unicode-org/icu |
| zstd | 1.5.7 | BSD-3-Clause | https://github.com/facebook/zstd |
| xz (liblzma) | 5.2.6 | public domain / 0BSD (several licences, per-file split in its COPYING) | https://tukaani.org/xz/ |
| zlib | 1.3.1 | Zlib | https://github.com/madler/zlib |
| pugixml | 1.15 | MIT | https://github.com/zeux/pugixml |
| docopt.cpp | 0.6.3 | MIT (LICENSE-MIT in the archive) | https://github.com/docopt/docopt.cpp |
| libcurl | 8.4.0 | curl licence (MIT-style ISC with the curl exceptions) | https://curl.se/ |
| libmicrohttpd | 0.9.76 | LGPL-2.1-or-later (dual-licensed with the eCos exception; this build enables no TLS, so the LGPL path applies) | https://www.gnu.org/software/libmicrohttpd/ |
| mustache.hpp (kainjow/Mustache) | 4.1 | BSL-1.0 (per the pinned archive's LICENSE) | https://dev.kiwix.org/kiwix-build/mustache-4.1.tar.gz (upstream https://github.com/kainjow/Mustache) |

## Go module dependencies (linked into the kids-shelf binaries)

Resolved from go.mod; sources and licences are the ones the module cache
and go.sum pin. Versions match go.mod at the time of writing.

| Module | Version | Licence |
| --- | --- | --- |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause |
| modernc.org/sqlite | v1.61.0 | BSD-3-Clause (also bundles SQLite itself, public domain; see its LICENSE-SQLITE) |
| github.com/dustin/go-humanize | v1.1.0 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/ncruces/go-strftime | v1.1.0 | MIT |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| modernc.org/libc | v1.79.0 | BSD-3-Clause |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.12.1 | BSD-3-Clause |

## Build-only tools (not shipped, but inside the sources archive)

zig 0.17.0 (MIT), nasm 3.02 (BSD-2-Clause), meson 1.12.1 (Apache-2.0),
ninja 1.13.2 (Apache-2.0), plus the kiwix-build static-build patches and
the meson wrapdb patches (zlib, liblzma, docopt.cpp) that the kiwix
stack builds from. Versions, URLs and SHA-256s are pinned in
build/helpers/versions.env.
