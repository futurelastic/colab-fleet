# clients/

Working clients of this service. They exist to be run, not just read: a client
that nobody has executed is a guess about the API, and the guesses in
`docs/client-guide.md` were wrong three times before anything ran.

## `fcode.zsh`

A session launcher over the fleet service — list, attach, create, destroy,
watch — in about 320 lines of zsh with no dependencies beyond `curl` and
`python3`.

```sh
export FLEET_URL=http://127.0.0.1:<your port>
source clients/fcode.zsh

fcode                     # every session in the fleet, with state
fcode <prefix>            # attach to the one that matches
fcode watch               # stream state changes
fcode up                  # is the layer up, what build, which machines
fcode new <machine> <name> <cwd>
fcode kill <prefix>
```

Configuration is environment only — there are no machine names in the file:

| variable | meaning | default |
|---|---|---|
| `FLEET_URL` | the service on **this** machine | none — required |
| `FLEET_TOKEN_FILE` | file holding this client's token | `~/.config/colab-fleet/token` |
| `FLEET_SSH_FMT` | how to reach another machine, `%s` = machine id | `ssh -t %s` |

### It is deliberately a second tool

It does not replace, wrap, patch or import any existing launcher, and sourcing
it shadows nothing — the command names differ on purpose. Run both, compare,
and keep whichever earns it. The incumbent stays in production untouched until
somebody decides otherwise.

### What it demonstrates

Three behaviours that are awkward without a session service, and fall out here:

- **One endpoint for the whole fleet.** `fcode` lists every machine's sessions
  in a single round trip. There is no peer list in this file.
- **Attach without knowing the substrate.** The service returns argv; the
  client only decides *where* to run it, by comparing the session's machine
  against `self` from `/v1/machines`. Verified across two machines whose
  multiplexer binaries live at different paths, because they are different
  architectures — a client that hardcoded either would be wrong on one.
- **Watching instead of polling.** `fcode watch` is a push stream. It is
  usually silent, because events fire on state transitions and not on output.

### What it refuses to do

- It never renders an empty list when it could not reach the service; that
  reads as "no sessions" and is a lie.
- It never collapses "not found in a partial view" into "gone" — if a machine
  did not answer, the session may be alive on it, and the exit code says
  unknown rather than absent.
- It quotes `startedAt` back when destroying, so it cannot kill a different
  session that inherited a recycled id.
- It sends an idempotency key on every create.

## `fcode-ui.zsh` — the incumbent's UI, this service underneath

`fcode.zsh` above has its own small interface, which makes it a poor instrument
for judging the session layer: you end up comparing two interfaces at once.

This file takes the other approach. It keeps the launcher you already use —
picker, folder browser, grouping, naming rules, keybindings — and replaces only
the functions that talk to the multiplexer.

```sh
source /path/to/your/launcher.zsh    # unchanged
source clients/fcode-ui.zsh

fcode      # sessions on THIS machine   — replaces the local launcher
sfcode     # sessions on EVERY machine  — replaces the remote one
```

The pair mirrors the incumbent's own split, because the split is right: one is
what you use while working on a machine, the other is how you reach the rest of
the fleet.

**`fcode` is the same command on every machine.** It asks the local service for
its own view (`scope=local`), so it needs no idea what the machine is called
and no per-host configuration. Verified on two: 27 sessions where the
incumbent's `tmux ls` reported 27, and 73 where it reported 73.

**`sfcode` is where the remote launcher's shape changes.** That one ssh'd into
a second launcher on one named host. This asks the local service, which fans
out to every configured peer — and when a machine does not answer it says so
rather than quietly showing fewer sessions.

The incumbent's own commands keep working: every override delegates to the
original unless `FCODE_ACTIVE` is set, and only `fcode` sets it.

**Verified identical.** Driving the launcher's own tree builder from both
layers on the same machine produced byte-identical picker rows — the tree, the
counts, the grouping, all still the launcher's. Switch to fleet scope and the
same UI renders the whole fleet instead.

Three seams were enough:

| seam | before | after |
|---|---|---|
| listing | one `tmux ls` on one host | one HTTP call covering every machine |
| attach | `tmux attach` | the argv the owning machine reports, run here or over ssh |
| kill | `tmux kill-session` | corroborated by start time, routed to the right machine |

The picker calls two of those **inline** rather than through a function, so the
multiplexer command itself is shimmed while `fcode` runs. That is not a trick
around the design; it is the boundary being replaced, which is why so little
code was needed.

**Not routed, deliberately:** `new` still uses the incumbent's spawn path,
which builds a richer command than the driver's, and `rename` has no operation
in the API at all — a real gap, recorded rather than faked.

### A fifth trap: the C locale eats emoji, and a mangled name is a different name

Session names carry emoji. Quoting one for a remote shell under a non-UTF-8
locale renders `💬` as `$'\237'$'\222'` — a **different session name**, so the
attach silently targets nothing. Correct under `en_US.UTF-8`, broken under `C`,
and `C` is what a LaunchAgent, a bare `ssh` or a phone client hands you.

Setting `LC_CTYPE` alone would have looked like a fix and changed nothing,
because `LC_ALL` overrides it. Both are pinned.

### And a fourth zsh trap: state does not survive a subshell

The launcher loads sessions as `... <<< "$(_ccode_sessions_rooted)"`. Command
substitution runs in a **subshell**, so a producer that records anything in a
global — a name→machine map, a count, a flag — sees it discarded on return.
Only what goes to stdout survives.

This shipped as a working picker with a broken attach: the session was listed,
and attaching reported not knowing which machine held it. The fix is not a
better way to populate the map; it is to **stop keeping state across a boundary
the caller is free to put a subshell on.** The machine is now looked up on
demand — one request, cannot go stale, cannot be lost.

### Three zsh names that are not free

Written down because all three were hit while building this, and each fails
differently enough to waste an afternoon:

| name | what happens |
|---|---|
| `path` | tied to `$PATH` — a `local path=` empties the search path, and everything afterwards is "command not found" |
| `status` | **read-only** (it mirrors `$?`) — assigning it aborts the function |
| `argv` | the positional parameters |

Also: a `while` loop fed by a **pipe** runs in a subshell, where `local` is
outside any function and prints its declaration instead of declaring quietly.
Feed loops with a here-string.
