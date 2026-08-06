# fcode — the incumbent launcher's UI, with the fleet service underneath.
#
# `fleetctl.zsh` (beside this file) is a standalone client with its own small
# interface. This file is the opposite approach and the more useful one for
# deciding anything: it keeps the launcher you already use — the picker, the
# folder browser, the grouping, the naming rules, every keybinding — and
# replaces ONLY the part that talks to the terminal multiplexer.
#
# That isolates the variable. If the interface is identical, then anything you
# notice is the session layer, which is the only thing being evaluated.
#
#   source /path/to/<launcher>.zsh     # your existing launcher, unchanged
#   source /path/to/fcode.zsh          # this file
#
#   fcode     pick a machine, then that machine's sessions
#   sfcode    deprecated — prints a notice and forwards to `fcode`
#
# ONE MACHINE AT A TIME, ALWAYS
#
# `fcode` opens a MACHINE PICKER first, then the launcher's own session picker
# scoped to the machine you chose. Not a machine column — a machine *choice*.
#
# There is deliberately NO fleet-wide list, not even as an option, because the
# wide list is what produced the two defects this gate exists to close:
#
#   1. Rows could not say which machine they came from. The listing emitted the
#      incumbent's `name<TAB>label<TAB>rel` so the UI above it stayed untouched
#      — which was the right call while proving equivalence, and became
#      unreadable the moment the list spanned machines.
#   2. A name existing on BOTH machines always resolved to the local one, so
#      the far copy could not be reached at all. Names come from folder names,
#      the folders are synced, so collision is the normal case.
#
# A picker fixes (1) by making it unnecessary rather than by widening the row,
# and fixes (2) by making the ambiguity inexpressible: the machine is context,
# not a field you must read correctly on every line.
#
# `ccode` and `sccode` keep working exactly as before. The overrides below
# delegate to the originals unless FCODE_ACTIVE is set, and only `fcode` sets
# it — so a bug here cannot change what the incumbent does. Nothing in the
# incumbent's own file is edited; delete the source line and this all reverts.
#
# Config (no machine names in this file):
#   FLEET_URL         this machine's service        (required)
#   FLEET_TOKEN_FILE  token for this client         (default ~/.config/colab-fleet/token)
#   FLEET_SSH_FMT     how to reach a peer, %s = machine  (default "ssh -t %s")
#   FCODE_MACHINE     pin to one machine, skipping the picker (scripts, habit)
#
# WHAT CHANGES, AND WHAT DOES NOT
#
#   listing   → one HTTP call to the local service, for the PINNED machine.
#               Pinned to this machine it asks scope=local, which is the same
#               question the incumbent's local mode answers — so the rows are
#               byte-identical. Pinned elsewhere it asks the fleet and keeps
#               that machine's items.
#   attach    → the service says how; this decides where. A session on another
#               machine attaches without a second launcher on the far side.
#               The machine comes from the PIN, never from a first match.
#   kill      → same, corroborated by start time, so a recycled id cannot be
#               mistaken for the session you looked at.
#   rename    → ROUTED to the pinned machine (both halves: the session id via
#               /rename, and the agent's own name via /input). Earlier versions
#               of this file and its README said the API had no rename
#               operation. That was true when written; the operation exists,
#               and an unrouted rename on a pinned remote session silently
#               acted locally and found nothing — the same wrong-machine class
#               as (2) above.
#   new       → REFUSED when pinned to another machine, naming that machine.
#               Creating there should go through the service, but the driver
#               spawns `tmux new-session -- claude …` while the launcher runs
#               `zsh -lc '… exec claude --remote-control …'`: a service-created
#               session is not reachable from the phone client and inherits the
#               daemon's environment instead of the login shell's credentials.
#               That is a service change. Until it lands, refuse and say so —
#               never create silently on the wrong machine.

(( $+functions[_ccode_sessions_rooted] )) || {
  print -u2 "fcode: source your launcher first — this file overrides parts of it."
  return 1
}
[[ -n ${FLEET_URL:-} ]] || print -u2 "fcode: set FLEET_URL to this machine's colab-fleet service."

