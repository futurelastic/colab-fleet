# fcode — a session launcher that talks to colab-fleet instead of to tmux.
#
# This is a SECOND tool, deliberately. It does not replace, wrap, patch or
# import the existing launcher: run both, compare, and keep whichever you
# trust. Nothing here writes to the incumbent's files, and sourcing this does
# not shadow its commands — the names are different on purpose.
#
#   source /path/to/colab-fleet/clients/fcode.zsh
#
#   fcode                       list every session in the fleet
#   fcode <prefix>              attach to the one session matching <prefix>
#   fcode watch                 stream state changes as they happen
#   fcode up                    is the session layer reachable, and what is it running
#   fcode new <machine> <name> <cwd>
#   fcode kill <prefix>
#
# Configuration, all environment, no machine names in this file:
#
#   FLEET_URL         the service on THIS machine — REQUIRED, no default.
#                     The port is an operational fact, not a constant, and a
#                     guessed default would quietly probe the wrong thing.
#   FLEET_TOKEN_FILE  file holding this client's token  (default ~/.config/colab-fleet/token)
#   FLEET_SSH_FMT     how to reach another machine by name, %s = machine id
#                     e.g. "ssh -t host-%s"             (default "ssh -t %s")
#
# Requires: curl, python3. No jq.
#
# Written against docs/client-guide.md. Where a rule below looks fussy, the
# guide explains why; the short version is in each comment.

if [[ -z ${FLEET_URL:-} ]]; then
  print -u2 "fcode: set FLEET_URL to this machine's colab-fleet service, e.g."
  print -u2 "       export FLEET_URL=http://127.0.0.1:<port>"
fi
: ${FLEET_TOKEN_FILE:=$HOME/.config/colab-fleet/token}
: ${FLEET_SSH_FMT:=ssh -t %s}

# ── plumbing ────────────────────────────────────────────────────────────────

# zsh reserves more variable names than it looks like, and each one fails
# differently. `path` is tied to $PATH, so a `local path=` empties the command
# search path for the rest of the function — everything then reports "command
# not found". `status` is READ-ONLY (it mirrors $?), so assigning it aborts the
# function outright. `argv` is the positional parameters. All three were hit
# writing this file. Prefix locals rather than trusting a name to be free.
_fc_curl() {
  local method="$1" route="$2" body="$3"
  local token
  [[ -r $FLEET_TOKEN_FILE ]] || { print -u2 "fcode: no token at $FLEET_TOKEN_FILE"; return 2 }
  token="$(<$FLEET_TOKEN_FILE)"
  local -a args=(-sS -m 20 -X "$method"
    -H "Authorization: Bearer ${token//[$'\t\r\n ']}"
    -H "Content-Type: application/json"
    -w '\n%{http_code}')
  [[ -n $body ]] && args+=(-d "$body")
  [[ -n $4 ]] && args+=(-H "$4")
  curl "${args[@]}" "${FLEET_URL}${route}" 2>/dev/null
}

# _fc_get ROUTE → body on stdout, 0 on 2xx.
# A transport failure is NOT an empty result: the guide is emphatic that a
# client must never render "no sessions" when it means "I could not ask".
_fc_get() {
  local raw code
  raw="$(_fc_curl GET "$1")" || { print -u2 "fcode: session layer unreachable at $FLEET_URL"; return 2 }
  code="${raw##*$'\n'}"; raw="${raw%$'\n'*}"
  [[ -z $code ]] && { print -u2 "fcode: session layer unreachable at $FLEET_URL"; return 2 }
  if [[ $code != 2* ]]; then
    print -u2 "fcode: HTTP $code — $(print -r -- "$raw" | _fc_py 'import sys,json
try: print(json.load(sys.stdin)["error"]["message"])
except Exception: print("(unparseable error body)")')"
    return 1
  fi
  print -r -- "$raw"
}

_fc_py() { python3 -c "$1" "${@:2}" }

