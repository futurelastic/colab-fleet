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
#   scripts/deploy.sh HOST REMOTE_PATH
#   scripts/deploy.sh local REMOTE_PATH
#
#   HOST          ssh destination, as your ssh config understands it, or the
#                 literal word "local" to deploy to the machine you are
#                 standing on — no ssh, no scp, otherwise every step below is
#                 identical, including the read-back at the end. This is the
#                 ordinary case: the machine most likely to need a deploy is
#                 the one this session is already running on.
#   REMOTE_PATH   where the binary lands. Required — colab-fleet issue #66:
#                 an earlier default of ~/bin/colab-fleetd did not match what
#                 the service manager on either machine actually execs, so a
#                 deploy could install a correct, freshly-stamped binary to a
#                 path nothing runs, restart the service onto the OLD one,
#                 and report FAILED with a message that pointed at
#                 FLEET_RESTART — the one thing that was actually right. That
#                 failure is indistinguishable from the one this script
#                 exists to prevent (two machines silently running different
#                 builds), so it joins FLEET_RESTART and FLEET_HEALTH_URL
#                 below: an operational fact this script will not guess.
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
#   FLEET_HEALTH_TOKEN       bearer token to verify with, taken literally.
#                             Takes precedence over FLEET_HEALTH_TOKEN_FILE
#                             below when set (colab-fleet #93 — see the verify
#                             section for why this exists).
#   FLEET_HEALTH_TOKEN_FILE  path to a token file, read ON THE HOST via `cat`.
#                             One of FLEET_HEALTH_TOKEN or this is REQUIRED
#                             whenever FLEET_HEALTH_URL is set (colab-fleet
#                             #108) — the script no longer falls back to a
#                             hardcoded path. It has no way to tell a
#                             single-token deployment from a principal-table
#                             one before asking, and the two need different
#                             credentials, so a silent default is right for
#                             one and silently wrong for the other. If this
#                             host's operator convention is a token file at
#                             ~/.config/colab-fleet/token, set it explicitly:
#                             that path is no longer assumed on your behalf.
#   FLEET_VERIFY_TIMEOUT      seconds to poll the health URL before giving up.
#                             Default 180 — colab-fleet #93 measured a real,
#                             successful startup taking 98s under load, so the
#                             deadline needs slack above that, not just above
#                             a quiet-box startup.
#   FLEET_VERIFY_INTERVAL     seconds between polls while waiting. Default 2.
#   ALLOW_DIRTY=1       build from a modified tree anyway. The resulting
#                       binary reports itself as dirty and will never compare
#                       equal to anything, including the next deploy.
#
# Deliberately absent: hostnames, ports, paths and service labels. Those are
# operational facts, and this repository is public.

set -eu

HOST=${1:-}
REMOTE_PATH=${2:-}

if [ -z "$HOST" ] || [ -z "$REMOTE_PATH" ]; then
	echo "usage: scripts/deploy.sh HOST REMOTE_PATH" >&2
	echo "       scripts/deploy.sh local REMOTE_PATH" >&2
	echo "" >&2
	echo "REMOTE_PATH is required (colab-fleet #66) — it must match what the" >&2
	echo "service manager on that machine execs, and this script has no way" >&2
	echo "to know that on your behalf." >&2
	exit 2
fi

cd "$(dirname "$0")/.."

# --- local vs. remote, behind one seam --------------------------------------
#
# Every step past this point is written once and runs identically either way.
# What differs is only how a command reaches its target and how a file gets
# there — ssh/scp for a peer, direct execution for this machine.
if [ "$HOST" = "local" ]; then
	# A leading ~ is expanded by the REMOTE shell over ssh, and by nothing at
	# all here: `cp` takes it literally and fails on a directory named "~".
	# The default path carries one, so local mode broke on its own default —
	# found by using it, which is the only way this class of bug is found.
	case "$REMOTE_PATH" in
	'~/'*) REMOTE_PATH="$HOME/${REMOTE_PATH#\~/}" ;;
	'~') REMOTE_PATH="$HOME" ;;
	esac
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

