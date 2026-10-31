#!/usr/bin/env bash
# release-finish.sh verifies the signature over a release's SHA256SUMS
# against the pinned release public key and, with KS_PUBLISH=1, creates
# the draft GitHub release. It is the second half of the release flow
# and runs where the signature is: on the signing machine after the
# release.sh build, or anywhere with the kssign binary and
# ~/.config/kids-shelf/release.key. The private key never enters the
# build machine.
#
# Usage: release-finish.sh <outdir>        (the dir release.sh wrote into)
#
# Environment:
#   KS_KSSIGN          path to the kssign binary (default
#                      $KS_BUILD/bin/kssign, else kssign on PATH)
#   KS_BUILD           the build dir (default: the kids-shelf-build
#                      folder next to the repo)
#   KS_PUBLISH=1       create the draft GitHub release with gh (needs
#                      `gh auth login` on the host)
#   KS_RELEASE_REPO    owner/name of the repository the draft release is
#                      created in (default edementhon/kids-shelf)
#
# Fails (non-zero) without publishing when SHA256SUMS.sig does not verify
# against the pinned public key, when SHA256SUMS does not name a
# vMAJOR.MINOR.PATCH version, or when listed files are missing.
set -euo pipefail

log() { printf 'release-finish: %s\n' "$*"; }
die() { printf 'release-finish: %s\n' "$*" >&2; exit 1; }

if [ "$#" -ne 1 ]; then
	die "usage: release-finish.sh <outdir>"
fi
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! OUT="$(cd "$1" 2>/dev/null && pwd)"; then
	die "cannot read the release dir: $1"
fi

[ -f "$OUT/SHA256SUMS" ] || die "no SHA256SUMS in $OUT"
[ -f "$OUT/SHA256SUMS.sig" ] || die "no SHA256SUMS.sig in $OUT; sign it first (docs/RELEASING.md)"

# --- the pinned release public key (fail closed) ----------------------------
PUBKEY_HEX=""
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
if [[ ! "${PUBKEY_HEX:-}" =~ ^[0-9a-f]{64}$ ]]; then
	die "no 64-hex-digit release public key: set KS_RELEASE_PUBKEY or paste the key into packaging/release-pubkey.txt"
fi

# --- the kssign binary -------------------------------------------------------
KS_BUILD="${KS_BUILD:-"$(cd "$REPO_ROOT/.." && pwd)/kids-shelf-build"}"
KSSIGN="${KS_KSSIGN:-$KS_BUILD/bin/kssign}"
if [ -z "${KSSIGN:-}" ] || [ ! -x "${KSSIGN:-}" ]; then
	die "kssign not found; set KS_KSSIGN (it is built by scripts/build-kssign.sh)"
fi
# --- verify ----------------------------------------------------------------
log "verifying SHA256SUMS.sig against the pinned release public key"
if ! "$KSSIGN" verify "$PUBKEY_HEX" "$OUT/SHA256SUMS" "$OUT/SHA256SUMS.sig"; then
	die "SHA256SUMS.sig does not verify against the pinned release public key $PUBKEY_HEX; refusing to publish"
fi
log "SHA256SUMS.sig verifies against the pinned release public key"

VERSION="$(sed -n 's/^version: //p' "$OUT/SHA256SUMS" | head -1)"
if [[ ! "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	die "SHA256SUMS does not name a canonical vMAJOR.MINOR.PATCH version (got \"$VERSION\"); refusing to publish"
fi

HELPER_SOURCES="$OUT/kids-shelf-helper-sources-$VERSION.tar.gz"
[ -f "$HELPER_SOURCES" ] || die "missing the helper sources archive: $HELPER_SOURCES"

while IFS=' ' read -r _SUM _NAME; do
	[ -n "$_NAME" ] || continue
	[ -f "$OUT/$_NAME" ] || die "SHA256SUMS lists $_NAME but $OUT/$_NAME is missing"
done < <(grep -v '^version:' "$OUT/SHA256SUMS")

# --- publish (optional) ------------------------------------------------------
if [ "${KS_PUBLISH:-0}" = "1" ]; then
	command -v gh >/dev/null 2>&1 || die "KS_PUBLISH=1 needs gh; run 'gh auth login' on this host first"
	KS_RELEASE_REPO="${KS_RELEASE_REPO:-edementhon/kids-shelf}"
	TITLE="Kids Shelf $VERSION"
	log "creating the draft GitHub release \"$TITLE\" in $KS_RELEASE_REPO"
	# Parents land on the release page from the parent guide, so the notes
	# say which zip is theirs. Passing --title and --notes also keeps gh
	# from asking questions; the release is a normal one (not a prerelease):
	# the updater and releases/latest skip prereleases.
	NOTES="Download the zip for your computer, then follow the parent guide: https://github.com/$KS_RELEASE_REPO/blob/main/docs/parent-guide.md

- Windows PC: kids-shelf_windows_amd64.zip
- Mac (Apple chip or Intel): kids-shelf_darwin_universal.zip
- Linux PC: kids-shelf_linux_amd64.zip; Raspberry Pi and other ARM computers: kids-shelf_linux_arm64.zip

Already installed? Kids Shelf updates itself."
	# The tag goes on the commit the binaries were built from (release.sh
	# writes SOURCE_COMMIT), so a docs commit pushed after the build cannot
	# move it. The commit must already be pushed.
	TARGET_ARGS=()
	if [ -f "$OUT/SOURCE_COMMIT" ]; then
		COMMIT="$(tr -d '[:space:]' < "$OUT/SOURCE_COMMIT")"
		[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "SOURCE_COMMIT in $OUT does not hold a commit hash"
		TARGET_ARGS=(--target "$COMMIT")
		log "tagging $VERSION at the source commit $COMMIT"
	fi
	# gh runs with --repo and absolute asset paths: $OUT usually lives
	# outside any repository checkout, where gh cannot resolve the target
	# repo, so the --repo flag carries it explicitly.
	gh release create --repo "$KS_RELEASE_REPO" --draft "$VERSION" "${TARGET_ARGS[@]}" --title "$TITLE" --notes "$NOTES" \
		"$OUT"/kids-shelf_*.zip "$OUT/SHA256SUMS" "$OUT/SHA256SUMS.sig" "$HELPER_SOURCES"
	log "draft created: https://github.com/$KS_RELEASE_REPO/releases (review and publish it there)"
else
	log "KS_PUBLISH unset: signature verified, nothing published"
fi

log "done"
