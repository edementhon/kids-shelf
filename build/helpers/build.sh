#!/usr/bin/env bash
# Spike build: minimal static ffmpeg + ffprobe, QuickJS-NG qjs and kiwix-serve
# for the five kids-shelf targets, cross-compiled from one Linux box with Zig
# 0.17 as the C toolchain (no root, no Docker, no macOS SDK). See
# ../docs/spikes/helpers.md for the spike report that this script backs.
# kiwix-serve (D4: ZIM libraries) is built from source with its whole static
# dependency stack by kiwix.sh, which this script sources.
#
# Host requirements: bash, curl, tar, xz, git, make, gcc (host build of nasm),
# cmake (qjs), pkg-config (ffmpeg's libx264 check), python3 (Mach-O/PE checks),
# unzip + patch (kiwix wrap/patches), file.
#
# Behaviour:
#   * every version/checksum is pinned in versions.env (source .env here); each
#     download is verified against its SHA-256 (zig against the checksum that
#     ziglang.org publishes; the rest against the pins in versions.env);
#   * a target is skipped once its four binaries exist (ffmpeg, ffprobe, qjs,
#     kiwix-serve), so reruns are cheap; the kiwix stack itself skips
#     dependency by dependency on its installed static libraries;
#   * per-target sizes, wall times, file(1) output, the Mach-O signature
#     finding, the darwin dylib lists and the windows import list are appended
#     to helpers-results.txt next to the build root;
#   * on linux/amd64 the ffmpeg build is exercised by transcoding the pinned
#     public-domain sample to H.264+AAC MP4 with +faststart and re-probing it,
#     plus a decode-to-discard check (`-f null` with `-xerror`) on the clean
#     sample and on a copy with 4 KB of bytes zeroed in the middle of its mdat;
#   * on linux/amd64 kiwix-serve is exercised against the pinned vikidia ZIM
#     (welcome page 200, full-text search >= 1 result, one article 200).
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=versions.env
. "$SELF_DIR/versions.env"

# The build root defaults to $KS_BUILD (see scripts/env.sh), else the
# kids-shelf-build folder beside the repo.
BUILD_ROOT="${BUILD_ROOT:-${KS_BUILD:-$(dirname "$(cd "$SELF_DIR/../.." && pwd)")/kids-shelf-build}/helpers}"
BUILD_JOBS="${BUILD_JOBS:-$(nproc)}"
D="$BUILD_ROOT/downloads"
S="$BUILD_ROOT/src"
T="$BUILD_ROOT/toolchain"
O="$BUILD_ROOT/out"
BUILD_LOG="$BUILD_ROOT/helpers-build.log"
RESULTS="$BUILD_ROOT/helpers-results.txt"

mkdir -p "$D" "$S" "$T" "$O"
: > "$BUILD_LOG"
: > "$RESULTS"

log() { printf '[%(%H:%M:%S)T] %s\n' -1 "$*" | tee -a "$BUILD_LOG"; }
die() { log "FATAL: $*"; exit 1; }

fetch() { # name url sha256 dest  (re-uses a verified file)
  local name="$1" url="$2" sha="$3" dest="$4"
  if [ -s "$dest" ] && printf '%s  %s\n' "$sha" "$dest" | sha256sum -c - >/dev/null 2>&1; then
    log "have $name (checksum ok)"
    return 0
  fi
  log "downloading $name"
  curl -fL --retry 3 --retry-delay 2 -o "$dest.tmp" "$url" || die "download failed: $name ($url)"
  printf '%s  %s\n' "$sha" "$dest.tmp" | sha256sum -c - >/dev/null || die "checksum mismatch: $name"
  mv "$dest.tmp" "$dest"
}

# True (exit 0) when $1 is not an unstripped ELF: any .debug* section or a
# .symtab/.strtab pair means the binary still carries debug info and must be
# rebuilt.  Non-ELF files (Mach-O, PE) count as stripped: darwin and windows
# binaries are intentionally not stripped (darwin arm64 ad-hoc signatures must
# stay valid; windows sizes are fine), so the check is only ever *applied* to
# the linux targets.  Reads the ELF section table with python3 (host
# requirement) instead of `file`, whose "with debug_info" tag varies by magic
# db version; exit codes: 0 = stripped/not-ELF, 1 = unstripped ELF.
kz_stripped() {
  [ -f "$1" ] || return 1
  python3 - "$1" <<'PYEOF'
import struct, sys
p = open(sys.argv[1], 'rb').read()
if p[:4] != b'\x7fELF':
    sys.exit(0)
e = '<' if p[5] == 1 else '>'
if p[4] == 2:  # ELF64
    shoff, = struct.unpack_from(e + 'Q', p, 0x28)
    shentsize, shnum, shstrndx = struct.unpack_from(e + 'HHH', p, 0x3A)
    stroff, strsize = struct.unpack_from(e + 'QQ', p, shoff + shstrndx * shentsize + 24)
else:          # ELF32
    shoff, = struct.unpack_from(e + 'I', p, 0x20)
    shentsize, shnum, shstrndx = struct.unpack_from(e + 'HHH', p, 0x2E)
    stroff, strsize = struct.unpack_from(e + 'II', p, shoff + shstrndx * shentsize + 16)
if shoff == 0 or shnum == 0 or shstrndx >= shnum:
    sys.exit(0)  # no section table: already stripped
shstr = p[stroff:stroff + strsize]
for i in range(shnum):
    nameoff, = struct.unpack_from(e + 'I', p, shoff + i * shentsize)
    end = shstr.find(b'\0', nameoff)
    if end < 0:
        continue
    name = shstr[nameoff:end]
    if name.startswith(b'.debug') or name in (b'.symtab', b'.strtab'):
        sys.exit(1)
sys.exit(0)
PYEOF
}

tot0=$SECONDS

# ---- 1. toolchain: zig (cross C compiler) + nasm (x86 assembler) ----------
ZIG="$T/zig/zig"
if [ ! -x "$ZIG" ]; then
  fetch zig "$ZIG_URL" "$ZIG_SHA256" "$D/zig-$ZIG_VERSION.tar.xz"
  tar xJf "$D/zig-$ZIG_VERSION.tar.xz" -C "$T"
  mv "$T"/zig-x86_64-linux-* "$T/zig"
fi
[ "$("$ZIG" version)" = "$ZIG_VERSION" ] || die "zig version is not $ZIG_VERSION"

NASM_BIN="$T/nasm/bin/nasm"
if [ ! -x "$NASM_BIN" ]; then
  fetch nasm "$NASM_URL" "$NASM_SHA256" "$D/nasm-$NASM_VERSION.tar.xz"
  rm -rf "$S/nasm-src"
  mkdir -p "$S/nasm-src"
  tar xJf "$D/nasm-$NASM_VERSION.tar.xz" -C "$S/nasm-src"
  ( cd "$S/nasm-src/nasm-$NASM_VERSION" \
      && ./configure --prefix="$T/nasm" >"$BUILD_LOG.nasm" 2>&1 \
      && make -j"$BUILD_JOBS" >>"$BUILD_LOG.nasm" 2>&1 \
      && make install >>"$BUILD_LOG.nasm" 2>&1 ) \
    || die "nasm host build failed (see $BUILD_LOG.nasm)"
fi
export PATH="$T/nasm/bin:$PATH"
log "toolchain: $("$ZIG" version) zig, $(nasm -v | head -1)"

# ---- 2. sources ------------------------------------------------------------
if [ ! -d "$S/x264/.git" ]; then
  log "cloning x264 ($X264_BRANCH)"
  git clone -q --depth 1 --branch "$X264_BRANCH" "$X264_REPO" "$S/x264" || die "x264 clone failed"
fi
if [ "$(git -C "$S/x264" rev-parse HEAD)" != "$X264_COMMIT" ]; then
  log "moving x264 to pinned commit $X264_COMMIT"
  git -C "$S/x264" fetch -q --depth 1 origin "$X264_COMMIT" || die "cannot fetch pinned x264 commit"
  git -C "$S/x264" checkout -q "$X264_COMMIT"
