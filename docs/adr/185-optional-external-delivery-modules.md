# 185 — optional external delivery modules: discovery, a switch, a fallback that never loses a message

**Issue:** #185
**Status:** decided; the parts that need a person's ratification are listed at
the end.

## Context

#180 shipped the delivery-module **seam** (`internal/delivery`) and one module,
the built-in terminal path, and deliberately stopped there: designing discovery,
a configuration switch and fallback against one implementation would have been
guessing. This is the change that adds the second implementation's worth of
requirements — an **optional external module** an operator can install beside
the daemon, that delivers `/input` text by a channel other than the composer.

Three things make that harder than "call another function":

- **A module cannot be a Go plugin.** `internal/` cannot be imported across a
  module boundary, and a plugin ABI would tie two release cadences together. A
  module is a separate program spoken to over a small local protocol.
- **A lane is decided before the agent starts.** A module's environment has to be
  in the agent process from its first instruction, so a module can only be
  offered to a session this service *launches*. Nothing can be retrofitted.
- **A second path is a second way to deliver twice.** #184 spent an ADR on
  keeping the inbox and the terminal from both carrying one message. A module
  adds a third path with the same hazard.

## Decision

### A module is a child process speaking JSON lines

One executable per module in a modules directory, named by the module.
`colab-fleetd` starts `<dir>/<name> serve` as a same-user child with a minimal
environment, exchanges newline-delimited JSON over its stdio, and treats every
byte it returns as untrusted input: a line is capped at 4 MiB (an oversize line
is drained, never buffered), unknown fields are ignored, and nothing a response
says is ever used to build a path, a command line or a state key. Six operations
— `prepare-launch`, `attach`, `send`, `confirm`, `close`, `health` — are the
whole contract; the wire is specified in the issue and implemented in
`internal/delivery/modclient`.

The client is one goroutine reading, one writing, and a pending table keyed by a
request id that never repeats across restarts. Two errors matter to callers and
are kept apart: **not sent** (nothing reached the child, so falling back is
safe) and **lost** (bytes went out and no answer came, so the outcome is
unknown). A request whose deadline passes marks the module *suspect* and probes
its health at once; two consecutive failed probes make it unhealthy, and if both
were deadline misses the child is killed and restarted.

### The switch, and off by default

