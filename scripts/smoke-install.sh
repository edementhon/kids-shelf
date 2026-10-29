#!/usr/bin/env bash
# smoke-install.sh exercises one released kids-shelf zip from install to
# removal, in a throwaway HOME, exactly like a fresh family machine:
#
#   1. unzip into a temp dir.
#   2. macOS: codesign --verify --deep --strict on the .app bundle; spctl
#      --assess is recorded but expected to REJECT the ad-hoc signature,
#      so it never fails the run.
#   3. run the extracted build with the temp HOME: a release build with no
#      installation takes the installer role, installs itself and starts
#      the root launcher. Wait for /healthz on the public port
#      instance.json records (8070 or the next free port).
#   4. check the install layout: Linux ~/.local/share/kids-shelf/app,
#      macOS ~/Library/Application Support/kids-shelf/app, plus the entry
#      app at /Applications/kids-shelf.app (~/Applications/kids-shelf.app
#      when the account cannot write /Applications).
#   5. make the parent PIN like a parent's first visit does (POST
#      /api/admin/pin/setup; the parent page always needs one), keep the
#      session cookie, then POST /api/admin/remove with it, the same-origin
#      Origin header and the JSON body the parent page sends. Wait for the
#      remover, and check that only the lock files remain (install.lock,
#      instance.lock, kids-shelf.db.lock; internal/install/remove.go never
#      unlinks them).
#
# Usage: smoke-install.sh <release.zip>
# Requirements: bash, unzip, curl, python3 (instance.json), and on macOS
# codesign + spctl. Runs on macOS (arm64 and amd64) and Linux.
# Exit code is non-zero when any check fails; messages are plain words.
set -euo pipefail

ZIP="${1:-}"
if [ "$#" -ne 1 ] || [ -z "$ZIP" ]; then
	echo "smoke-install: usage: smoke-install.sh <release.zip>" >&2
	exit 2
fi
[ -f "$ZIP" ] || { echo "smoke-install: no such file: $ZIP" >&2; exit 2; }

OS="$(uname -s)"
if [ "$OS" != "Darwin" ] && [ "$OS" != "Linux" ]; then
	echo "smoke-install: unsupported OS: $OS (this script runs on macOS and Linux)" >&2
	exit 2
fi

