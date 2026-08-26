#!/bin/sh
# Back up what a colab-fleetd deploy replaces, so it can be put back.
#
# WHY THIS EXISTS
#
# scripts/deploy.sh installs atomically (write beside, rename into place) which
# prevents a half-written binary — but it does not keep what it replaced. That
# is fine when the previous binary can be rebuilt from git. It is NOT fine here:
#
#   both machines are currently running a build stamped `modified: true`,
#   meaning it was built from a dirty tree. There is no commit that reproduces
#   it. If a deploy goes wrong, the ONLY way back to today's known-working
#   service is a copy of the binary file itself.
#
# So this runs first, every time, and refuses rather than proceeding on doubt.
#
# What it captures, per machine:
#   - the installed binary (the irreplaceable part)
#   - the state directory (session records, idempotency, events)
#   - the revision the running service reports, for verifying a revert landed
#
# Usage:
#   scripts/fleet-backup.sh local
#   scripts/fleet-backup.sh <ssh-host>
#
# Env (no defaults — the same discipline deploy.sh applies, for the same reason):
#   FLEET_BIN         installed binary path on the target
#   FLEET_STATE_DIR   state directory on the target
#   FLEET_HEALTH_URL  health endpoint, curled ON THE TARGET
#   FLEET_HEALTH_TOKEN       literal bearer token, taken literally.
#                            Takes precedence over FLEET_HEALTH_TOKEN_FILE below.
#   FLEET_HEALTH_TOKEN_FILE  path to a token file, read ON THE TARGET via `cat`.
#                            At least one of these is REQUIRED whenever
#                            FLEET_HEALTH_URL is set.

set -eu

HOST=${1:-}
[ -n "$HOST" ] || { echo "usage: $0 local|<ssh-host>" >&2; exit 2; }

for v in FLEET_BIN FLEET_STATE_DIR FLEET_HEALTH_URL; do
	eval "val=\${$v:-}"
	[ -n "$val" ] || { echo "backup: $v is required and has no default" >&2; exit 2; }
done

if [ -n "${FLEET_HEALTH_TOKEN:-}" ]; then
	AUTH_HEADER="Authorization: Bearer ${FLEET_HEALTH_TOKEN}"
elif [ -n "${FLEET_HEALTH_TOKEN_FILE:-}" ]; then
	AUTH_HEADER="Authorization: Bearer \$(cat ${FLEET_HEALTH_TOKEN_FILE})"
else
	echo "backup: no verification credential was given." >&2
	echo "        Set FLEET_HEALTH_TOKEN (a literal bearer token) or" >&2
	echo "        FLEET_HEALTH_TOKEN_FILE (a path, read on the target via cat) to" >&2
	echo "        the credential the target's service actually accepts." >&2
	exit 2
fi

if [ "$HOST" = "local" ]; then
	run() { sh -c "$1"; }
	fetch() { cp "$1" "$2"; }
else
	run() { ssh "$HOST" "$1"; }
	fetch() { scp -q "${HOST}:$1" "$2"; }
fi

STAMP=$(run 'date -u +%Y%m%dT%H%M%SZ')
LABEL=$(run 'hostname -s')
DEST="$HOME/.local/state/colab-fleet-backups/${LABEL}-${STAMP}"

echo "backup: target=${HOST} label=${LABEL} stamp=${STAMP}"

# 1. The running revision — captured BEFORE anything moves, because after a bad
#    deploy this is the only record of what was known to work.
RUNNING=$(run "curl -s -H \"${AUTH_HEADER}\" ${FLEET_HEALTH_URL}") || true
echo "$RUNNING" | grep -q '"build"' || {
	echo "backup: health did not return a build — refusing to back up a service I cannot identify" >&2
	echo "backup: response was: $(echo "$RUNNING" | head -c 200)" >&2
	exit 1
}

# 2. The binary. This is the irreplaceable artifact.
run "test -f ${FLEET_BIN}" || { echo "backup: no binary at ${FLEET_BIN}" >&2; exit 1; }
SUM_REMOTE=$(run "shasum -a 256 ${FLEET_BIN} | cut -d' ' -f1")
SIZE_REMOTE=$(run "wc -c < ${FLEET_BIN} | tr -d ' '")

mkdir -p "$DEST"
fetch "${FLEET_BIN}" "${DEST}/colab-fleetd"
SUM_LOCAL=$(shasum -a 256 "${DEST}/colab-fleetd" | cut -d' ' -f1)

[ "$SUM_REMOTE" = "$SUM_LOCAL" ] || {
	echo "backup: checksum mismatch after copy — the backup is NOT trustworthy" >&2
	echo "  on target: $SUM_REMOTE" >&2
	echo "  copied:    $SUM_LOCAL" >&2
	rm -f "${DEST}/colab-fleetd"
	exit 1
}

# 3. The state directory, as a tarball taken on the target.
run "tar -czf /tmp/fleet-state-${STAMP}.tgz -C ${FLEET_STATE_DIR} ." >/dev/null 2>&1 || {
	echo "backup: could not archive ${FLEET_STATE_DIR}" >&2; exit 1; }
fetch "/tmp/fleet-state-${STAMP}.tgz" "${DEST}/state.tgz"
run "rm -f /tmp/fleet-state-${STAMP}.tgz"

# 4. The manifest — what this backup is, and what putting it back should produce.
cat > "${DEST}/manifest.json" <<EOF
{
  "label": "${LABEL}",
  "stamp": "${STAMP}",
  "target": "${HOST}",
  "binary": {
    "path": "${FLEET_BIN}",
    "sha256": "${SUM_REMOTE}",
    "bytes": ${SIZE_REMOTE}
  },
  "stateDir": "${FLEET_STATE_DIR}",
  "healthUrl": "${FLEET_HEALTH_URL}",
  "runningHealth": ${RUNNING}
}
EOF

echo "backup: OK -> ${DEST}"
echo "  binary sha256: ${SUM_REMOTE}"
echo "  revision:      $(echo "$RUNNING" | python3 -c 'import json,sys; print((json.load(sys.stdin).get("build") or {}).get("revision","?"))' 2>/dev/null || echo '?')"
echo "  revert with:   scripts/fleet-revert.sh ${HOST} ${DEST}"
