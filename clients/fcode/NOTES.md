# NOTES — what building these clients taught

Kept for the same reason the specification keeps its findings log: a reader who
knows only a rule will restate it, while a reader who knows how it was violated
will recognise the next instance.

Nothing here is hypothetical. Every item cost time.

---

## 1. Four zsh names and boundaries that are not free

| trap | what happens |
|---|---|
| `local path=` | `path` is tied to `$PATH`. The search path is empty for the rest of the function, and *everything* afterwards is "command not found" — including `mktemp`, which is how it was found. |
| `local status=` | `status` is **read-only** (it mirrors `$?`). Assigning it aborts the function on the spot. |
| `local -a argv` | `argv` is the positional parameters. |
| `local` inside a piped `while` | A piped loop runs in a subshell, where `local` is outside any function and **prints its declaration** instead of quietly declaring. Feed loops with a here-string. |

The first three were hit in one file, one after another, each failing
differently enough to look like a new problem.

## 2. State does not survive a subshell — and rows do

The launcher loads sessions as:

```zsh
_ccode_load_sessions <<< "$(_ccode_sessions_rooted)"
```

`$( )` is a **subshell**. A producer that records anything in a global — a
name→machine map, a counter, a flag — watches it vanish on return. Only what
goes to stdout survives.

This shipped as a working picker with a broken attach: the session was listed,
and attaching said it did not know which machine held it.

**The fix was not a better way to fill the map.** It was to stop keeping state
across a boundary the caller is free to put a subshell on. The machine is now
looked up on demand — one request, cannot be lost, cannot go stale.

> A cache is a fine thing to have and a terrible thing to require.

## 3. The C locale eats emoji, and a mangled name is a different name

Session names carry emoji. Quoting one for a remote shell under a non-UTF-8
locale renders `💬` as `$'\237'$'\222'` — which is a **different session
name**, so a remote attach reaches the far machine and targets nothing.

Correct under `en_US.UTF-8`. Broken under `C`. And `C` is what a LaunchAgent, a
bare `ssh`, or a phone client hands you — so it fails in exactly the
environments nobody tests in.

Setting `LC_CTYPE` alone would have *looked* like a fix and changed nothing,
because **`LC_ALL` overrides it**. Both are pinned.

## 4. Reading and attaching are not the same reachability

A fleet can be fully readable in both directions over HTTP while ssh works only
one way. `sfcode` on one machine listed the other's sessions perfectly and
could not attach to any of them.

The cause was not a missing credential — the right key was *already
authorized*. `ssh` never offered it, because the key did not have a default
filename and that host had no `~/.ssh/config` at all.

Two lessons:

- **Report which half failed.** "Connection refused" immediately after a
  successful listing is baffling; "the fleet can see it but this machine cannot
  ssh to it" is actionable.
- **`IdentitiesOnly yes` is not a detail.** Without it, ssh may fall back to a
  key that is byte-identical on both machines (an OS-clone artifact), and the
  two hosts become indistinguishable in the target's auth log — destroying the
  identity separation the keys existed to provide.

## 5. A check that shows nothing is not a check that passed

Twice.

Once, a diff of picker rows reported `IDENTICAL` — of two **empty** files,
because the tree builder writes into arrays rather than stdout. The comparison
was meaningless and looked like the strongest possible result.

Once, a debug line printed the name→machine map as empty and it was read past,
because the rows beside it were correct. That empty map was §2's bug, visible
an hour before it was reported from real use.

> Before believing a comparison, confirm that either side has content.

## 6. The cost of watching is not paid by the watcher

The severe one. A forgotten `curl` — a subscription whose parent shell had been
killed — held one control client per session on a 69-session machine for two
hours. Each is a connection to a multiplexer server that launchers,
supervisors and a human's terminal also use, and that server's descriptor
budget is shared by all of them.

It reached its limit and began refusing new clients. Every connection then
failed with *"server exited unexpectedly"*, while all 69 sessions and their
agents were alive and healthy. The supervisor watching that machine concluded
**67 sessions had vanished**, and only its own implausible-disappearance
threshold stopped it from acting.

Fixed in two places, and both were necessary:

- **The client** now kills its stream on every exit path, rather than trusting
  that killing a shell kills what it started.
- **The service** now caps content clients per subscription, because a client
  that misbehaves must not be able to exhaust a machine. The cap costs nothing:
  notifications are triggers rather than data, so any one of them makes the
  service re-read *every* session.

> A limit that only appears in documentation is a limit the system does not
> have. If exceeding it damages something outside the component, the component
> must refuse, not describe.

## 7. "Byte-identical rows" stopped being a virtue once the view spanned machines

Reported from real use and reproduced on the peer machine, 2026-08-06. The
service is not implicated: `/v1/machines` reported both peers `ok`,
`scope=fleet` returned 84 sessions across the two (53 + 31, `complete: true`),
and ssh to the peer worked. Both defects were in this client.

**The row does not say which machine.** `_ccode_sessions_rooted` parses
`machine` out of the JSON and then discards it, emitting the incumbent's
`name<TAB>label<TAB>rel`. That was the trial's success criterion — identical
rows isolate the variable — and it is exactly what makes `sfcode` unreadable: a
session on either machine looks the same. The criterion was right for proving
the layer and wrong the moment the layer's whole point was that the list now
spans machines.

