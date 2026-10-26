#!/bin/sh
# Remove the dustkeeper timer for the current user.
#
# Order matters and is not configurable:
#   1. stop the schedule, so nothing new starts
#   2. ask the running instance to reach a safe boundary (pause)
#   3. refuse to continue while operations are still pending
#   4. remove the unit files
#
# It never deletes the state directory or a quarantine vault. Those hold the
# only record of what was moved; removing them is a separate, deliberate act
# described in docs/operations.md.
#
#   ./packaging/uninstall.sh --dry-run     show the plan, change nothing
#   ./packaging/uninstall.sh               stop and remove the timer
#   ./packaging/uninstall.sh --force       continue even with pending work
#   ./packaging/uninstall.sh --purge-state also delete the state directory
#                                          (never touches a vault)

set -eu

DRY_RUN=0
FORCE=0
PURGE_STATE=0
LABEL="dustkeeper"
BIN=""

usage() {
	cat <<'EOF'
usage: uninstall.sh [--dry-run] [--force] [--purge-state]
                    [--bin PATH] [--label NAME]

  --dry-run      print every action, execute none
  --force        proceed even when operations are still pending
  --purge-state  also delete the state directory (the vault is never touched)
  --bin PATH     path to the dustkeeper binary (default: found on PATH)
  --label NAME   unit / launchd label (default: dustkeeper)
EOF
}

die() { printf 'uninstall: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

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
	--force) FORCE=1 ;;
	--purge-state) PURGE_STATE=1 ;;
	--bin) shift; [ $# -gt 0 ] || die "--bin needs a value"; BIN="$1" ;;
	--label) shift; [ $# -gt 0 ] || die "--label needs a value"; LABEL="$1" ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $1 (try --help)" ;;
	esac
	shift
done

[ "$(id -u)" -ne 0 ] || die "do not run this as root"

if [ -z "$BIN" ]; then
	BIN="$(command -v dustkeeper 2>/dev/null || true)"
fi

note "uninstalling dustkeeper timer"
note ""

# 1. Stop the schedule first. Everything below assumes nothing new will start.
case "$(uname -s)" in
Linux)
	if systemctl --user list-unit-files "$LABEL.timer" >/dev/null 2>&1; then
		run systemctl --user disable --now "$LABEL.timer"
	fi
	;;
Darwin)
	PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
	if [ -f "$PLIST" ]; then
		run launchctl bootout "gui/$(id -u)" "$PLIST" || true
	fi
	;;
*)
	note "unsupported platform: $(uname -s); nothing to stop"
	;;
esac

# 2. Ask the instance to stop taking new work. In-flight files still finish.
if [ -n "$BIN" ] && [ -x "$BIN" ]; then
	note ""
	note "asking the instance to stop starting new work"
	run "$BIN" pause || note "  (pause reported an error; continuing so the files can be removed)"
else
	note ""
	note "dustkeeper binary not found; skipping the pause step"
fi

# 3. Refuse to walk away from work that has no explanation yet.
if [ -n "$BIN" ] && [ -x "$BIN" ] && [ "$DRY_RUN" -eq 0 ]; then
	if "$BIN" recover inspect --json 2>/dev/null | grep -q '"count": 0'; then
		note "  no pending operations"
	else
		if [ "$FORCE" -eq 1 ]; then
			note ""
			note "  WARNING: operations are still pending and --force was given."
			note "  Pending means: a file may have moved and its record was never"
			note "  finished. Run 'dustkeeper recover reconcile' before deleting"
			note "  anything, or you will have objects nobody can explain."
		else
			note ""
			die "operations are still pending; run 'dustkeeper recover reconcile',
     then 'dustkeeper recover inspect' until the count is 0, or pass --force"
		fi
	fi
fi

# 4. Remove the unit files.
note ""
note "removing the unit files"
case "$(uname -s)" in
Linux)
	run rm -f "$HOME/.config/systemd/user/$LABEL.service" "$HOME/.config/systemd/user/$LABEL.timer"
	run systemctl --user daemon-reload
	note "  if lingering was enabled for this user, turn it off deliberately:"
	note "    loginctl disable-linger $USER"
	;;
Darwin)
	run rm -f "$HOME/Library/LaunchAgents/$LABEL.plist"
	;;
esac

# 5. Optional: the state directory. The vault is never included.
if [ "$PURGE_STATE" -eq 1 ]; then
	STATE="${DUSTKEEPER_STATE_DIR:-}"
	if [ -z "$STATE" ]; then
		case "$(uname -s)" in
		Darwin) STATE="$HOME/Library/Application Support/Dustkeeper" ;;
		*) STATE="${XDG_STATE_HOME:-$HOME/.local/state}/dustkeeper" ;;
		esac
	fi
	note ""
	note "deleting the state directory: $STATE"
	note "  this discards the audit trail and the retention records."
	note "  quarantine vaults live inside the scopes and are NOT touched."
	run rm -rf "$STATE"
fi

note ""
note "done. Quarantine vaults are still on disk."
note "Check each scope before removing its .dustkeeper directory:"
note "  dustkeeper quarantine list --scope <id>"
note "  dustkeeper quarantine restore --object <item_id>"
