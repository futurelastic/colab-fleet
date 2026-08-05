# fcode-ui — the incumbent launcher's UI, with the fleet service underneath.
#
# `fcode.zsh` (beside this file) is a standalone client with its own small
# interface. This file is the opposite approach and the more useful one for
# deciding anything: it keeps the launcher you already use — the picker, the
# folder browser, the grouping, the naming rules, every keybinding — and
# replaces ONLY the part that talks to the terminal multiplexer.
#
# That isolates the variable. If the interface is identical, then anything you
# notice is the session layer, which is the only thing being evaluated.
#
#   source /path/to/ccode.zsh          # your existing launcher, unchanged
#   source /path/to/fcode-ui.zsh       # this file
#
#   fcode     sessions on THIS machine        (mirrors the local launcher)
#   sfcode    sessions on EVERY machine       (mirrors the remote one)
#
# The pair mirrors the incumbent's own split, and for the same reason: one is
# what you use while working on a machine, the other is how you reach the rest
# of the fleet. `fcode` runs unchanged on either machine — it asks the local
# service for its own view and never needs to know its own name.
#
# `ccode` and `sccode` keep working exactly as before. The overrides below
# delegate to the originals unless FCODE_ACTIVE is set, and only `fcode` sets
# it — so a bug here cannot change what the incumbent does.
#
# Config (no machine names in this file):
#   FLEET_URL         this machine's service        (required)
#   FLEET_TOKEN_FILE  token for this client         (default ~/.config/colab-fleet/token)
#   FLEET_SSH_FMT     how to reach a peer, %s = machine  (default "ssh -t %s")
#   FCODE_MACHINE     limit the view to one machine (default: the whole fleet)
#
# WHAT CHANGES, AND WHAT DOES NOT
#
#   listing   → one HTTP call to the local service, covering EVERY machine.
#               The incumbent's remote mode runs `tmux ls` over ssh on one
#               host; this sees the fleet, and says so when a machine did not
#               answer instead of showing a shorter list.
#   attach    → the service says how; this decides where. A session on another
#               machine attaches without a second launcher on the far side.
#   kill      → corroborated by start time, so a recycled id cannot be
#               mistaken for the session you looked at.
#   rename    → NOT routed. The API has no rename operation, so renaming a
#               session on another machine is not expressible; the picker's
#               rename still acts locally and will not find a remote session.
#               Recorded rather than faked.
#   new       → NOT overridden. The incumbent builds a rich command (agent
#               flags, MCP credentials, restore behaviour) that the driver's
#               spawn path does not reproduce yet. Creating still goes through
#               the launcher you already trust; pretending otherwise would
#               change more than the session layer.

(( $+functions[_ccode_sessions_rooted] )) || {
  print -u2 "fcode-ui: source your ccode.zsh first — this file overrides parts of it."
  return 1
}
[[ -n ${FLEET_URL:-} ]] || print -u2 "fcode-ui: set FLEET_URL to this machine's colab-fleet service."

: ${FLEET_TOKEN_FILE:=$HOME/.config/colab-fleet/token}
: ${FLEET_SSH_FMT:=ssh -t %s}

# Which slice of the fleet the current invocation is about. `local` asks this
# machine's service for its own sessions only (§13.1's scope=local), which is
# the same question the incumbent's local mode answers — and, usefully, needs
# no knowledge of what this machine is called.
typeset -g  _fcode_scope=local

typeset -gA _fcode_machine_of      # cache only: id → machine. NEVER depended on.
typeset -g  _fcode_partial=0       # was the last listing missing a machine?