fi

if [ ! -d "$S/ffmpeg-$FFMPEG_VERSION" ]; then
  fetch ffmpeg "$FFMPEG_URL" "$FFMPEG_SHA256" "$D/ffmpeg-$FFMPEG_VERSION.tar.xz"
  tar xJf "$D/ffmpeg-$FFMPEG_VERSION.tar.xz" -C "$S"
fi
if [ ! -d "$S/quickjs" ]; then
  fetch qjs "$QJS_URL" "$QJS_SHA256" "$D/quickjs-ng-v$QJS_VERSION.tar.gz"
  tar xzf "$D/quickjs-ng-v$QJS_VERSION.tar.gz" -C "$S"
  mv "$S/quickjs-$QJS_VERSION" "$S/quickjs"
fi
if [ ! -d "$S/dav1d-$DAV1D_VERSION" ]; then
  fetch dav1d "$DAV1D_URL" "$DAV1D_SHA256" "$D/dav1d-$DAV1D_VERSION.tar.xz"
  tar xJf "$D/dav1d-$DAV1D_VERSION.tar.xz" -C "$S"
fi
 fetch sample "$SAMPLE_URL" "$SAMPLE_SHA256" "$D/sample.mp4"

# ---- 2c. hardware-encoder sources (per-target, see the loop below) --------
# ffnvcodec (h264_nvenc) and AMF (h264_amf) are header-only; oneVPL
# (h264_qsv) is built as a static dispatcher per target; libva+libdrm build
# only for the glibc ffmpeg-hw (headers and link stubs; the run-time libva
# comes from the user's system, never bundled).  The macOS SDK supplies the
# VideoToolbox/CoreMedia/CoreVideo/CoreFoundation framework headers and .tbd
# link stubs for h264_videotoolbox (DECISIONS.md 2026-10-26 accepts Apple's
# SDK licence); only the framework/usr trees ffmpeg needs are extracted, but
# the full Frameworks tree is kept so the header closure always resolves.
# The upstream tag (and so the tarball's top directory) carries an "n":
# nv-codec-headers-n<version>.
if [ ! -d "$S/nv-codec-headers-n$FFNVCODEC_VERSION" ]; then
  fetch ffnvcodec "$FFNVCODEC_URL" "$FFNVCODEC_SHA256" \
    "$D/nv-codec-headers-n$FFNVCODEC_VERSION.tar.gz"
  tar xzf "$D/nv-codec-headers-n$FFNVCODEC_VERSION.tar.gz" -C "$S"
fi
if [ ! -d "$S/amf-headers-v$AMF_VERSION" ]; then
  fetch amf "$AMF_URL" "$AMF_SHA256" "$D/AMF-headers-v$AMF_VERSION.tar.gz"
  tar xzf "$D/AMF-headers-v$AMF_VERSION.tar.gz" -C "$S"
fi
if [ ! -d "$S/libvpl-$VPL_VERSION" ]; then
  fetch vpl "$VPL_URL" "$VPL_SHA256" "$D/libvpl-v$VPL_VERSION.tar.gz"
  tar xzf "$D/libvpl-v$VPL_VERSION.tar.gz" -C "$S"
fi
if [ ! -d "$S/libva-$LIBVA_VERSION" ]; then
  fetch libva "$LIBVA_URL" "$LIBVA_SHA256" "$D/libva-$LIBVA_VERSION.tar.gz"
  tar xzf "$D/libva-$LIBVA_VERSION.tar.gz" -C "$S"
fi
if [ ! -d "$S/libdrm-$LIBDRM_VERSION" ]; then
  fetch libdrm "$LIBDRM_URL" "$LIBDRM_SHA256" "$D/libdrm-$LIBDRM_VERSION.tar.xz"
  tar xJf "$D/libdrm-$LIBDRM_VERSION.tar.xz" -C "$S"
fi
SDK="$T/sdk/MacOSX11.3.sdk"
if [ ! -f "$SDK/System/Library/Frameworks/VideoToolbox.framework/Versions/A/VideoToolbox.tbd" ]; then
  fetch macos-sdk "$MACOS_SDK_URL" "$MACOS_SDK_SHA256" \
    "$D/MacOSX$MACOS_SDK_VERSION.sdk.tar.xz"
  mkdir -p "$T/sdk"
  tar xJf "$D/MacOSX$MACOS_SDK_VERSION.sdk.tar.xz" -C "$T/sdk" \
    MacOSX11.3.sdk/System/Library/Frameworks \
    MacOSX11.3.sdk/usr/lib/libobjc.A.tbd \
    MacOSX11.3.sdk/usr/lib/libobjc.tbd \
    MacOSX11.3.sdk/usr/include/libDER \
    || die "macOS SDK extraction failed"
  mkdir -p "$T/sdk/extra-include"
  mv "$SDK/usr/include/libDER" "$T/sdk/extra-include/"
fi
HVA="$O/linux_amd64_hw/libva"
HP="$O/linux_amd64_hw/prefix"

# ---- 2b. kiwix-serve stack (D4): host tools, sources, host ICU ------------
# shellcheck source=kiwix.sh
. "$SELF_DIR/kiwix.sh"
kiwix_host_tools
kiwix_sources

# ---- 2d. dav1d (software AV1 decoder for ffmpeg's libdav1d) ---------------
# ffmpeg's native av1 decoder only drives hardware accelerators (none are
# enabled in these builds), so it fails every frame; AV1 input decodes
# through libdav1d on all five targets and in ffmpeg-hw instead.  Static
# library only (meson + ninja from kiwix_host_tools; CLI tools, tests and
# examples off), both bit depths (8 and 16, i.e. 10/12-bit AV1 too).
# Assembly is on everywhere: nasm (the $NASM_BIN built above, named in the
# cross file) for x86_64 elf64/win64/macho64, the compiler's integrated
# assembler for aarch64 linux and darwin.
# dav1d_build <label> <cross file> <prefix> <build dir>
dav1d_build() {
  local label="$1" cf="$2" prefix="$3" bd="$4"
  if [ -f "$prefix/lib/libdav1d.a" ]; then
    log "have libdav1d.a ($label)"
    return 0
  fi
  rm -rf "$bd"
  log "dav1d: meson setup ($label)"
  ( cd "$S/dav1d-$DAV1D_VERSION" && python3 -m mesonbuild.mesonmain setup "$bd" \
      --cross-file "$cf" --prefix "$prefix" --libdir lib \
      --buildtype release -Ddefault_library=static -Denable_asm=true \
      -Denable_tools=false -Denable_tests=false -Denable_examples=false \
      -Denable_docs=false ) 2>&1 | tee -a "$BUILD_LOG" \
    || die "dav1d meson failed for $label (see $BUILD_LOG)"
  log "dav1d: build + install ($label)"
  ( cd "$bd" && ninja -j"$BUILD_JOBS" && ninja install ) 2>&1 | tee -a "$BUILD_LOG" \
    || die "dav1d build failed for $label (see $BUILD_LOG)"
}

# ---- 3. five targets -------------------------------------------------------
# label | zig triple | arch | ffmpeg --target-os | x264 --host
#       | cmake system | cmake arch | qjs CLI static?
TARGETS=(
  "linux/amd64|x86_64-linux-musl|x86_64|linux|x86_64-unknown-linux-musl|Linux|x86_64|ON"
  "linux/arm64|aarch64-linux-musl|aarch64|linux|aarch64-unknown-linux-musl|Linux|aarch64|ON"
  "windows/amd64|x86_64-windows-gnu|x86_64|mingw32|x86_64-w64-mingw32|Windows|x86_64|ON"
  "darwin/amd64|x86_64-macos|x86_64|darwin|x86_64-apple-darwin|Darwin|x86_64|OFF"
  "darwin/arm64|aarch64-macos|aarch64|darwin|aarch64-apple-darwin|Darwin|aarch64|OFF"
)