: ${FLEET_TOKEN_FILE:=$HOME/.config/colab-fleet/token}
: ${FLEET_SSH_FMT:=ssh -t %s}

# Which slice of the fleet the current invocation is about. `local` asks this
# machine's service for its own sessions only (§13.1's scope=local), which is
# the same question the incumbent's local mode answers — and, usefully, needs
# no knowledge of what this machine is called.
typeset -g  _fcode_scope=local

typeset -gA _fcode_machine_of      # cache only: id → machine. NEVER depended on.
typeset -g  _fcode_partial=0       # was the last listing missing a machine?
typeset -g  _fcode_self=""         # this machine's own name, per the service

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
#
# # Why it resolves against the PIN and not the first match
#
# It used to return the first item whose id matched, and a fleet listing puts
# the answering machine first. With three names live on both machines, every
# one of them resolved local and the far copy was unreachable — the picker
# showed both rows, indistinguishably, and both attached to the same session.
#
# First-match is only sound when names are globally unique, and they are not:
# names derive from folder names and the folders are synced to both machines.
# So the pin decides. What is still ASKED is whether the name exists THERE —
# answering "yes, on the pinned machine" without checking would trade a
# wrong-machine attach for a confident attach to nothing.
_fcode_machine_for() {
  local name="$1"
  [[ -n ${_fcode_machine_of[$name]:-} ]] && { print -r -- "${_fcode_machine_of[$name]}"; return }
  local body
  body="$(_fcode_body GET "/v1/sessions?scope=${_fcode_scope}")" || return 1
  print -r -- "$body" | FCODE_WANT="$name" python3 -c '
import sys, json, os
want = os.environ["FCODE_WANT"]
pin  = os.environ.get("FCODE_MACHINE") or ""
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for s in d.get("items", []):
    if s.get("id") != want: continue
    if pin and s.get("machine") != pin: continue   # the pin decides, never position
    print(s.get("machine","")); break'
}

# This machine, as the service names it. Asked rather than derived: the client
# is deliberately free of machine names, and `hostname` is not guaranteed to
# match what the fleet calls this host.
_fcode_whoami() {
  [[ -n $_fcode_self ]] && { print -r -- "$_fcode_self"; return }
  # Fetch, THEN parse. Piping a failed request straight into python prints a
  # JSONDecodeError traceback over the caller's terminal and returns python's
  # exit status, so an unreachable service reads as a client crash. This is the
  # first call `fcode` makes, so that traceback would be the whole first
  # impression of a service being down.
  local body
  body="$(_fcode_body GET /v1/machines)" || return 1
  _fcode_self="$(print -r -- "$body" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for m in d.get("items", []):
    if m.get("self"): print(m["machine"]); break')" || return 1
  print -r -- "$_fcode_self"
}

