#!/bin/sh
# Exercise session creation END TO END against the service as the PROCESS
# MANAGER runs it, and check the thing that actually matters.
#
# WHY THIS EXISTS, AND WHY IT REFUSES TO RUN IN THE EASY PLACE
#
# Creating a session through the API used to produce a session that was not the
# same KIND of session an on-machine launcher produces: no remote-control
# binding, and no shell, therefore none of the credentials an agent's tool
# servers need. Every one of those failures is INVISIBLE at creation. The
# session starts, lists, reads and drives perfectly, and the consequence turns
# up later, somewhere else, looking like an agent fault.
#
# Which makes this area unusually good at producing false passes:
#
#   - Testing from an interactive terminal proves nothing. That terminal has
#     already read the startup file the whole defect is about, so anything it
#     spawns inherits the credentials and works — demonstrating only that the
#     tester's own shell is configured, which was never in question.
#
#   - Running the daemon by hand has exactly the same problem, for exactly the
#     same reason.
#
#   - And "did the session start?" is not the question. It always starts. It
#     started throughout the entire defect.
#
# So this script asserts its own preconditions rather than trusting the
# operator to have arranged them, and it checks the environment the session
# RECEIVED rather than the fact that it exists.
#
# USAGE
#
#   FLEET_URL=…  FLEET_TOKEN=…  FLEET_MACHINE=…  FLEET_MANAGED_PID=…  \
#       scripts/create-parity-check.sh [CWD]
#
#   FLEET_URL          base URL of the service, e.g. http://host:port
#   FLEET_TOKEN        a token whose principal holds create/send/close
#   FLEET_MACHINE      the machine id to create on
#   FLEET_MANAGED_PID  the pid your PROCESS MANAGER reports for the service.
#                      Required, and checked: it is what distinguishes this
#                      run from the false pass above. Get it from your service
#                      manager, not from `pgrep` — pgrep would happily find a
#                      daemon you started by hand two minutes ago, which is the
#                      exact thing being ruled out.
#   CWD                working directory for the test session. Default: $HOME.
#
# Deliberately absent: hostnames, ports, service labels, paths. Those are
# operational facts and this repository is public.

set -eu

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
need() {
	eval "v=\${$1:-}"
	[ -n "$v" ] || fail "$1 must be set (see the usage block in this script)"
}

need FLEET_URL
need FLEET_TOKEN
need FLEET_MACHINE
need FLEET_MANAGED_PID

CWD=${1:-$HOME}
AUTH="Authorization: Bearer ${FLEET_TOKEN}"
api() { curl -fsS -H "$AUTH" "$@"; }

# --- 0. the precondition that makes the rest mean anything ------------------
#
# Confirm the service answering us is the one the process manager owns. Without
# this the whole script is the false pass it exists to prevent.
echo "== 0. the service under test is the process-managed one =="
ps -p "$FLEET_MANAGED_PID" >/dev/null 2>&1 ||
	fail "no process with pid ${FLEET_MANAGED_PID}; the service your manager reports is not running"

HEALTH=$(api "${FLEET_URL}/v1/health") || fail "cannot reach ${FLEET_URL}"
REV=$(printf '%s' "$HEALTH" | tr ',' '\n' | sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$REV" ] || fail "the service reports no build revision; it is too old to verify anything against"
echo "   pid ${FLEET_MANAGED_PID} alive · service revision ${REV}"

if [ -n "${EXPECT_REVISION:-}" ] && [ "$REV" != "$EXPECT_REVISION" ]; then
	fail "service is running ${REV}, not ${EXPECT_REVISION} — you are testing the OLD binary.
      This is the failure mode scripts/deploy.sh exists for: the copy succeeded
      and the service kept serving what it already had."
fi

# --- 1. create ---------------------------------------------------------------
KEY="parity-$(date +%s)-$$"
NAME="parity-check-$$"
echo "== 1. create a session through the API =="
CREATED=$(api -X POST "${FLEET_URL}/v1/machines/${FLEET_MACHINE}/sessions" \
	-H "Idempotency-Key: ${KEY}" -H 'Content-Type: application/json' \
	-d "{\"cwd\":\"${CWD}\",\"name\":\"${NAME}\"}") || fail "create was refused"

ID=$(printf '%s' "$CREATED" | tr ',' '\n' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$ID" ] || fail "create returned no id: ${CREATED}"
echo "   created id=${ID}"

cleanup() {
	[ -n "${ID:-}" ] || return 0
	echo "== cleanup: closing ${ID} =="
	api -X DELETE "${FLEET_URL}/v1/machines/${FLEET_MACHINE}/sessions/${ID}" >/dev/null 2>&1 ||
		echo "   (close failed; session ${ID} may need closing by hand)" >&2
}
trap cleanup EXIT

# --- 2. the environment it ACTUALLY received --------------------------------
#
# The load-bearing check. An agent with no credentials starts perfectly and
# fails at its first tool call, so "it started" is worth nothing here and the
# only honest evidence is what the process ended up holding.
echo "== 2. the environment the session actually received =="
i=0
ENV=""
while [ "$i" -lt 30 ]; do
	ENV=$(api "${FLEET_URL}/v1/machines/${FLEET_MACHINE}/sessions/${ID}/environment") || true
	case "$ENV" in *'"known":true'*) break ;; esac
	i=$((i + 1))
	sleep 1
done
case "$ENV" in
*'"known":true'*) ;;
*) fail "no environment record was captured within 30s: ${ENV}" ;;
esac

