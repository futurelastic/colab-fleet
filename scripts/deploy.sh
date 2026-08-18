#!/bin/sh
# Build colab-fleetd for a target host, install it, restart the service, and
# verify that what is now running is what was just built.
#
# WHY THIS EXISTS
#
# It replaces cross-compiling and copying by hand, which is how two machines in
# one fleet came to run different builds without anyone noticing. The older one
# still had a bug the newer had fixed, and the symptom looked like a defect in
# code that no longer existed. Every step below exists to make that specific
# failure impossible rather than unlikely:
#
#   - the build is stamped from version control, so it HAS an identity;
#   - a dirty tree is refused by default, because a binary built from
#     uncommitted changes has no identity to report;
#   - the running service is asked what it is, AFTER restart, and the answer is
#     compared against what was uploaded. A deploy that does not verify is a
#     deploy that can silently not have happened.
#
# The last one matters most. The failure mode was never "the copy failed
# loudly" — it was a service that kept happily serving the old binary.
#
# USAGE
#
#   scripts/deploy.sh HOST [REMOTE_PATH]
#   scripts/deploy.sh local [REMOTE_PATH]
#
#   HOST          ssh destination, as your ssh config understands it, or the
#                 literal word "local" to deploy to the machine you are
#                 standing on — no ssh, no scp, otherwise every step below is
#                 identical, including the read-back at the end. This is the
#                 ordinary case: the machine most likely to need a deploy is
#                 the one this session is already running on.
#   REMOTE_PATH   where the binary lands. Default: ~/bin/colab-fleetd
#
# Environment:
#   GOOS, GOARCH        target platform. Defaults to the remote host's own,
#                       discovered over ssh — guessing is how you ship an
#                       arm64 binary to an amd64 host and learn about it from
#                       an exec format error.
#   FLEET_RESTART       command run on the host to restart the service. If
#                       unset, the script installs the binary and tells you to
#                       restart it yourself; it will not invent a service
#                       manager.
#   FLEET_HEALTH_URL    URL to verify against, curled ON THE HOST. If unset,
#                       verification is skipped and the script says so loudly,
#                       because an unverified deploy is the thing this script
#                       was written to stop.
#   ALLOW_DIRTY=1       build from a modified tree anyway. The resulting
#                       binary reports itself as dirty and will never compare
#                       equal to anything, including the next deploy.
#
# Deliberately absent: hostnames, ports, paths and service labels. Those are
# operational facts, and this repository is public.

set -eu

HOST=${1:-}
REMOTE_PATH=${2:-'~/bin/colab-fleetd'}

if [ -z "$HOST" ]; then
	echo "usage: scripts/deploy.sh HOST [REMOTE_PATH]" >&2
	echo "       scripts/deploy.sh local [REMOTE_PATH]" >&2
	exit 2
fi

cd "$(dirname "$0")/.."

# --- local vs. remote, behind one seam --------------------------------------
#
# Every step past this point is written once and runs identically either way.
# What differs is only how a command reaches its target and how a file gets
# there — ssh/scp for a peer, direct execution for this machine.
if [ "$HOST" = "local" ]; then
	run() { sh -c "$1"; }
	put() { cp "$1" "$2"; }
else
	run() { ssh "$HOST" "$1"; }
	put() { scp -q "$1" "${HOST}:$2"; }
fi

# --- refuse to ship something that cannot be identified ---------------------
if [ "${ALLOW_DIRTY:-0}" != "1" ]; then
	if ! git diff --quiet HEAD 2>/dev/null; then
		echo "deploy: working tree is modified." >&2
		echo "        A binary built from uncommitted changes has no build" >&2
		echo "        identity — it can never be compared against a peer or" >&2
		echo "        against itself. Commit, or set ALLOW_DIRTY=1." >&2
		exit 1
	fi
fi

REV=$(git rev-parse HEAD)
echo "deploy: revision ${REV}"

