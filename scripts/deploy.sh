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
#   FLEET_MODULE_SOURCES    OPTIONAL. Space-separated name=source entries, one
#                             per optional delivery module to install beside
#                             the daemon (colab-fleet #185). Each is fetched
#                             and built with THIS user's own access by
#                             scripts/fetch-module.sh; a source this user
#                             cannot fetch installs nothing and says nothing.
#                             A malformed entry gets one warning line. Unset or
#                             empty: this step does not run at all.
#   FLEET_MODULES_DIR       where those modules are installed on the host.
#                             Default: <parent of REMOTE_PATH's directory>/
#                             libexec/colab-fleet/modules - the same place the
#                             daemon looks. Set the same value in the
#                             service's own environment if you set it here.
#   ALLOW_DIRTY=1       build from a modified tree anyway. The resulting
#                       binary reports itself as dirty and will never compare
#                       equal to anything, including the next deploy.
#   ALLOW_WORKTREE_BUILD=1  build from a linked git worktree anyway
#                           (colab-fleet #140). Go's own VCS stamp can embed
#                           the PRIMARY checkout's revision instead of this
#                           worktree's own; only set this if you have
#                           verified your toolchain does not do that.
#
# The dirty-tree gate below runs `git status --porcelain`, not `git diff
# --quiet HEAD` — colab-fleet #139: the latter only inspects tracked files,
# but Go's own VCS build stamp (what a running service reports as
# `build.modified`) is computed by `cmd/go/internal/vcs`'s gitStatus, which
# runs plain `git status --porcelain` and is flagged by ANY untracked file
# too. A gate that only checks tracked changes can report "clean" and still
# hand you a binary stamped modified — silently, because nothing here would
# have caught it.
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

# --- refuse to build from a linked worktree ---------------------------------
#
# colab-fleet #140: Go's own VCS build stamp (cmd/go/internal/vcs) was
# measured embedding the PRIMARY checkout's HEAD, not the linked worktree's
# own, when `go build -buildvcs=true` ran from inside a worktree — reproduced
# twice, including after `go clean -cache`. Plain `git rev-parse HEAD` (used
# for REV below) gets the right answer from inside a worktree; Go's detector
# does not. That gap is silent at build time: the build succeeds, and the
# binary just carries the wrong revision baked in.
#
# Step 4's verify loop below happens to catch this today — REV (correct,
# from git) would not match RUNNING (wrong, from the stamp), so the deploy
# reports FAILED rather than shipping a lying binary — but that is a
# different check catching a symptom, not this script knowing its own
# precondition. Refuse up front instead, before spending a build on it.
#
# `git rev-parse --git-dir` and `--git-common-dir` agree (both resolve to the
# same `.git`) from the primary checkout; a linked worktree's git-dir lives
# under the common dir's `worktrees/<name>/` instead, so the two diverge.
# That comparison is what git itself uses to tell "am I a linked worktree"
# apart from "am I the primary checkout" — this reuses it rather than
# string-matching a path shape that could change.
GIT_DIR=$(git rev-parse --git-dir 2>/dev/null || true)
GIT_COMMON_DIR=$(git rev-parse --git-common-dir 2>/dev/null || true)
if [ -n "$GIT_DIR" ] && [ -n "$GIT_COMMON_DIR" ] && [ "$GIT_DIR" != "$GIT_COMMON_DIR" ]; then
	if [ "${ALLOW_WORKTREE_BUILD:-0}" != "1" ]; then
		echo "deploy: this checkout is a linked git worktree, not the primary" >&2
		echo "        checkout — colab-fleet #140. Go's own VCS build stamp can" >&2
		echo "        embed the PRIMARY checkout's revision instead of this" >&2
		echo "        worktree's own, which would ship a binary that lies about" >&2
		echo "        what commit it was built from." >&2
		echo "        Run this from the primary checkout instead, or set" >&2
		echo "        ALLOW_WORKTREE_BUILD=1 if you have verified this" >&2
		echo "        toolchain/environment does not exhibit the bug." >&2
		exit 1
	fi
	echo "deploy: WARNING building from a linked worktree (ALLOW_WORKTREE_BUILD=1)." >&2
	echo "        The VCS stamp on this build may not be trustworthy — see" >&2
	echo "        colab-fleet #140." >&2