# _fcode_machine_for NAME → the machine holding that session, or empty.
#
# # Why this asks the service instead of reading a map
#
# The launcher loads its session list as `... <<< "$(_ccode_sessions_rooted)"`.
# Command substitution runs in a SUBSHELL, so anything the producer records in
# a global — like a name→machine map — is discarded the moment it returns. The
# rows survive because they travel on stdout; nothing else does.
#
# That cost a working attach: the picker showed the session, and attaching
# reported not knowing which machine held it. The lesson is not "populate the
# map differently" but "do not keep state across a boundary the caller is free
# to put a subshell on". Asking is one request and cannot go stale.
_fcode_machine_for() {
  local name="$1"
  [[ -n ${_fcode_machine_of[$name]:-} ]] && { print -r -- "${_fcode_machine_of[$name]}"; return }
  _fcode_body GET "/v1/sessions?scope=${_fcode_scope}" | python3 -c '
import sys, json
want = sys.argv[1]
for s in json.load(sys.stdin).get("items", []):
    if s.get("id") == want:
        print(s.get("machine","")); break' "$name"
}

# ── keep the originals, once ────────────────────────────────────────────────
if (( ! $+functions[_fcode_orig_sessions_rooted] )); then
  functions[_fcode_orig_sessions_rooted]=$functions[_ccode_sessions_rooted]
  functions[_fcode_orig_local_attach]=$functions[_ccode_local_attach]
  functions[_fcode_orig_local]=$functions[_ccode_local]
fi

_fcode_token() { [[ -r $FLEET_TOKEN_FILE ]] && print -r -- "${$(<$FLEET_TOKEN_FILE)//[$'\t\r\n ']}" }

# Token via a curl config on stdin, never argv: argv is visible in `ps` to
# every process on the machine.
_fcode_api() {
  local verb="$1" route="$2" data="$3" extra="$4" tok
  tok="$(_fcode_token)" || return 2
  local -a a=(-sS -m 20 -X "$verb" --config - -H "Content-Type: application/json" -w '\n%{http_code}')
  [[ -n $data  ]] && a+=(-d "$data")
  [[ -n $extra ]] && a+=(-H "$extra")
  print -r -- "header = \"Authorization: Bearer ${tok}\"" | curl "${a[@]}" "${FLEET_URL}${route}" 2>/dev/null
}

_fcode_body() {   # → body on stdout, non-zero on any failure
  local raw code
  raw="$(_fcode_api "$@")" || return 2
  code="${raw##*$'\n'}"; raw="${raw%$'\n'*}"
  [[ -z $code || $code != 2* ]] && return 1
  print -r -- "$raw"
}

