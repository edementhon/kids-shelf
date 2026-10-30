#!/usr/bin/env bash
# release.sh builds and packages one kids-shelf release into the
# files internal/update consumes: five per-target zips
# (kids-shelf_<goos>_<goarch>.zip, PLAN.md 1a "Updates"), a SHA256SUMS
# listing them and its ed25519 signature (DECISIONS.md "Updates stay
# signed"), plus the helper sources archive (THIRD_PARTY_NOTICES.md).
#
# The signing key never enters the build machine: the private key lives
# only on the maintainer's own machine (~/.config/kids-shelf/release.key;
# agent sessions never see it). So by default release.sh takes the
# release public key from KS_RELEASE_PUBKEY or
# packaging/release-pubkey.txt (comment lines ignored), builds
# everything, writes SHA256SUMS and stops, printing the exact host
# commands to sign. With KS_SIGNING_KEY set (the throwaway-key testing
# mode) it signs and verifies here and, with KS_PUBLISH=1, creates the
# draft GitHub release too.
#
# Run from the clean-history clone, never the one that still reaches the
# legacy/ path: the script refuses to cut a release from a tree whose
# history contains legacy/ at all (DECISIONS.md "legacy/ never published").
#
# Requirements: bash, git, curl, tar, python3 (Mach-O signature scan),
# sha256sum and the pinned Go toolchain (scripts/env.sh). The helper
# binaries must already be built (build/helpers/build.sh).
#
# Usage: release.sh vX.Y.Z
#
# Environment:
#   KS_SIGNING_KEY  path to the ed25519 key file (kssign genkey); unset
#                   for the default unsigned flow (sign on the host), set
#                   for the throwaway-key testing flow (sign here)
#   KS_RELEASE_PUBKEY  64-hex-digit release public key; required when
#                   KS_SIGNING_KEY is unset, either here or in
#                   packaging/release-pubkey.txt. When both the signing
#                   key and a public key are given they must match.
#   KS_BUILD        the build dir holding the toolchain and the helpers
#                   (default: the kids-shelf-build folder next to the
#                   repo; scripts/env.sh sets the same rule)
#   KS_HELPERS_OUT  built helper binaries, one <goos>_<goarch>/bin per
#                   target (default $KS_BUILD/helpers/out)
#   KS_HELPERS_ROOT the helpers build root whose downloads/ cache feeds
#                   the helper-sources archive (default the parent of
#                   KS_HELPERS_OUT)
#   KS_RELEASE_OUT  where zips, staged builds and the signature land
#                   (default $KS_BUILD/release/vX.Y.Z)
#   KS_ALLOW_DIRTY=1    skip the dirty-tree gate (testing only; see the
#                   "Testing-only knobs" section of docs/RELEASING.md)
#   KS_PUBLISH=1    also create a draft GitHub release with gh (only in
#                   the KS_SIGNING_KEY flow)
#   KS_RELEASE_REPO owner/name of the repository the draft release is
#                   created in (default edementhon/kids-shelf)
set -euo pipefail

# --- pinned release tools ---------------------------------------------------
# go-winres v0.3.3 embeds the icon, the asInvoker/per-monitor manifest and
# the file/product version into the Windows build (latest as of 2026-10-27).
GO_WINRES_VERSION="v0.3.3"

# rcodesign (apple-codesign) 0.29.0 ad-hoc signs the darwin/amd64 helper
# binaries and the whole .app bundle (latest as of 2026-10-27). Provenance:
# indygreg/apple-platform-rs publishes a .sha256 asset next to every
# release binary; RCODESIGN_SHA256 is that published digest, recomputed
# locally from the downloaded tarball on 2026-10-27 and matching
# byte-for-byte.
RCODESIGN_VERSION="0.29.0"
RCODESIGN_URL="https://github.com/indygreg/apple-platform-rs/releases/download/apple-codesign/${RCODESIGN_VERSION}/apple-codesign-${RCODESIGN_VERSION}-x86_64-unknown-linux-musl.tar.gz"
RCODESIGN_SHA256="dbe85cedd8ee4217b64e9a0e4c2aef92ab8bcaaa41f20bde99781ff02e600002"

