#!/bin/zsh
# Oracle for the remote-create path.
#
# THE PROPERTY UNDER TEST: after creating a session through the service, the
# client must attach to the id the SERVICE RETURNED, never to the name it
# asked for.
#
# That distinction is newly load-bearing. The service owns the naming rules —
# it sanitizes a requested name and NUMBERS it when a session of that name is
# already live — so the two strings differ exactly when a second session is
# created in a project that already has one. A client that attaches by the
# requested name is correct until the first collision and then silently
# attaches to SOMEBODY ELSE'S session, which is the wrong-session class this
# whole file exists to close.
#
# Run:  zsh clients/fcode/create_test.zsh
#
# No live service, no multiplexer: the HTTP call and the attach are both stubbed
# so the test asserts the client's own logic and nothing else.

emulate -L zsh

typeset -g FAILURES=0
fail() { print -u2 "FAIL: $*"; (( FAILURES++ )) }
ok()   { print "  ok — $*" }

# --- stubs the file needs at source time ------------------------------------
#
# The real ones belong to the incumbent launcher, which this file wraps. Only
# their existence matters here.
_ccode_sessions_rooted() { : }
_ccode_local()           { : }
_ccode_flow()            { : }
_ccode_tab_title()       { : }
_ccode_local_attach()    { : }

SRC="${0:A:h}/fcode.zsh"
source "$SRC" >/dev/null 2>&1 || true

if (( ! $+functions[tmux] )); then
  print -u2 "FAIL: sourcing fcode.zsh did not install the multiplexer shim"
  exit 1
fi

# --- harness ----------------------------------------------------------------
typeset -g ATTACHED="" POSTED_ROUTE="" POSTED_BODY="" POSTED_EXTRA="" RETURNED_ID=""

# The client reads the create response through $( ), which runs the stub in a
# SUBSHELL — so anything it records in a variable is lost on the way out. The
# recording therefore goes through the filesystem. Worth stating: the first
# version of this test asserted on variables that could never have been set,
# and reported the client broken when it was the harness.
typeset -g REC="$(mktemp -d)"
trap 'rm -rf "$REC"' EXIT

# A REAL multiplexer must never be reachable from this test.
#
# The pass-through cases below end in `command tmux "$@"`, which deliberately
# bypasses the shim — and would then create actual sessions on the machine
# running the test. The first version of this file got away with it only
# because the fixture directory did not exist and the real binary failed; on a
# machine where it does exist, running the tests would litter the fleet.
#
# So a stub goes first on PATH, and it records the fact that the real binary
# was reached, which is what the pass-through cases actually want to assert.
mkdir -p "$REC/bin"
cat > "$REC/bin/tmux" <<STUB
#!/bin/sh
echo "\$@" >> "$REC/passthrough"
exit 0
STUB
chmod +x "$REC/bin/tmux"
path=("$REC/bin" $path)
rec_get() { [[ -r "$REC/$1" ]] && print -r -- "$(<"$REC/$1")" }

_fcode_whoami() { print -r -- "here" }

# Stand in for the service. Records what it was asked and answers with an id
# DELIBERATELY DIFFERENT from the requested name — the collision case.
_fcode_body() {
  print -r -- "$2" > "$REC/route"
  print -r -- "$3" > "$REC/body"
  print -r -- "$4" > "$REC/extra"
  print -r -- "{\"machine\":\"there\",\"id\":\"${RETURNED_ID}\",\"name\":\"${RETURNED_ID}\"}"
}

# Stand in for the attach path; records the id it was handed.
_ccode_local_attach() { ATTACHED="$1" }

run_create() {
  ATTACHED=""; rm -f "$REC"/route "$REC"/body "$REC"/extra
  FCODE_ACTIVE=1 FCODE_MACHINE="there" \
    tmux new-session -s "$1" -c "$2" zsh -lc 'exec agent' launcher "$1" "$2" >/dev/null 2>&1
}

# --- 1. the id is read back and used ----------------------------------------
print "== the returned id, not the requested name =="
RETURNED_ID="review-2💬"
run_create "review" "/work/thing"

