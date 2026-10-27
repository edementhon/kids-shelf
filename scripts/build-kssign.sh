#!/usr/bin/env bash
# build-kssign.sh builds the static linux/amd64 kssign binary that signs
# releases on the signing machine: the private key
# (~/.config/kids-shelf/release.key) never enters the build machine, so
# the key tool is built here and run there.
#
# Environment:
#   KS_BUILD   the build dir (default: the kids-shelf-build folder next
#              to the repo); the binary lands in $KS_BUILD/bin/kssign
set -euo pipefail

log() { printf 'build-kssign: %s\n' "$*"; }
die() { printf 'build-kssign: %s\n' "$*" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# The pinned Go toolchain (idempotent; hash -r forgets any older go).
# shellcheck source=env.sh
. ./scripts/env.sh
hash -r

KS_BUILD="${KS_BUILD:-"$(cd "$REPO_ROOT/.." && pwd)/kids-shelf-build"}"
OUT="$KS_BUILD/bin/kssign"

log "building the static linux/amd64 kssign to $OUT"
mkdir -p "$(dirname "$OUT")"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$OUT" ./cmd/kssign
chmod 0755 "$OUT"

# Sanity: the binary answers.
"$OUT" help >/dev/null
log "kssign built: $OUT ($(wc -c < "$OUT") bytes, sha256 $(sha256sum "$OUT" | cut -d' ' -f1))"
log "run it on the signing machine (copy $OUT there if the build folder is not shared)"