# ── the machine picker ──────────────────────────────────────────────────────
#
# The screen that was missing. The pin mechanism (FCODE_MACHINE) already
# existed and the entry point already set a scope per command; what no code
# path offered was a way for a person to choose.
#
# Reuses the incumbent's own picker so the keys are the ones already in your
# fingers — ↑/↓, j/k, Enter, Esc/q — and owns the alternate screen for its own
# duration, in an always{} block, exactly as the launcher's menu flow does.
# Otherwise a Ctrl-C at the machine list would leave the terminal on the alt
# buffer with the cursor hidden.
_fcode_pick_machine() {
  emulate -L zsh
  local body
  body="$(_fcode_body GET /v1/machines)" || {
    print -u2 "fcode: cannot reach the session layer at ${FLEET_URL} — refusing rather than guessing a machine"
    return 1
  }

  # machine<|>self<|>status, self first: you are usually staying put.
  local -a rows
  rows=("${(@f)$(print -r -- "$body" | python3 -c '
import sys, json
items = json.load(sys.stdin).get("items", [])
items.sort(key=lambda m: (not m.get("self"), m.get("machine","")))
for m in items:
    print("%s<|>%s<|>%s" % (m.get("machine",""), "1" if m.get("self") else "0",
                            m.get("status","")))')}")

  local -a names labels
  local line m is_self st mark
  for line in "${rows[@]}"; do
    [[ -z $line ]] && continue
    m="${line%%<|>*}"; line="${line#*<|>}"
    is_self="${line%%<|>*}"; st="${line#*<|>}"
    [[ -z $m ]] && continue
    (( is_self )) && { _fcode_self="$m"; mark="  (this machine)" } || mark=""
    # A machine that is not `ok` stays SELECTABLE. It is reported, not hidden:
    # hiding it would make an unreachable machine indistinguishable from one
    # that does not exist, which is the failure this whole layer exists to
    # avoid. Choosing it gets an honest empty listing with a reason.
    [[ $st == ok ]] || mark="${mark}  ⚠️  ${st:-unknown}"
    names+=("$m")
    labels+=("$(_ccode_glyph "$m" $( (( is_self )) && print local || print remote )) ${m}${mark}")
  done

  (( ${#names} )) || { print -u2 "fcode: the session layer knows no machines"; return 1 }

  # One machine is not a choice. The incumbent's folder browser skips its own
  # root picker on a single root for the same reason.
  (( ${#names} == 1 )) && { REPLY="$names[1]"; return 0 }

  local rc=1
  {
    [[ -t 1 ]] && _ccode_screen_enter
    trap ':' INT
    _CCODE_PICK_SKIP=(); _CCODE_PICK_RIGHT=0
    if _ccode_pick "Which machine?  (↑/↓ · Enter · Esc/q cancel)" "${labels[@]}"; then
      REPLY="${names[$_CCODE_IDX]}"; rc=0
    fi
  } always {
    trap - INT
    _ccode_screen_leave
  }
  return $rc
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

  # machine<|>name<|>cwd, plus a PARTIAL marker if the PINNED machine is the one
  # that did not answer.
  #
  # A fleet-scope call fans out to every peer, so before the pin existed any
  # unreachable machine printed a warning here — including machines whose
  # sessions this listing was never going to show. Scoped to the pin, the
  # warning stops being noise and starts being the whole story: if the pinned
  # machine is down, this list is not partial, it is empty for a reason.
  local -a rows
  rows=("${(@f)$(print -r -- "$body" | python3 -c '
import sys, json, os
d = json.load(sys.stdin)
want = os.environ.get("FCODE_MACHINE") or ""
down = [s.get("machine","") for s in d.get("sources", []) if s.get("status") != "ok"]
if want:
    down = [m for m in down if m == want]
if down:
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
      print -u2 "⚠️  fcode: no answer from ${name} — its sessions are NOT listed and are NOT known to be gone"
      continue
    fi
    [[ -z $name ]] && continue
    # With a pin every item is from the pinned machine, so this map has one
    # possible value per name and the collision that made it wrong is gone.
    # It stays a cache and stays never-depended-on: the subshell in
    # `... <<< "$(_ccode_sessions_rooted)"` still discards it.
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
  self="$(_fcode_whoami)"

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

  # The pin has to survive the choice that set it. Once you are attached the
  # machine picker is three screens ago, and a session named after a folder
  # that exists on both machines looks exactly the same either way — so the tab
  # carries the machine, and takes the incumbent's own remote cue (a SQUARE
  # glyph instead of a circle, same hue) when it is not this one.
  if [[ $machine == $self ]]; then
    _ccode_tab_title "$name" local 2>/dev/null
    "${cmdv[@]}"
    return
  fi
  _ccode_tab_title "${name}·${machine}" remote 2>/dev/null
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

# ── rename ──────────────────────────────────────────────────────────────────
#
# `POST /v1/machines/{m}/sessions/{id}/rename` exists (api-http.md §3.2). This
# file and its README both used to say the API had no rename operation and
# recorded it as deliberately-not-faked. That was true when it was written, and
# a documented limitation is a claim with an expiry date: this one outlived the
# API that justified it, and an unrouted rename on a pinned remote session
# acted locally and quietly found nothing.
#
# Renaming is TWO writes, and doing only the first is its own silent failure:
# the id the operator sees changes while the agent keeps announcing the old
# name in its own UI. The launcher does both (rename-session, then send-keys
# "/rename"), so both are routed.
_fcode_rename() {                     # $1 = old name  $2 = new name  $3 = machine
  emulate -L zsh
  local old="$1" new="$2" machine="$3" enc started
  enc="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$old")"
  started="$(_fcode_body GET "/v1/machines/${machine}/sessions/${enc}" | python3 -c '
import sys, json; print(json.load(sys.stdin).get("startedAt",""))')"

  # Same reason DELETE wants it: acting on the wrong session here succeeds
  # SILENTLY and leaves it wearing somebody else's name.
  local route="/v1/machines/${machine}/sessions/${enc}/rename"
  [[ -n $started ]] && route+="?startedAt=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$started")"

  local payload
  payload="$(FCODE_NEW="$new" python3 -c 'import json,os; print(json.dumps({"name": os.environ["FCODE_NEW"]}))')"
  _fcode_body POST "$route" "$payload" >/dev/null || {
    print -u2 "fcode: could not rename \"$old\" on $machine — the name may be taken there, or it changed since you looked"
    return 1
  }
  return 0
}

# ── the multiplexer shim ────────────────────────────────────────────────────
#
# The launcher calls `tmux kill-session`, `has-session`, `rename-session`,
# `send-keys` and `new-session` INLINE, not through a function, so there is
# nothing to override — and a bare call run here would either fail for a
# session on another machine or, worse, match a same-named session on this one.
#
# Shimming the multiplexer command itself is not a trick to get around that;
# it is precisely the boundary being replaced. The verbs below are intercepted
# and routed by which machine actually holds the session. Everything else is
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

    rename-session)
      # `tmux rename-session -t "=OLD" NEW`
      [[ $2 == -t && -n $3 && -n $4 ]] || { command tmux "$@"; return }
      name="${3#=}"
      machine="$(_fcode_machine_for "$name")"
      [[ -z $machine ]] && { command tmux "$@"; return }
      _fcode_rename "$name" "$4" "$machine"
      return ;;

    send-keys)
      # The launcher's second half of a rename: `send-keys -t "=NEW" "/rename
      # NEW" Enter`, typed into the agent so its own UI agrees with the id.
      # Routed through the input endpoint, which is the same act.
      [[ $2 == -t && -n $3 && -n $4 ]] || { command tmux "$@"; return }
      name="${3#=}"
      machine="$(_fcode_machine_for "$name")"
      [[ -z $machine ]] && { command tmux "$@"; return }
      [[ $machine == "$(_fcode_whoami)" ]] && { command tmux "$@"; return }
      local enc payload submit=0 outcome
      [[ "${@[-1]}" == Enter ]] && submit=1
      enc="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$name")"
      payload="$(FCODE_TEXT="$4" FCODE_SUBMIT="$submit" python3 -c '
import json, os
print(json.dumps({"text": os.environ["FCODE_TEXT"],
                  "submit": os.environ["FCODE_SUBMIT"] == "1"}))')"
      # A refusal is a 200 carrying a reason, not an HTTP error (api-http.md
      # §3.2) — so success here is not the same as the text having landed.
      outcome="$(_fcode_body POST "/v1/machines/${machine}/sessions/${enc}/input" "$payload" | python3 -c '
import sys, json
d = json.load(sys.stdin) or {}
print("%s\t%s" % (d.get("outcome",""), d.get("reason","")))' 2>/dev/null)"
      case "${outcome%%$'\t'*}" in
        submitted|queued) return 0 ;;
        "")               print -u2 "fcode: could not reach \"$name\" on $machine to type into it"; return 1 ;;
        *)                print -u2 "fcode: $machine refused the input for \"$name\" — ${outcome#*$'\t'}"
                          print -u2 "       The session id was renamed; the agent still announces the old name."
                          return 1 ;;
      esac ;;

    new-session)
      # Create on the pinned machine THROUGH the session layer, then attach.
      #
      # This replaces a refusal. The refusal was correct while the service
      # produced a different KIND of session — no remote-control binding, and
      # no login shell, so none of the credentials an agent's tool servers
      # need. Both are now the service's job and it does them; the environment
      # a created session receives is even readable back from it.
      #
      # Falling through to the real binary is still wrong for the same reason
      # it always was: it would create the session HERE while the picker, the
      # header and the tab all say elsewhere.
      machine="${FCODE_MACHINE:-}"
      [[ -z $machine || $machine == "$(_fcode_whoami)" ]] && { command tmux "$@"; return }

      # The launcher's own invocation is
      #   new-session -s <name> -c <dir> zsh -lc '…' ccode <name> <dir>
      # so the two things the service needs are already in argv. Read them by
      # flag rather than by position: the trailing argv is the launcher's
      # business and may grow.
      local want_name="" want_dir="" i=1
      while (( i <= $# )); do
        case "${@[i]}" in
          -s) (( i++ )); want_name="${@[i]}" ;;
          -c) (( i++ )); want_dir="${@[i]}" ;;
        esac
        (( i++ ))
      done
      if [[ -z $want_name || -z $want_dir ]]; then
        print -u2 "fcode: cannot create on ${machine} — no name/-c directory in this invocation"
        return 1
      fi

      # An idempotency key is REQUIRED (§10) and one is minted per invocation.
      #
      # Deliberately not derived from name+directory: the launcher numbers a
      # colliding name precisely so a second session in one project is a normal
      # thing to want, and a deterministic key would hand back the FIRST
      # session instead of creating it. The failure that trade buys — a create
      # that times out, gets retried by the human, and produces two agents in
      # one directory — is the one a person can see and undo, whereas silently
      # attaching to somebody else's older session is not.
      local key payload created id
      key="fcode-$(date +%s)-$$-${RANDOM}"
      payload="$(FCODE_NAME="$want_name" FCODE_DIR="$want_dir" python3 -c '
