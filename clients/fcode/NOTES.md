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