fail() { echo "smoke-install: FAIL: $*" >&2; exit 1; }
ok() { echo "smoke-install: ok: $*"; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/ks-smoke.XXXXXX")"
HOME_TMP="$(mktemp -d "${TMPDIR:-/tmp}/ks-home.XXXXXX")"
APP_PID=""
DATA_DIR=""

cleanup() {
	# Stop anything the smoke test started, so a failed run never leaves a
	# kids-shelf serving on a throwaway HOME.
	if [ -n "$APP_PID" ] && kill -0 "$APP_PID" 2>/dev/null; then
		kill "$APP_PID" 2>/dev/null || true
	fi
	if [ -n "$DATA_DIR" ]; then
		# The launcher's command line names the data dir (--data-dir), so
		# pkill -f finds the supervisor and its child without touching
		# anything else.
		pkill -f "$DATA_DIR" 2>/dev/null || true
		sleep 1
	fi
	rm -rf "$TMP" "$HOME_TMP"
}
trap cleanup EXIT

echo "smoke-install: testing $ZIP on $OS"
unzip -tq "$ZIP" >/dev/null || fail "unzip -t failed for $ZIP"
echo "smoke-install: unzipping"
unzip -q -o "$ZIP" -d "$TMP" || fail "unzip failed for $ZIP"

if [ "$OS" = "Darwin" ]; then
	APP="$TMP/kids-shelf.app"
	[ -d "$APP" ] || fail "the zip does not contain kids-shelf.app"
	APP_BIN="$APP/Contents/MacOS/kids-shelf"
	[ -x "$APP_BIN" ] || fail "kids-shelf.app has no executable at Contents/MacOS/kids-shelf"

	echo "smoke-install: checking the code signature (codesign --verify --deep --strict)"
	if codesign --verify --deep --strict "$APP" >"$TMP/codesign.out" 2>&1; then
		ok "codesign --verify --deep --strict passed"
	else
		fail "codesign --verify --deep --strict failed: $(cat "$TMP/codesign.out")"
	fi
	# spctl --assess assesses Gatekeeper policy; an ad-hoc signed app has
	# no notarization, so rejection is expected. Record the output, never
	# fail on it.
	SPCTL_OUT="$(spctl --assess --type execute "$APP" 2>&1 || true)"
	echo "smoke-install: spctl --assess (expected to reject an ad-hoc app): $SPCTL_OUT"
else
	APP_BIN="$TMP/kids-shelf"
	[ -f "$APP_BIN" ] || fail "the zip does not contain a kids-shelf executable"
	[ -x "$APP_BIN" ] || fail "kids-shelf is not executable"
fi

# Temp HOME: the installer must see a fresh profile. On Linux the data and
# install dirs honor XDG_DATA_HOME, so drop it and use the predictable
# ~/.local/share fallback.
export HOME="$HOME_TMP"
unset XDG_DATA_HOME
if [ "$OS" = "Darwin" ]; then
	DATA_DIR="$HOME/Library/Application Support/kids-shelf"
	INSTALL_DIR="$DATA_DIR/app"
	# The same choice as internal/install entryBundleDst: /Applications when
	# the account can write it (admin accounts, GitHub's runners), else
	# ~/Applications. HOME does not move /Applications.
	if [ -w /Applications ]; then
		ENTRY_BUNDLE="/Applications/kids-shelf.app"
	else
		ENTRY_BUNDLE="$HOME/Applications/kids-shelf.app"
	fi
else
	DATA_DIR="$HOME/.local/share/kids-shelf"
	INSTALL_DIR="$DATA_DIR/app"
fi
echo "smoke-install: temp HOME $HOME"

# The extracted build is a release (version != dev) with no installation,
# so it is the installer role: it installs itself, then starts the root
# launcher (the supervisor). --no-browser only stops THIS process from
# opening one; the installed launcher still tries, which is best effort.
echo "smoke-install: running the extracted build (installer role)"
"$APP_BIN" --no-browser >"$TMP/app.log" 2>&1 &
APP_PID=$!

# Wait for instance.json, then for /healthz on the recorded public port
# (8070, or the next free port when it is taken: internal/launch). The
# installer starts the installed launcher and exits 0, which is the normal
# hand-off; only a non-zero exit is a failure. On macOS the install takes
# long enough that the installer is often gone before the launcher writes
# instance.json (macos-smoke, v0.2.0).
PORT=""
INSTALLER_EXIT=""
for _ in $(seq 1 120); do
	if [ -f "$DATA_DIR/instance.json" ]; then
		PORT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("port", 0))' "$DATA_DIR/instance.json" 2>/dev/null || true)"
		if [[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -gt 0 ]; then
			break
		fi
	fi
	if [ -z "$INSTALLER_EXIT" ] && ! kill -0 "$APP_PID" 2>/dev/null; then
		INSTALLER_EXIT=0
		wait "$APP_PID" || INSTALLER_EXIT=$?
		if [ "$INSTALLER_EXIT" != 0 ]; then
			fail "the installer exited with status $INSTALLER_EXIT before instance.json appeared; its log ends with: $(tail -5 "$TMP/app.log")"
		fi
		echo "smoke-install: the installer handed off to the installed launcher (exit 0)"
	fi
	sleep 1
done
[ -n "$PORT" ] || fail "instance.json never appeared in $DATA_DIR; the app log ends with: $(tail -5 "$TMP/app.log")"

echo "smoke-install: waiting for /healthz on http://127.0.0.1:$PORT"
HEALTHY=""
for _ in $(seq 1 120); do
	if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
		HEALTHY=1
		break
	fi
	sleep 1
done
[ -n "$HEALTHY" ] || fail "no /healthz answer on port $PORT within 120 s; the app log ends with: $(tail -5 "$TMP/app.log")"
ok "/healthz answers on port $PORT"

echo "smoke-install: checking the install layout"
[ -d "$INSTALL_DIR" ] || fail "install root missing: $INSTALL_DIR"
[ -x "$INSTALL_DIR/kids-shelf" ] || fail "no root launcher at $INSTALL_DIR/kids-shelf"
[ -f "$INSTALL_DIR/install.lock" ] || fail "no install.lock in $INSTALL_DIR"
VERSION_DIRS="$(find "$INSTALL_DIR/versions" -mindepth 1 -maxdepth 1 -type d 2>/dev/null || true)"
[ -n "$VERSION_DIRS" ] || fail "no published version under $INSTALL_DIR/versions"
for V in $VERSION_DIRS; do
	[ -x "$V/kids-shelf" ] || fail "version build missing its executable: $V/kids-shelf"
done
[ -f "$DATA_DIR/kids-shelf.db" ] || fail "no database at $DATA_DIR/kids-shelf.db"
[ -f "$DATA_DIR/instance.json" ] || fail "no instance.json in $DATA_DIR"
if [ "$OS" = "Darwin" ]; then
	[ -d "$ENTRY_BUNDLE" ] || fail "entry app missing: $ENTRY_BUNDLE"
	[ -x "$ENTRY_BUNDLE/Contents/MacOS/kids-shelf" ] || fail "entry app has no executable: $ENTRY_BUNDLE/Contents/MacOS/kids-shelf"
	ok "install layout verified (install root $INSTALL_DIR, entry app $ENTRY_BUNDLE)"
else
	ok "install layout verified (install root $INSTALL_DIR)"
fi

# The parent page always needs a PIN: make one like a parent's first visit
# does, keeping the session cookie it issues for the removal below.
ORIGIN="http://127.0.0.1:$PORT"
COOKIES="$TMP/parent-cookies.txt"
echo "smoke-install: POST /api/admin/pin/setup (the first parent PIN)"
SETUP_CODE="$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -X POST \
	-c "$COOKIES" \
	-H "Content-Type: application/json" \
	-H "Origin: $ORIGIN" \
	--data '{"pin":"2468"}' \
	"$ORIGIN/api/admin/pin/setup" 2>&1 || true)"
[ "$SETUP_CODE" = "200" ] || fail "parent PIN setup answered $SETUP_CODE, want 200"
ok "parent PIN made"

# Remove, exactly like the parent page: the session cookie, same-origin
# Origin header plus a JSON body. deleteData=true so the data dir is swept
# too, leaving the three lock files as the only files (the page defaults
# to false; the smoke test asks for the empty end state).
echo "smoke-install: POST /api/admin/remove (Origin header and session, like the parent page)"
REMOVE_OUT="$(curl -sS --max-time 30 -X POST \
	-b "$COOKIES" \
	-H "Content-Type: application/json" \
	-H "Origin: $ORIGIN" \
	--data '{"deleteData":true}' \
	"$ORIGIN/api/admin/remove" 2>&1 || true)"
echo "smoke-install: remove API answered: $REMOVE_OUT"

# The app records the request durably, exits, and the supervisor hands
# over to the remover, which waits for both locks and empties the install
# root and (deleteData) the data dir. Lock files are never unlinked
# (internal/install/remove.go), so the end state is: only install.lock,
# instance.lock and kids-shelf.db.lock remain.
echo "smoke-install: waiting for the remover to finish (only lock files remain)"
DONE=""
for _ in $(seq 1 180); do
	# Inside the data dir only the three lock files may remain: the two
	# at the top plus install.lock inside the nested app/ install root.
	LEFTOVERS="$(find "$DATA_DIR" -type f \
		! -name instance.lock ! -name kids-shelf.db.lock \
		! -path "$DATA_DIR/app/install.lock" 2>/dev/null || true)"
	if [ -z "$LEFTOVERS" ] && [ -f "$INSTALL_DIR/install.lock" ]; then
		DONE=1
		break
	fi
	sleep 1
done
[ -n "$DONE" ] || fail "the removal did not finish within 180 s; leftovers in $DATA_DIR: $(find "$DATA_DIR" -type f 2>/dev/null | head -20)"
if [ "$OS" = "Darwin" ] && [ -e "$ENTRY_BUNDLE" ]; then
	fail "entry app still present after removal: $ENTRY_BUNDLE"
fi
ok "removal finished; only the lock files remain:"
find "$DATA_DIR" -type f | sort | sed 's/^/smoke-install:   /'

echo "smoke-install: PASS"