**Worse, a colliding name always resolves local.** `_fcode_machine_for` breaks
at the first JSON match, and the fleet listing puts this machine first. Three
names existed on both machines at the time of the report, each pair pointing at
different working directories — every one of them resolved to the local machine.
There was **no way** to attach to the far machine's copy: the picker showed both
rows, indistinguishably, and both attached locally. This is the reported symptom
"the fleet-wide command does not reach the other machine."

The first-match assumption is only sound when names are globally unique, and
they are not: names are derived from folders, and the folders are synced to both
machines. §2 replaced a lost cache with an on-demand lookup and carried the
assumption across intact — a correctness bug can survive the fix of the bug that
was hiding it.

**Fixed 2026-08-06 (Boss's call): collapse `fcode` and `sfcode` into one command
with a machine picker in front of the session picker.** Not a machine column — a
machine *choice*. You pick a machine, then see that machine's sessions, and the
ambiguity cannot be expressed: the machine is context rather than a field you
must read on every row. It also retires a split the incumbent only had because
reaching another host meant a second launcher over ssh, which the service
already removed.

Note which defect got *fixed* and which got *deleted*. (2) was fixed: the pin
decides, and the first-match lookup is no longer consulted. (1) was not fixed at
all — the row still says nothing about its machine, and no longer needs to. **A
display defect in a view that should not exist is cheaper to remove than to
render.**

What it took, and what was already there:

- **One machine at a time, always.** No fleet-wide view, not even as an option.
- **`fcode` only.** `sfcode` prints a one-line notice and forwards.
- The pin mechanism **already existed** (`FCODE_MACHINE`, and a per-entry-point
  scope). What was missing was the screen that sets it — so the fix is mostly a
  picker, and the picker is the incumbent's own, reused. The gate is ~130 lines
  and every one of them is on this side of the boundary; the incumbent's file is
  still untouched.
- **The pin must survive the choice.** It shows in the picker header and in the
  tab title, and the tab reuses the incumbent's existing local/remote glyph
  grammar (circle vs square) rather than inventing a cue. A pin you cannot see
  is a pin you forget you set — which is the original defect with extra steps.

**Verified, not asserted** (both halves, because §5 of this file was written
about exactly this):

- Pinned to this machine, **31 of 32 rows byte-identical** to the incumbent's,
  both sides non-empty. The 32nd is the service being more right: the launcher
  reads the multiplexer's `session_path` captured at creation, the service reads
  the process's live cwd, and a folder had been moved under a running session —
  the launcher was naming a directory that no longer exists. The same row
  differs against the *pre-gate* client too, so the gate changed nothing.
- All **three** colliding names now resolve to the pinned machine in both
  directions, corroborated by fetching each one's working directory from the
  machine it resolved to — different dirs per machine is the whole reason this
  is checkable at all. Against the pre-gate client, the same probe resolved
  local six times out of six.

**And a papercut found while testing, worth more than it looks.** The client's
default `FLEET_TOKEN_FILE` points at a token whose principal held `read` only.
Every listing worked perfectly and every mutation failed with *"does not hold
the … grant"* — so a first-run user gets a launcher that browses beautifully and
silently cannot kill or rename anything. Read-only defaults are the right call;
a read-only default that is **also** the documented path's neighbour is how you
spend an hour debugging your own new code that was never wrong.

**Measured while planning this, 2026-08-06 — creating through the service is a
service change, not a client one.** `POST /v1/machines/{m}/sessions` exists and
is semantic, but the tmux driver runs `tmux new-session -d -s … -- claude …`
while the launcher runs `zsh -lc '… exec claude --remote-control "$n" -n "$n"'`.
Two consequences, neither visible in a listing: the session is **not reachable
from the phone client** (no `--remote-control`), and it has **no MCP tokens**,
because those are exported by `.zshrc` and there is no login shell — a
service-spawned process inherits the *daemon's* environment. `claudeCodeCommand`
is documented as "the default CommandBuilder", so the seam is deliberate and the
work is configuring it; the env half needs an explicit contract, since testing
from a terminal that already has the tokens would hide the gap entirely.

So the client **refuses, and names the machine it is pinned to.** Falling
through to a local create while the header, the picker and the tab all say
elsewhere would be the same silent-wrong-machine defect wearing a different hat.

**Also found: this file and the README were stale about rename.** Both stated
the API has no rename operation and recorded it as faked-nothing. `POST
/v1/machines/{m}/sessions/{id}/rename` is in the spec and in `auth.go`. Now
routed, and **both halves of it** — the launcher renames the session and then
types `/rename` into the agent so its own UI agrees, and routing only the first
would leave the id changed while the agent kept announcing the old name. Round
trip verified end to end against the running service on a throwaway session.

**The spec has a `machine=` filter on the listing endpoint that the service does
not implement.** Documented in `api-http.md` §3.2, absent from
`handleListSessions`, which reads only `status`, `agent` and `cwdPrefix`. The
client filters its own copy, which costs one pass over a list already in hand —
recorded because the next person to read the spec will assume it works.

> A row format inherited to prove equivalence must be re-opened the moment the
> thing it displays is no longer equivalent.
>
> A documented limitation is a claim with an expiry date. This one outlived the
> API that justified it, and was quoted as fact while planning its replacement.
>
> The cheapest fix for "this view is unreadable" is sometimes not a better view.