log() { printf 'release: %s\n' "$*"; }
die() { printf 'release: %s\n' "$*" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

VERSION="${1:-}"
if [ "$#" -ne 1 ]; then
	die "usage: release.sh vX.Y.Z"
fi
if [[ ! "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	die "version $VERSION is not a canonical vMAJOR.MINOR.PATCH release version (no leading zeros)"
fi

if [ "${KS_ALLOW_DIRTY:-0}" != "1" ]; then
	if [ -n "$(git status --porcelain)" ]; then
		die "the working tree is dirty; commit or stash before cutting a release"
	fi
fi
# Fail closed on the clean-history gate: grep -q would close the pipe as
# soon as it found a match, SIGPIPE the still-writing producer, and with
# pipefail the whole pipeline would come back nonzero, making the if
# condition skip the refusal. Capture the whole history in a separately
# checked command instead, and treat a query failure as fatal.
if ! LEGACY_COMMITS="$(git rev-list HEAD -- legacy)"; then
	die "git rev-list failed; cannot verify the clean-history gate (DECISIONS.md \"legacy/ never published\")"
fi
if [ -n "$LEGACY_COMMITS" ]; then
	die "HEAD reaches the legacy/ path; cut releases from the clean-history clone, never this one (DECISIONS.md \"legacy/ never published\")"
fi

KS_SIGNING_KEY="${KS_SIGNING_KEY:-}"
if [ -n "$KS_SIGNING_KEY" ] && [ ! -f "$KS_SIGNING_KEY" ]; then
	die "KS_SIGNING_KEY does not point at a file: $KS_SIGNING_KEY"
fi
KS_PUBLISH="${KS_PUBLISH:-0}"
KS_RELEASE_REPO="${KS_RELEASE_REPO:-edementhon/kids-shelf}"

# The pinned Go toolchain (idempotent; hash -r forgets any older go).
# shellcheck source=env.sh
. ./scripts/env.sh
hash -r

# The build dir: the kids-shelf-build folder next to the repo (env.sh
# sets KS_BUILD when it can), or KS_BUILD when given.
KS_BUILD="${KS_BUILD:-"$(cd "$REPO_ROOT/.." && pwd)/kids-shelf-build"}"
KS_HELPERS_OUT="${KS_HELPERS_OUT:-$KS_BUILD/helpers/out}"
KS_RELEASE_OUT="${KS_RELEASE_OUT:-$KS_BUILD/release/$VERSION}"
KS_HELPERS_ROOT="${KS_HELPERS_ROOT:-"$(dirname "$KS_HELPERS_OUT")"}"

GO="go"
OUT="$KS_RELEASE_OUT"
mkdir -p "$OUT/tools" "$OUT/build"

# --- the release public key (fail closed) -----------------------------------
# The private key lives only on the signing machine, so the public key travels as
# text: KS_RELEASE_PUBKEY or packaging/release-pubkey.txt (comment and
# blank lines ignored). Default builds embed this key and stop before
# signing; the KS_SIGNING_KEY flow signs here and its key must agree with
# the pinned public key when both are given.
KEY_PUBKEY_HEX=""
if [ -n "$KS_SIGNING_KEY" ]; then
	log "deriving the release public key from $KS_SIGNING_KEY"
	KEY_PUBKEY_HEX="$("$GO" run ./cmd/kssign pubkey "$KS_SIGNING_KEY")"
	if [[ ! "$KEY_PUBKEY_HEX" =~ ^[0-9a-f]{64}$ ]]; then
		die "kssign pubkey did not return a 64-hex-digit public key; refusing to build an unverifiable release"
	fi
fi

if [ -n "${KS_RELEASE_PUBKEY:-}" ]; then
	PUBKEY_HEX="$(printf '%s' "$KS_RELEASE_PUBKEY" | tr -d '[:space:]')"
else
	PUBKEY_FILE="$REPO_ROOT/packaging/release-pubkey.txt"
	if [ -f "$PUBKEY_FILE" ]; then
		while IFS= read -r LINE || [ -n "$LINE" ]; do
			LINE="${LINE%%#*}" # strip a trailing comment
			LINE="$(printf '%s' "$LINE" | tr -d '[:space:]')"
			if [ -n "$LINE" ]; then
				PUBKEY_HEX="$LINE"
				break
			fi
		done < "$PUBKEY_FILE"
	fi
fi
# The throwaway-key testing mode pins nothing: its own key is the release key.
if [ -z "${PUBKEY_HEX:-}" ] && [ -n "$KEY_PUBKEY_HEX" ]; then
	PUBKEY_HEX="$KEY_PUBKEY_HEX"
fi
if [[ ! "${PUBKEY_HEX:-}" =~ ^[0-9a-f]{64}$ ]]; then
	die "no 64-hex-digit release public key: set KS_RELEASE_PUBKEY or paste the key into packaging/release-pubkey.txt; refusing to build an unverifiable release"
fi
if [ -n "$KEY_PUBKEY_HEX" ] && [ "$KEY_PUBKEY_HEX" != "$PUBKEY_HEX" ]; then
	die "KS_SIGNING_KEY does not match the pinned release public key (derived $KEY_PUBKEY_HEX, pinned $PUBKEY_HEX)"
fi
log "release public key: $PUBKEY_HEX"

# --- release tools ----------------------------------------------------------
RCODESIGN="$OUT/tools/rcodesign"
if [ ! -x "$RCODESIGN" ] || ! printf '%s  %s\n' "$RCODESIGN_SHA256" "$RCODESIGN" | sha256sum -c - >/dev/null 2>&1; then
	rm -f "$RCODESIGN"
	log "fetching rcodesign $RCODESIGN_VERSION (pinned sha256 $RCODESIGN_SHA256)"
	TARBALL="$OUT/tools/rcodesign.tar.gz"
	curl -fSL --retry 3 -o "$TARBALL" "$RCODESIGN_URL"
	if ! printf '%s  %s\n' "$RCODESIGN_SHA256" "$TARBALL" | sha256sum -c - >/dev/null 2>&1; then
		rm -f "$TARBALL"
		die "rcodesign download failed its pinned sha256 check; refusing to sign with an unverified tool"
	fi
	tar -xzf "$TARBALL" -C "$OUT/tools"
	rm -f "$TARBALL"
	mv "$OUT/tools/apple-codesign-$RCODESIGN_VERSION-x86_64-unknown-linux-musl/rcodesign" "$RCODESIGN"
	rm -rf "$OUT/tools/apple-codesign-$RCODESIGN_VERSION-x86_64-unknown-linux-musl"
fi
"$RCODESIGN" --version

# macho_has_codesig exits 0 if $1 is a thin Mach-O carrying an
# LC_CODE_SIGNATURE load command (0x1D), 1 if it is intact but unsigned, 2
# if it is not a thin Mach-O at all. The helper builds are thin Mach-Os
# (build/helpers/build.sh scans them the same way), so exit 2 means a
# corrupted file and the else branch's rcodesign run fails loudly.
macho_has_codesig() {
	python3 - "$1" <<'PYEOF'
import struct
import sys

data = open(sys.argv[1], 'rb').read()
magic = data[:4]
if magic not in (b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf'):
    sys.exit(2)
le = magic == b'\xcf\xfa\xed\xfe'
endian = '<' if le else '>'
ncmds = struct.unpack_from(endian + 'I', data, 16)[0]
offset = 32
for _ in range(ncmds):
    cmd, size = struct.unpack_from(endian + 'II', data, offset)
    if cmd == 0x1D:  # LC_CODE_SIGNATURE
        sys.exit(0)
    offset += size
sys.exit(1)
PYEOF
}

# --- Windows resources (icon, manifest, version) ----------------------------
# go-winres make writes cmd/kids-shelf/rsrc_windows_amd64.syso, which the
# windows build links and every other GOOS ignores. The per-release
# file/product version is passed on the command line; the committed
# packaging/windows/winres.json carries the icon, manifest and the fixed
# version-info strings.
log "generating the Windows resources (.syso) with go-winres $GO_WINRES_VERSION"
"$GO" run "github.com/tc-hib/go-winres@$GO_WINRES_VERSION" make \
	--in packaging/windows/winres.json \
	--out cmd/kids-shelf/rsrc \
	--arch amd64 \
	--file-version "$VERSION" \
	--product-version "$VERSION"

# --- the five targets -------------------------------------------------------
TARGETS=(
	"windows/amd64"
	"darwin/amd64"
	"darwin/arm64"
	"linux/amd64"
	"linux/arm64"
)

for TARGET in "${TARGETS[@]}"; do
	GOOS="${TARGET%/*}"
	GOARCH="${TARGET#*/}"
	KEY="${TARGET//\//_}"
	EXT=""
	[ "$GOOS" = "windows" ] && EXT=".exe"

	BUNDLE_DIR="internal/bundle/files/$KEY"
	HELPERS_SRC="$KS_HELPERS_OUT/$KEY/bin"
	BIN="$OUT/build/$KEY/kids-shelf$EXT"

	log "== $TARGET =="
	[ -d "$HELPERS_SRC" ] || die "$HELPERS_SRC is missing; build the helpers first (build/helpers/build.sh)"

	rm -rf "$BUNDLE_DIR"
	mkdir -p "$BUNDLE_DIR"
	HELPERS=(ffmpeg ffprobe qjs kiwix-serve)
	# linux/amd64 also ships ffmpeg-hw, the dynamically linked glibc build
	# with the VAAPI/NVENC/QSV encoders (build/helpers/build.sh); the other
	# targets carry their hardware encoders inside the regular ffmpeg.
	if [ "$KEY" = "linux_amd64" ]; then
		HELPERS+=(ffmpeg-hw)
	fi
	for HELPER in "${HELPERS[@]}"; do
		[ -f "$HELPERS_SRC/$HELPER$EXT" ] || die "$HELPERS_SRC/$HELPER$EXT is missing"
		cp "$HELPERS_SRC/$HELPER$EXT" "$BUNDLE_DIR/"
	done

	# darwin/arm64 helpers come ad-hoc signed from Zig's linker and are
	# skipped; darwin/amd64 needs a signature or macOS refuses to run it.
	# Signing happens before manifestgen so the bundled hashes describe the
	# signed bytes.
	if [ "$GOOS" = "darwin" ]; then
		for HELPER in ffmpeg ffprobe qjs kiwix-serve; do
			if macho_has_codesig "$BUNDLE_DIR/$HELPER"; then
				log "  $HELPER already carries a code signature, skipping"
			else
				log "  ad-hoc signing $HELPER with rcodesign"
				"$RCODESIGN" sign "$BUNDLE_DIR/$HELPER" >/dev/null
			fi
		done
	fi

	"$GO" run ./internal/bundle/manifestgen -dir "$BUNDLE_DIR"

	LD_FLAGS="-s -w -X main.version=$VERSION -X github.com/edementhon/kids-shelf/internal/update.PublicKeyHex=$PUBKEY_HEX"
	if [ "$GOOS" = "windows" ]; then
		LD_FLAGS="$LD_FLAGS -H=windowsgui"
	fi
	log "  building the $TARGET launcher"
	CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" "$GO" build -tags bundle -trimpath \
		-ldflags "$LD_FLAGS" -o "$BIN" ./cmd/kids-shelf

	# Darwin bundles are signed after staging and before zipping, so
	# rcodesign sees the finished .app (every nested Mach-O, including the
	# launcher, gets a signature).
	if [ "$GOOS" = "darwin" ]; then
		"$GO" run ./build/pack -target "$TARGET" -bin "$BIN" -version "$VERSION" -out "$OUT" -stage
		log "  ad-hoc signing $OUT/staged/$KEY/kids-shelf.app"
		"$RCODESIGN" sign "$OUT/staged/$KEY/kids-shelf.app" >/dev/null
		"$GO" run ./build/pack -target "$TARGET" -out "$OUT" -zip
	else
		"$GO" run ./build/pack -target "$TARGET" -bin "$BIN" -version "$VERSION" -out "$OUT"
	fi
done

# --- checksums --------------------------------------------------------------
log "writing SHA256SUMS"
mapfile -t ZIPS < <(find "$OUT" -maxdepth 1 -type f -name 'kids-shelf_*.zip' -print | sort)
if [ "${#ZIPS[@]}" -ne 5 ]; then
	die "expected five release zips in $OUT, found ${#ZIPS[@]}"
fi
printf 'version: %s\n' "$VERSION" > "$OUT/SHA256SUMS"
for ZIP in "${ZIPS[@]}"; do
	SUM="$(sha256sum "$ZIP")"
	printf '%s  %s\n' "${SUM%% *}" "$(basename "$ZIP")" >> "$OUT/SHA256SUMS"
done
# The commit the binaries were built from (Go embeds the same revision as
# vcs.revision): release-finish.sh tags the release there, not at whatever
# main points to by the time the release is published.
git -C "$REPO_ROOT" rev-parse HEAD > "$OUT/SOURCE_COMMIT"

# --- helper sources archive (GPL corresponding sources) ---------------------
# The bundled helpers (ffmpeg/ffprobe built with --enable-gpl and x264,
# qjs, kiwix-serve and its library stack, see THIRD_PARTY_NOTICES.md) are
# GPL. Their exact pinned upstream sources live in the helpers build's
# download cache: build/helpers/build.sh stores every verified download
# under $KS_HELPERS_ROOT/downloads (its fetch() function). This archive
# ships them plus build/helpers itself as a separate release asset, so
# the app zips and the updater's size gates are untouched.
log "building the helper sources archive"
HELPERS_DOWNLOADS="$KS_HELPERS_ROOT/downloads"
[ -d "$HELPERS_DOWNLOADS" ] || die "helpers download cache $HELPERS_DOWNLOADS is missing; build the helpers first (build/helpers/build.sh)"
# shellcheck source=versions.env
. "$REPO_ROOT/build/helpers/versions.env"
HELPER_SOURCES_ARCHIVE="$OUT/kids-shelf-helper-sources-$VERSION.tar.gz"
HELPER_STAGE="$OUT/build/helper-sources"
rm -rf "$HELPER_STAGE"
mkdir -p "$HELPER_STAGE/downloads" "$HELPER_STAGE/build/helpers"
for DL in "$HELPERS_DOWNLOADS"/*; do
	[ -f "$DL" ] || continue
	# The cache also holds a test video and the build tools (the zig
	# compiler, ninja, meson), which are not part of any shipped binary,
	# and Apple's macOS SDK, which the darwin helpers link against as the
	# system's own frameworks: Apple's licence forbids redistributing it,
	# and the GPL's system-library exception does not ask for it.
	case "$(basename "$DL")" in
	sample.mp4 | zig-* | ninja-* | meson-* | MacOSX*.sdk*) continue ;;
	esac
	cp "$DL" "$HELPER_STAGE/downloads/"
done
# x264 is a pinned git checkout, not a cached tarball; export the exact
# commit (git archive keeps the tree byte-clean, no .git) so the archive
# still carries the GPL source the ffmpeg build links in.
X264_ARCHIVE="$HELPER_STAGE/downloads/x264-$X264_COMMIT.tar.gz"
if [ ! -f "$X264_ARCHIVE" ]; then
	if [ -d "$KS_HELPERS_ROOT/src/x264/.git" ] && [ "$(git -C "$KS_HELPERS_ROOT/src/x264" rev-parse HEAD)" = "$X264_COMMIT" ]; then
		git -C "$KS_HELPERS_ROOT/src/x264" archive --format=tar.gz \
			--prefix="x264-$X264_COMMIT/" -o "$X264_ARCHIVE" "$X264_COMMIT"
	else
		die "no x264 source at commit $X264_COMMIT (expected $KS_HELPERS_ROOT/src/x264 at the pinned commit)"
	fi
fi
cp "$REPO_ROOT/build/helpers/build.sh" \
	"$REPO_ROOT/build/helpers/kiwix.sh" \
	"$REPO_ROOT/build/helpers/versions.env" \
	"$HELPER_STAGE/build/helpers/"
printf '# kids-shelf %s helper sources\n\nThis archive holds the exact pinned upstream source archives that\nbuild/helpers/build.sh downloads (every SHA-256 is pinned in\nbuild/helpers/versions.env), x264 exported at commit %s, and the\nbuild/helpers scripts themselves. They are the corresponding sources for\nthe GPL components bundled in the kids-shelf_*.zip binaries; see\nTHIRD_PARTY_NOTICES.md for the full list with versions, licences and\nupstream URLs.\n' "$VERSION" "${X264_COMMIT:0:12}" > "$HELPER_STAGE/README.txt"
tar -czf "$HELPER_SOURCES_ARCHIVE" -C "$HELPER_STAGE" .
rm -rf "$HELPER_STAGE"
log "helper sources archive: $HELPER_SOURCES_ARCHIVE ($(wc -c < "$HELPER_SOURCES_ARCHIVE") bytes)"

# --- smoke test -------------------------------------------------------------
log "smoke: $OUT/build/linux_amd64/kids-shelf --version"
SMOKE_OUT="$("$OUT/build/linux_amd64/kids-shelf" --version)"
if [ "$SMOKE_OUT" != "kids-shelf $VERSION" ]; then
	die "the linux/amd64 build printed \"$SMOKE_OUT\", want \"kids-shelf $VERSION\""
fi

# --- sign here, or stop and sign on the host --------------------------------
cleanup_release_tree() {
	log "cleaning generated release files"
	rm -rf internal/bundle/files
	rm -f cmd/kids-shelf/rsrc_windows_amd64.syso
	if [ "${KS_ALLOW_DIRTY:-0}" != "1" ] && [ -n "$(git status --porcelain)" ]; then
		die "the working tree is dirty after the release: $(git status --porcelain)"
	fi
}

list_artifacts() {
	log "release artifacts in $OUT:"
	find "$OUT" -maxdepth 1 \( -name 'kids-shelf_*.zip' -o -name 'SHA256SUMS' \
		-o -name 'SHA256SUMS.sig' -o -name 'kids-shelf-helper-sources-*.tar.gz' \) \
		-exec ls -lh {} + | awk '{ print $5, $9 }'
}

if [ -z "$KS_SIGNING_KEY" ]; then
	cleanup_release_tree
	# KS_HOST_BUILD: the build folder as the signing machine sees it (it
	# may be the same folder synced elsewhere); defaults to KS_BUILD.
	HOST_BUILD="${KS_HOST_BUILD:-$KS_BUILD}"
	HOST_OUT="${KS_RELEASE_OUT/#$KS_BUILD/$HOST_BUILD}"
	log "SHA256SUMS written; nothing is signed on this machine:"
	cat <<EOF | sed 's/^/release:   /'
the private key (~/.config/kids-shelf/release.key) lives only on the
signing machine. Sign there (kssign is scripts/build-kssign.sh's output).
Exact commands:

  KSSIGN=$HOST_BUILD/bin/kssign
  OUT=$HOST_OUT
  "\$KSSIGN" sign ~/.config/kids-shelf/release.key "\$OUT/SHA256SUMS" > "\$OUT/SHA256SUMS.sig"
  "\$KSSIGN" verify $PUBKEY_HEX "\$OUT/SHA256SUMS" "\$OUT/SHA256SUMS.sig"
  cd /path/to/kids-shelf && KS_KSSIGN="\$KSSIGN" KS_PUBLISH=1 ./scripts/release-finish.sh "\$OUT"

release-finish.sh refuses to publish when the signature does not verify.
EOF
	list_artifacts
	log "done (unsigned: sign on the signing machine, then run scripts/release-finish.sh)"
	exit 0
fi

log "signing SHA256SUMS"
"$GO" run ./cmd/kssign sign "$KS_SIGNING_KEY" "$OUT/SHA256SUMS" > "$OUT/SHA256SUMS.sig"
"$GO" run ./cmd/kssign verify "$PUBKEY_HEX" "$OUT/SHA256SUMS" "$OUT/SHA256SUMS.sig"
log "SHA256SUMS.sig verifies against the release public key"

cleanup_release_tree

if [ "$KS_PUBLISH" = "1" ]; then
	log "creating the draft GitHub release in $KS_RELEASE_REPO"
	# gh runs from REPO_ROOT with absolute asset paths: $OUT usually lives
	# outside the repository, where gh cannot resolve the target repo, so
	# the --repo flag carries it explicitly.
	gh release create --repo "$KS_RELEASE_REPO" --draft "$VERSION" --title "kids-shelf $VERSION" \
		"$OUT"/kids-shelf_*.zip "$OUT/SHA256SUMS" "$OUT/SHA256SUMS.sig" "$HELPER_SOURCES_ARCHIVE"
fi

list_artifacts
log "done"