import json, os
print(json.dumps({"name": os.environ["FCODE_NAME"], "cwd": os.environ["FCODE_DIR"]}))')"

      created="$(_fcode_body POST "/v1/machines/${machine}/sessions" "$payload" \
                   "Idempotency-Key: ${key}")" || {
        print -u2 "fcode: ${machine} would not create \"${want_name}\" — the service refused or is unreachable"
        return 1 }

      # READ THE ID BACK. It is not necessarily the name that was asked for:
      # the service owns the naming rules, so it sanitizes the name and numbers
      # it when a session of that name is already live. Measured: asking for
      # "gate.verify" returns "gate-verify" plus the type marker.
      #
      # Everything after this point must use the returned id. Assuming the
      # requested name addresses the new session is right until the first
      # collision, and then it silently addresses SOMEBODY ELSE'S session —
      # which is the whole class of wrong-machine, wrong-session defect this
      # file exists to close.
      id="$(print -r -- "$created" | python3 -c '
import sys, json
d = json.load(sys.stdin) or {}
i = d.get("id")
if not i: sys.exit(3)
print(i)')" || {
        print -u2 "fcode: ${machine} created a session but returned no id"; return 1 }

      if [[ $id != $want_name ]]; then
        print -u2 "fcode: ${machine} named it \"${id}\" (asked for \"${want_name}\")"
      fi

      # Attach through the same path every other session uses, by id. It
      # resolves which machine holds the session and ssh's there when that is
      # not this one, so nothing about attaching is special-cased here.
      _ccode_local_attach "$id"
      return ;;

    *) command tmux "$@"; return ;;
  esac
}