if [[ $ATTACHED == "review" ]]; then
  fail "attached to the REQUESTED name; on a collision this addresses another session"
elif [[ $ATTACHED != "review-2💬" ]]; then
  fail "attached to '${ATTACHED}', want the returned id 'review-2💬'"
else
  ok "attached to the returned id"
fi

# --- 2. the request carries what the service needs ---------------------------
print "== the create request =="
POSTED_ROUTE="$(rec_get route)"
if [[ $POSTED_ROUTE == "/v1/machines/there/sessions" ]]; then
  ok "posted to the pinned machine's create route"
else
  fail "posted to '${POSTED_ROUTE}'"
fi

POSTED_EXTRA="$(rec_get extra)"
case "$POSTED_EXTRA" in
  Idempotency-Key:*) ok "carried an Idempotency-Key" ;;
  *) fail "no Idempotency-Key header (§10 requires one; a create without it is rejected)" ;;
esac

POSTED_BODY="$(rec_get body)"
if print -r -- "$POSTED_BODY" | python3 -c '
import json, sys
b = json.load(sys.stdin)
assert b["name"] == "review", b
assert b["cwd"] == "/work/thing", b
' 2>/dev/null; then
  ok "body carried the requested name and working directory"
else
  fail "body did not carry name/cwd as sent: ${POSTED_BODY}"
fi

# --- 3. flags are read by name, not by position ------------------------------
#
# The trailing argv belongs to the launcher and may grow; reading -s/-c
# positionally would break silently the next time it does.
print "== argv is parsed by flag =="
RETURNED_ID="other💬"
ATTACHED=""; rm -f "$REC"/route "$REC"/body "$REC"/extra
FCODE_ACTIVE=1 FCODE_MACHINE="there" \
  tmux new-session -d -c "/other/dir" -s "other" zsh -lc 'x' launcher >/dev/null 2>&1
POSTED_BODY="$(rec_get body)"
if print -r -- "$POSTED_BODY" | python3 -c '
import json, sys
b = json.load(sys.stdin)
assert b["name"] == "other", b
assert b["cwd"] == "/other/dir", b
' 2>/dev/null; then
  ok "reordered flags still parsed"
else
  fail "reordering -s/-c broke parsing: ${POSTED_BODY}"
fi

# --- 4. an unpinned invocation is NOT intercepted ----------------------------
#
# With no pin, or pinned to this machine, creation is the launcher's own
# business and must reach the real binary untouched.
print "== no pin means no interception =="
ATTACHED=""; rm -f "$REC"/route "$REC"/passthrough
FCODE_ACTIVE=1 FCODE_MACHINE="" \
  tmux new-session -s "local-one" -c "/w" zsh -lc 'x' launcher >/dev/null 2>&1
if [[ -n "$(rec_get route)" ]]; then
  fail "an unpinned create was sent to the service"
elif [[ -z "$(rec_get passthrough)" ]]; then
  fail "an unpinned create reached neither the service nor the real binary"
else
  ok "unpinned create left to the real binary"
fi

ATTACHED=""; rm -f "$REC"/route "$REC"/passthrough
FCODE_ACTIVE=1 FCODE_MACHINE="here" \
  tmux new-session -s "local-two" -c "/w" zsh -lc 'x' launcher >/dev/null 2>&1
if [[ -n "$(rec_get route)" ]]; then
  fail "a create pinned to THIS machine was sent to the service"
elif [[ -z "$(rec_get passthrough)" ]]; then
  fail "a create pinned to this machine reached neither path"
else
  ok "create pinned to this machine left to the real binary"
fi

# --- 5. a refusal does not attach --------------------------------------------
print "== a refused create must not attach =="
_fcode_body() { return 1 }
ATTACHED=""
run_create "refused" "/w"
if [[ -z $ATTACHED ]]; then ok "no attach after a refusal"
else fail "attached after the service refused the create"; fi

print
if (( FAILURES )); then
  print -u2 "${FAILURES} failure(s)"
  exit 1
fi
print "all checks passed"