# ── listing ─────────────────────────────────────────────────────────────────
#
# Emits exactly what the original emitted — "name<TAB>label<TAB>rel" — so every
# line of UI above it is untouched. The only difference is where the pairs of
# (name, working directory) come from, and that they now span machines.
_ccode_sessions_rooted() {
  (( ${FCODE_ACTIVE:-0} )) || { _fcode_orig_sessions_rooted "$@"; return }
  emulate -L zsh
  local line name dir label root rel tab=$'\t' machine
  local -x LC_CTYPE=en_US.UTF-8

  _fcode_machine_of=(); _fcode_partial=0
  local body
  body="$(_fcode_body GET "/v1/sessions?scope=${_fcode_scope}")" || {
    # Never silently empty: an empty picker reads as "no sessions", which is a
    # claim, and the honest answer here is "I could not ask".
    print -u2 "⚠️  fcode: session layer unreachable — showing nothing is NOT the same as nothing running"
    return 1
  }

  # machine<|>name<|>cwd, plus a PARTIAL marker if a machine did not answer.
  local -a rows
  rows=("${(@f)$(print -r -- "$body" | python3 -c '
import sys, json, os
d = json.load(sys.stdin)
want = os.environ.get("FCODE_MACHINE") or ""
if not d.get("complete", True):
    down = [s["machine"] for s in d.get("sources", []) if s.get("status") != "ok"]
    print("PARTIAL<|>" + ",".join(down) + "<|>")
for s in d.get("items", []):
    if want and s.get("machine") != want: continue
    if (s.get("state") or {}).get("status") == "dead": continue
    print("%s<|>%s<|>%s" % (s.get("machine",""), s.get("id",""), s.get("cwd","")))')}")

  for line in "${rows[@]}"; do
    [[ -z $line ]] && continue
    machine="${line%%<|>*}"; line="${line#*<|>}"
    name="${line%%<|>*}"; dir="${line#*<|>}"
    if [[ $machine == PARTIAL ]]; then
      _fcode_partial=1
      print -u2 "⚠️  fcode: no answer from: ${name} — its sessions are NOT listed and are NOT known to be gone"
      continue
    fi
    [[ -z $name ]] && continue
    # A name can exist on two machines. Prefer the one already recorded (the
    # fleet listing puts this machine first), because attaching locally is the
    # cheaper and safer of the two.
    [[ -n ${_fcode_machine_of[$name]:-} ]] || _fcode_machine_of[$name]="$machine"
    label="$(_ccode_label_of_dir "$dir")"
    rel=""
    if [[ -n "$label" ]]; then
      root="${CCODE_ROOTS[$label]}"
      [[ "$dir" == "$root" ]] && rel="" || rel="${dir#$root/}"
    fi
    print -r -- "${name}${tab}${label}${tab}${rel}"
  done
}

# ── attach ──────────────────────────────────────────────────────────────────
#
# The service reports the argv that attaches ON THAT SESSION'S MACHINE; this
# decides whether to run it here or over ssh. Two machines in one fleet had
# their multiplexer at different paths (different architectures), so the argv
# has to come from the machine that owns the session rather than from a
# constant here.
_ccode_local_attach() {
  (( ${FCODE_ACTIVE:-0} )) || { _fcode_orig_local_attach "$@"; return }
  emulate -L zsh
  # Session names carry emoji, and quoting one for a remote shell under a
  # non-UTF-8 locale mangles it — `printf %q` renders 💬 as $'\237'$'\222'.
  # The mangled string is a DIFFERENT session name, so the attach silently
  # targets nothing. Measured: correct under en_US.UTF-8, broken under C, and
  # C is what a LaunchAgent, a bare ssh or a phone client hands you.
  #
  # LC_ALL is emptied as well as LC_CTYPE being set, because LC_ALL overrides
  # LC_CTYPE — setting the latter alone would have looked like a fix and
  # changed nothing in exactly the environments that need it.
  local -x LC_ALL= LC_CTYPE=en_US.UTF-8
  local name="$1" machine
  machine="$(_fcode_machine_for "$name")"

  if [[ -z $machine ]]; then
    print -u2 "fcode: no session named \"$name\" anywhere in the fleet"
    return 1
  fi

  local self body
  self="$(_fcode_body GET /v1/machines | python3 -c '
import sys, json
for m in json.load(sys.stdin).get("items", []):
    if m.get("self"): print(m["machine"]); break')"

  body="$(_fcode_body GET "/v1/machines/${machine}/sessions/$(python3 -c '
import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$name")")" || {
    print -u2 "fcode: could not read \"$name\" on $machine"; return 1 }

  local -a cmdv
  cmdv=("${(@f)$(print -r -- "$body" | python3 -c '
import sys, json
a = (json.load(sys.stdin) or {}).get("attach") or {}
cmd = a.get("command")
if not cmd: sys.exit(3)
print("\n".join(cmd))')}") || {
    print -u2 "fcode: \"$name\" reports no attach hint"; return 1 }

  _ccode_tab_title "$name" 2>/dev/null
  if [[ $machine == $self ]]; then
    "${cmdv[@]}"
    return
  fi
  # Remote: quote every argument — ids carry spaces and emoji.
  local remote_cmd; remote_cmd="$(printf '%q ' "${cmdv[@]}")"
  ${=${FLEET_SSH_FMT/\%s/$machine}} "$remote_cmd"
  local rc=$?
  # Reaching another machine is the client's job, and it is not symmetric: a
  # fleet can be fully readable in both directions over HTTP while ssh works
  # only one way. Say which of the two failed, because "connection refused"
  # after a successful listing is otherwise baffling.
  if (( rc == 255 )); then
    print -u2 "fcode: the fleet can SEE $machine but this machine cannot ssh to it."
    print -u2 "       Attach from $machine itself, or set FLEET_SSH_FMT to a route that works."
  fi
  return $rc
}