# ── the pinned machine, in the launcher's own header ────────────────────────
#
# The launcher builds its menu header from `hostname`, which is right for it
# and wrong here: with a pin, the sessions on screen may be another machine's
# while the header names this one. Wrapping the flow is enough — the header
# arrives as $1 — and it keeps the edit on this side of the boundary.
if (( ! $+functions[_fcode_orig_flow] )); then
  functions[_fcode_orig_flow]=$functions[_ccode_flow]
fi
_ccode_flow() {
  (( ${FCODE_ACTIVE:-0} )) || { _fcode_orig_flow "$@"; return }
  local machine="${FCODE_MACHINE:-}" hdr="$1"; shift
  if [[ -n $machine ]]; then
    if [[ $machine == "$(_fcode_whoami)" ]]; then
      hdr="Sessions on ${machine}  (this machine)"
    else
      hdr="Sessions on ${machine}  ⟵ pinned"
    fi
  fi
  _fcode_orig_flow "$hdr" "$@"
}

# ── entry point ─────────────────────────────────────────────────────────────
#
# Same flow as the incumbent, with FCODE_ACTIVE set for the duration. Anything
# the launcher spawns that re-enters it (a restored tab, a nested call) sees
# the flag exported, so it stays on the same session layer rather than silently
# reverting.
#
# The pin is chosen HERE, before the launcher is entered, because everything
# downstream — the listing, the header, attach, kill, rename, the refusal to
# create — reads it. Choosing it later would mean a screen that shows sessions
# before it knows whose they are.
#
# Scope follows the pin rather than the entry point:
#
#   pinned to this machine → scope=local. The same question the incumbent's
#     local mode asks, so the rows come back byte-identical. That is this
#     client's own standard for "the gate changed nothing it should not".
#   pinned elsewhere → scope=fleet, keeping that machine's items. There is a
#     documented `machine=` filter on the listing endpoint (api-http.md §3.2)
#     that the service does not implement yet; filtering here costs one pass
#     over a list we already have.
fcode() {
  emulate -L zsh
  if [[ -z ${FLEET_URL:-} ]]; then
    print -u2 "fcode: FLEET_URL is not set — refusing rather than guessing a port"
    return 2
  fi

  # Resolve this machine's name ONCE, up front, and EXPORT it.
  #
  # Everything below asks "is the pin this machine?", much of it from inside
  # `$( )`. A global cache cannot help there — §2 of NOTES.md, the same lesson
  # that removed the name→machine map: a subshell throws away what it learns.
  # An exported value goes the other way, so one request answers all of them.
  #
  # It doubles as the reachability check, deliberately before any UI: finding
  # out the session layer is down is better than finding out after drawing a
  # machine picker built from nothing.
  local -x _fcode_self=""
  # Two different failures, said differently. "I could not ask" and "it
  # answered and no machine claimed to be this one" send you to opposite ends
  # of the system, and this client's whole posture is that a report which
  # cannot distinguish them is not a report.
  _fcode_self="$(_fcode_whoami)" || {
    print -u2 "fcode: cannot reach the session layer at ${FLEET_URL}"
    return 1
  }
  # Pinning against "" would silently take every comparison below down the
  # remote branch — an empty answer must not become a quiet wrong one.
  [[ -n $_fcode_self ]] || {
    print -u2 "fcode: the session layer at ${FLEET_URL} answered, but no machine claims to be this one"
    return 1
  }

  local pin="${FCODE_MACHINE:-}"
  if [[ -z $pin ]]; then
    if (( $# )); then
      # `fcode <name>` is the incumbent's fast path: no UI, act now. Putting a
      # picker in front of it would be a regression in the one path the trial
      # promised to leave alone — so it pins to THIS machine, which is exactly
      # what the incumbent does with the same arguments. Explicit, and never a
      # first-match guess.
      pin="$_fcode_self"
    else
      _fcode_pick_machine || { print "Cancelled."; return 1 }
      pin="$REPLY"
    fi
  fi

  local -x FCODE_ACTIVE=1
  local -x FCODE_MACHINE="$pin"
  local -x _fcode_scope=fleet
  [[ $pin == $_fcode_self ]] && _fcode_scope=local

  ccode "$@"
}

# Folded into `fcode`, which now asks which machine instead of having one
# command per answer. Kept as a forwarder because it is in muscle memory and in
# shell history; it goes once that stops being true.
sfcode() {
  print -u2 "sfcode: merged into fcode — it now asks which machine. Forwarding…"
  fcode "$@"
}