# ── reading ─────────────────────────────────────────────────────────────────

# Every plural response can be partial. `items` alone is a lie of omission when
# a machine did not answer, so `complete` is checked on every listing and the
# unreachable machines are named rather than silently dropped.
_fc_sessions() {
  _fc_get "/v1/sessions?scope=fleet" | _fc_py '
import sys, json
d = json.load(sys.stdin)
if not d.get("complete", True):
    down = [s["machine"] for s in d.get("sources", []) if s.get("status") != "ok"]
    print("PARTIAL " + ",".join(down), file=sys.stderr)
for s in d.get("items", []):
    st = s.get("state", {})
    # Tab-separated: ids contain spaces and emoji, so a space-delimited
    # format cannot be parsed back apart.
    print("\t".join([s.get("machine",""), st.get("status","?"), s.get("id",""),
                     s.get("cwd",""), st.get("since","") or "",
                     (s.get("attach") or {}).get("kind","")]))'
}

fcode_ls() {
  local partial=0 out
  out="$(_fc_sessions 2>/tmp/.fcode.err)" || return $?
  [[ -s /tmp/.fcode.err ]] && { partial=1; print -u2 "fcode: PARTIAL VIEW — $(</tmp/.fcode.err)"; }
  # Here-string, not a pipe: a piped `while` runs in a subshell in zsh, where
  # `local` is outside any function scope and prints its declaration instead
  # of quietly declaring.
  local mark
  while IFS=$'\t' read -r machine sstate id cwd since kind; do
    case $sstate in
      working)       mark=$'\e[33m●\e[0m' ;;
      waiting_input) mark=$'\e[35m?\e[0m' ;;
      idle)          mark=$'\e[32m○\e[0m' ;;
      starting)      mark=$'\e[36m↑\e[0m' ;;
      dead)          mark=$'\e[31m×\e[0m' ;;
      *)             mark=$'\e[90m·\e[0m' ;;
    esac
    printf "%s %-11s %-13s %s\n      %s\n" "$mark" "$machine" "$sstate" "$id" "${cwd/#$HOME/~}"
  done <<< "$out"
  (( partial )) && print -u2 "fcode: some machines did not answer; sessions there are NOT shown and are NOT known to be gone"
  return 0
}

# ── which machine am I ──────────────────────────────────────────────────────
#
# Needed before attaching: a session on this machine is reachable from the
# terminal you are in, one on a peer is not. Without this a client SSHes to
# its own host for every local session.
_fc_self() {
  _fc_get /v1/machines | _fc_py '
import sys, json
d = json.load(sys.stdin)
for m in d.get("items", []):
    if m.get("self"): print(m["machine"]); break'
}