for spec in "${TARGETS[@]}"; do
  IFS='|' read -r LABEL TRIPLE ARCH FFOS XHOST SYSNAME CMARCH CLI_STATIC <<< "$spec"
  KEY="${LABEL//\//_}"
  P="$O/$KEY/prefix"
  B="$O/$KEY/bin"
  EXT=""; [ "$FFOS" = mingw32 ] && EXT=.exe
  IS_LINUX=0
  if [ "${LABEL#linux/}" != "$LABEL" ]; then
    IS_LINUX=1
  fi
  mkdir -p "$P" "$B"

  # A target is complete only when all four binaries exist, and on windows
  # only when kiwix-serve.exe was not linked against the stale (stub) ICU
  # data archive; on linux additionally only when the four binaries are
  # stripped (debug info is dropped at link time, see kz_stripped), so a
  # build that predates the strip flags is rebuilt, not skipped (see
  # kiwix_windows_stale / kiwix_build_target in kiwix.sh). A run interrupted
  # before kiwix-serve links must build it on the next run, not skip.
  if [ -x "$B/ffmpeg$EXT" ] && [ -x "$B/ffprobe$EXT" ] && [ -x "$B/qjs$EXT" ] \
      && [ -x "$B/kiwix-serve$EXT" ] && ! kiwix_windows_stale \
      && { [ "$LABEL" != "linux/amd64" ] \
           || { [ -x "$B/ffmpeg-hw" ] && kz_stripped "$B/ffmpeg-hw"; }; } \
      && { [ "$IS_LINUX" = 0 ] \
           || { kz_stripped "$B/ffmpeg$EXT" && kz_stripped "$B/ffprobe$EXT" \
                && kz_stripped "$B/qjs$EXT" && kz_stripped "$B/kiwix-serve$EXT"; }; }; then
    log "target $LABEL already complete, skipping"
    kz_icu_data_check
    continue
  fi

  # zig wrappers (bare-command paths for CMake; archives must be built by zig:
  # zig's Mach-O linker cannot read GNU ar archives). x264 ignores
  # --disable-shared and builds static-only unless --enable-shared.
  WRAP="$S/zigcc-$KEY"
  ZAR="$S/zigar-$KEY"
  ZRAN="$S/zigranlib-$KEY"
  {
    printf '#!/bin/sh\nexec "%s" cc -target %s "$@"\n' "$ZIG" "$TRIPLE" > "$WRAP"
    printf '#!/bin/sh\nexec "%s" c++ -target %s "$@"\n' "$ZIG" "$TRIPLE" > "$S/zigcxx-$KEY"
    printf '#!/bin/sh\nexec "%s" ar "$@"\n' "$ZIG" > "$ZAR"
    printf '#!/bin/sh\nexec "%s" ranlib "$@"\n' "$ZIG" > "$ZRAN"
    chmod +x "$WRAP" "$S/zigcxx-$KEY" "$ZAR" "$ZRAN"
  }

  # ffmpeg's configure adds ld64-only flags (-Wl,-dynamic,-search_paths_first)
  # for darwin targets; zig's driver rejects -dynamic, so drop it (a static
  # executable does not need it). $LD is used for configure test links and for
  # the final link.
  if [ "$FFOS" = darwin ]; then
    LDWRAP="$S/zigld-$KEY"
    cat > "$LDWRAP" <<EOF
#!/bin/sh
out=""
oldifs=\$IFS
for a in "\$@"; do
  case "\$a" in
    -Wl,*)
      f=""
      IFS=,
      for p in \$a; do
        case "\$p" in -Wl|-dynamic) ;; *) f="\$f,\$p" ;; esac
      done
      IFS=\$oldifs
      f="\${f#,}"
      [ -n "\$f" ] && out="\$out -Wl,\$f"
      ;;
    *) out="\$out \$a" ;;
  esac
done
exec "$ZIG" cc -target $TRIPLE \$out
EOF
    chmod +x "$LDWRAP"
    # host nm cannot read Mach-O; without a working nm, ffmpeg's configure
    # fails its symbol-prefix check, never appends -DPREFIX to X86ASMFLAGS,
    # and the nasm Mach-O symbols then mismatch the C references at link.
    NM="$S/zignm-$KEY"
    cat > "$NM" <<'PYEOF'