fi

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
#
# git status --porcelain, not git diff --quiet HEAD (colab-fleet #139): the
# latter only sees tracked changes, but Go's own VCS build stamp is flagged
# dirty by ANY untracked file too (cmd/go/internal/vcs's gitStatus runs the
# same plain `git status --porcelain`). Matching that check here is what
# makes this gate a reliable predictor of build.modified instead of an
# independent, weaker opinion that can pass while the stamp is already lying.
if [ "${ALLOW_DIRTY:-0}" != "1" ]; then
	DIRTY=$(git status --porcelain 2>/dev/null)
	if [ -n "$DIRTY" ]; then
		echo "deploy: working tree is modified (tracked or untracked)." >&2
		echo "        A binary built from a tree in this state has no build" >&2
		echo "        identity — Go's own VCS stamp will report it modified," >&2
		echo "        and it can never be compared against a peer or against" >&2
		echo "        itself. Commit or remove the untracked files, or set" >&2
		echo "        ALLOW_DIRTY=1." >&2
		echo "$DIRTY" | sed 's/^/        /' >&2
		exit 1
	fi
fi

REV=$(git rev-parse HEAD)
echo "deploy: revision ${REV}"

# colab-fleet #161: the release version, stamped at link time because the
# toolchain's own build info carries the revision but never the tag — a
# checkout builds as "(devel)". `--match 'v[0-9]*'` and no `--always`: with no
# release tag reachable there is no release to report, and a bare sha in this
# field would read as one. Left empty, the service reports `version: null`
# ("not stamped"), which is the honest answer. `--dirty` only ever shows when
# ALLOW_DIRTY=1 let a modified tree past the gate above.
VERSION=$(git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || true)
if [ -n "$VERSION" ]; then
	echo "deploy: version ${VERSION}"
else
	echo "deploy: no release tag reachable from ${REV} — build.version will be null"
fi

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
GOOS="$GOOS" GOARCH="$GOARCH" go build -buildvcs=true \
	-ldflags "-X github.com/godx-jp/colab-fleet.version=${VERSION}" \
	-o "$TMPBIN" ./cmd/colab-fleetd

# --- install ----------------------------------------------------------------
#
# Uploaded beside the target and renamed into place: a rename is atomic, while
# writing over a running binary is how you get a half-written executable and a
# service that will not start.
echo "deploy: installing to ${HOST}:${REMOTE_PATH}"
run "mkdir -p \"\$(dirname ${REMOTE_PATH})\""
put "$TMPBIN" "${REMOTE_PATH}.incoming"
run "chmod 0755 ${REMOTE_PATH}.incoming && mv ${REMOTE_PATH}.incoming ${REMOTE_PATH}"

