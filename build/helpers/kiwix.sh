#!/usr/bin/env bash
# kiwix-serve build support for the helpers spike (sourced by build.sh).
#
# D4 (DECISIONS.md 2026-10-27): ZIM libraries are served by kiwix-serve, built
# from source for all five kids-shelf targets in the same Zig pipeline as the
# ffmpeg/qjs helpers.  kiwix/kiwix-build (kiwixbuild/dependencies/*.py) is the
# reference for the dependency tree, versions, configure/meson options and the
# static-build patches; every download is pinned in versions.env and goes
# through build.sh's fetch().
#
# Tree (leaf -> root; everything static, tests/docs/examples/extra binaries off):
#   kiwix-tools 3.8.2  (kiwix-serve; meson, -Dstatic-linkage)
#   + docopt.cpp 0.6.3 (CLI parsing; meson via wrapdb patch + kiwix pkgconfig patch)
#   + libkiwix 14.2.1  (meson, -Dstatic-linkage)
#   |   + libzim 9.8.2 (meson, -Dwithout_writer -Dwith_xapian)
#   |   |   + xapian-core 1.4.23 (autotools; full-text index read support)
#   |   |   + icu4c 73.2 (autotools; host build first, --with-cross-build)
#   |   |   + zstd 1.5.7 (meson)
#   |   |   + xz/liblzma 5.2.6 (meson via wrapdb patch + kiwix install patch)
#   |   + pugixml 1.15 (meson via kiwix-build patch)
#   |   + libcurl 8.4.0 (autotools; minimal, no TLS)
#   |   + libmicrohttpd 0.9.76 (autotools; minimal)
#   |   + zlib 1.3.1 (meson via wrapdb patch)
#   |   + mustache.hpp (header-only; kiwix's build of kainjow/Mustache 4.1)
#
# Notes on target-specific choices (details in docs/spikes/helpers.md):
#   * xapian uuid: on linux-musl autotools falls back to reading
#     /proc/sys/kernel/random/uuid (USE_PROC_FOR_UUID, the code path autotools
#     itself provides for Android) so no libuuid build is needed; darwin finds
#     uuid/uuid.h + uuid_generate in libSystem; mingw uses the win32 API
#     (-lrpcrt4).  xapian-core 1.4.23 autotools cross-compiles to mingw, so
#     kiwix-build's openzim/xapian-meson fork (needed for native-MSVC
#     Windows) is not used here - its meson.build reads build_machine instead
#     of host_machine, which breaks any Linux-hosted cross build.
#   * ICU 73.2's configure does not emit pkgconfig files; kiwix-build writes
#     icu-i18n.pc manually and so do we (full ICU data, statically packaged;
#     the size ends up in helpers-results.txt).
#   * every artifact is gated on its installed library/binary, so reruns and
#     partial failures resume where they stopped.
set -euo pipefail

# Full ICU data statically packaged is ~32 MB on every target; ICU's mingw
# stubdata archive is ~12 KB.  The installed data archive must be larger than
# this bound: kiwix-serve linked against the stub ships without any ICU data.
KZ_ICU_MIN_BYTES=$((25 * 1024 * 1024))

# ---- host build tools (meson + ninja), installed once under $T -------------
kiwix_host_tools() {
  if [ ! -f "$T/meson/mesonbuild/mesonmain.py" ]; then
    fetch meson "$MESON_URL" "$MESON_SHA256" "$D/meson-$MESON_VERSION.tar.gz"
    tar xzf "$D/meson-$MESON_VERSION.tar.gz" -C "$T"
    mv "$T/meson-$MESON_VERSION" "$T/meson"
  fi
  if [ ! -x "$T/ninja/ninja" ]; then
    fetch ninja "$NINJA_URL" "$NINJA_SHA256" "$D/ninja-linux.zip"
    mkdir -p "$T/ninja"
    unzip -qo "$D/ninja-linux.zip" -d "$T/ninja"
    chmod +x "$T/ninja/ninja"
  fi
  export PATH="$T/ninja:$PATH"
  export PYTHONPATH="$T/meson"
  log "kiwix host tools: meson $(python3 -m mesonbuild.mesonmain --version), ninja $($T/ninja/ninja --version)"
}

