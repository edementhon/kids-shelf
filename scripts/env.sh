# Source this from the repo root before any go command: `. scripts/env.sh`
# The pinned Go toolchain and a private module cache live in the build
# directory: $KS_BUILD if set, else a folder named kids-shelf-build next to
# this repo (it holds toolchains and caches, never source).
_ks_repo="$(cd "$(dirname "${BASH_SOURCE:-scripts/env.sh}")/.." && pwd)"
KS_BUILD="${KS_BUILD:-$(dirname "$_ks_repo")/kids-shelf-build}"
unset _ks_repo
export PATH="$KS_BUILD/toolchains/go/bin:$PATH"
export GOTOOLCHAIN=local
export GOPATH="$KS_BUILD/gopath"
export GOMODCACHE="$KS_BUILD/gopath/pkg/mod"
export GOCACHE="$KS_BUILD/gocache"
export GOFLAGS=-modcacherw