# >>> optional delivery modules (#185) >>>
#
# --- install optional delivery modules, when asked ---------------------------
#
# colab-fleet #185. Runs ONLY when FLEET_MODULE_SOURCES is set and non-empty;
# otherwise nothing below executes and a deploy is byte-for-byte what it was.
#
# A module is an optional helper program that lives beside the daemon and is
# built from a source this operator may or may not be able to reach. So the
# rule is the installer's, not this script's: fetch it with the operator's OWN
# access (scripts/fetch-module.sh), and when that fails - no access, no such
# version, no network - install nothing and say nothing. A machine with only
# the built-in terminal module is a supported state, not a deploy problem, and
# a fetch that failed must never remove a module an earlier deploy installed.
#
# Built here, for the TARGET's platform (GOOS/GOARCH, settled above), then put
# on the host the same way the daemon binary was: uploaded beside its final
# name and renamed into place, never written over a file in use.
#
# What still speaks up, on stderr, and still never fails the deploy:
#   - a malformed entry (no '=', an empty side, a name the service would not
#     accept) - one line, naming the entry's position and, only when the name
#     side is itself a valid name, that name. Never the source: it can carry
#     a credential in its shape;
#   - a module that WAS fetched but could not be put on the host.
# The daemon lists its modules directory at startup, so the restart below is
# what makes a newly installed module visible.
if [ -n "${FLEET_MODULE_SOURCES:-}" ]; then
	# Explicit character sets, not ranges: a range means different things in
	# different locales. Same rule as scripts/fetch-module.sh.
	module_name_ok() {
		case "$1" in
		'' | auto | terminal | inbox | module) return 1 ;;
		[!abcdefghijklmnopqrstuvwxyz]* | *[!abcdefghijklmnopqrstuvwxyz0123456789-]*) return 1 ;;
		esac
		[ "${#1}" -le 32 ]
	}

	# $1 = local file, $2 = destination on the host. Written as explicit &&
	# steps because it is called from an `if`, where `set -e` does not apply.
	module_install() {
		run "mkdir -p -m 0755 ${MODULES_DIR}" &&
			put "$1" "$2.incoming" &&
			run "chmod 0755 $2.incoming && mv $2.incoming $2"
	}

	if [ -n "${FLEET_MODULES_DIR:-}" ]; then
		MODULES_DIR=$FLEET_MODULES_DIR
		if [ "$HOST" = "local" ]; then
			# Same leading-~ handling, and for the same reason, as REMOTE_PATH.
			case "$MODULES_DIR" in
			'~/'*) MODULES_DIR="$HOME/${MODULES_DIR#\~/}" ;;
			'~') MODULES_DIR="$HOME" ;;
			esac
		fi
	else
		# Where the daemon looks by default: the parent of the directory its
		# own binary runs from, resolved on the host (symlinks and all, as the
		# daemon resolves its own path), plus libexec/colab-fleet/modules.
		MODULE_BIN_DIR=$(run "cd \"\$(dirname ${REMOTE_PATH})\" && pwd -P") || MODULE_BIN_DIR=""
		if [ -n "$MODULE_BIN_DIR" ]; then
			MODULES_DIR="$(dirname "$MODULE_BIN_DIR")"
			MODULES_DIR="${MODULES_DIR%/}/libexec/colab-fleet/modules"
		else
			MODULES_DIR=""
			echo "deploy: WARNING could not work out where ${HOST} keeps delivery modules;" >&2
			echo "        none installed. Set FLEET_MODULES_DIR." >&2
		fi
	fi

	# A module problem never fails the deploy, so even the staging directory
	# is asked for inside an `if`.
	if [ -n "$MODULES_DIR" ] && MODSTAGE=$(mktemp -d -t colab-fleet-modules.XXXXXX); then
		# Restates the trap above (which this replaces) and adds the staging
		# directory; ${...:-} so this is safe wherever TMPBIN is not set.
		trap 'rm -f "${TMPBIN:-}"; rm -rf "${MODSTAGE:-}"' EXIT

		# No globbing: an entry is data, and a stray * must not expand.
		set -f
		MOD_N=0
		for MOD_ENTRY in $FLEET_MODULE_SOURCES; do
			MOD_N=$((MOD_N + 1))
			case "$MOD_ENTRY" in
			*=*)
				MOD_NAME=${MOD_ENTRY%%=*}
				MOD_SRC=${MOD_ENTRY#*=}
				;;
			*)
				MOD_NAME=""
				MOD_SRC=""
				;;
			esac

			if ! module_name_ok "$MOD_NAME"; then
				echo "deploy: WARNING module entry ${MOD_N} is malformed (expected name=source); skipped." >&2
				continue
			fi
			if [ -z "$MOD_SRC" ]; then
				echo "deploy: WARNING module entry ${MOD_N} (${MOD_NAME}) is malformed (expected name=source); skipped." >&2
				continue
			fi

			MOD_OUT="$MODSTAGE/$MOD_NAME"
			rm -f "$MOD_OUT"
			# Silent by contract, and always exits 0; a failed fetch is the
			# absence of $MOD_OUT, which is all this step looks at.
			GOOS="$GOOS" GOARCH="$GOARCH" sh scripts/fetch-module.sh "$MOD_NAME" "$MOD_SRC" "$MOD_OUT" || true
			if [ ! -f "$MOD_OUT" ]; then
				continue
			fi

			if module_install "$MOD_OUT" "${MODULES_DIR}/${MOD_NAME}"; then
				echo "deploy: installed delivery module ${MOD_NAME} to ${HOST}:${MODULES_DIR}/${MOD_NAME}"
			else
				run "rm -f ${MODULES_DIR}/${MOD_NAME}.incoming" 2>/dev/null || true
				echo "deploy: WARNING delivery module ${MOD_NAME} was fetched but could not be installed to ${HOST}:${MODULES_DIR}; skipped." >&2
			fi
		done
		set +f
		rm -rf "$MODSTAGE"
	fi
fi
# <<< optional delivery modules (#185) <<<

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
RUNNING_VERSION=""
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
		RUNNING_VERSION=$(printf '%s\n' "$BODY" | tr ',' '\n' | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -1)
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

# #161: the revision matched, so this is the right binary — a version that
# does not match is a stamping defect, not a deploy that failed to land, and
# it is reported as that. It still fails: a client enforcing a version floor
# reads this field, and a wrong one refuses (or admits) on false evidence.
if [ "$RUNNING_VERSION" != "$VERSION" ]; then
	echo "deploy: FAILED — running revision is ${REV}, as built, but it reports" >&2
	echo "        version '${RUNNING_VERSION:-null}' where '${VERSION:-null}' was stamped." >&2
	echo "        The binary landed; its release stamp did not survive the build" >&2
	echo "        (check the -X symbol path against build.go's version variable)." >&2
	exit 1
fi

echo "deploy: verified — ${HOST} is running ${REV} (version ${VERSION:-null})"