# ---- sources: downloads (verified), extraction, wraps/patches, host ICU ----
kiwix_sources() {
  # sources ----------------------------------------------------------------
  fetch zlib "$ZLIB_URL" "$ZLIB_SHA256" "$D/zlib-$ZLIB_VERSION.tar.gz"
  fetch xz "$XZ_URL" "$XZ_SHA256" "$D/xz-$XZ_VERSION.tar.gz"
  fetch zstd "$ZSTD_URL" "$ZSTD_SHA256" "$D/zstd-$ZSTD_VERSION.tar.gz"
  fetch pugixml "$PUGIXML_URL" "$PUGIXML_SHA256" "$D/pugixml-$PUGIXML_VERSION.tar.gz"
  fetch docopt "$DOCOPT_URL" "$DOCOPT_SHA256" "$D/docopt-v$DOCOPT_VERSION.tar.gz"
  [ -d "$S/icu" ] && log "have sources icu" \
    || tar xzf "$D/icu4c-${ICU_VERSION//./_}-src.tgz" -C "$S"
  fetch curl "$CURL_URL" "$CURL_SHA256" "$D/curl-$CURL_VERSION.tar.xz"
  fetch microhttpd "$LIBMICROHTTPD_URL" "$LIBMICROHTTPD_SHA256" "$D/libmicrohttpd-$LIBMICROHTTPD_VERSION.tar.gz"
  fetch xapian "$XAPIAN_URL" "$XAPIAN_SHA256" "$D/xapian-core-$XAPIAN_VERSION.tar.xz"
  fetch mustache "$MUSTACHE_URL" "$MUSTACHE_SHA256" "$D/mustache-$MUSTACHE_VERSION.tar.gz"
  fetch libzim "$LIBZIM_URL" "$LIBZIM_SHA256" "$D/libzim-$LIBZIM_VERSION.tar.gz"
  fetch libkiwix "$LIBKIWIX_URL" "$LIBKIWIX_SHA256" "$D/libkiwix-$LIBKIWIX_VERSION.tar.gz"
  fetch kiwix-tools "$KIWIX_TOOLS_URL" "$KIWIX_TOOLS_SHA256" "$D/kiwix-tools-$KIWIX_TOOLS_VERSION.tar.gz"
  # meson/lzma/docopt wrapdb patches and kiwix-build static-build patches
  fetch zlib-wrap "$ZLIB_WRAP_URL" "$ZLIB_WRAP_SHA256" "$D/zlib_1.3.1-1_patch.zip"
  fetch lzma-wrap "$LZMA_WRAP_URL" "$LZMA_WRAP_SHA256" "$D/liblzma_5.2.6-3_patch.zip"
  fetch docopt-wrap "$DOCOPT_WRAP_URL" "$DOCOPT_WRAP_SHA256" "$D/docopt_0.6.3-3_patch.zip"
  fetch kiwix-lzma-patch "$KIWIX_LZMA_PATCH_URL" "$KIWIX_LZMA_PATCH_SHA256" "$D/kiwix-lzma_meson_install.patch"
  fetch kiwix-docopt-patch "$KIWIX_DOCOPT_PATCH_URL" "$KIWIX_DOCOPT_PATCH_SHA256" "$D/kiwix-docopt_pkgconfig.patch"
  fetch kiwix-pugixml-patch "$KIWIX_PUGIXML_PATCH_URL" "$KIWIX_PUGIXML_PATCH_SHA256" "$D/kiwix-pugixml_meson.patch"
  fetch kiwix-icu-patch "$KIWIX_ICU_PATCH_URL" "$KIWIX_ICU_PATCH_SHA256" "$D/kiwix-icu_mingw_static.patch"

  # extraction (idempotent) -------------------------------------------------
  [ -d "$S/zlib-$ZLIB_VERSION" ] && { log "have sources zlib"; \
    } || { tar xzf "$D/zlib-$ZLIB_VERSION.tar.gz" -C "$S"; }
  [ -d "$S/xz-$XZ_VERSION" ] && log "have sources xz" \
    || tar xzf "$D/xz-$XZ_VERSION.tar.gz" -C "$S"
  [ -d "$S/zstd-$ZSTD_VERSION" ] && log "have sources zstd" \
    || tar xzf "$D/zstd-$ZSTD_VERSION.tar.gz" -C "$S"
  [ -d "$S/pugixml-$PUGIXML_VERSION" ] && log "have sources pugixml" \
    || tar xzf "$D/pugixml-$PUGIXML_VERSION.tar.gz" -C "$S"
  [ -d "$S/docopt.cpp-$DOCOPT_VERSION" ] && log "have sources docopt" \
    || tar xzf "$D/docopt-v$DOCOPT_VERSION.tar.gz" -C "$S"
  [ -d "$S/icu" ] && log "have sources icu" \
    || tar xzf "$D/icu4c-${ICU_VERSION//./_}-src.tgz" -C "$S"
  [ -d "$S/curl-$CURL_VERSION" ] && log "have sources curl" \
    || tar xJf "$D/curl-$CURL_VERSION.tar.xz" -C "$S"

  # curl's darwin configure adds -framework CoreFoundation/CoreServices/
  # SystemConfiguration (+ Security with Secure Transport) to every link; the
  # framework-using code paths compile out without SDK headers (zig's darwin
  # stubs are libSystem-only), so the flags only break cross-build conftests
  # and the final links.  Strip them from curl's generated configure.
  if grep -q 'LDFLAGS="$LDFLAGS -framework CoreFoundation' \
      "$S/curl-$CURL_VERSION/configure"; then
    sed -i 's|LDFLAGS="$LDFLAGS -framework CoreFoundation -framework CoreServices -framework SystemConfiguration"|LDFLAGS="$LDFLAGS"|;
            s|LDFLAGS="$LDFLAGS -framework CoreFoundation -framework CoreServices -framework Security"|LDFLAGS="$LDFLAGS"|' \
      "$S/curl-$CURL_VERSION/configure"
    log "patched curl configure: dropped darwin -framework LDFLAGS"
  fi

  # windows: curl's configure probe for HAVE_IOCTLSOCKET_FIONBIO links the
  # conftest without -lws2_32, which a zig cross configure never supplies, so
  # the define stays off and lib/nonblock.c hits its #error; HAVE_IOCTLSOCKET
  # *is* detected, so use it as the fallback windows non-blocking path.
  if grep -q '#  error "no non-blocking method was found/used/set"' \
      "$S/curl-$CURL_VERSION/lib/nonblock.c" \
      && ! grep -q 'defined(HAVE_IOCTLSOCKET)' "$S/curl-$CURL_VERSION/lib/nonblock.c"; then
    sed -i 's@#elif defined(HAVE_IOCTLSOCKET_FIONBIO)@#elif defined(HAVE_IOCTLSOCKET_FIONBIO) || defined(HAVE_IOCTLSOCKET)@' \
      "$S/curl-$CURL_VERSION/lib/nonblock.c"
    log "patched curl nonblock.c: HAVE_IOCTLSOCKET covers the windows path"
  fi

  # darwin: macos.c's IPv6-NAT64 proxy pre-fetch (CURL_MACOS_CALL_COPYPROXIES)
  # includes SystemConfiguration/CoreFoundation headers zig's SDK stubs do not
  # ship; the SCDynamicStoreCopyProxies call is a no-op for a local server.
  if grep -q '^#    define CURL_MACOS_CALL_COPYPROXIES 1$' \
      "$S/curl-$CURL_VERSION/lib/curl_setup.h"; then
    sed -i 's|^#    define CURL_MACOS_CALL_COPYPROXIES 1$|/* CURL_MACOS_CALL_COPYPROXIES disabled: zig darwin stubs lack the\n   SystemConfiguration/CoreFoundation headers it needs */|' \
      "$S/curl-$CURL_VERSION/lib/curl_setup.h"
    log "patched curl curl_setup.h: disabled CURL_MACOS_CALL_COPYPROXIES"
  fi
  [ -d "$S/libmicrohttpd-$LIBMICROHTTPD_VERSION" ] && log "have sources microhttpd" \
    || tar xzf "$D/libmicrohttpd-$LIBMICROHTTPD_VERSION.tar.gz" -C "$S"
  [ -d "$S/xapian-core-$XAPIAN_VERSION" ] && log "have sources xapian" \
    || tar xJf "$D/xapian-core-$XAPIAN_VERSION.tar.xz" -C "$S"
  [ -d "$S/Mustache-$MUSTACHE_VERSION" ] && log "have sources mustache" \
    || tar xzf "$D/mustache-$MUSTACHE_VERSION.tar.gz" -C "$S"
  [ -d "$S/libzim-$LIBZIM_VERSION" ] && log "have sources libzim" \
    || tar xzf "$D/libzim-$LIBZIM_VERSION.tar.gz" -C "$S"
  [ -d "$S/libkiwix-$LIBKIWIX_VERSION" ] && log "have sources libkiwix" \
    || tar xzf "$D/libkiwix-$LIBKIWIX_VERSION.tar.gz" -C "$S"
  [ -d "$S/kiwix-tools-$KIWIX_TOOLS_VERSION" ] && log "have sources kiwix-tools" \
    || tar xzf "$D/kiwix-tools-$KIWIX_TOOLS_VERSION.tar.gz" -C "$S"

  # meson build files upstream lacks: wrapdb patches (extract over the tree) --
  [ -f "$S/zlib-$ZLIB_VERSION/meson.build" ] && log "have zlib meson wrap" \
    || unzip -qo "$D/zlib_1.3.1-1_patch.zip" -d "$S"
  [ -f "$S/xz-$XZ_VERSION/meson.build" ] && log "have lzma meson wrap" \
    || unzip -qo "$D/liblzma_5.2.6-3_patch.zip" -d "$S"
  [ -f "$S/docopt.cpp-$DOCOPT_VERSION/meson.build" ] && log "have docopt meson wrap" \
    || unzip -qo "$D/docopt_0.6.3-3_patch.zip" -d "$S"

  # kiwix-build patches ------------------------------------------------------
  # lzma: wrapdb's liblzma build defines the library but installs nothing;
  # kiwix-build's patch adds install_headers + liblzma.pc generation.
  if ! grep -q 'pkg.generate(liblzma' "$S/xz-$XZ_VERSION/src/liblzma/meson.build"; then
    ( cd "$S/xz-$XZ_VERSION" && patch --batch -p1 < "$D/kiwix-lzma_meson_install.patch" ) \
      || die "cannot apply kiwix lzma install patch"
  fi
  # docopt: wrapdb's meson.build neither installs nor emits docopt.pc, which
  # kiwix-tools needs (dependency('docopt')); kiwix-build's patch adds both.
  if ! grep -q 'pkg_mod.generate' "$S/docopt.cpp-$DOCOPT_VERSION/meson.build"; then
    ( cd "$S/docopt.cpp-$DOCOPT_VERSION" && patch --batch -p1 < "$D/kiwix-docopt_pkgconfig.patch" ) \
      || die "cannot apply kiwix docopt pkgconfig patch"
  fi
  # pugixml: upstream has no meson build; kiwix-build ships a tiny one
  # (static lib + pugixml.pc + headers).
  if [ ! -f "$S/pugixml-$PUGIXML_VERSION/meson.build" ]; then
    ( cd "$S" && patch --batch -p0 < "$D/kiwix-pugixml_meson.patch" ) \
      || die "cannot apply kiwix pugixml meson patch"
  fi
  # icu: on mingw, icu names the static libs libsicu*.a unless the STATIC_PREFIX
  # is dropped; kiwix-build ships mh-mingw/mh-mingw64 tweaks for that.
  if ! grep -q 'STATIC_PREFIX =' "$S/icu/source/config/mh-mingw64"; then
    ( cd "$S/icu" && patch --batch -p1 < "$D/kiwix-icu_mingw_static.patch" ) \
      || die "cannot apply kiwix icu mingw patch"
  fi

  # host ICU (build reference for the cross builds' --with-cross-build) -----
  if [ ! -f "$S/icu/.host-built" ]; then
    log "icu: host configure (reference for cross builds)"
    ( cd "$S/icu/source" && CFLAGS=-O2 CXXFLAGS=-O2 ./configure \
        --prefix="$T/icu-host-prefix" \
        --disable-samples --disable-tests --disable-extras --disable-dyload \
        --enable-rpath --disable-icuio --disable-layoutex ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "icu host configure failed (see $BUILD_LOG)"
    log "icu: host make ($(nproc) jobs)"
    ( cd "$S/icu/source" && make -j"$BUILD_JOBS" && make install ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "icu host build failed (see $BUILD_LOG)"
    touch "$S/icu/.host-built"
  else
    log "have icu host build"
  fi
}

# ---- ICU data archive helpers ----------------------------------------------
# The full ICU data archive is ~32 MB on every target.  On mingw the build
# does produce that archive, but ICU's data install (data/Makefile honours
# MINGW_MOVEDLLSTOBINDIR=YES) drops it into $P/bin/icudt.a while stubdata
# installs a ~12 KB placeholder as $P/lib/libicudt.a -- exactly the name
# -licudt (icu-uc.pc) resolves, so kiwix-serve.exe links the placeholder and
# ships without ICU data.  The three helpers below share one 25 MB bound so
# the staleness probe, the rebuild trigger and the hard check agree.

# True (exit 0) when the installed windows ICU data archive is the stub, i.e.
# the previous windows build linked kiwix-serve without ICU data.  Non-windows
# targets install the data archive directly as libicudata.a and are never
# stale; "no archive yet" is not stale either (that is a fresh build).
kiwix_windows_stale() {
  [ "$KEY" = windows_amd64 ] || return 1
  [ -f "$P/lib/libicudt.a" ] || return 1
  [ "$(stat -c %s "$P/lib/libicudt.a")" -le "$KZ_ICU_MIN_BYTES" ]
}

# Sets KZ_ICU to the size (bytes) of the installed ICU data archive -- the
# file the link resolves: libicudata.a on unix, libicudt.a on mingw -- or 0
# when neither is installed.
kz_icu_data_size() {
  if [ -f "$P/lib/libicudata.a" ]; then
    KZ_ICU=$(stat -c %s "$P/lib/libicudata.a")
  elif [ -f "$P/lib/libicudt.a" ]; then
    KZ_ICU=$(stat -c %s "$P/lib/libicudt.a")
  else
    KZ_ICU=0
  fi
}

# Hard gate: the installed ICU data archive must be larger than 25 MB on
# every target and every rerun (including skipped ones), or the target dies.
kz_icu_data_check() {
  kz_icu_data_size
  if [ "$KZ_ICU" -le "$KZ_ICU_MIN_BYTES" ]; then
    die "ICU data archive for $LABEL is $KZ_ICU bytes, not larger than 25 MB (see $BUILD_LOG)"
  fi
  log "kiwix: icu-data-check $LABEL $KZ_ICU bytes (need > $KZ_ICU_MIN_BYTES)"
}

# ---- per-target: build the whole kiwix stack into $P, ship kiwix-serve -----
# Caller (build.sh target loop) provides: $LABEL $KEY $TRIPLE $ARCH $XHOST
# $FFOS $P $B $EXT and the zig wrappers $WRAP/$ZAR/$ZRAN.
kiwix_build_target() {
  # windows: a stale (stub) ICU data archive means the previous windows run
  # linked kiwix-serve without ICU data; drop every artifact that consumes
  # ICU (data archive, i18n/uc, libzim, libkiwix, kiwix-serve) so this run
  # rebuilds them.  Other targets keep their built outputs.
  if kiwix_windows_stale; then
    log "windows ICU data archive stale ($(stat -c %s "$P/lib/libicudt.a") bytes <= 25 MB); rebuilding icu/libzim/libkiwix/kiwix-serve for $LABEL"
    rm -f "$P"/lib/libicu* "$P"/lib/pkgconfig/icu-*.pc \
          "$P/lib/libzim.a" "$P/lib/libkiwix.a" "$B/kiwix-serve$EXT"
  fi
  if [ -x "$B/kiwix-serve$EXT" ] && { [ "$IS_LINUX" = 0 ] \
      || kz_stripped "$B/kiwix-serve$EXT"; }; then
    # skip, but still record the size row so helpers-results.txt is complete
    log "kiwix-serve for $LABEL already built, skipping"
    kz_icu_data_check
    printf '\n%-14s %-24s\n' kiwix-target "$LABEL ($TRIPLE)" >> "$RESULTS"
    printf '%-14s %10d\n' 'kiwix-serve-bytes' "$(stat -c %s "$B/kiwix-serve$EXT")" >> "$RESULTS"
    printf '%-14s %10d\n' 'kiwix-wall-seconds' 0 >> "$RESULTS"
    printf '%-14s %10d\n' 'icu-data-bytes' "$KZ_ICU" >> "$RESULTS"
    file "$B/kiwix-serve$EXT" >> "$RESULTS"
    return 0
  fi
  log "======== kiwix stack: $LABEL ($TRIPLE) ========"
  local MK="$S/kw-$KEY"
  local KZ_T=0 t0
  mkdir -p "$MK" "$P/lib/pkgconfig"

  # zig c++ wrapper + windres shim (build.sh makes the cc/ar/ranlib ones).
  local CXXW="$S/zigcxx-$KEY"
  local WRW="$S/zigwindres-$KEY"
  local CF="$S/meson-$KEY.ini"
  if [ ! -x "$CXXW" ]; then
    { printf '#!/bin/sh\nexec "%s" c++ -target %s "$@"\n' "$ZIG" "$TRIPLE" > "$CXXW"; chmod +x "$CXXW"; }
  fi
  if [ ! -x "$WRW" ]; then
    { printf '#!/bin/sh\nexec "%s" rc -target %s "$@"\n' "$ZIG" "$TRIPLE" > "$WRW"; chmod +x "$WRW"; }
  fi
  # curl's configure hard-requires a `windres` on PATH for windows builds
  # (AC_PATH_PROG does not honour an absolute WINDRES env value); keep a
  # per-target dir on PATH with a shim so the tool name resolves.
  mkdir -p "$S/kwpath-$KEY"
  cat > "$S/kwpath-$KEY/windres" <<EOF
#!/bin/sh
# zig rc is a strict rc.exe emulation: libtool's -g probe flag and -o output
# spelling are unsupported, so translate them (zig rc takes INPUT [OUTPUT]).
args=""
out=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -g) shift ;;
    -o) shift; out="\$1"; shift ;;
    *) args="\$args \$1"; shift ;;
  esac
