# ADR: authoritative, verified process identity — not the delivery itself

**Issue:** #116 (prerequisite for #119; #115 is the discovery note; #117's
still-open human ruling and #119's own delivery change are both untouched by
this)
**Status:** decided

## Context

#115 measured a relay that resolved a session name to a process by
inspecting the process table and derived a delivery target's socket from
that process id. Delivery succeeded — auth accepted, transport clean — and
still landed in the **wrong session**. The socket was real; the identity
backing it was inferred, not verified, and the inference was wrong.

That is a different failure shape from most of this driver's other
refusals. A failed send is recoverable and visible. A send that succeeds
against the wrong target writes a user turn into a session with no context
for it, and nothing in the transport says anything went wrong. #116 was
filed as the prerequisite that has to close before #119 (delivery over a
session's own inbox, replacing the terminal-surface path) is allowed to
exist at all.

#116's own four requirements, quoted from the issue, are about **resolution
and verification**, not about the socket itself:

1. Resolution from an authoritative source at send time, never a cached or
   inferred map.
2. A verification step before the write, not after.
3. A refusal when identity cannot be established — never a best guess.
4. A coverage check: do sessions this driver creates always appear wherever
   the authoritative mapping lives?

## What was already here, unused

`internal/drivers/tmux/tmux.go`'s `enumerate()` already parses
`#{pane_pid}` into `paneRow.pid` on **every** call — never cached, the same
freshness `List`/`State` already rely on for everything else they report.
Nothing downstream ever read that field before this change (`grep -rn
"\.pid\b"` found only its own assignment). It sat there because nothing had
asked resolution requirement #1 of this driver before #116; the raw material
was already correct, just unsurfaced.

It is also already the runtime's own pid, not a shell sitting in front of
it: `environment.go`'s login-shell wrap ends `shift; exec "$@"`, and `exec`
replaces a process's image while keeping its pid. `pane_pid` is therefore
the runtime process's own pid for the life of the pane, not an ancestor of
it.

## Decision

Add `internal/drivers/tmux/processidentity.go`: a `ProcessIdentity{PID,
StartedAt}` pair (the process-axis analogue of `(Pane, Created)`, ADR 97/102
— a bare pid recycles, `StartedAt` is what tells a reused number apart from
the process that used to hold it) and three methods on `*Driver`:

