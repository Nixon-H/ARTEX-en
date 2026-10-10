#!/bin/sh
# ARTEX guardian startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in background
#   ./start.sh -addr :9000           Extra args are passed through to artex as-is
#
# It does one thing only: run artex, and decide whether to restart it based on the exit code.
#
#   0      User stopped normally      -> exit loop
#   75     Program requested restart  -> rerun immediately ("one-click update" or "rollback" clicked in UI)
#   Other  Crash                      -> rerun with backoff (1->2->4... up to 60 seconds)
#
# Deliberately no downloading, SHA256 verification, or swapping here: that logic would need two copies in sh and bat,
# yet it is exactly the part that must never go wrong -- once a broken binary is swapped in, this script would faithfully
# restart it over and over, leaving the user to fix the machine by hand. So verification/swapping all stays in Go (selfupdate package),
# completed by artex itself at startup, keeping the script foolproof.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (i.e. this script),
# not to child processes. Without forwarding, artex never receives the signal, cannot shut down gracefully, and is SIGKILLed after 10 seconds,
# killing running tasks abruptly mid-flight.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the child is actually still shutting down gracefully,
	# so wait must be called again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] Stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] Exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready: after rerun, artex will complete the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] Restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] Exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