case "$ENV" in
*'"interactive":true'*) echo "   shell is interactive ✓" ;;
*) fail "the session's shell was NOT interactive.
      A login shell that is not interactive does not read the interactive
      startup file, which is where credentials are exported. This is the
      difference between -lc and -lic, and it is measurable rather than
      arguable: the session will start and fail at its first tool call." ;;
esac

# Names the session has that the SERVICE process does not — what the startup
# files contributed. An empty difference is the defect, restated.
ADDED=$(printf '%s' "$ENV" | python3 -c '
import json,sys
e = json.load(sys.stdin)
svc = set(e.get("serviceNames") or [])
added = [n for n in (e.get("names") or []) if n not in svc]
print(len(added))
print(" ".join(added[:40]))
print(len(e.get("path") or []), len(e.get("servicePath") or []))
')
COUNT=$(printf '%s' "$ADDED" | sed -n 1p)
NAMES=$(printf '%s' "$ADDED" | sed -n 2p)
PATHS=$(printf '%s' "$ADDED" | sed -n 3p)

echo "   variables the startup files added: ${COUNT}"
echo "   PATH entries session/service: ${PATHS}"
[ "$COUNT" -gt 0 ] || fail "the startup files contributed NOTHING.
      The session has exactly the service's own environment, which is the
      defect this work exists to fix: the agent will start normally and fail
      at its first tool call."
echo "   added: ${NAMES}"

# --- 3. remote-control binding ----------------------------------------------
echo "== 3. remote-control binding =="
echo "   session id is '${ID}' — the SAME string is bound as the remote-control"
echo "   name and the agent's own name (asserted by the driver's unit tests)."
echo "   Whether a phone client can drive it cannot be asserted from here:"
echo "   completing a call FROM that client requires that client. Look for"
echo "   '${ID}' in the remote client and drive one turn to close this leg."

# --- 4. one tool call --------------------------------------------------------
#
# The question the environment check exists to predict. state.lastTurn reports
# how the most recent turn ENDED — structured, not scraped — so a turn that
# died on a missing credential is distinguishable from one that finished.
echo "== 4. complete one tool call =="
api -X POST "${FLEET_URL}/v1/machines/${FLEET_MACHINE}/sessions/${ID}/input" \
	-H 'Content-Type: application/json' \
	-d '{"text":"Run the shell command: echo FLEET-PARITY-OK","submit":true}' >/dev/null ||
	fail "could not deliver the prompt"

i=0
STATE=""
while [ "$i" -lt 90 ]; do
	STATE=$(api "${FLEET_URL}/v1/machines/${FLEET_MACHINE}/sessions/${ID}") || true
	case "$STATE" in
	*'"outcome":"failed"'*)
		REASON=$(printf '%s' "$STATE" | tr ',' '\n' | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p' | head -1)
		fail "the turn FAILED: ${REASON}
      This is the failure the environment check predicts. Compare section 2."
		;;
	*'"status":"idle"'*) break ;;
	esac
	i=$((i + 1))
	sleep 2
done
case "$STATE" in
*'"status":"idle"'*) echo "   the turn completed without reporting a failure ✓" ;;
*) echo "   INCONCLUSIVE: the session never settled within 180s. Not a pass." >&2; exit 1 ;;
esac

echo
echo "PASS: created through the managed service, inherited ${COUNT} variables"
echo "      from the startup files, and completed a turn without failure."
echo "NOTE: the phone-client leg (section 3) is not covered — see above."