# ── resolving a prefix to exactly one session ───────────────────────────────
#
# Three-valued on purpose. "Not found" while the view is partial is NOT
# "gone" — it may be sitting on the machine that failed to answer, and
# treating those the same is how a launcher offers to recreate work that is
# already running.
_fc_resolve() {
  local want="$1" out partial=0
  out="$(_fc_sessions 2>/tmp/.fcode.err)" || return 2
  [[ -s /tmp/.fcode.err ]] && partial=1
  local -a exact=() pfx=()
  local machine sstate id rest
  while IFS=$'\t' read -r machine sstate id rest; do
    [[ $id == $want ]] && exact+=("$machine"$'\t'"$id")
    [[ $id == ${want}* ]] && pfx+=("$machine"$'\t'"$id")
  done <<< "$out"
  local -a hit
  if (( ${#exact} )); then hit=("${exact[@]}"); else hit=("${pfx[@]}"); fi
  case ${#hit} in
    1) print -r -- "${hit[1]}"; return 0 ;;
    0) if (( partial )); then
         print -u2 "fcode: no session matches '$want' — but the fleet view was PARTIAL, so this is UNKNOWN, not absent"
         return 2
       fi
       print -u2 "fcode: no session matches '$want'"
       return 1 ;;
    *) print -u2 "fcode: '$want' matches ${#hit} sessions:"
       printf '  %s\n' "${(@)hit//$'\t'/  }" >&2
       return 1 ;;
  esac
}

# ── attach ──────────────────────────────────────────────────────────────────
#
# The service never attaches anything: attaching gives a terminal to a person,
# and no person is on the far end of an HTTP request. It hands back argv, and
# whether that argv runs here or over ssh is this client's business.
fcode_attach() {
  local target self machine id
  target="$(_fc_resolve "$1")" || return $?
  machine="${target%%$'\t'*}"; id="${target#*$'\t'}"
  self="$(_fc_self)" || return $?

  local hint
  hint="$(_fc_get "/v1/machines/$machine/sessions/$(_fc_urlenc "$id")")" || return $?
  local -a cmdv
  cmdv=("${(@f)$(print -r -- "$hint" | _fc_py '
import sys, json
a = (json.load(sys.stdin) or {}).get("attach")
if not a or not a.get("command"): sys.exit(3)
print("\n".join(a["command"]))')}") || {
    print -u2 "fcode: this session reports no attach hint — the driver has no interactive attachment to offer"
    return 1
  }

  if [[ $machine == $self ]]; then
    print -u2 "fcode: attaching locally to $id"
    exec "${cmdv[@]}"
  fi
  # Remote: the service told us how to attach ON THAT MACHINE; getting there
  # is ours. Quote every argument — ids contain spaces and emoji.
  local remote_cmd
  remote_cmd="$(printf '%q ' "${cmdv[@]}")"
  print -u2 "fcode: attaching on $machine to $id"
  exec ${=${(f)FLEET_SSH_FMT/\%s/$machine}} "$remote_cmd"
}

_fc_urlenc() { _fc_py 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1" }

# ── create ──────────────────────────────────────────────────────────────────
fcode_new() {
  local machine="$1" name="$2" cwd="$3"
  [[ -n $machine && -n $name && -n $cwd ]] || { print -u2 "usage: fcode new <machine> <name> <cwd>"; return 2 }
  # An idempotency key is required, not optional: a create that times out and
  # is retried without one produces two agents in one working directory, and
  # nothing afterwards can detect it.
  local key body raw code
  key="$(_fc_py 'import uuid; print(uuid.uuid4())')"
  body="$(_fc_py 'import json,sys; print(json.dumps({"name":sys.argv[1],"cwd":sys.argv[2]}))' "$name" "$cwd")"
  raw="$(_fc_curl POST "/v1/machines/$machine/sessions" "$body" "Idempotency-Key: $key")" || {
    print -u2 "fcode: session layer unreachable"; return 2 }
  code="${raw##*$'\n'}"; raw="${raw%$'\n'*}"
  if [[ $code != 2* ]]; then
    print -u2 "fcode: create failed (HTTP $code) — $(print -r -- "$raw" | _fc_py 'import sys,json
try: print(json.load(sys.stdin)["error"]["message"])
except Exception: print(sys.stdin.read())')"
    return 1
  fi
  # Key everything afterwards on the id the SERVER returned. It is not
  # promised to equal the name you asked for.
  print -r -- "$raw" | _fc_py 'import sys,json
s=json.load(sys.stdin); print(s["id"])'
}

# ── destroy ─────────────────────────────────────────────────────────────────
#
# Ids are recyclable, so a destroy quotes back the start time from the read.
# If the session at that id is not the one that was read, the service refuses
# with 409 rather than destroying a stranger's work.
fcode_kill() {
  local target machine id
  target="$(_fc_resolve "$1")" || return $?
  machine="${target%%$'\t'*}"; id="${target#*$'\t'}"

  local started
  started="$(_fc_get "/v1/machines/$machine/sessions/$(_fc_urlenc "$id")" | _fc_py '
import sys,json; print(json.load(sys.stdin).get("startedAt",""))')" || return $?
  local route="/v1/machines/$machine/sessions/$(_fc_urlenc "$id")"
  [[ -n $started ]] && route+="?startedAt=$(_fc_urlenc "$started")"

  local raw code
  raw="$(_fc_curl DELETE "$route")" || { print -u2 "fcode: session layer unreachable"; return 2 }
  code="${raw##*$'\n'}"; raw="${raw%$'\n'*}"
  case $code in
    2*) print -u2 "fcode: closed $id on $machine"; return 0 ;;
    409) print -u2 "fcode: REFUSED — the session at '$id' is not the one just read."
         print -u2 "       Re-read it before deciding; do not retry blindly."
         return 1 ;;
    *)  print -u2 "fcode: close failed (HTTP $code) — $(print -r -- "$raw" | _fc_py 'import sys,json
