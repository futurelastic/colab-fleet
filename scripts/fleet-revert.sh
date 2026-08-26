#!/bin/sh
# Put back a binary captured by scripts/fleet-backup.sh, restart, and verify
# that what is running is what was put back.
#
# This is the other half of deploy.sh's contract. deploy.sh verifies that a
# deploy HAPPENED; this verifies that an undo happened. Both matter for the
# same reason: the failure mode is never a loud one, it is a service that keeps
# happily serving something other than what you think.
#
# It deliberately does NOT restore the state directory by default. State is
# forward-compatible far more often than it is not, and a state rollback can
# discard real session records that the new binary wrote correctly. Restoring
# state is a separate, explicit act — pass --with-state only when the binary
# rollback alone left the service unable to start.
#
# Usage:
#   scripts/fleet-revert.sh local|<ssh-host> <backup-dir> [--with-state]
#
# Env (same discipline as deploy.sh — no guessing operational facts):
#   FLEET_BIN         installed binary path on the target
#   FLEET_RESTART     command run on the target to restart the service
#   FLEET_HEALTH_URL  health endpoint, curled ON THE TARGET
#   FLEET_HEALTH_TOKEN       literal bearer token, taken literally.
#                            Takes precedence over FLEET_HEALTH_TOKEN_FILE below.
#   FLEET_HEALTH_TOKEN_FILE  path to a token file, read ON THE TARGET via `cat`.
#                            At least one of these is REQUIRED whenever
#                            FLEET_HEALTH_URL is set.
#   FLEET_STATE_DIR   required only with --with-state

set -eu

HOST=${1:-}
DIR=${2:-}
WITH_STATE=${3:-}
[ -n "$HOST" ] && [ -n "$DIR" ] || { echo "usage: $0 local|<ssh-host> <backup-dir> [--with-state]" >&2; exit 2; }
[ -f "${DIR}/colab-fleetd" ] || { echo "revert: no binary in ${DIR}" >&2; exit 1; }
[ -f "${DIR}/manifest.json" ] || { echo "revert: no manifest in ${DIR} — refusing an unidentified backup" >&2; exit 1; }

for v in FLEET_BIN FLEET_RESTART FLEET_HEALTH_URL; do
	eval "val=\${$v:-}"
	[ -n "$val" ] || { echo "revert: $v is required and has no default" >&2; exit 2; }
done

if [ -n "${FLEET_HEALTH_TOKEN:-}" ]; then
	AUTH_HEADER="Authorization: Bearer ${FLEET_HEALTH_TOKEN}"
elif [ -n "${FLEET_HEALTH_TOKEN_FILE:-}" ]; then
	AUTH_HEADER="Authorization: Bearer \$(cat ${FLEET_HEALTH_TOKEN_FILE})"
else
	echo "revert: no verification credential was given." >&2
	echo "        Set FLEET_HEALTH_TOKEN (a literal bearer token) or" >&2
	echo "        FLEET_HEALTH_TOKEN_FILE (a path, read on the target via cat) to" >&2
	echo "        the credential the target's service actually accepts." >&2
	exit 2
fi

if [ "$HOST" = "local" ]; then
	run() { sh -c "$1"; }
	put() { cp "$1" "$2"; }
else
	run() { ssh "$HOST" "$1"; }
	put() { scp -q "$1" "${HOST}:$2"; }
fi

WANT_SHA=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["binary"]["sha256"])' "${DIR}/manifest.json")
WANT_REV=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(((d.get("runningHealth") or {}).get("build") or {}).get("revision",""))' "${DIR}/manifest.json")

# The backup must still be intact. A corrupted backup discovered DURING a
# rollback is the worst possible moment to discover it.
HAVE_SHA=$(shasum -a 256 "${DIR}/colab-fleetd" | cut -d' ' -f1)
[ "$HAVE_SHA" = "$WANT_SHA" ] || {
	echo "revert: backup is corrupt — sha does not match its own manifest" >&2
	echo "  manifest: $WANT_SHA" >&2
	echo "  on disk:  $HAVE_SHA" >&2
	exit 1
}

echo "revert: target=${HOST} restoring ${WANT_REV:-<unknown revision>}"

# Install atomically — same reason deploy.sh does: never write over a running
# binary in place.
TMP="${FLEET_BIN}.revert.$$"
put "${DIR}/colab-fleetd" "$TMP"
run "chmod 755 ${TMP} && mv -f ${TMP} ${FLEET_BIN}"

CHECK=$(run "shasum -a 256 ${FLEET_BIN} | cut -d' ' -f1")
[ "$CHECK" = "$WANT_SHA" ] || {
	echo "revert: installed binary does not match the backup — STOP, do not restart" >&2
	exit 1
}
echo "revert: binary in place (${WANT_SHA})"

if [ "$WITH_STATE" = "--with-state" ]; then
	[ -n "${FLEET_STATE_DIR:-}" ] || { echo "revert: --with-state needs FLEET_STATE_DIR" >&2; exit 2; }
	[ -f "${DIR}/state.tgz" ] || { echo "revert: no state.tgz in backup" >&2; exit 1; }
	echo "revert: restoring state directory (explicitly requested)"
	put "${DIR}/state.tgz" "/tmp/fleet-state-revert.$$.tgz"
	run "mkdir -p ${FLEET_STATE_DIR} && tar -xzf /tmp/fleet-state-revert.$$.tgz -C ${FLEET_STATE_DIR} && rm -f /tmp/fleet-state-revert.$$.tgz"
fi

echo "revert: restarting"
run "$FLEET_RESTART"

# Verify by asking the service what it is — poll, because startup does real
# work (trust seed, session reconciliation) and the busiest machine is the one
# most likely to be wrongly declared dead. deploy.sh learned this the hard way
# (a 98s startup); the same deadline applies here.
DEADLINE=${FLEET_VERIFY_TIMEOUT:-180}
INTERVAL=${FLEET_VERIFY_INTERVAL:-3}
ELAPSED=0
while [ "$ELAPSED" -lt "$DEADLINE" ]; do
	BODY=$(run "curl -s -H \"${AUTH_HEADER}\" ${FLEET_HEALTH_URL}" 2>/dev/null) || BODY=""
	GOT=$(printf '%s' "$BODY" | python3 -c 'import json,sys
try: print(((json.load(sys.stdin).get("build") or {}).get("revision") or ""))
except Exception: print("")' 2>/dev/null || echo "")
	if [ -n "$GOT" ]; then
		if [ -z "$WANT_REV" ] || [ "$GOT" = "$WANT_REV" ]; then
			echo "revert: OK — running ${GOT}"
			exit 0
		fi
		echo "revert: service answered but reports ${GOT}, wanted ${WANT_REV}" >&2
		exit 1
	fi
	sleep "$INTERVAL"
	ELAPSED=$((ELAPSED + INTERVAL))
	echo "revert: not up yet (${ELAPSED}s/${DEADLINE}s)"
done

echo "revert: service did not come back within ${DEADLINE}s — binary IS in place, restart it by hand" >&2
exit 1