done
exec "$ZIG" rc \$args \$out
EOF
  chmod +x "$S/kwpath-$KEY/windres"
  export PATH="$S/kwpath-$KEY:$PATH"

  # isolated pkg-config view: only this target's prefix is ever searched.
  export PKG_CONFIG_LIBDIR="$P/lib/pkgconfig"
  export PKG_CONFIG_PATH=

  # darwin-only: zig's libc stubs lack a couple of private/system headers that
  # ICU and xapian include on Apple platforms; supply minimal definitions in a
  # build-tree include dir (never installed).  See helpers.md for details.
  local EXTRA_INC=""
  local MEXTRA=""
  if [ "$FFOS" = darwin ]; then
    EXTRA_INC="$MK/inc"
    MEXTRA=", '-I$MK/inc'"
    mkdir -p "$EXTRA_INC/sys"
    cat > "$EXTRA_INC/tzfile.h" <<'TZEOF'
#ifndef _TZFILE_H_
#define _TZFILE_H_
/* minimal tzfile.h for darwin cross builds: zig's libc stubs lack this private
   Apple header; ICU (common/putil.cpp) only reads the TZDIR macro from it.
   Definitions follow RFC 8536 (tzif) as in the BSD/Apple header. */
#define TZDIR      "/usr/share/zoneinfo"
#define TZDEFAULT  "/etc/localtime"
#define TZDEFRULES "localtime"
#define TZ_MAGIC   "TZif"
struct tzhead {
    char tzh_magic[4];
    char tzh_version[1];
    char tzh_reserved[15];
    char tzh_ttisutcnt[4];
    char tzh_ttisstdcnt[4];
    char tzh_leapcnt[4];
    char tzh_timecnt[4];
    char tzh_typecnt[4];
    char tzh_charcnt[4];
};
#endif
TZEOF
    cat > "$EXTRA_INC/sys/timeb.h" <<'TBEOF'
