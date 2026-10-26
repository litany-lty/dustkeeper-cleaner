#!/bin/sh
# Install the dustkeeper timer for the current user.
#
# This script is run by an operator, on purpose. The program itself never runs
# systemctl, launchctl or sudo; it only renders templates. Everything that
# changes system state happens here, in the open.
#
#   ./packaging/install.sh --dry-run          show the plan, change nothing
#   ./packaging/install.sh                    install and enable
#   ./packaging/install.sh --no-enable        install the files only
#
# It refuses to run as root: cleaning up a user's files as root creates files
# the user cannot then restore.

set -eu

DRY_RUN=0
ENABLE=1
HOUR=3
MINUTE=30
BIN=""
LABEL="dustkeeper"

usage() {
	cat <<'EOF'
usage: install.sh [--dry-run] [--no-enable] [--hour H] [--minute M]
                  [--bin PATH] [--label NAME]

  --dry-run     print every action, execute none
  --no-enable   copy the unit files but do not start the timer
  --hour H      local hour to run (default 3)
  --minute M    local minute to run (default 30)
  --bin PATH    path to the dustkeeper binary (default: found on PATH)
  --label NAME  unit / launchd label (default: dustkeeper)
EOF
}

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

# run CMD... prints and executes, or just prints under --dry-run.
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '  [dry-run] %s\n' "$*"
		return 0
	fi
	printf '  + %s\n' "$*"
	"$@"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1 ;;
	--no-enable) ENABLE=0 ;;
	--hour) shift; [ $# -gt 0 ] || die "--hour needs a value"; HOUR="$1" ;;
	--minute) shift; [ $# -gt 0 ] || die "--minute needs a value"; MINUTE="$1" ;;
	--bin) shift; [ $# -gt 0 ] || die "--bin needs a value"; BIN="$1" ;;
	--label) shift; [ $# -gt 0 ] || die "--label needs a value"; LABEL="$1" ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $1 (try --help)" ;;
	esac
	shift
done

[ "$(id -u)" -ne 0 ] || die "do not run this as root; use the account that owns the files"

case "$(uname -s)" in
Linux) PLATFORM=linux ;;
Darwin) PLATFORM=darwin ;;
*) die "unsupported platform: $(uname -s)" ;;
esac

if [ -z "$BIN" ]; then
	BIN="$(command -v dustkeeper 2>/dev/null || true)"
	[ -n "$BIN" ] || die "dustkeeper is not on PATH; install the binary first or pass --bin"
fi
[ -x "$BIN" ] || die "$BIN is not executable"

# The rendered templates embed the absolute path of the binary, so the binary
# has to already live where it will keep living. Resolve symlinks first: a unit
# pointing at a symlink breaks the day the link is repointed.
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
"$BIN" version >/dev/null 2>&1 || die "$BIN does not look like a working dustkeeper binary"

note "installing dustkeeper timer"
note "  platform: $PLATFORM"
note "  binary:   $BIN"
note "  schedule: daily at $(printf '%02d' "$HOUR"):$(printf '%02d' "$MINUTE") local time"
note ""

# Render on the target machine, so the unit carries this machine's paths and
# this build's binary. Never ship a unit rendered somewhere else.
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

run "$BIN" schedule render --platform "$PLATFORM" --output "$TMP" --hour "$HOUR" --minute "$MINUTE"

case "$PLATFORM" in
linux)
	DEST="$HOME/.config/systemd/user"
	note ""
	note "installing into $DEST"
	run mkdir -p "$DEST"
	for f in "$LABEL.service" "$LABEL.timer"; do
		if [ "$DRY_RUN" -eq 0 ] && [ ! -f "$TMP/$f" ]; then
			die "$f was not rendered"
		fi
		run install -m 0644 "$TMP/$f" "$DEST/$f"
	done
	if [ "$ENABLE" -eq 1 ]; then
		run systemctl --user daemon-reload
		run systemctl --user enable --now "$LABEL.timer"
		if [ "$DRY_RUN" -eq 0 ]; then
			note ""
			systemctl --user list-timers "$LABEL.timer" || true
		fi
	else
		note ""
		note "not started. enable it with:"
		note "  systemctl --user daemon-reload && systemctl --user enable --now $LABEL.timer"
	fi
	note ""
	note "a user timer stops when the last session ends. On a machine without a"
	note "session, decide deliberately whether to persist it:"
	note "  loginctl enable-linger $USER"
	note "that choice is yours; this script does not make it."
	;;
darwin)
	DEST="$HOME/Library/LaunchAgents"
	note ""
	note "installing into $DEST"
	run mkdir -p "$DEST"
	if [ "$DRY_RUN" -eq 0 ] && [ ! -f "$TMP/$LABEL.plist" ]; then
		die "$LABEL.plist was not rendered"
	fi
	run install -m 0644 "$TMP/$LABEL.plist" "$DEST/$LABEL.plist"
	if [ "$ENABLE" -eq 1 ]; then
		run launchctl bootstrap "gui/$(id -u)" "$DEST/$LABEL.plist"
		if [ "$DRY_RUN" -eq 0 ]; then
			note ""
			launchctl print "gui/$(id -u)/$LABEL" || true
		fi
	else
		note ""
		note "not started. load it with:"
		note "  launchctl bootstrap gui/$(id -u) $DEST/$LABEL.plist"
	fi
	note ""
	note "launchd runs without a terminal. If the scope lives on an external"
	note "volume or in a protected location, grant Full Disk Access to"
	note "  $BIN"
	note "not to your terminal. Run 'dustkeeper doctor' afterwards to confirm."
	;;
esac

note ""
note "next: $BIN doctor"
note "      $BIN run --dry-run"