# ── kill ────────────────────────────────────────────────────────────────────
#
# Only the kill verb is intercepted; every other subcommand goes to the
# original untouched. Destroying quotes back the start time, so a recycled id
# cannot be mistaken for the session that was looked at.
_ccode_local() {
  (( ${FCODE_ACTIVE:-0} )) || { _fcode_orig_local "$@"; return }
  emulate -L zsh
  if [[ $1 != kill || -z $2 ]]; then
    _fcode_orig_local "$@"
    return
  fi
  local name="$2" machine
  machine="$(_fcode_machine_for "$name")"
  [[ -z $machine ]] && { _fcode_orig_local "$@"; return }

  local enc started
  enc="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$name")"
  started="$(_fcode_body GET "/v1/machines/${machine}/sessions/${enc}" | python3 -c '
import sys, json; print(json.load(sys.stdin).get("startedAt",""))')"
  local route="/v1/machines/${machine}/sessions/${enc}"
  [[ -n $started ]] && route+="?startedAt=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$started")"

  if _fcode_body DELETE "$route" >/dev/null; then
    print -- "🗑  deleted: $name  (on $machine)"
  else
    print -u2 "fcode: could not delete \"$name\" on $machine — it may have changed since you looked"
    return 1
  fi
}

# ── the multiplexer shim ────────────────────────────────────────────────────
#
# The picker calls `tmux kill-session` and `tmux has-session` INLINE, not
# through a function, so there is nothing to override — and a bare
# `kill-session` run here would either fail for a session on another machine
# or, worse, match a same-named session on this one.
#
# Shimming the multiplexer command itself is not a trick to get around that;
# it is precisely the boundary being replaced. Two verbs are intercepted and
# routed by which machine actually holds the session. Everything else is
# handed to the real binary untouched, and the whole shim is inert unless
# FCODE_ACTIVE is set.
tmux() {
  if (( ! ${FCODE_ACTIVE:-0} )); then
    command tmux "$@"
    return
  fi
  local verb="$1" target name machine
  case $verb in
    kill-session|has-session)
      # -t "=NAME" pins an exact name; strip the pin to look the session up.
      [[ $2 == -t && -n $3 ]] || { command tmux "$@"; return }
      target="$3"; name="${target#=}"
      machine="$(_fcode_machine_for "$name")"
      # Unknown to the fleet view → let the real binary answer, so a session
      # this client never saw behaves exactly as it does today.
      [[ -z $machine ]] && { command tmux "$@"; return }
      if [[ $verb == has-session ]]; then
        # It is in the listing, so it exists. Saying so without a round trip
        # keeps the picker's liveness checks free.
        return 0
      fi
      _ccode_local kill "$name"
      return ;;
    *) command tmux "$@"; return ;;
  esac
}

# ── entry point ─────────────────────────────────────────────────────────────
#
# Same flow as the incumbent, with FCODE_ACTIVE set for the duration. Anything
# the launcher spawns that re-enters ccode (a restored tab, a nested call) sees
# the flag exported, so it stays on the same session layer rather than silently
# reverting.
_fcode_run() {
  emulate -L zsh
  local -x FCODE_ACTIVE=1
  local -x _fcode_scope="$1"; shift
  if [[ -z ${FLEET_URL:-} ]]; then
    print -u2 "fcode: FLEET_URL is not set — refusing rather than guessing a port"
    return 2
  fi
  ccode "$@"
}

# Sessions on this machine. The replacement for the local launcher, and it is
# the same command on every machine: scope=local means "whoever you are".
fcode()  { _fcode_run local "$@" }

# Sessions everywhere. The replacement for the remote launcher — but where that
# one ssh'd into a second launcher on one named host, this asks the local
# service, which fans out to every configured peer and reports the ones that
# did not answer instead of quietly showing fewer sessions.
sfcode() { _fcode_run fleet "$@" }