#!/usr/bin/env python3
# Minimal nm for thin 64-bit Mach-O objects: prints GNU-nm-shaped lines.
import struct, sys
p = open(sys.argv[1], 'rb').read()
if p[:4] not in (b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf'):
    sys.exit(0)
le = p[:4] == b'\xcf\xfa\xed\xfe'
e = '<' if le else '>'
ncmds = struct.unpack_from(e + 'I', p, 16)[0]
cur = 32
symoff = nsyms = stroff = strsize = None
for _ in range(ncmds):
    cmd, size = struct.unpack_from(e + 'II', p, cur)
    if cmd == 0x2:  # LC_SYMTAB
        symoff, nsyms, stroff, strsize = struct.unpack_from(e + 'IIII', p, cur + 8)
    cur += size
if symoff is None:
    sys.exit(0)
strtab = p[stroff:stroff + strsize]
for i in range(nsyms):
    off = symoff + i * 16
    n_strx, n_type, n_sect, n_desc, n_value = struct.unpack_from(e + 'IBBHQ', p, off)
    if not n_strx or n_strx >= len(strtab):
        continue
    end = strtab.find(b'\0', n_strx)
    name = strtab[n_strx:end].decode('utf-8', 'replace')
    typ = {0x0: 'U', 0x2: 'A', 0xe: 'T', 0xc: 'S'}.get(n_type & 0xe, '?')
    kind = 'g' if n_type & 0x1 else 'l'
    print(f'{n_value:016x} {typ}{kind} {name}')
PYEOF
    chmod +x "$NM"
  else
    LDWRAP="$ZIG cc -target $TRIPLE"
    NM="nm"
  fi

  log "================ $LABEL ($TRIPLE) ================"

  # --- per-target hardware-encoder extras ----------------------------------
  # linux/amd64 keeps its software ffmpeg/ffprobe byte-identical (the
  # contract); its hardware encoders live entirely in ffmpeg-hw below.  Every
  # other target extends the configure line with $HW_OPTS and appends
  # $HW_ENCODERS/$HW_DECODERS/$HW_FILTERS; all are empty for linux/amd64, so
  # that invocation (and its -buildconf) stays exactly as it was.
  # Every hardware build also carries lavfi + testsrc (+ the wrapped_avframe
  # decoder lavfi frames arrive as): kids-shelf's encoder self-test makes its
  # one-second clip with them (internal/ingest hwenc.go selfTestArgs), and
  # without them no hardware encoder could ever pass that test.
  HW_OPTS=()
  HW_CFLAGS="" HW_LDFLAGS="" HW_ENCODERS="" HW_DECODERS="" HW_FILTERS=""
  HW_SELFTEST_OPTS=( --enable-indev=lavfi )
  case "$LABEL" in
    darwin/*)
      # h264_videotoolbox: frameworks from the extracted SDK (see sources
      # section).  zig's bundled Availability headers default to a 27.0 SDK,
      # so pin MIN/MAX_ALLOWED to the 11.3 SDK; without MAX_ALLOWED ffmpeg
      # emits CVBufferCopyAttachments (macOS 12) against the 11.3 headers.
      [ -f "$SDK/System/Library/Frameworks/VideoToolbox.framework/Versions/A/VideoToolbox.tbd" ] \
        || die "macOS SDK not extracted for $LABEL (see the sources section)"
      HW_OPTS=( --enable-videotoolbox "${HW_SELFTEST_OPTS[@]}" )
      HW_ENCODERS="h264_videotoolbox"
      HW_DECODERS="wrapped_avframe"
      HW_FILTERS="hwupload,testsrc"
      HW_CFLAGS="-F$SDK/System/Library/Frameworks -I$T/sdk/extra-include \
-D__MAC_OS_X_VERSION_MIN_REQUIRED=110000 -D__MAC_OS_X_VERSION_MAX_ALLOWED=110300"
      HW_LDFLAGS="-F$SDK/System/Library/Frameworks -L$SDK/usr/lib"
      ;;
    windows/*)
      # h264_mf (Media Foundation), h264_nvenc (ffnvcodec headers), h264_amf
      # (AMF headers) and h264_qsv (oneVPL static dispatcher).  mfplat/amf/
      # nvenc drivers are LoadLibrary'd at run time, so the exe's import
      # table stays system-only; -lmfplat in configure's check resolves via
      # zig's mingw .def import libraries.
      HW_OPTS=( --enable-libvpl --enable-ffnvcodec --enable-nvenc --enable-amf \
                --enable-mediafoundation --enable-d3d11va "${HW_SELFTEST_OPTS[@]}" )
      HW_ENCODERS="h264_mf,h264_nvenc,h264_amf,h264_qsv"
      HW_DECODERS="wrapped_avframe"
      HW_FILTERS="hwupload,testsrc"
      # oneVPL's dispatcher is C++ and its vpl.pc names no C++ runtime, so
      # configure's libvpl check (a C test program) fails on std:: symbols
      # without it. zig links its own libc++ statically here, so the exe's
      # import table stays system-only (checked after the build).
      HW_LDFLAGS="-lc++"
      ;;
    linux/arm64)
      # h264_v4l2m2m uses the kernel V4L2 API only (zig bundles the
      # linux/videodev2.h headers); no external library.
      HW_OPTS=( --enable-v4l2-m2m "${HW_SELFTEST_OPTS[@]}" )
      HW_ENCODERS="h264_v4l2m2m"
      HW_DECODERS="wrapped_avframe"
      HW_FILTERS="hwupload,testsrc"
      ;;
  esac

  # cmake toolchain file (qjs below, and the windows oneVPL build above it);
  # created up front so both consumers share one definition.
  TCF="$S/cmake-$KEY.cmake"
  QBD="$S/qjsbuild-$KEY"
  cat > "$TCF" <<EOF
set(CMAKE_SYSTEM_NAME $SYSNAME)
set(CMAKE_SYSTEM_PROCESSOR $CMARCH)
set(CMAKE_C_COMPILER $WRAP)
set(CMAKE_CXX_COMPILER $S/zigcxx-$KEY)
set(CMAKE_AR $ZAR)
set(CMAKE_RANLIB $ZRAN)
set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)
set(CMAKE_C_ARCHIVE_CREATE "<CMAKE_AR> qc <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_C_ARCHIVE_APPEND "<CMAKE_AR> q  <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_C_ARCHIVE_FINISH "<CMAKE_RANLIB> <TARGET>")
set(CMAKE_CXX_ARCHIVE_CREATE "<CMAKE_AR> qc <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_CXX_ARCHIVE_APPEND "<CMAKE_AR> q  <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_CXX_ARCHIVE_FINISH "<CMAKE_RANLIB> <TARGET>")
EOF

  # windows-only: oneVPL static dispatcher + ffnvcodec/AMF headers into $P.
  if [ "$LABEL" = "windows/amd64" ]; then
    if [ ! -f "$P/lib/pkgconfig/vpl.pc" ]; then
      VBD="$S/vplbuild-$KEY"
      rm -rf "$VBD"
      log "vpl: cmake configure ($LABEL)"
      ( cd "$S/libvpl-$VPL_VERSION" && cmake -B "$VBD" \
          -DCMAKE_TOOLCHAIN_FILE="$TCF" -DCMAKE_BUILD_TYPE=Release \
          -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF \
          -DINSTALL_EXAMPLES=OFF -DINSTALL_DEV=ON -DINSTALL_LIB=ON \
          -DBUILD_EXPERIMENTAL=OFF -DCMAKE_INSTALL_PREFIX="$P" ) \
          2>&1 | tee -a "$BUILD_LOG" || die "vpl cmake failed for $LABEL"
      log "vpl: build + install ($LABEL)"
      ( cd "$S/libvpl-$VPL_VERSION" \
          && cmake --build "$VBD" -j"$BUILD_JOBS" \
          && cmake --install "$VBD" ) 2>&1 | tee -a "$BUILD_LOG" \
        || die "vpl build failed for $LABEL"
    fi
    if [ ! -d "$P/include/ffnvcodec" ]; then
      log "ffnvcodec: install headers ($LABEL)"
      ( cd "$S/nv-codec-headers-n$FFNVCODEC_VERSION" \
          && make install PREFIX="$P" LIBDIR=lib ) 2>&1 | tee -a "$BUILD_LOG" \
        || die "ffnvcodec headers failed for $LABEL"
    fi
    if [ ! -f "$P/include/AMF/core/Version.h" ]; then
      mkdir -p "$P/include"
      cp -a "$S/amf-headers-v$AMF_VERSION/AMF" "$P/include/"
    fi
  fi

  # --- x264 (static lib for ffmpeg) -----------------------------------------
  # x264's Makefile does "$(AR)$@", so AR must carry its flags and a trailing
  # space (its default is "gcc-ar rc ").
  if [ ! -f "$P/lib/libx264.a" ]; then
    rm -rf "$S/x264-$KEY"
    cp -a "$S/x264" "$S/x264-$KEY"
    log "x264: configure ($LABEL)"
    ( cd "$S/x264-$KEY" && CC="$ZIG cc -target $TRIPLE" \
        ./configure --host="$XHOST" --prefix="$P" \
          --enable-static --disable-shared --enable-pic \
          --disable-cli --disable-lavf --disable-swscale ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "x264 configure failed for $LABEL"
    x264_t0=$SECONDS
    log "x264: make ($LABEL)"
    ( cd "$S/x264-$KEY" && make -j"$BUILD_JOBS" AR="$ZIG ar rc " RANLIB="$ZRAN" \
        && make install AR="$ZIG ar rc " RANLIB="$ZRAN" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "x264 build failed for $LABEL"
    x264_t=$((SECONDS - x264_t0))
  else
    x264_t=0
  fi

  # --- dav1d (static libdav1d for ffmpeg, see section 2d) --------------------
  case "$LABEL" in
    linux/*)   DSYS=linux ;;
    windows/*) DSYS=windows ;;
    darwin/*)  DSYS=darwin ;;
  esac
  cat > "$S/cross-dav1d-$KEY.txt" <<EOF
[binaries]
c = '$WRAP'
ar = '$ZAR'
ranlib = '$ZRAN'
nasm = '$NASM_BIN'
pkg-config = 'pkg-config'

[host_machine]
system = '$DSYS'
cpu_family = '$ARCH'
cpu = '$ARCH'
endian = 'little'
EOF
  dav1d_build "$LABEL" "$S/cross-dav1d-$KEY.txt" "$P" "$S/dav1dbuild-$KEY"

  # --- ffmpeg + ffprobe (out-of-tree build) ---------------------------------
  # FFmpeg 9 has no separate mp4/webm demuxer components (mov/matroska cover
  # them) and no mp3/mpeg4/eac3 parsers (mpegaudio/mpeg4video/ac3 cover them);
  # zscale needs the external zimg library, so it is left out. lavfi is an
  # indev and gets disabled by --disable-everything (see the runtime section).
  # The null muxer (plus its default codecs wrapped_avframe/pcm_s16le) is added
  # for the decode-to-discard check in the runtime section.
  # AV1 decodes through libdav1d only: ffmpeg's native av1 decoder is a
  # hwaccel front end that fails every frame without one (and no hwaccel is
  # enabled), so it is left out rather than kept as a dead second choice.
  # ffmpeg/ffprobe are binary-gated like the rest of the stack: a per-target
  # repair (e.g. the windows ICU data fix) must not rebuild them -- they do
  # not depend on ICU. On linux an existing but unstripped pair is equally
  # incomplete (a build that predates the -s link flag) and is rebuilt.
  if [ ! -x "$B/ffmpeg$EXT" ] || [ ! -x "$B/ffprobe$EXT" ] \
      || { [ "$IS_LINUX" = 1 ] && { ! kz_stripped "$B/ffmpeg$EXT" \
          || ! kz_stripped "$B/ffprobe$EXT"; }; }; then
    FBD="$S/ffbuild-$KEY"
    rm -rf "$FBD"
    mkdir -p "$FBD"
    # linux only: -s in the link flags makes zig's lld strip the final
    # ffmpeg/ffprobe executables (--strip-all) so they ship without the debug
    # info zig cc otherwise emits (-g is on by default); there are no shared
    # libraries in this build, so FFEXTRALDFLAGS only ever reaches
    # executable links (plus configure's throwaway test links, where -s is
    # harmless). darwin/windows keep unstripped binaries as before.
    FF_LDFLAGS="-L$P/lib"
    if [ "$IS_LINUX" = 1 ]; then
      FF_LDFLAGS="-L$P/lib -s"
    fi
    log "ffmpeg: configure ($LABEL)"
    ( cd "$FBD" && export PKG_CONFIG_PATH="$P/lib/pkgconfig" \
        && "$S/ffmpeg-$FFMPEG_VERSION/configure" \
        --cc="$ZIG cc -target $TRIPLE" \
        --cxx="$ZIG c++ -target $TRIPLE" \
        --ld="$LDWRAP" \
        --ar="$ZIG ar" --ranlib="$ZIG ranlib" \
        --nm="$NM" \
        --target-os="$FFOS" --arch="$ARCH" --enable-cross-compile \
        --disable-everything --disable-autodetect --disable-doc --disable-network \
        --disable-stripping --enable-gpl --enable-static --disable-shared --enable-libx264 \
        --enable-libdav1d \
        ${HW_OPTS[@]+"${HW_OPTS[@]}"} \
        --enable-decoder=h264,hevc,aac,ac3,eac3,dca,mp3,mp2,vorbis,flac,vp9,libdav1d,mpeg4,opus${HW_DECODERS:+,$HW_DECODERS} \
        --enable-encoder=libx264,aac,mjpeg,wrapped_avframe,pcm_s16le${HW_ENCODERS:+,$HW_ENCODERS} \
        --enable-parser=aac,ac3,dca,h264,hevc,mpeg4video,mpegaudio,opus,vp9,av1 \
        --enable-demuxer=mov,matroska,mpegts,avi \
        --enable-muxer=mp4,mov,image2,null \
        --enable-protocol=file,pipe \
        --enable-filter=scale,aresample,format,setpts,fps,thumbnail,null,anull${HW_FILTERS:+,$HW_FILTERS} \
        --enable-bsf=h264_mp4toannexb,aac_adtstoasc \
        --extra-cflags="-I$P/include${HW_CFLAGS:+ $HW_CFLAGS}" --extra-ldflags="$FF_LDFLAGS${HW_LDFLAGS:+ $HW_LDFLAGS}" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "ffmpeg configure failed for $LABEL"
    ff_t0=$SECONDS
    log "ffmpeg: make ($LABEL)"
    # ARFLAGS=rcDs: 's' makes zig ar write the archive symbol table (ffmpeg's
    # default rcD leaves none and its ranlib step does not run here; zig's
    # Mach-O linker resolves archive members through that table).
    ( cd "$FBD" && make -j"$BUILD_JOBS" ARFLAGS=rcDs ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "ffmpeg build failed for $LABEL"
    ff_t=$((SECONDS - ff_t0))
    cp "$FBD/ffmpeg$EXT" "$FBD/ffprobe$EXT" "$B/"
  else
    ff_t=0
  fi

  # --- QuickJS-NG qjs (CMake build; shared per-target toolchain file) ------
  # qjs is binary-gated like ffmpeg (see above): per-target repairs of the
  # kiwix stack must not rebuild it; on linux an existing but unstripped qjs
  # (predating the -s flag) is equally incomplete and is rebuilt.
  QJS_LDFLAGS=""
  if [ "$IS_LINUX" = 1 ]; then
    # -s reaches only executable links (CMAKE_EXE_LINKER_FLAGS); the static
    # libquickjs archive and darwin/windows executables are unaffected.
    QJS_LDFLAGS="-DCMAKE_EXE_LINKER_FLAGS=-s"
  fi
  if [ ! -x "$B/qjs$EXT" ] || { [ "$IS_LINUX" = 1 ] && ! kz_stripped "$B/qjs$EXT"; }; then
    rm -rf "$QBD"
    log "qjs: cmake configure ($LABEL)"
    ( cd "$S/quickjs" && cmake -B "$QBD" -DCMAKE_TOOLCHAIN_FILE="$TCF" \
        -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF $QJS_LDFLAGS \
        -DQJS_BUILD_CLI_STATIC="$CLI_STATIC" -DQJS_ENABLE_INSTALL=OFF ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "qjs cmake failed for $LABEL"
    qjs_t0=$SECONDS
    log "qjs: build ($LABEL)"
    ( cd "$S/quickjs" && cmake --build "$QBD" --target qjs_exe -j"$BUILD_JOBS" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "qjs build failed for $LABEL"
    qjs_t=$((SECONDS - qjs_t0))
    cp "$QBD/qjs$EXT" "$B/qjs$EXT"
    # if-form: a short-circuited && list as the branch's last statement would
    # return 1 for non-linux targets and set -e would kill the build.
    if [ "$LABEL" = "linux/amd64" ]; then
      chmod +x "$B/qjs"
      "$B/qjs" -e 'console.log("qjs smoke ok")' >/dev/null
    fi
  else
    qjs_t=0
  fi

  # --- kiwix-serve stack (D4) -----------------------------------------------
  kiwix_build_target

  # --- linux/amd64 ffmpeg-hw (dynamic glibc: VAAPI + NVENC + QSV) ----------
  # A second linux/amd64 binary, dynamically linked against glibc with a
  # 2.28 symbol baseline (zig target x86_64-linux-gnu.2.28): x264 and the
  # oneVPL dispatcher stay static inside it, libva/libva-drm are linked
  # dynamically and resolved from the user's system at run time (never
  # bundled), and nvenc/qsv dlopen their runtimes, so the binary starts
  # without any GPU.  On a machine without libva it fails to start before
  # main; kids-shelf treats that as "no hardware".
  if [ "$LABEL" = "linux/amd64" ]; then
    if [ ! -x "$B/ffmpeg-hw" ] || ! kz_stripped "$B/ffmpeg-hw"; then
      mkdir -p "$HP" "$HVA"
      # zig cc wrapper for the glibc target (pkg-config sees the same triple)
      ZHW="$S/zigcc-hw"
      printf '#!/bin/sh\nexec "%s" cc -target x86_64-linux-gnu.2.28 "$@"\n' "$ZIG" > "$ZHW"
      chmod +x "$ZHW"
      # x264 static (glibc), into the hw prefix
      if [ ! -f "$HP/lib/libx264.a" ]; then
        rm -rf "$S/x264-hw"
        cp -a "$S/x264" "$S/x264-hw"
        log "x264-hw: configure (x86_64-linux-gnu.2.28)"
        ( cd "$S/x264-hw" && CC="$ZHW" \
            ./configure --host=x86_64-unknown-linux-gnu --prefix="$HP" \
              --enable-static --disable-shared --enable-pic \
              --disable-cli --disable-lavf --disable-swscale ) 2>&1 | tee -a "$BUILD_LOG" \
          || die "x264-hw configure failed"
        ( cd "$S/x264-hw" && make -j"$BUILD_JOBS" AR="$ZIG ar rc " RANLIB="$ZIG ranlib" \
            && make install AR="$ZIG ar rc " RANLIB="$ZIG ranlib" ) 2>&1 | tee -a "$BUILD_LOG" \
          || die "x264-hw build failed"
      fi
      # oneVPL static dispatcher (glibc).  zig's glibc model is 2.28 for the
      # x86_64-linux-gnu.2.28 target, but lacks sys/sysctl.h (glibc dropped it
      # in 2.32); ffmpeg's cpu detection only #includes it on the glibc path
      # and never calls sysctl() (sched_getaffinity wins), so ship the glibc
      # 2.28 header as a compile stub.
      mkdir -p "$HP/include/sys"
      cat > "$HP/include/sys/sysctl.h" <<'SYSCTLEOF'
/* glibc 2.28 sys/sysctl.h (removed in glibc 2.32); see build.sh ffmpeg-hw. */
#ifndef _SYS_SYSCTL_H
#define _SYS_SYSCTL_H 1
#include <features.h>
#include <stddef.h>
#define CTL_MAXNAME 12
struct __sysctl_args {
	int *name;
	int nlen;
	void *oldval;
	size_t *oldlenp;
	void *newval;
	size_t newlen;
};
__BEGIN_DECLS
extern int sysctl (int *__name, int __nlen, void *__oldval,
		   size_t *__oldlenp, void *__newval, size_t __newlen);
__END_DECLS
#endif
SYSCTLEOF
      if [ ! -f "$HP/lib/pkgconfig/vpl.pc" ]; then
        cat > "$S/cmake-hw.cmake" <<EOF
set(CMAKE_SYSTEM_NAME Linux)
set(CMAKE_SYSTEM_PROCESSOR x86_64)
set(CMAKE_C_COMPILER $ZHW)
set(CMAKE_CXX_COMPILER $S/zigcxx-hw)
set(CMAKE_AR $ZAR)
set(CMAKE_RANLIB $ZRAN)
set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)
set(CMAKE_C_ARCHIVE_CREATE "<CMAKE_AR> qc <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_C_ARCHIVE_APPEND "<CMAKE_AR> q  <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_C_ARCHIVE_FINISH "<CMAKE_RANLIB> <TARGET>")
set(CMAKE_CXX_ARCHIVE_CREATE "<CMAKE_AR> qc <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_CXX_ARCHIVE_APPEND "<CMAKE_AR> q  <TARGET> <LINK_FLAGS> <OBJECTS>")
set(CMAKE_CXX_ARCHIVE_FINISH "<CMAKE_RANLIB> <TARGET>")
EOF
        printf '#!/bin/sh\nexec "%s" c++ -target x86_64-linux-gnu.2.28 "$@"\n' "$ZIG" > "$S/zigcxx-hw"
        chmod +x "$S/zigcxx-hw"
        VBD="$S/vplbuild-hw"
        rm -rf "$VBD"
        log "vpl-hw: cmake configure"
        ( cd "$S/libvpl-$VPL_VERSION" && cmake -B "$VBD" \
            -DCMAKE_TOOLCHAIN_FILE="$S/cmake-hw.cmake" -DCMAKE_BUILD_TYPE=Release \
            -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF \
            -DINSTALL_EXAMPLES=OFF -DINSTALL_DEV=ON -DINSTALL_LIB=ON \
            -DBUILD_EXPERIMENTAL=OFF -DCMAKE_INSTALL_PREFIX="$HP" ) \
            2>&1 | tee -a "$BUILD_LOG" || die "vpl-hw cmake failed"
        ( cd "$S/libvpl-$VPL_VERSION" \
            && cmake --build "$VBD" -j"$BUILD_JOBS" \
            && cmake --install "$VBD" ) 2>&1 | tee -a "$BUILD_LOG" \
          || die "vpl-hw build failed"
      fi
      # ffnvcodec headers into the hw prefix
      if [ ! -d "$HP/include/ffnvcodec" ]; then
        ( cd "$S/nv-codec-headers-n$FFNVCODEC_VERSION" \
            && make install PREFIX="$HP" LIBDIR=lib ) 2>&1 | tee -a "$BUILD_LOG" \
          || die "ffnvcodec-hw headers failed"
      fi
      # libdrm static + libva/libva-drm shared into $HVA (meson cross; meson
      # and ninja come from the kiwix host tools).  Only headers and link
      # stubs are used: the shipped ffmpeg-hw never bundles libva.
      MESON="python3 -m mesonbuild.mesonmain"
      export PYTHONPATH="$T/meson" PATH="$T/ninja:$PATH"
      cat > "$S/cross-hw.txt" <<EOF
[binaries]
c = ['$ZHW']
ar = ['$ZIG', 'ar']
ranlib = ['$ZIG', 'ranlib']
nasm = '$NASM_BIN'
pkg-config = 'pkg-config'
[host_machine]
system = 'linux'
cpu_family = 'x86_64'
cpu = 'x86_64'
endian = 'little'
EOF
      if [ ! -f "$HVA/lib/libdrm.a" ]; then
        DRMBD="$S/drmbuild-hw"
        rm -rf "$DRMBD"
        log "libdrm-hw: meson setup"
        ( cd "$S/libdrm-$LIBDRM_VERSION" && $MESON setup "$DRMBD" \
            --cross-file "$S/cross-hw.txt" --prefix "$HVA" --libdir lib \
            --buildtype release -Ddefault_library=static -Dtests=false \
            -Dman-pages=disabled -Dvalgrind=disabled -Dintel=disabled \
            -Damdgpu=disabled -Dradeon=disabled -Dnouveau=disabled \
            -Dfreedreno=disabled -Dvc4=disabled -Dvmwgfx=disabled \
            -Detnaviv=disabled -Domap=disabled -Dexynos=disabled \
            -Dtegra=disabled -Dudev=false -Dcairo-tests=disabled ) \
            2>&1 | tee -a "$BUILD_LOG" || die "libdrm-hw meson failed"
        ( cd "$DRMBD" && ninja -j"$BUILD_JOBS" && $MESON install -C "$DRMBD" ) \
            2>&1 | tee -a "$BUILD_LOG" || die "libdrm-hw build failed"
      fi
      if [ ! -f "$HVA/lib/libva.so.2" ]; then
        VABD="$S/vabuild-hw"
        rm -rf "$VABD"
        export PKG_CONFIG_PATH="$HVA/lib/pkgconfig"
        log "libva-hw: meson setup"
        ( cd "$S/libva-$LIBVA_VERSION" && $MESON setup "$VABD" \
            --cross-file "$S/cross-hw.txt" --prefix "$HVA" --libdir lib \
            --buildtype release -Dwith_x11=no -Dwith_glx=no \
            -Dwith_wayland=no -Dwith_win32=no -Ddisable_drm=false \
            -Ddriverdir="$HVA/lib/dri" -Denable_docs=false ) \
            2>&1 | tee -a "$BUILD_LOG" || die "libva-hw meson failed"
        ( cd "$VABD" && ninja -j"$BUILD_JOBS" && $MESON install -C "$VABD" ) \
            2>&1 | tee -a "$BUILD_LOG" || die "libva-hw build failed"
      fi
      # static libdav1d (glibc) into the hw prefix, see section 2d
      dav1d_build "linux/amd64 ffmpeg-hw" "$S/cross-hw.txt" "$HP" "$S/dav1dbuild-hw"
      # ffmpeg-hw itself: same component set as the software build plus
      # h264_vaapi/h264_nvenc/h264_qsv and the hardware filters.  lavfi +
      # testsrc are added so the software path is exercisable without an
      # input file; wrapped_avframe decoding is what lavfi's testsrc emits.
      FHW="$S/ffbuild-hw"
      rm -rf "$FHW"
      mkdir -p "$FHW"
      log "ffmpeg-hw: configure"
      ( cd "$FHW" && export PKG_CONFIG_PATH="$HP/lib/pkgconfig:$HVA/lib/pkgconfig" \
          && "$S/ffmpeg-$FFMPEG_VERSION/configure" \
          --cc="$ZHW" --cxx="$S/zigcxx-hw" --ld="$ZHW" \
          --ar="$ZIG ar" --ranlib="$ZIG ranlib" --nm=nm \
          --target-os=linux --arch=x86_64 --enable-cross-compile \
          --disable-everything --disable-autodetect --disable-doc --disable-network \
          --disable-stripping --disable-ffplay --disable-ffprobe --enable-indev=lavfi \
          --enable-gpl --enable-static --disable-shared --enable-libx264 --enable-libdav1d \
          --enable-libvpl --enable-ffnvcodec --enable-nvenc --enable-vaapi \
          --enable-decoder=h264,hevc,aac,ac3,eac3,dca,mp3,mp2,vorbis,flac,vp9,libdav1d,mpeg4,opus,wrapped_avframe \
          --enable-encoder=libx264,aac,mjpeg,wrapped_avframe,pcm_s16le,h264_vaapi,h264_nvenc,h264_qsv \
          --enable-parser=aac,ac3,dca,h264,hevc,mpeg4video,mpegaudio,opus,vp9,av1 \
          --enable-demuxer=mov,matroska,mpegts,avi \
          --enable-muxer=mp4,mov,image2,null \
          --enable-protocol=file,pipe \
          --enable-filter=scale,aresample,format,setpts,fps,thumbnail,null,anull,hwupload,scale_vaapi,testsrc \
          --enable-bsf=h264_mp4toannexb,aac_adtstoasc \
          --extra-cflags="-I$HP/include -I$HVA/include" \
          --extra-ldflags="-L$HP/lib -lc++ -ldl -s" ) 2>&1 | tee -a "$BUILD_LOG" \
        || die "ffmpeg-hw configure failed"
      log "ffmpeg-hw: make"
      ( cd "$FHW" && make -j"$BUILD_JOBS" ARFLAGS=rcDs ) 2>&1 | tee -a "$BUILD_LOG" \
        || die "ffmpeg-hw build failed"
      cp "$FHW/ffmpeg" "$B/ffmpeg-hw"
    fi
  fi

  # --- record sizes + times -------------------------------------------------
  S_FF=$(stat -c %s "$B/ffmpeg$EXT")
  S_FP=$(stat -c %s "$B/ffprobe$EXT")
  S_QJ=$(stat -c %s "$B/qjs$EXT")
  printf '\n%-14s %-24s\n' target "$LABEL ($TRIPLE)" | tee -a "$RESULTS"
  printf '%-14s %10d %10d %10d\n' 'size-bytes' "$S_FF" "$S_FP" "$S_QJ" | tee -a "$RESULTS"
  if [ -f "$B/ffmpeg-hw" ]; then
    printf '%-14s %10d\n' 'size-bytes-hw' "$(stat -c %s "$B/ffmpeg-hw")" | tee -a "$RESULTS"
  fi
  printf '%-14s %10d %10d %10d\n' 'wall-seconds' "$x264_t" "$ff_t" "$qjs_t" | tee -a "$RESULTS"
  # linux binaries are stripped at link time (-s, zig lld --strip-all):
  # zig objcopy --strip-all is unimplemented in zig 0.17 and host strip
  # handles only the native ELF arch, so the strip flag rides the final
  # executable links instead. darwin/windows ship as-built.

  # --- identification -------------------------------------------------------
  file "$B/ffmpeg$EXT" "$B/ffprobe$EXT" "$B/qjs$EXT" | tee -a "$RESULTS"
done

echo >> "$RESULTS"
log "total wall time: $((SECONDS - tot0)) s (BUILD_JOBS=$BUILD_JOBS)"
printf 'total-wall-seconds %d\n' "$((SECONDS - tot0))" | tee -a "$RESULTS"

# ---- 4. runtime exercise (linux/amd64 only) --------------------------------
B="$O/linux_amd64/bin"
FF="$B/ffmpeg"
FP="$B/ffprobe"
{
  echo
  echo "==== runtime exercise: $($FF -version | head -1)"
} | tee -a "$RESULTS"

# lavfi is an indev so --disable-everything turns it off; it is checked to keep
# parity with the spike spec, and its lavfi *source filters* would be disabled
# by the minimal filter list anyway, so the sample-transcode path is the demo.
LAVFI=no
if "$FF" -hide_banner -devices 2>&1 | grep -qw lavfi; then LAVFI=yes; fi
echo "lavfi enabled: $LAVFI" | tee -a "$RESULTS"

echo "sample input (ffprobe):" | tee -a "$RESULTS"
"$FP" -v error -show_entries stream=codec_name,codec_type,profile -of csv=p=0 "$D/sample.mp4" | tee -a "$RESULTS"

echo "transcoding first 5 s to h264+aac+faststart:" | tee -a "$RESULTS"
if ! "$FF" -hide_banner -y -i "$D/sample.mp4" -t 5 \
  -c:v libx264 -preset veryfast -crf 30 -c:a aac -b:a 96k \
  -movflags +faststart "$B/test-out.mp4" 2>> "$BUILD_LOG"; then
  die "transcode failed for linux/amd64 (see $BUILD_LOG)"
fi
echo "transcode ok: $(stat -c %s "$B/test-out.mp4") bytes" | tee -a "$RESULTS"

echo "output (ffprobe):" | tee -a "$RESULTS"
"$FP" -v error -show_entries stream=codec_name,codec_type,profile -of csv=p=0 "$B/test-out.mp4" | tee -a "$RESULTS"

echo "faststart check (moov before mdat):" | tee -a "$RESULTS"
python3 - "$B/test-out.mp4" <<'PYEOF' | tee -a "$RESULTS"
import sys
d = open(sys.argv[1], 'rb').read()
moov, mdat = d.find(b'moov'), d.find(b'mdat')
print(f'moov@{moov} mdat@{mdat} -> {"FASTSTART ok" if 0 < moov < mdat else "moov trailing"}')
PYEOF

# decode-to-discard check: -f null needs the null muxer and its default codecs
# (wrapped_avframe for video, pcm_s16le for audio), enabled in the configure
# line above. -xerror makes any decode error a non-zero exit, so this also
# catches corrupt input, which plain decoding would only warn about.
echo "null muxer present:" | tee -a "$RESULTS"
"$FF" -hide_banner -muxers | tee -a "$RESULTS" | grep -qw null \
  || die "null muxer missing from linux/amd64 build (see $BUILD_LOG)"

echo "decode-to-discard (clean sample):" | tee -a "$RESULTS"
if ! "$FF" -v error -xerror -i "$D/sample.mp4" -map 0:v:0 -map 0:a:0 -f null -; then
  die "clean sample failed to decode to null for linux/amd64 (see $BUILD_LOG)"
fi
echo "decode-to-discard ok (exit 0)" | tee -a "$RESULTS"

echo "decode-to-discard (corrupt sample, 4 KB zeroed in mdat):" | tee -a "$RESULTS"
cp "$D/sample.mp4" "$B/test-corrupt.mp4"
python3 - "$B/test-corrupt.mp4" <<'PYEOF' | tee -a "$RESULTS"
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
def find_mdat(data):
    off = 0
    while off + 8 <= len(data):
        size = int.from_bytes(data[off:off+4], 'big')
        typ = data[off+4:off+8]
        hdr = 8
        if size == 1:
            size = int.from_bytes(data[off+8:off+16], 'big'); hdr = 16
        elif size == 0:
            size = len(data) - off
        if size < hdr:
            break
        if typ == b'mdat':
            return off, size, hdr
        # mdat sits at top level for this (non-fragmented) sample; skip past
        # other boxes (moov, ftyp, …) and stop at the first mdat.
        off += size
hit = find_mdat(d)
if not hit:
    print('corrupt-check: no mdat box found, aborting')
    sys.exit(2)
off, size, hdr = hit
payload = off + hdr
mid = payload + (size - hdr) // 2
core = d[mid:mid+4096]
if len(core) < 4096:
    print('corrupt-check: mdat payload shorter than 4096, aborting')
    sys.exit(2)
d[mid:mid+4096] = b'\0' * 4096
open(p, 'wb').write(d)
print(f'corrupt-check: zeroed 4096 bytes at mdat+{mid-payload} (payload {size-hdr} bytes)')
PYEOF
if "$FF" -v error -xerror -i "$B/test-corrupt.mp4" -map 0:v:0 -map 0:a:0 -f null - \
    2>> "$BUILD_LOG"; then
  die "corrupt sample decoded cleanly to null; -xerror expected to fail (see $BUILD_LOG)"
else
  rc=$?   # after `fi` the status would be the if-statement's (0), not ffmpeg's
fi
echo "decode-to-discard ok (corrupt clip rejected, exit $rc)" | tee -a "$RESULTS"

echo "==== ffmpeg-hw runtime exercise (linux/amd64)" | tee -a "$RESULTS"
HWFF="$O/linux_amd64/bin/ffmpeg-hw"
# This container has no system libva; point the loader at the link-stub
# libva we built (same SONAME the user's system libva provides).
if ! env LD_LIBRARY_PATH="$HVA/lib" "$HWFF" -version > /dev/null 2>> "$BUILD_LOG"; then
  die "ffmpeg-hw -version failed (see $BUILD_LOG)"
fi
echo "ffmpeg-hw -version ok (exit 0)" | tee -a "$RESULTS"
echo "ffmpeg-hw h264 encoders:" | tee -a "$RESULTS"
env LD_LIBRARY_PATH="$HVA/lib" "$HWFF" -hide_banner -encoders 2>&1 \
  | grep -E '^ V' | grep -E 'h264|libx264' | tee -a "$RESULTS"
echo "ffmpeg-hw software-path lavfi encode:" | tee -a "$RESULTS"
if ! env LD_LIBRARY_PATH="$HVA/lib" "$HWFF" -hide_banner -y \
    -f lavfi -i testsrc=size=320x240:rate=25 -t 1 \
    -c:v libx264 -pix_fmt yuv420p "$O/linux_amd64/bin/hw-test-out.mp4" 2>> "$BUILD_LOG"; then
  die "ffmpeg-hw lavfi encode failed (see $BUILD_LOG)"
fi
echo "ffmpeg-hw lavfi encode ok" | tee -a "$RESULTS"
echo "ffmpeg-hw ldd (LD_LIBRARY_PATH=built libva stub):" | tee -a "$RESULTS"
env LD_LIBRARY_PATH="$HVA/lib" ldd "$HWFF" | tee -a "$RESULTS"

# ---- 4b. kiwix-serve runtime exercise (linux/amd64, D4) --------------------
kiwix_exercise

# ---- 5. code-signature finding + dylib list (darwin binaries) --------------
# Zig does not ad-hoc sign the x86_64 binaries; scan the load commands for
# LC_CODE_SIGNATURE (0x1D).  In the same pass, list every LC_LOAD_DYLIB and
# fail if anything but the allowed system dylibs is linked (D4 contract:
# darwin kiwix-serve may only use /usr/lib/libSystem.B.dylib, libc++).
for KEY in darwin_amd64 darwin_arm64; do
  B="$O/$KEY/bin"
  for bin in ffmpeg ffprobe qjs kiwix-serve; do
    if python3 - "$B/$bin" "$bin" <<'PYEOF' | tee -a "$RESULTS"
import struct, sys
p = open(sys.argv[1], 'rb').read()
name = sys.argv[2]
magic = p[:4]
if magic not in (b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf'):
    print(f'{name}: not a thin Mach-O ({magic!r})')
    sys.exit(0)
le = magic == b'\xcf\xfa\xed\xfe'
e = '<' if le else '>'
ncmds = struct.unpack_from(e + 'I', p, 16)[0]
cur = 32
codesig = False
dylibs = []
for _ in range(ncmds):
    cmd, size = struct.unpack_from(e + 'II', p, cur)
    if cmd == 0x1D:                       # LC_CODE_SIGNATURE
        codesig = True
    elif cmd == 0xC:                      # LC_LOAD_DYLIB
        nameoff = struct.unpack_from(e + 'I', p, cur + 8)[0]
        dylibs.append(p[cur+nameoff:cur+size].split(b'\0')[0].decode())
    cur += size
print(f'{name}: LC_CODE_SIGNATURE present: {codesig}')
allowed = {'/usr/lib/libSystem.B.dylib', '/usr/lib/libc++.1.dylib',
           '/usr/lib/libc++abi.dylib', '/usr/lib/libobjc.A.dylib',
           '/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation',
           '/System/Library/Frameworks/CoreMedia.framework/Versions/A/CoreMedia',
           '/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices',
           '/System/Library/Frameworks/CoreVideo.framework/Versions/A/CoreVideo',
           '/System/Library/Frameworks/VideoToolbox.framework/Versions/A/VideoToolbox'}
bad = [d for d in dylibs if d not in allowed]
for d in sorted(set(dylibs)):
    print(f'{name}: dylib {d}' + ('   <-- NON-SYSTEM' if d in bad else ''))
if bad:
    print('FAIL: non-system dylibs: ' + ', '.join(bad))
    sys.exit(1)
PYEOF
    then
      :
    else
      die "non-system dylib found in $B/$bin (see $RESULTS)"
    fi
  done
done

# ---- 6. windows import check (kiwix-serve.exe, D4) -------------------------
kiwix_windows_import_check

# ---- 6b. windows ffmpeg.exe import table (hardware DLLs are LoadLibrary'd
# at run time, so the import table must stay system-only) -------------------
B="$O/windows_amd64/bin"
if python3 - "$B/ffmpeg.exe" <<'PYEOF' | tee -a "$RESULTS"
import struct, sys
p = open(sys.argv[1], 'rb').read()
assert p[:2] == b'MZ'
pe = struct.unpack_from('<I', p, 0x3C)[0]
assert p[pe:pe + 4] == b'PE\0\0'
opt = pe + 24
magic = struct.unpack_from('<H', p, opt)[0]
nsec = struct.unpack_from('<H', p, pe + 6)[0]
sec = opt + (240 if magic == 0x20B else 224)
import_rva = struct.unpack_from('<I', p, opt + (112 if magic == 0x20B else 96))[0]
def rva2off(rva):
    for i in range(nsec):
        vsz, vaddr, rsz, praw = struct.unpack_from('<IIII', p, sec + i * 40 + 8)
        if vaddr <= rva < vaddr + max(vsz, rsz):
            return rva - vaddr + praw
    return None
dlls = []
off = rva2off(import_rva)
while off:
    dll_rva = struct.unpack_from('<I', p, off)[0]
    if not dll_rva:
        break
    noff = rva2off(struct.unpack_from('<I', p, off + 12)[0])
    end = p.find(b'\0', noff)
    dlls.append(p[noff:end].decode())
    off += 40
sysdll = {'KERNEL32.dll', 'ADVAPI32.dll', 'SHELL32.dll', 'USER32.dll',
          'GDI32.dll', 'MSVCRT.dll', 'WS2_32.dll', 'bcrypt.dll', 'ole32.dll',
          'OLEAUT32.dll', 'IPHLPAPI.DLL', 'USERENV.dll', 'SECUR32.dll',
          'WINMM.dll', 'VERSION.dll', 'SHLWAPI.dll', 'UxTheme.dll',
          'DWMWAPI.dll', 'WTSAPI32.dll'}
print('ffmpeg.exe imports: ' + ', '.join(sorted(dlls)))
bad = [d for d in dlls
       if not (d.startswith('api-ms-win-crt') or d in sysdll)]
if bad:
    print('FAIL: non-system DLLs at load time: ' + ', '.join(bad))
    sys.exit(1)
print('windows ffmpeg.exe import check ok (system DLLs only at load time)')
PYEOF
then
  :
else
  die "non-system DLL in ffmpeg.exe import table (see $RESULTS)"
fi



log "done. results in $RESULTS"
