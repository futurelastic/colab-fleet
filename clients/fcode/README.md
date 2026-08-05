# fcode — session launcher over colab-fleet

Two commands that do what a terminal-multiplexer launcher does, except they ask
a **service** instead of the multiplexer:

```
fcode      sessions on THIS machine       — the local launcher's job
sfcode     sessions on EVERY machine      — the remote launcher's job, without the ssh hop
```

Plus a standalone client for scripts and machines with no launcher installed:

```
fleetctl   ls · up · watch · new · kill · attach
```

| file | what it is |
|---|---|
| `fcode.zsh` | the launcher integration — defines `fcode` and `sfcode` |
| `fleetctl.zsh` | the standalone client — defines `fleetctl`, depends on nothing |
| `NOTES.md` | what building these taught, including four ways zsh will bite you |

---

## Install

Both files are plain zsh with no dependencies beyond `curl` and `python3`.

```sh
export FLEET_URL=http://127.0.0.1:<port>              # your machine's service
export FLEET_TOKEN_FILE=~/.config/colab-fleet/fcode.token
export FLEET_SSH_FMT='ssh -t <alias-pattern>-%s'      # how THIS host reaches a peer

source /path/to/your/launcher.zsh                     # unchanged
source /path/to/clients/fcode/fcode.zsh
```

To make it permanent, put that block in `~/.zshrc` **after** the line that
sources your launcher, and mark it so it can be removed in one edit.

`fleetctl` is independent — source it (or not) separately.

### Configuration

| variable | meaning | default |
|---|---|---|
| `FLEET_URL` | this machine's service | none — **required**, no port is guessed |
| `FLEET_TOKEN_FILE` | file holding this client's token | `~/.config/colab-fleet/token` |
| `FLEET_SSH_FMT` | how to reach a peer, `%s` = machine id | `ssh -t %s` |
| `FCODE_MACHINE` | pin the view to one machine | unset (scope decides) |

**`fcode` is the same command on every machine.** It asks for `scope=local`, so
the service answers for itself — the client never needs to know what host it is
on, and there is no per-machine configuration to drift apart.

### Your existing launcher is not modified

`fcode.zsh` overrides three of the launcher's functions, and every override
delegates to the original unless `FCODE_ACTIVE` is set — which only `fcode` and
`sfcode` set. The incumbent commands keep working exactly as before, and a bug
in here cannot change what they do. Removing the source line removes everything.

---

## What is actually different

Only the session layer. The picker, the folder browser, the grouping, the
naming rules and every keybinding are the launcher's own code, untouched.

Verified rather than asserted: driving the launcher's own tree builder from
both layers on the same machine produced **byte-identical picker rows**.

| seam | before | after |
|---|---|---|
| listing | one `tmux ls` on one host | one HTTP call, every machine, with unreachable ones named |
| attach | `tmux attach` | the argv the **owning** machine reports, run here or over ssh |
| kill | `tmux kill-session` | corroborated by start time, routed to the right machine |

Three seams were enough, which says more about the incumbent's design than
about this one: its session layer was already nearly separable.

### Why the multiplexer command itself is shimmed

The picker calls `kill-session` and `has-session` **inline**, not through a
function, so there is nothing to override — and a bare `kill-session` run
locally would either fail for a session on another machine or, worse, match a
same-named session on this one. While `fcode` runs, the multiplexer command is
a shim that routes those two verbs by machine and hands everything else to the
real binary. That is not a workaround; it is the boundary being replaced.

### What is deliberately NOT routed

- **`new`** — the launcher builds a richer command than the driver's spawn path
  (agent flags, credentials, restore behaviour). Creating still goes through
  the launcher you already trust; routing it would change more than the session
  layer, which is the one thing this is supposed to isolate.
- **`rename`** — the API has no rename operation. A real gap, recorded rather
  than faked. Renaming still acts locally and will not find a remote session.

---

## What it refuses to do

Each of these is a rule the service's own guide states, applied here:

- **Never an empty list when the service is unreachable.** An empty picker
  reads as "no sessions", which is a claim. "I could not ask" is a different
  answer and gets said out loud.
- **Never "gone" for a session missing from a partial view.** If a machine did
  not answer, its sessions are not listed and are *not known to be absent* —
  the message says so in those words.
- **Never a destroy without corroboration.** Deleting quotes back the session's
  start time, so a recycled id cannot be mistaken for the session you looked at.
- **Never the token in `argv`.** It goes to `curl` through a config file on
  stdin, because anything in argv is visible in `ps` to every process on the
  machine.

---

## Troubleshooting

**"FLEET_URL is not set — refusing rather than guessing a port."** Intentional:
a guessed default quietly probes the wrong thing.

**"session layer unreachable."** The service is down or the URL is wrong. Note
what this does *not* say: it does not say there are no sessions.

**"no answer from: `<machine>`."** That peer did not respond. Its sessions are
not shown, and are not known to be gone.

**"the fleet can SEE `<machine>` but this machine cannot ssh to it."** Reading
is HTTP and attaching is ssh, and the two are not symmetric — a fleet can be
fully readable in both directions while ssh works only one way. Either attach
from that machine, or fix `FLEET_SSH_FMT` / your ssh config.

**A remote attach that reaches the far machine and finds nothing.** Almost
always the locale: see `NOTES.md`.

**Mutations refused** — *"principal … does not hold the … grant"*. Grants are
per verb and per machine. `relay` is what permits mutating a *peer*, and it is
off by default on purpose.
