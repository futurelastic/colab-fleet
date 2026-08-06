# fcode — session launcher over colab-fleet

One command that does what a terminal-multiplexer launcher does, except it asks
a **service** instead of the multiplexer:

```
fcode      pick a machine, then that machine's sessions
sfcode     deprecated — prints a notice and forwards to fcode
```

**One machine at a time, always.** `fcode` opens a machine picker first, then
the launcher's own session picker scoped to what you chose. There is no
fleet-wide list, deliberately: see [Why there is no fleet-wide view](#why-there-is-no-fleet-wide-view).

Plus a standalone client for scripts and machines with no launcher installed:

```
fleetctl   ls · up · watch · new · kill · attach
```

| file | what it is |
|---|---|
| `fcode.zsh` | the launcher integration — defines `fcode` (and `sfcode`, deprecated) |
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
| `FCODE_MACHINE` | pin to one machine, skipping the picker | unset — you are asked |

⚠️ **The default token file is probably not the one you want.** It defaults to
`token`, and a principal named `operator` holding only `read` is a common thing
to find there. Everything *lists* perfectly and every mutation fails with
`does not hold the … grant`. Point `FLEET_TOKEN_FILE` at a principal that holds
`close` and `rename`, as the install block above does.

**`fcode` is the same command on every machine.** It asks the service which
machine is `self` rather than deriving it from `hostname`, so the client still
needs no per-machine configuration and carries no machine names.

### Your existing launcher is not modified

`fcode.zsh` overrides four of the launcher's functions, and every override
delegates to the original unless `FCODE_ACTIVE` is set — which only `fcode`
sets. The incumbent commands keep working exactly as before, and a bug in here
cannot change what they do. Removing the source line removes everything.

---

## What is actually different

Only the session layer. The picker, the folder browser, the grouping, the
naming rules and every keybinding are the launcher's own code, untouched.

Verified rather than asserted: pinned to the machine you are on, driving the
launcher's own tree builder from both layers produced **31 of 32 byte-identical
picker rows**, and the one that differed is the service being *more* right —
see below.

| seam | before | after |
|---|---|---|
| listing | one `tmux ls` on one host | one HTTP call, for the pinned machine |
| attach | `tmux attach` | the argv the **owning** machine reports, run here or over ssh |
| kill | `tmux kill-session` | corroborated by start time, routed to the pinned machine |
| rename | `tmux rename-session` | routed to the pinned machine, both halves |

Four seams were enough, which says more about the incumbent's design than about
this one: its session layer was already nearly separable.

#### The one row that differs

The launcher reads the multiplexer's `session_path`, recorded when the session
was created. The service reads the process's **actual** working directory. Move
a folder under a running session and they disagree — measured: the launcher
still named a directory that no longer exists, while the service named the one
the process had followed the move into.

Worth stating plainly because "byte-identical" was this client's whole standard
of proof, and the exception is not a defect in it.

### Why there is no fleet-wide view

There was one, and it produced two defects that only appear once a list spans
machines:

1. **Rows could not say which machine they came from.** They emitted the
   incumbent's `name<TAB>label<TAB>rel` so the UI above stayed untouched — the
   right call while proving equivalence, unreadable afterwards.
2. **A name on both machines always resolved to the local one**, so the far
   copy could not be reached at all. Names come from folder names, folders are
   synced, so collision is the *normal* case. Measured: three names live on
   both machines, every one resolved local.

A machine picker fixes (1) by making it unnecessary rather than by widening the
row, and fixes (2) by making the ambiguity inexpressible. Keeping the wide list
as an option would have kept the wrong-machine attach reachable for the rarer
case, which is where it is hardest to notice.

The machine stays visible **after** the choice — in the picker header, and in
the tab title, which takes the incumbent's own remote cue (a square glyph
instead of a circle) when the pin is not this machine.

### Why the multiplexer command itself is shimmed

The launcher calls `kill-session`, `has-session`, `rename-session`, `send-keys`
and `new-session` **inline**, not through a function, so there is nothing to
override — and a bare call run locally would either fail for a session on
another machine or, worse, match a same-named session on this one. While
`fcode` runs, the multiplexer command is a shim that routes those verbs by
machine and hands everything else to the real binary. That is not a workaround;
it is the boundary being replaced.

### `rename` is routed — both halves

Renaming is two writes, and doing only the first is its own silent failure: the
id an operator sees changes while the agent keeps announcing the old name in
its own UI. So `rename-session` goes to `POST …/rename` (with `?startedAt=`,
for the same reason `DELETE` wants it), and the launcher's follow-up
`send-keys "/rename …"` goes to `POST …/input`.

A refusal from `input` is a `200` carrying a reason, not an HTTP error — so it
is read and reported, including the specific case worth knowing about: the id
was renamed and the agent's own name was not.

### What is deliberately NOT routed

- **`new`** — and, pinned to another machine, **refused outright, naming that
  machine**. `POST /v1/machines/{m}/sessions` exists and is semantic, but the
  driver spawns `tmux new-session -- claude …` while the launcher runs
  `zsh -lc '… exec claude --remote-control …'`. Two consequences, neither
  visible in a listing: no remote-control flag, so the session is unreachable
  from the phone client; and no login shell, so it inherits the daemon's
  environment instead of the credentials the rc file exports. That is a service
  change. Until it lands, refusing is the honest answer — creating locally
  while the header says otherwise is the same wrong-machine defect in a
  different coat.

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

**"no answer from `<machine>`."** The machine you pinned did not respond. Its
sessions are not shown, and are not known to be gone. Only the *pinned*
machine's silence is reported — a peer you did not choose is not your problem
this run.

**"pinned to `<machine>` — refusing to create a session HERE."** Working as
intended; see [What is deliberately NOT routed](#what-is-deliberately-not-routed).
Run `fcode` on that machine, or pick this one.

**"the fleet can SEE `<machine>` but this machine cannot ssh to it."** Reading
is HTTP and attaching is ssh, and the two are not symmetric — a fleet can be
fully readable in both directions while ssh works only one way. Either attach
from that machine, or fix `FLEET_SSH_FMT` / your ssh config.

**A remote attach that reaches the far machine and finds nothing.** Almost
always the locale: see `NOTES.md`.

**Mutations refused** — *"principal … does not hold the … grant"*. Grants are
per verb and per machine. `relay` is what permits mutating a *peer*, and it is
off by default on purpose.