try: print(json.load(sys.stdin)["error"]["message"])
except Exception: print("(no body)")')"
        return 1 ;;
  esac
}

# ── watch ───────────────────────────────────────────────────────────────────
#
# The thing the old launcher could not do at all. Events fire on transitions,
# never on content, so a quiet stream is normal rather than broken.
fcode_watch() {
  local token; token="$(<$FLEET_TOKEN_FILE)"
  print -u2 "fcode: watching ${FLEET_URL} — transitions only, silence is normal. ^C to stop."
  curl -sN -H "Authorization: Bearer ${token//[$'\t\r\n ']}" -H "Accept: text/event-stream" \
    "${FLEET_URL}/v1/events" | _fc_py '
import sys, json, datetime
for line in sys.stdin:
    if not line.startswith("data: "): continue
    ev = json.loads(line[6:])
    now = datetime.datetime.now().strftime("%H:%M:%S")
    k = ev.get("kind"); p = ev.get("payload") or {}
    ref = p.get("ref") or p
    sid = ref.get("id", "?"); st = (p.get("state") or {}).get("status", "")
    if k == "control.resync":
        print(f"{now}  RESYNC — your view is stale, re-list", flush=True)
    else:
        mach = ev.get("machine", "?")
        print(f"{now}  {mach:<11} {k:<16} {sid} {st}", flush=True)'
}

# ── health ──────────────────────────────────────────────────────────────────
fcode_up() {
  local h
  h="$(_fc_get /v1/health)" || { print -u2 "fcode: session layer DOWN at $FLEET_URL"; return 1 }
  print -r -- "$h" | _fc_py '
import sys, json
d = json.load(sys.stdin)
b = d.get("build", {})
rev = b.get("revision","")[:12] or "unknown"
if b.get("modified"): rev += "+dirty"
go = b.get("go", "")
cur = d.get("cursor")
print(f"session layer: up   build {rev}   {go}   cursor {cur}")'
  # Machines are reported too: a service that is up while a peer is not is a
  # different situation from a fleet that is fine.
  _fc_get /v1/machines | _fc_py '
import sys, json
d = json.load(sys.stdin)
for m in d.get("items", []):
    who = "self" if m.get("self") else "peer"
    name = m["machine"]; st = m.get("status")
    print(f"  {name:<12} {who:<5} {st}")
if not d.get("complete", True): print("  (view incomplete)")'
}

# ── entry point ─────────────────────────────────────────────────────────────
fcode() {
  case "$1" in
    ""|ls|list) fcode_ls ;;
    up|status)  fcode_up ;;
    watch)      fcode_watch ;;
    new)        shift; fcode_new "$@" ;;
    kill)       shift; fcode_kill "$@" ;;
    help|-h|--help)
      print -r -- "fcode                list the fleet"
      print -r -- "fcode <prefix>       attach (local or over ssh, decided by the service)"
      print -r -- "fcode watch          stream state changes"
      print -r -- "fcode up             health + machines"
      print -r -- "fcode new <machine> <name> <cwd>"
      print -r -- "fcode kill <prefix>" ;;
    *)          fcode_attach "$1" ;;
  esac
}