#ifndef _SYS_TIMEB_H_
#define _SYS_TIMEB_H_
/* minimal sys/timeb.h for darwin cross builds: zig's libc stubs lack it;
   xapian's common/realtime.h (and others) include it for ftime(), which
   libSystem provides at runtime.  Shape matches the Apple header. */
#include <time.h>
struct timeb {
    time_t        time;
    unsigned short millitm;
    short         timezone;
    short         dstflag;
};
int ftime(struct timeb *);
#endif
TBEOF
  fi
  local SYS CPUF LSTRIP
  case "$LABEL" in
    linux/*)   SYS=linux;   CPUF="$ARCH" ;;
    windows/*) SYS=windows; CPUF="$ARCH" ;;
    darwin/*)  SYS=darwin;  CPUF="$ARCH" ;;
    *) die "kiwix: unknown target $LABEL" ;;
  esac
  # header-only deps such as mustache.hpp have no .pc, so the prefix include
  # dir must be on the default search path; link args mirror the -L autotools
  # builds already pass.  darwin adds $MK/inc (tzfile.h / sys/timeb.h stubs).
  # linux-only `, '-s'`: every meson project in this stack builds static
  # archives only (default_library=static, tests/examples disabled), so the
  # link args reach just the final executables -- kiwix-serve (and the
  # unshipped kiwix-manage/kiwix-search) get stripped by zig's lld at link.
  LSTRIP=""
  if [ "$IS_LINUX" = 1 ]; then
    LSTRIP=", '-s'"
  fi
  cat > "$CF" <<EOF
[binaries]
c = '$WRAP'
cpp = '$CXXW'
ar = '$ZAR'
ranlib = '$ZRAN'
windres = '$WRW'
pkg-config = 'pkg-config'

[host_machine]
system = '$SYS'
cpu_family = '$CPUF'
cpu = '$CPUF'
endian = 'little'

[built-in options]
c_args = ['-I$P/include'$MEXTRA]
cpp_args = ['-I$P/include'$MEXTRA]
c_link_args = ['-L$P/lib'$LSTRIP]
cpp_link_args = ['-L$P/lib'$LSTRIP]
EOF

  # meson helper: $1=marker lib basename $2=source dir $3=build dir, rest args
  kz_meson() {
    local marker="$1" src="$2" bd="$3"; shift 3
    if [ -f "$P/lib/$marker" ]; then log "have $marker ($LABEL)"; return 0; fi
    t0=$SECONDS
    rm -rf "$MK/$bd"
    log "  $bd: meson setup ($LABEL)"
    ( cd "$src" && python3 -m mesonbuild.mesonmain setup "$MK/$bd" "$src" \
        --cross-file "$CF" --prefix "$P" --libdir lib -Ddefault_library=static \
        --buildtype release "$@" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "$bd meson failed for $LABEL (see $BUILD_LOG)"
    log "  $bd: build + install ($LABEL)"
    ( cd "$MK/$bd" && ninja -j"$BUILD_JOBS" && ninja install ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "$bd build failed for $LABEL (see $BUILD_LOG)"
    KZ_T=$((KZ_T + SECONDS - t0))
  }
  # autotools helper: $1=marker lib $2=dep name $3=build dir $4=source dir, rest args
  kz_auto() {
    local marker="$1" name="$2" bd="$3" src="$4"; shift 4
    if [ -f "$P/lib/$marker" ]; then log "have $marker ($LABEL)"; return 0; fi
    t0=$SECONDS
    rm -rf "$MK/$bd"; mkdir -p "$MK/$bd"
    log "  $bd: configure ($LABEL)"
    ( cd "$MK/$bd" \
        && CC="$WRAP" CXX="$CXXW" AR="$ZIG ar" RANLIB="$ZIG ranlib" \
           WINDRES="$WRW" \
           CFLAGS=-O2 CXXFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC} -O2" \
           CPPFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC}" \
           LDFLAGS="-L$P/lib" \
           "$src/configure" --host="$XHOST" --prefix="$P" \
           --disable-shared --enable-static "$@" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "$bd configure failed for $LABEL (see $BUILD_LOG)"
    log "  $bd: make ($LABEL)"
    ( cd "$MK/$bd" && make -j"$BUILD_JOBS" && make install ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "$bd build failed for $LABEL (see $BUILD_LOG)"
    KZ_T=$((KZ_T + SECONDS - t0))
  }

  # --- zlib 1.3.1 (meson via wrapdb) ---------------------------------------
  kz_meson libz.a "$S/zlib-$ZLIB_VERSION" zlib -Dtests=disabled

  # --- xz/liblzma 5.2.6 (meson via wrapdb + kiwix install patch) -----------
  kz_meson liblzma.a "$S/xz-$XZ_VERSION" lzma

  # --- zstd 1.5.7 (official meson build; lib only) --------------------------
  kz_meson libzstd.a "$S/zstd-$ZSTD_VERSION/build/meson" zstd \
    -Dbin_programs=false -Dbin_tests=false -Dbin_contrib=false \
    -Dzlib=disabled -Dlzma=disabled -Dlz4=disabled

  # --- pugixml 1.15 (meson via kiwix-build patch) ---------------------------
  kz_meson libpugixml.a "$S/pugixml-$PUGIXML_VERSION" pugixml

  # --- docopt.cpp 0.6.3 (meson via wrapdb + kiwix pkgconfig patch) ----------
  # kiwix-build additionally swaps std::regex out on Windows (MSVC); with
  # zig's libc++ std::regex is fine, so the boostregex patch is not applied.
  kz_meson libdocopt.a "$S/docopt.cpp-$DOCOPT_VERSION" docopt

  # --- mustache.hpp (header-only) -------------------------------------------
  if [ ! -f "$P/include/mustache.hpp" ]; then
    mkdir -p "$P/include"
    cp "$S/Mustache-$MUSTACHE_VERSION/mustache.hpp" "$P/include/mustache.hpp"
    log "installed mustache.hpp ($LABEL)"
  fi

  # --- icu4c 73.2 (autotools; needs the host build, --with-cross-build) -----
  # Full ICU data, statically packaged (--with-data-packaging=static); its
  # size lands in helpers-results.txt below.
  if { [ -f "$P/lib/libicui18n.a" ] || [ -f "$P/lib/libicuin.a" ]; } \
      && [ -f "$P/lib/pkgconfig/icu-i18n.pc" ]; then
    log "have icu ($LABEL)"
  else
    [ -x "$S/icu/source/bin/icupkg" ] \
      || die "icu host build missing for $LABEL (kiwix_sources builds it)"
    t0=$SECONDS
    rm -rf "$S/icu-$KEY"
    # clear any ICU leftovers from an interrupted previous build (a partial
    # install leaves shared libs behind, which would hijack the static link)
    rm -f "$P"/lib/libicu* "$P"/lib/pkgconfig/icu-*.pc
    cp -a "$S/icu" "$S/icu-$KEY"
    log "  icu: configure ($LABEL)"
    ( cd "$S/icu-$KEY/source" \
        && CC="$WRAP" CXX="$CXXW" AR="$ZIG ar" RANLIB="$ZIG ranlib" \
           CFLAGS=-O2 CXXFLAGS=-O2 CPPFLAGS="${EXTRA_INC:+-I$EXTRA_INC}" \
           ./configure --host="$XHOST" --prefix="$P" \
           --with-cross-build="$S/icu/source" \
           --with-data-packaging=static \
           --disable-shared --enable-static \
           --disable-tools \
           --disable-samples --disable-tests --disable-extras --disable-dyload \
           --disable-icuio --disable-layoutex ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "icu configure failed for $LABEL (see $BUILD_LOG)"
    log "  icu: make ($LABEL)"
    ( cd "$S/icu-$KEY/source" && make -j"$BUILD_JOBS" \
        && make install ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "icu build failed for $LABEL (see $BUILD_LOG)"
    # ICU 73.2 ships its own pkgconfig output (icu-i18n.pc / icu-uc.pc) whose
    # Libs.private pulls in the uc + data archives for static linking; that
    # covers libzim/libkiwix's dependency('icu-i18n', static:true).
    # mingw: the full data archive is built (pkgdata -m static), but
    # data/Makefile's install honours MINGW_MOVEDLLSTOBINDIR=YES and drops it
    # into $P/bin/icudt.a while stubdata installs a ~12 KB placeholder as
    # $P/lib/libicudt.a -- exactly the name -licudt (icu-uc.pc) resolves, so
    # the link would consume the placeholder and kiwix-serve.exe would ship
    # without ICU data.  Put the real archive under the link name.
    if [ "$KEY" = windows_amd64 ] && [ -f "$P/bin/icudt.a" ]; then
      cp "$P/bin/icudt.a" "$P/lib/libicudt.a"
      log "installed full ICU data archive as lib/libicudt.a ($LABEL)"
    fi
    KZ_T=$((KZ_T + SECONDS - t0))
  fi
  # hard gate: the installed data archive must be the full one on every
  # target (and, via the skip branch above, on every rerun): die if <= 25 MB
  kz_icu_data_check


  # --- libcurl 8.4.0 (autotools; minimal, no TLS; library only) -------------
  # kiwix-build feeds curl zlib + disables the same feature set; TLS is off
  # because kiwix-serve is a local server (plain HTTP to loopback clients).
  # --without-secure-transport is required on darwin cross builds: curl's
  # configure auto-enables Secure Transport when cross_compiling (the
  # /System/Library SDK probe is skipped), which would add
  # -framework CoreFoundation/CoreServices/Security to darwin links (zig ships
  # no such frameworks) and compile in Security.framework code.
  if [ -f "$P/lib/libcurl.a" ]; then
    log "have libcurl.a ($LABEL)"
  else
    t0=$SECONDS
    rm -rf "$MK/curl"; mkdir -p "$MK/curl"
    log "  curl: configure ($LABEL)"
    ( cd "$MK/curl" \
        && CC="$WRAP" CXX="$CXXW" AR="$ZIG ar" RANLIB="$ZIG ranlib" \
           WINDRES="$WRW" \
           CFLAGS=-O2 CXXFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC} -O2" \
           CPPFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC}" \
           LDFLAGS="-L$P/lib" \
           "$S/curl-$CURL_VERSION/configure" --host="$XHOST" --prefix="$P" \
           --disable-shared --enable-static \
           --with-zlib="$P" --without-ssl --without-secure-transport \
           --without-libidn2 --without-brotli \
           --without-zstd --without-nghttp2 --without-nghttp3 --without-ngtcp2 \
           --without-libpsl --without-libssh2 --without-librtmp \
           --without-libgsasl --without-gssapi --disable-ldap --disable-ldaps \
           --disable-rtsp --disable-dict --disable-telnet --disable-tftp \
           --disable-pop3 --disable-imap --disable-smb --disable-smtp \
           --disable-gopher --disable-ftp --disable-file --disable-rtmp \
           --disable-docs --disable-manual --disable-libcurl-option \
           --disable-versioned-symbols --disable-ares --disable-alt-svc ) 2>&1 \
        | tee -a "$BUILD_LOG" \
      || die "curl configure failed for $LABEL (see $BUILD_LOG)"
    log "  curl: make libcurl.la ($LABEL)"
    # Library only: the src/ curl tool has no autotools off-switch and its
    # mingw object naming breaks cross builds (kiwix-build's meson build
    # disables the tool too); install manually like xapian.
    ( cd "$MK/curl/lib" && make -j"$BUILD_JOBS" libcurl.la ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "curl build failed for $LABEL (see $BUILD_LOG)"
    cp "$MK/curl/lib/.libs/libcurl.a" "$P/lib/libcurl.a"
    mkdir -p "$P/include/curl"
    cp "$S/curl-$CURL_VERSION/include/curl/"*.h "$P/include/curl/"
    cp "$MK/curl/libcurl.pc" "$P/lib/pkgconfig/libcurl.pc"
    KZ_T=$((KZ_T + SECONDS - t0))
  fi

  # --- libmicrohttpd 0.9.76 (autotools; minimal) ----------------------------
  kz_auto libmicrohttpd.a microhttpd mhd "$S/libmicrohttpd-$LIBMICROHTTPD_VERSION" \
    --disable-doc --disable-examples --disable-tests --disable-benchmark \
    --disable-curl --disable-expat --disable-gnutls --disable-gcrypt \
    --disable-openssl --disable-nls

  # --- xapian-core 1.4.23 (autotools; used by libzim's full-text search) ----
  # uuid source per target: win32 API (-lrpcrt4) on mingw, system libSystem on
  # darwin, /proc/sys/kernel/random/uuid on linux-musl (autotools' own
  # USE_PROC_FOR_UUID fallback), so no libuuid build is required.
  if [ -f "$P/lib/libxapian.a" ]; then
    log "have libxapian.a ($LABEL)"
  else
    t0=$SECONDS
    rm -rf "$MK/xapian"; mkdir -p "$MK/xapian"
    log "  xapian: configure ($LABEL)"
    ( cd "$MK/xapian" \
        && CC="$WRAP" CXX="$CXXW" AR="$ZIG ar" RANLIB="$ZIG ranlib" \
           CFLAGS=-O2 CXXFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC} -O2" \
           CPPFLAGS="-I$P/include${EXTRA_INC:+ -I$EXTRA_INC}" \
           LDFLAGS="-L$P/lib" \
           "$S/xapian-core-$XAPIAN_VERSION/configure" --host="$XHOST" --prefix="$P" \
           --disable-shared --enable-static --disable-sse \
           --disable-backend-chert --disable-backend-remote \
           --disable-documentation ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "xapian configure failed for $LABEL (see $BUILD_LOG)"
    log "  xapian: make libxapian.la ($LABEL)"
    # Only the library is built.  `make all` would also build bin/ + examples/
    # programs (kiwix-build's xapian meson fork drops both); on darwin those
    # links pass -Wl,-bind_at_load which zig's lld rejects, and on windows
    # they hit a libc++ gap (codecvt<char,char,int>::id, see helpers.md).
    ( cd "$MK/xapian" && make -j"$BUILD_JOBS" libxapian.la ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "xapian build failed for $LABEL (see $BUILD_LOG)"
    # install manually: automake's `make install` would build (and fail on)
    # the same programs; install exactly what libzim consumes.
    cp "$MK/xapian/.libs/libxapian.a" "$P/lib/libxapian.a"
    mkdir -p "$P/include/xapian"
    cp "$S/xapian-core-$XAPIAN_VERSION/include/xapian/"*.h "$P/include/xapian/"
    # version.h is generated by configure into the build dir, not the source
    cp "$MK/xapian/include/xapian/version.h" "$P/include/xapian/"
    cp "$MK/xapian/pkgconfig/xapian-core.pc" "$P/lib/pkgconfig/xapian-core.pc"
    # the master <xapian.h> lives at include/xapian.h (consumers include it)
    cp "$S/xapian-core-$XAPIAN_VERSION/include/xapian.h" "$P/include/xapian.h"
    KZ_T=$((KZ_T + SECONDS - t0))
  fi

  # --- libzim 9.8.2 (meson; reader only, with xapian) -----------------------
  kz_meson libzim.a "$S/libzim-$LIBZIM_VERSION" libzim \
    -Dwithout_writer=true -Dwith_xapian=true -Dexamples=false -Dtests=false \
    -Ddoc=false -Dwerror=false

  # --- libkiwix 14.2.1 (meson; static-linkage) ------------------------------
  kz_meson libkiwix.a "$S/libkiwix-$LIBKIWIX_VERSION" libkiwix \
    -Dstatic-linkage=true -Ddoc=false -Dwerror=false

  # --- kiwix-tools 3.8.2 (meson; ships kiwix-serve) -------------------------
  # Rebuild when kiwix-serve is missing or, on linux, is an unstripped build
  # that predates the -s link arg in $CF.  Re-running meson setup + ninja
  # only compiles the kiwix-tools sources again -- every library above is
  # marker-gated and already installed.
  if [ ! -x "$B/kiwix-serve$EXT" ] \
      || { [ "$IS_LINUX" = 1 ] && ! kz_stripped "$B/kiwix-serve$EXT"; }; then
    t0=$SECONDS
    rm -rf "$MK/tools"
    log "  kiwix-tools: meson setup ($LABEL)"
    ( cd "$S/kiwix-tools-$KIWIX_TOOLS_VERSION" \
        && python3 -m mesonbuild.mesonmain setup "$MK/tools" \
           "$S/kiwix-tools-$KIWIX_TOOLS_VERSION" \
           --cross-file "$CF" --prefix "$P" --libdir lib \
           -Ddefault_library=static --buildtype release \
           -Dstatic-linkage=true -Ddoc=false -Dwerror=false ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "kiwix-tools meson failed for $LABEL (see $BUILD_LOG)"
    log "  kiwix-tools: build ($LABEL)"
    # kiwix-build builds kiwix-serve, kiwix-manage and kiwix-search together
    # (the meson.build has no per-tool switch); only kiwix-serve is shipped.
    ( cd "$MK/tools" && ninja -j"$BUILD_JOBS" ) 2>&1 | tee -a "$BUILD_LOG" \
      || die "kiwix-tools build failed for $LABEL (see $BUILD_LOG)"
    # atomic replace: rename() succeeds even when a running kiwix-serve
    # (e.g. qa's ks-libraries) still has the previous binary mapped -- plain
    # cp to the busy path fails with ETXTBSY.  The running process keeps the
    # old inode; every subsequent exec of the path gets the new binary.
    cp "$MK/tools/src/server/kiwix-serve$EXT" "$B/kiwix-serve$EXT.tmp"
    mv -f "$B/kiwix-serve$EXT.tmp" "$B/kiwix-serve$EXT"
    chmod +x "$B/kiwix-serve$EXT"
    KZ_T=$((KZ_T + SECONDS - t0))
  fi
  KZ_TIME=$KZ_T
  KZ_SIZE=$(stat -c %s "$B/kiwix-serve$EXT")

  # --- results --------------------------------------------------------------
  printf '\n%-14s %-24s\n' kiwix-target "$LABEL ($TRIPLE)" | tee -a "$RESULTS"
  printf '%-14s %10d\n' 'kiwix-serve-bytes' "$KZ_SIZE" | tee -a "$RESULTS"
  printf '%-14s %10d\n' 'kiwix-wall-seconds' "$KZ_TIME" | tee -a "$RESULTS"
  printf '%-14s %10d\n' 'icu-data-bytes' "$KZ_ICU" | tee -a "$RESULTS"
  file "$B/kiwix-serve$EXT" | tee -a "$RESULTS"
  # if-form, not `[ ... ] && ...`: as the last statement of the function the
  # short-circuited && list would return 1 on non-linux targets and set -e
  # would kill the whole build after the first such target.
  if [ "$LABEL" = "linux/amd64" ]; then
    "$B/kiwix-serve" --version 2>&1 | tee -a "$RESULTS"
  fi
}

# ---- 4b. kiwix-serve runtime exercise (linux/amd64) ------------------------
# Serves the pinned vikidia ZIM on a free loopback port and checks:
#   * GET / is 200,
#   * the full-text search (public API) for a word from the ZIM returns >= 1
#     result,
#   * GET of one result article is 200.
# kiwix-serve is killed afterwards; any failed step fails the script.
kiwix_exercise() {
  local KS="$O/linux_amd64/bin/kiwix-serve"
  # A small test ZIM (the Go tests use the same variable); unset skips.
  local ZIM="${KS_TEST_ZIM:-}"
  if [ ! -x "$KS" ]; then
    log "kiwix-serve for linux/amd64 missing; skipping runtime exercise"
    return 0
  fi
  if [ -z "$ZIM" ] || [ ! -f "$ZIM" ]; then
    log "no test ZIM (set KS_TEST_ZIM to a small .zim); skipping kiwix-serve runtime exercise"
    return 0
  fi
  local PORT
  PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
  local NAME
  NAME="$(basename "$ZIM" .zim)"   # kiwix-serve names a book after its file
  {
    echo
    echo "==== kiwix-serve runtime exercise (linux/amd64): $("$KS" --version 2>&1 | head -1)"
  } | tee -a "$RESULTS"
  "$KS" --address 127.0.0.1 --port "$PORT" "$ZIM" >/dev/null 2>&1 &
  local SRV=$!
  local ok=1 rc i code
  # wait for the listener (up to 15 s)
  i=0
  while ! curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; do
    i=$((i+1)); [ "$i" -ge 75 ] && { ok=0; break; }
    sleep 0.2
  done
  if [ "$ok" = 1 ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/")
    echo "GET / -> $code" | tee -a "$RESULTS"
    [ "$code" = 200 ] || { ok=0; log "kiwix-serve: GET / returned $code (expected 200)"; }
  fi
  if [ "$ok" = 1 ]; then
    curl -fsS -o "$B/kiwix-search.xml" \
      "http://127.0.0.1:$PORT/search?books.name=$NAME&pattern=sicilia&format=xml" \
      || { ok=0; log "kiwix-serve: search request failed"; }
    if [ "$ok" = 1 ]; then
      rc=$(python3 - "$B/kiwix-search.xml" <<'PYEOF'
import re, sys
d = open(sys.argv[1], 'rb').read().decode('utf-8', 'replace')
# <opensearch:totalResults>N</opensearch:totalResults> is the result count
m = re.search(r'<opensearch:totalResults>(\d+)</opensearch:totalResults>', d)
sys.stdout.write(m.group(1) if m else '0')
PYEOF
      )
      echo "full-text search (pattern=sicilia) results: $rc" | tee -a "$RESULTS"
      [ "$rc" -ge 1 ] || { ok=0; log "kiwix-serve: search returned 0 results"; }
    fi
  fi
  if [ "$ok" = 1 ]; then
    local APATH
    APATH=$(python3 - "$B/kiwix-search.xml" <<'PYEOF'
import re, sys
d = open(sys.argv[1], 'rb').read().decode('utf-8', 'replace')
# results are <item><link>/content/ZIMNAME/PATH/IN/ZIM</link> entries
m = re.search(r'<link>/content/[^/<]+/([^<]+)</link>', d)
sys.stdout.write(m.group(1) if m else '')
PYEOF
    )
    if [ -z "$APATH" ]; then
      ok=0; log "kiwix-serve: no article path found in search results"
    else
      code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/content/$NAME/$APATH")
      echo "GET /content/$NAME/$APATH -> $code" | tee -a "$RESULTS"
      [ "$code" = 200 ] || { ok=0; log "kiwix-serve: article GET returned $code (expected 200)"; }
    fi
  fi
  kill "$SRV" 2>/dev/null || true
  wait "$SRV" 2>/dev/null || true
  [ "$ok" = 1 ] || die "kiwix-serve runtime exercise failed (see $BUILD_LOG and $RESULTS)"
  echo "kiwix-serve runtime exercise ok" | tee -a "$RESULTS"
}

# ---- 6. windows import check ------------------------------------------------
# Parse the PE import table; fail on any DLL outside the Windows system set.
kiwix_windows_import_check() {
  local PE="$O/windows_amd64/bin/kiwix-serve.exe"
  [ -f "$PE" ] || { log "windows kiwix-serve.exe missing; skipping import check"; return 0; }
  if python3 - "$PE" <<'PYEOF' | tee -a "$RESULTS"
import struct, sys
p = open(sys.argv[1], 'rb').read()
if p[:2] != b'MZ':
    print('not a PE file'); sys.exit(1)
pe = struct.unpack_from('<I', p, 0x3c)[0]
assert p[pe:pe+4] == b'PE\0\0'
machine, nsects, timestamp, symptr, nsyms, optsize, chars = struct.unpack_from('<HHIIIHH', p, pe+4)
opt = pe + 24
magic = struct.unpack_from('<H', p, opt)[0]
if magic != 0x20b:  # PE32+
    print(f'not PE32+ (magic {magic:#x})'); sys.exit(1)
# data directory 1 = import table (RVA, size)
import_rva, import_size = struct.unpack_from('<II', p, opt + 112)
# map RVA -> file offset via section headers
sections = []
for i in range(nsects):
    off = opt + optsize + i*40
    vsize, vaddr, rsize, roff = struct.unpack_from('<IIII', p, off+8)
    sections.append((vaddr, max(vsize, rsize), roff))
def rva2off(rva):
    for vaddr, size, roff in sections:
        if vaddr <= rva < vaddr + size:
            return roff + (rva - vaddr)
    return None
dlls = set()
off = rva2off(import_rva) if import_rva else None
while off and struct.unpack_from('<I', p, off)[0] != 0:  # OriginalFirstThunk
    name_rva = struct.unpack_from('<I', p, off + 12)[0]
    noff = rva2off(name_rva)
    end = p.index(b'\0', noff)
    dlls.add(p[noff:end].decode('ascii', 'replace'))
    off += 20
SYSTEM = {'KERNEL32.dll', 'ntdll.dll', 'msvcrt.dll', 'WS2_32.dll',
          'ADVAPI32.dll', 'bcrypt.dll', 'USER32.dll', 'GDI32.dll',
          'SHELL32.dll', 'ole32.dll', 'IMM32.dll', 'uxtheme.dll', 'version.dll'}
bad = sorted(d for d in dlls - SYSTEM)
for d in sorted(dlls):
    print(f'imports: {d}' + ('   <-- NON-SYSTEM' if d in bad else ''))
if bad:
    print('FAIL: non-system DLL imports: ' + ', '.join(bad))
    sys.exit(1)
print('windows import check ok (system DLLs only)')
PYEOF
  then
    :
  else
    die "windows import check failed for $PE (see $RESULTS)"
  fi
}