`FLEET_DELIVERY_MODULES` — an ordered, comma-separated list of module names, in
order of preference. Empty or unset means none: no child is started, no state
file is written, no field appears, and behaviour is byte-for-byte #180. Set the
same value in every machine's service environment for a fleet-wide setting, and
override it in one machine's own for a per-machine one. `FLEET_MODULES_DIR`
overrides the directory (default `<prefix>/libexec/colab-fleet/modules`, where
prefix is the parent of the daemon binary's directory after symlinks resolve —
set it explicitly when the binary is reached through a symlink).
`FLEET_DELIVERY_MODULE_ENV` lists the only environment names forwarded to the
child from the daemon's own environment, beyond `PATH HOME USER LANG TMPDIR
FLEET_STATE_DIR`; a name beginning `FLEET_` is never forwarded, because this
service's credentials are not the module's.

An enabled name with no executable, a file that is not executable, or one that
is group- or world-writable is logged once and skipped; the doctor reports it as
a warning, never a failure — a machine that could not fetch the module is a
legitimate state, not a fault.

### Startup, asynchronous

The children are started at daemon startup, without blocking it. The alternative
— start on first need — was rejected: re-attaching adopted sessions' lanes needs
the module up anyway, the first health probe and the reserved prefixes are wanted
before the first create, and a lazy start would put spawn and handshake (up to 3
s) on the first create's path. The cost is one idle child on a machine that
enabled a module and has not used it. A create that arrives before the module is
ready gets the built-in lane, with the reason recorded.

A module whose handshake is refused (an unsupported protocol version, a missing
operation, a malformed line, no line in 3 s) is **disabled until the daemon
restarts**, not restarted in a loop.

### A lane is one session's, chosen at create

At create the enabled modules are asked in order and the first that answers
`prepare-launch` holds the session's lane. Its environment is merged into the
launch environment **after** the reserved-name filter, and rides the staged-env
file: values never reach a command line. Every name it returns must begin with a
prefix it declared reserved, or the whole answer is refused and the session
launches without a lane. The record `{module, laneKey, pid}` is written **before**
the process is launched, so a crash leaves something the next start can close.
The attach is asynchronous — the create response never waits on it — and is
polled for up to two minutes while the module says the agent's record or socket
is not there yet, because a first-run trust dialog can hold the agent back.

The session's `delivery` field says which lane an `auto` send would take now, and
why. **Absent means "not configured or not yet probed", never "terminal".**

A lane is live only while **all** of these hold: the module is available and not
suspect; its last health probe said `peerCheck: true`; the lane's last attach
said `live: true` and did not say `peerVerified: false`; and that attach ran
under the module process that is running now. A restarted module has forgotten
every connection, so until each lane is re-attached it is not live.

**A module that reports `peerCheck: false` is treated as not live** (maintainer
ruling, 2026-09-24). Nothing is ever delivered without the peer check.

### Reserved environment names, by prefix

#180 reserved exact names. A module declares **prefixes** in its own handshake,
so they cannot be listed ahead of time. The guard: a caller-supplied variable
under a reserved prefix is a `400` naming it; a configured machine `sessionEnv`
entry under one is dropped at launch; and the module's returned environment is
the only source. The prefixes are **persisted**, so the guard holds on a machine
where the module is not installed or not running right now — the property that
makes it a guard and not a courtesy. A module that has never run on a machine
reserves nothing there: the gap is real and is stated, not papered over.

### Routing: what may use a module

`route` becomes a closed set: `""`/`auto`, `terminal`, `inbox`, and the name of
each module in this machine's `FLEET_DELIVERY_MODULES` — whether or not its
executable is present. It is validated in the service's request-shape switch,
before any driver is resolved; an unknown value is a `400` naming every accepted
value.

A module carries the **user's own turn**, exactly as the terminal does, so it
inherits the terminal's authority rules rather than the inbox's:

| send | path |
|---|---|
| a non-human sender, `auto` | inbox → module → built-in (ADR 184's preference for the inbox is kept; the spec was written when `route` was a boolean) |
| a human relay, `auto` | module → built-in, never the inbox (the service still turns it into `terminal`, and now marks it `TerminalFromAuto`) |
| an explicit `terminal` | built-in only, never a module |
| `<module>` | that module, or a refusal — never a downgrade |

A forced module route from a non-human caller needs a label that prints, like
`route:"terminal"` — an unlabelled agent send would otherwise be recorded as
human-typed input just because it named a lane. A forced module route with
`submit:false`, `resumeIfStranded` or `replaceIfStranded` is a `400`: a module has
no composer.

`TerminalFromAuto` is never a caller's field and is **not forwarded to a peer**.
A relayed human send reaches the owning machine as an explicit `terminal`, so it
stays on the built-in path there; the human can still force a module by name.
This is cheaper than a wire field an older peer would not know, and it is
reversible without a migration.

The authority guard is unchanged and stays first: a leading `!` or `/` from an
unlabelled sender is refused before routing, on the sanitised text; the module
receives the **labelled** text; `allowLeadingSlash` is passed only for a human
relay. The module enforces its own floor as well; neither is the only wall. The
create-time prompt stays pinned to the terminal.

### Outcomes: never twice

| the module says | receipt | then |
|---|---|---|
| confirmed | `queued` — never `submitted`: a user-origin turn the runtime accepted is not the agent's acknowledgement | — |
| queued when the window ends | `unknown` | ledger entry |
| silent, or a confirm that got no answer | `unknown`; **no second write on any path** | lane degraded; ledger entry |
| rejected | `refused` | lane degraded |
| not-live, unknown-lane, unsupported-version, version-unknown, or nothing written | handed back: `auto` → built-in **for this send only**; forced → refused, "nothing was written" | lane not live |
| refused, too-large, bad-request | `refused`, the same wording the built-in path gives | — |
| write-failed, internal, an unknown code, a send with no answer | `unknown`, never resent | lane degraded; ledger entry |

An `unknown` from a module leaves an entry in #184's cross-path ledger, holding
the module, the lane handle and the send handle. A follow-up of the same text is
answered from it for 30 minutes: the module is asked once, with no wait —
confirmed answers `queued` and drops the entry; rejected drops it and lets the
send proceed; anything else holds, and **nothing is written on either path**. It
also stops an `auto` send from pasting into the composer text a module may
already have delivered.

A degraded lane returns to service only when a later attach reports it live: the
background pass re-attaches degraded lanes, so later `auto` sends use the built-in
path until then.

### Restart, adoption, teardown

After a restart, an **adopted** session's lane is re-attached when its module is
ready; a **vanished** session's lane is closed and dropped; a lane whose pane
process changed is closed (a resume relaunch goes through create and gets its
own). A close that could not reach the module is remembered and flushed when it is
next ready. `Close` stays the kill authority: the module hears about it after or
alongside, and its failure never blocks it. A rename re-keys the record — the lane
belongs to the process, not the name.

### Health, and what an operator sees

`health` is called at startup and every 30 s, and on any suspicious failure.
`GET /v1/runtimes` carries `deliveryModules` — name, status
(`starting|available|unavailable|disabled`), reason, protocol, version, platform,
`peerCheck`, and per-lane state counts. `GET /v1/health` carries the counters,
all under `module.<name>.`: `hello_ok`, `hello_refused`, `spawned`, `exited`,
`restarted`, `deadline_missed`, `prepare_ok`, `prepare_refused`, `attach_live`,
`attach_not_live`, `reattach_on_restart`, `reattach_failed`, `send_written`,
`send_confirmed`, `send_unknown`, `send_rejected`, `send_refused_no_live_lane`,
`fallback_to_builtin`, `closed`. The middle segment is the module's name because
more than one can be enabled. `delivery.<name>.*` (#180's family) is recorded as
well. `fallback_to_builtin` counts only sends that had a lane and could not use
it; a session that was never offered one is not a fallback.

### The installer

An optional module is **installed only when the installing user can fetch it**;
otherwise nothing is installed and nothing is said. `scripts/fetch-module.sh`
takes a name, a source and an output path, always exits 0, prints nothing, and
leaves the output absent on any failure. A source is a Go package path with a
version, built with the installing user's own credentials — which is exactly the
access test — or an absolute directory holding a main package. `deploy.sh` runs
it for each `name=source` in `FLEET_MODULE_SOURCES` between install and restart,
and installs the result beside the daemon with the same atomic rename it uses for
the binary. A failed fetch never fails the deploy and never removes a module
that is already installed.

### What this does not do

- **`claudeVersion` is not passed to `prepare-launch`.** The spec says colab-fleet
  should: it resolved the binary. It has not: the login shell resolves it, after
  this service is out of the picture, and a wrong answer would make a module
  refuse a runtime it could have served. The module's own `PATH` is the better
  witness, and `unsupported-version` from `attach`/`send` still stops a bad build
  reaching the module lane.
- **No override for `unsupported-version`.** A version the module refuses stays on
  the built-in path.
- **No relay of module operations.** Only the `route` string, the session's
  `delivery` field and the receipt's module route cross machines; a lane is
  decided on the machine that owns it. A peer built before this change answers an
  unknown route with a `400`, which is the right answer to a forced request.
- **The module's `transcript.path` is carried and never opened**; the module's own
  `confirm` is used, because it already knows the runtime's transcript shapes.
- **`internal/compat` is untouched:** it checks the terminal path's assumptions
  about a runtime build; a module owns its own.

## Needs ratification

1. **Inbox before module for a non-human `auto`.** Keeps ADR 184; the alternative
   is "module first" as the spec's table reads. It is one line.
2. **A relayed human's `auto` does not reach a module lane.** `TerminalFromAuto` is
   local. The alternative is a trusted relay header, like the human-relay one.
3. **Disabled until restart** after a refused handshake, versus retry with
   backoff. A module with a bad build would otherwise be respawned forever.
4. **A forced module route needs a printing label** unless the caller is a human
   relay — the terminal's rule, applied to the same authority.
5. **One lane per session, from the first module in order that answers.** Two
   modules' environments could conflict, and a session with two lanes has two
   answers to "where did it go".

## Alternatives rejected

- **A Go plugin, or a shared library.** Ties release cadences together; `internal/`
  cannot cross the boundary.
- **A network service.** The module is a same-user local child: nothing is exposed,
  and there is nothing to authenticate.
- **Lazy spawn.** See above.
- **Restart a refused module with backoff.** A refusal is a fact about the build,
  not a transient.
- **Pass the module a token.** The module keeps its own credentials, so none
  crosses the boundary and none can leak from here.
- **Silently paste through the built-in path after a module `unknown`.** That is
  the duplicate the whole design exists to prevent.