- **`ResolveProcessIdentity(ctx, ref)`** — a fresh `enumerate()` plus a
  fresh OS query for that pid's start time. Refuses
  (`ErrProcessIdentityUnresolved`) on: no such session, a dead pane, an
  unparsable pid (`pid <= 0`, `parseRows`' `strconv.Atoi` failure mode), or
  the OS no longer having that pid by the time this call reaches it. This is
  requirement #1 and half of #3.
- **`VerifyProcessIdentity(ctx, want)`** — re-queries the OS for `want.PID`
  right now and compares against `want.StartedAt`. A future #119 call
  resolves once, then calls this again immediately before the write it is
  about to make, closing the gap between resolving an identity and using it
  — the exact gap a recycled pid exploits. This is requirement #2 and the
  other half of #3.
- **`ProcessIdentityCoverage(ctx)`** — sweeps every live session this
  enumeration finds and reports which ones do not resolve. This is
  requirement #4, answerable on demand rather than latched.

A new counter, `identity.process_unresolved` (same idiom as the existing
`identity.contested`, ADR 97/102's own counter for a different identity
axis), increments from both single-session refusals and coverage-sweep gaps
— one number, so a coverage hole is a rate an operator can watch, not a
one-off log line lost the moment it scrolls.

### Why the OS query is `ps`, shelled out, not a syscall

Peer-credential verification of a unix socket (`SO_PEERCRED` /
`LOCAL_PEERCRED`) is the more common way to answer "is the process on the
other end of this connection who I think it is" — and it is **not in Go's
standard library on this driver's own platform**. Checked directly against
the Go toolchain's own source: `types_linux.go` carries `Ucred`/credential
support, and no darwin equivalent exists in `syscall` at all. `.github/project.yml`'s
own `stack:` line commits this repo to "standard library only, no third-party
dependencies" — so `golang.org/x/sys` is out, and so is cgo, for one feature
on a repo that otherwise has zero build-time dependencies of any kind.

`ps -o lstart= -p <pid>` is the same shape this driver already trusts for
its own substrate: an external process query, shelled out through an
injectable `execFunc`, exactly like `tmux` itself. It also does not require
a live connection to exist yet — #119 has not landed, there is no socket to
attach a peer-credential check to, and #116 was never asked to build one.

### Why a separate exec seam (`psRun`/`psBin`), not `d.run`

`d.run`'s own doc comment says it "runs the multiplexer binary" —
`execFunc`'s type is generic, but every real call site passes `d.bin`, and
every test double built against it (`fakeMux`, shared by dozens of existing
tests) dispatches purely on `args[0]`, assuming those are tmux subcommand
arguments. Routing `ps` calls through the same field would make any fake
built for one seam silently — and wrongly — answer for the other the moment
a test exercised both. A second field costs three lines in `New` and one new
option (`WithPSBinary`/`withPSExec`) and removes that failure mode entirely,
the same tradeoff `dial` already makes as its own field distinct from `run`.

`psBin` defaults to the absolute path `/bin/ps`, not a bare name resolved
against `PATH` — `docs/session-identity.md`'s "Two traps this feature
inherits" already documents why: this call runs outside a created session's
login-shell wrap, on the clean four-entry search path
(`/bin:/usr/bin:/usr/ucb:/usr/local/bin`) everything else this daemon shells
out on directly is held to.

### Why no `fleet.Request` parameter

`internal/driver/driver.go`'s own doc comment requires every `Driver`
interface method to carry the caller's authority, because a proxying
service must present the ORIGINAL caller's credential to a peer, and a
remote driver must refuse rather than substitute its own. These three
methods are not part of that interface and are not independently
network-reachable; they are an internal capability this driver's own
already-authorized operations (a future #119's `Send`) will call from
inside a method that already received `req`. Adding a parameter nothing
here uses would be decoration, not the safeguard it is for the real
interface.

## What this deliberately does not do

- **No socket connection, no auth-line handshake.** #115 established that
  protocol empirically; re-deriving or hard-coding it here, ahead of #117's
  ruling on whether this service may even hold the credential the handshake
  needs, would be building on the "optimistic branch" #118's own issue
  explicitly warns against for a related question.
- **No wiring into `cmd/colab-fleetd`'s startup/interval maintainer.**
  `ProcessIdentityCoverage` is a query a future caller — a health surface,
  or #119 itself before it starts trusting this driver — can run on demand
  or on a schedule it owns. Scheduling it here would touch the composition
  root for a feature with no consumer yet; #116's own file list never named
  `cmd/`.
- **No `fleet`-package (wire) type.** Nothing here needs to cross the
  HTTP boundary yet; `ProcessIdentity` is `internal/drivers/tmux`-private
  because the concept it names — a pid that is the runtime's own process
  because this driver's login-shell wrap `exec`s into it — is specific to
  this one driver's substrate, not a claim `opencode` or `remote` could make
  about themselves.

## Alternatives considered

**Read `pane_pid` once at create time and cache it on the session record.**
Rejected outright: this is precisely the "cached or inferred map"
requirement #1 forbids, and it is exactly the shape of bug #115 measured —
a value that was once true, unrefreshed, presented as still true.

**Verify via the inbox protocol itself (send the per-session auth token,
treat a successful handshake as identity proof).** Stronger in principle —
application-layer proof beats OS metadata — but it needs this service to
hold a per-session credential, which is precisely #117's own open question,
unruled. Revisit once #117 lands; #116 does not depend on its answer and
should not anticipate it.

## Consequences

- `#119`'s delivery path has two concrete calls to make before a write:
  `ResolveProcessIdentity` once, `VerifyProcessIdentity` immediately before
  using the result. Neither exists as an obligation yet — #119 is a
  separate, still-blocked issue — but the shape either the socket write
  itself or #117's answer needs to interpose is not `#116`'s to design.
- `identity.process_unresolved` is live on every machine running this build
  the moment anything calls `ResolveProcessIdentity` or
  `ProcessIdentityCoverage` — but nothing calls either yet, so the counter
  reads zero until a caller exists. That is honest, not a placeholder: a
  metric with no reader is not the same defect as a metric that lies.