# colab-fleet #93: verification used to assume the token file under the
# service's own config directory, read ON THE HOST, was always a credential
# that host's service accepts. Measured false on a federated fleet: the
# credential that answers for a peer can be one only the machine running THIS
# script holds, so the host's own file answered 401 for a perfectly healthy
# service. Take the credential as configuration instead of assuming it.
#
# colab-fleet #108: #93 stopped short of that for whoever set NEITHER
# variable, keeping a hardcoded fallback (~/.config/colab-fleet/token, still
# read ON THE HOST — the fallback was never the "wrong machine" bug, that was
# already fixed). That fallback is correct for a single-token deployment,
# where the file conventionally holds the same value as the service's own
# token, and silently wrong for a principal-table deployment, where that
# value is never one of the table's principals — this script has no way to
# tell which kind of host it is about to talk to before it asks.
#
# Two other defaults were considered and both lose to asking: reading a
# principal out of the host's table needs a path this script is never given
# (FLEET_CONFIG is the daemon's own env var, not threaded through here) and
# then a choice of WHICH principal if the table holds more than one — a
# second guess, not a smaller one. Reusing the service's own outbound peer
# identity (colab-fleet #98's "system:"+machine) has the same access problem,
# plus nothing establishes that identity also carries a local read grant on
# THIS host's table — #98 only required the *peer's* table to grant it one,
# for a different purpose than health-checking this host. Neither trades the
# guess away; both just move where it happens. So: refuse to guess, and fail
# before the network call, the same "not a smarter guess, refusing to guess
# at all" call this script already made for REMOTE_PATH (#66).
if [ -n "${FLEET_HEALTH_TOKEN:-}" ]; then
	AUTH_HEADER="Authorization: Bearer ${FLEET_HEALTH_TOKEN}"
elif [ -n "${FLEET_HEALTH_TOKEN_FILE:-}" ]; then
	AUTH_HEADER="Authorization: Bearer \$(cat ${FLEET_HEALTH_TOKEN_FILE})"
else
	echo "deploy: FLEET_HEALTH_URL is set but no verification credential was given." >&2
	echo "        Set FLEET_HEALTH_TOKEN (a literal bearer token) or" >&2
	echo "        FLEET_HEALTH_TOKEN_FILE (a path, read on the host via cat) to" >&2
	echo "        the credential THIS deployment's service actually accepts." >&2
	echo "        A single-token deployment and a principal-table deployment" >&2
	echo "        need different credentials, and this script cannot tell" >&2
	echo "        which one it is about to talk to — see colab-fleet #108." >&2
	exit 2
fi

# colab-fleet #93: a single probe cannot tell "not up yet" from "not coming
# up" — startup does real work (a trust-seed pass and a session
# reconciliation) that scales with how much the machine is carrying, and was
# measured taking 98 seconds on the busiest machine. Poll to a deadline
# instead, generous enough to clear that: the cost of waiting a few minutes
# longer is nothing next to the cost of an operator rolling back, or
# abandoning, a deploy that was already fine.
FLEET_VERIFY_TIMEOUT=${FLEET_VERIFY_TIMEOUT:-180}
FLEET_VERIFY_INTERVAL=${FLEET_VERIFY_INTERVAL:-2}