# --- target platform, asked rather than assumed -----------------------------
if [ -z "${GOOS:-}" ] || [ -z "${GOARCH:-}" ]; then
	echo "deploy: asking ${HOST} what it is"
	REMOTE_UNAME=$(run 'uname -s; uname -m')
	REMOTE_OS=$(echo "$REMOTE_UNAME" | sed -n 1p)
	REMOTE_ARCH=$(echo "$REMOTE_UNAME" | sed -n 2p)
	case "$REMOTE_OS" in
	Darwin) GOOS=${GOOS:-darwin} ;;
	Linux) GOOS=${GOOS:-linux} ;;
	*)
		echo "deploy: unrecognised remote OS '${REMOTE_OS}'; set GOOS explicitly" >&2
		exit 1
		;;
	esac
	case "$REMOTE_ARCH" in
	arm64 | aarch64) GOARCH=${GOARCH:-arm64} ;;
	x86_64 | amd64) GOARCH=${GOARCH:-amd64} ;;
	*)
		echo "deploy: unrecognised remote arch '${REMOTE_ARCH}'; set GOARCH explicitly" >&2
		exit 1
		;;
	esac
fi
echo "deploy: building for ${GOOS}/${GOARCH}"

# -buildvcs=true is the default, and is named here because it is the entire
# mechanism behind build identity: without the stamp, /v1/health reports
# "unknown" and skew becomes undetectable again.
TMPBIN=$(mktemp -t colab-fleetd.XXXXXX)
trap 'rm -f "$TMPBIN"' EXIT
GOOS="$GOOS" GOARCH="$GOARCH" go build -buildvcs=true -o "$TMPBIN" ./cmd/colab-fleetd

# --- install ----------------------------------------------------------------
#
# Uploaded beside the target and renamed into place: a rename is atomic, while
# writing over a running binary is how you get a half-written executable and a
# service that will not start.
echo "deploy: installing to ${HOST}:${REMOTE_PATH}"
run "mkdir -p \"\$(dirname ${REMOTE_PATH})\""
put "$TMPBIN" "${REMOTE_PATH}.incoming"
run "chmod 0755 ${REMOTE_PATH}.incoming && mv ${REMOTE_PATH}.incoming ${REMOTE_PATH}"

# --- restart ----------------------------------------------------------------
if [ -n "${FLEET_RESTART:-}" ]; then
	echo "deploy: restarting"
	run "$FLEET_RESTART"
else
	echo "deploy: no FLEET_RESTART set — binary installed, service NOT restarted."
	echo "        The old build is still serving until you restart it."
fi

# --- verify -----------------------------------------------------------------
#
# The step that makes this a deploy rather than a copy.
if [ -z "${FLEET_HEALTH_URL:-}" ]; then
	echo "deploy: WARNING no FLEET_HEALTH_URL — nothing verified."
	echo "        You have no evidence the running service is the build above."
	exit 0
fi

echo "deploy: verifying"
i=0
while [ "$i" -lt 10 ]; do
	RUNNING=$(run "curl -fsS -H \"Authorization: Bearer \$(cat ~/.config/colab-fleet/token)\" ${FLEET_HEALTH_URL}" 2>/dev/null |
		tr ',' '\n' | sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1) || true
	if [ -n "$RUNNING" ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done

if [ -z "$RUNNING" ]; then
	echo "deploy: FAILED to read a build identity from the running service." >&2
	echo "        Either it did not come back up, or it is running a binary" >&2
	echo "        old enough not to report one — which is itself the problem." >&2
	exit 1
fi

if [ "$RUNNING" != "$REV" ]; then
	echo "deploy: FAILED — running revision ${RUNNING} is not ${REV}." >&2
	echo "        The service restarted onto a different binary than the one" >&2
	echo "        just installed. Check that FLEET_RESTART starts ${REMOTE_PATH}." >&2
	exit 1
fi

echo "deploy: verified — ${HOST} is running ${REV}"