echo "deploy: verifying (up to ${FLEET_VERIFY_TIMEOUT}s)"
START=$(date +%s)
NEXT_NOTICE=30
RUNNING=""
STATUS=""
BODY=""
while :; do
	ELAPSED=$(($(date +%s) - START))
	if [ "$ELAPSED" -ge "$FLEET_VERIFY_TIMEOUT" ]; then
		break
	fi

	# No -f: colab-fleet #66 found a health URL pointing at the WRONG
	# service (a stray port, an unrelated server on the same host) producing
	# a near-identical FAILED to a service that never came back up — because
	# -f discards the body on any non-2xx status, so a 403 from something
	# else entirely looked exactly like no answer at all. The status rides
	# along on its own trailing line so it survives whatever shape the body
	# is, and both are kept for the diagnostic below regardless of status.
	RAW=$(run "curl -sS -w '\\n%{http_code}' -H \"${AUTH_HEADER}\" ${FLEET_HEALTH_URL}" 2>/dev/null) && CURL_OK=1 || CURL_OK=0
	if [ "$CURL_OK" = "1" ]; then
		STATUS=$(printf '%s\n' "$RAW" | tail -n1)
		BODY=$(printf '%s\n' "$RAW" | sed '$d')
		RUNNING=$(printf '%s\n' "$BODY" | tr ',' '\n' | sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1)
	else
		STATUS=""
		BODY=""
		RUNNING=""
	fi
	if [ -n "$RUNNING" ]; then
		break
	fi
	if [ -n "$STATUS" ]; then
		# Reached something concrete (a real HTTP status) rather than
		# nothing at all. Retrying blindly will not fix a wrong URL or a
		# rejected credential — that is a configuration problem, not a
		# timing one — so stop here instead of waiting out the whole
		# deadline on an answer that will not change.
		break
	fi

	if [ "$ELAPSED" -ge "$NEXT_NOTICE" ]; then
		echo "deploy: not up yet (${ELAPSED}s elapsed) — this is expected while the service is still starting"
		NEXT_NOTICE=$((NEXT_NOTICE + 30))
	fi
	sleep "$FLEET_VERIFY_INTERVAL"
done

if [ -z "$RUNNING" ]; then
	if [ -n "$STATUS" ]; then
		# Reached SOMETHING, and it did not report a build identity. #66:
		# this is not "it did not come back up" — the connection itself
		# worked — so say what actually answered instead of the one
		# explanation that fits the OTHER failure. #93: this also covers a
		# wrong credential — a 401 with no build identity in the body, from
		# a service that is otherwise perfectly healthy, is what a wrong
		# FLEET_HEALTH_TOKEN or FLEET_HEALTH_TOKEN_FILE produces.
		echo "deploy: FAILED — ${FLEET_HEALTH_URL} answered (status ${STATUS}) but the" >&2
		echo "        body carried no build identity. That means the URL names" >&2
		echo "        something that is not this service, a binary old enough not" >&2
		echo "        to report one, or a credential this service does not accept" >&2
		echo "        (see FLEET_HEALTH_TOKEN / FLEET_HEALTH_TOKEN_FILE) — not that" >&2
		echo "        the restart failed. First line of what came back:" >&2
		printf '%s\n' "$BODY" | head -1 | sed 's/^/            /' >&2
	else
		# #93: distinct from the branch above on purpose. Nothing answered at
		# all for the full deadline — unlike a single failed probe mid-poll,
		# this is not "not up yet": FLEET_VERIFY_TIMEOUT was chosen to clear
		# a slow, heavily-loaded start, so a service that still has not
		# answered by then is the genuine "did not come up".
		echo "deploy: FAILED — ${FLEET_HEALTH_URL} did not come up within ${FLEET_VERIFY_TIMEOUT}s." >&2
		echo "        Raise FLEET_VERIFY_TIMEOUT if this machine is carrying more" >&2
		echo "        than that normally covers, then check it by hand before" >&2
		echo "        assuming the restart itself failed." >&2
	fi
	exit 1
fi

if [ "$RUNNING" != "$REV" ]; then
	echo "deploy: FAILED — running revision ${RUNNING} is not ${REV}." >&2
	echo "        The service restarted onto a different binary than the one" >&2
	echo "        just installed. Check that FLEET_RESTART starts ${REMOTE_PATH}." >&2
	exit 1
fi

echo "deploy: verified — ${HOST} is running ${REV}"
