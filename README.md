# colab-fleet

An abstraction layer for **running agent sessions across machines**.

A fleet of coding agents needs two separable things: something that decides
*what work should happen*, and something that knows *how a session is actually
run and where*. Most implementations fuse them — the supervisor shells out to a
terminal multiplexer, scrapes the screen to guess what the agent is doing, and
is thereby permanently bound to one runtime on one host.

`colab-fleet` is the second half, extracted. It is a machine-local service that
owns sessions, exposes them over HTTP, and federates with peer instances on
other machines. Supervisors become clients.

> **Status: an agent has been started, questioned and answered on another
> machine, end to end.** From one machine: create a session on a peer, wait for
> the runtime to become ready, deliver its first instruction, watch it stop at a
> tool-permission dialog, read that dialog remotely with every option
> enumerated, answer it by index, and see the agent proceed. That is the whole
> point of the layer, and it works.
>
> A driver over a terminal multiplexer running an interactive agent CLI
> implements the read path, the write path and the answer path, and has been
> exercised against 93 concurrent live sessions across two machines. It reports
> a full fleet view — with differentiated per-session state — in ~30ms, using a
> constant number of subprocess spawns rather than one per session.
>
> **Nothing in that fleet reads `unknown`.** It was 11% until two screen-reading
> bugs were found by asking why: one screen shape that a single capture
> genuinely cannot settle (fixed by looking twice rather than by guessing), and
> a status line whose leading glyph turned out to be an animation frame — five
> were in use at one instant, and the detector matched one of them.
>
> Event subscription is live over the substrate's own push channel and served
> as SSE with cursors, epoch, retention and announced resync — no polling
> anywhere, and watching costs what the subscribers actually asked for. Verified end to end: a session created after subscribing was
> reported within half a second, and its death reported after it.
>
> **Events cross machines.** A relayed event keeps its originating machine and
> the origin's own cursor and epoch as provenance, and takes the relaying
> service's cursor for local ordering — so resumption is never ambiguous about
> whose sequence. Verified live: a session created on one machine appeared on
> the other's stream in under a second, with both sets of coordinates intact.
>
> **Federation is proven.** A second driver — an HTTP client to a peer service —
> satisfies the same interface, and the whole path runs end to end: a caller
> asks a service that holds no local drivers, which proxies through the remote
> driver, over HTTP, into a second service, into the multiplexer driver, and
> back with 22 real sessions in ~30ms. `confidence: inferred` survives the round
> trip rather than being flattened. Neither service needed a special case, and
> the proxying one never learns its peer is remote.
>
> **Authorization is per principal, per verb.** Each caller is a named identity
> with its own credential and its own grants; a relaying service authenticates
> as itself and carries the original principal as an assertion the peer
> records. Every mutation is audited with an actor, not an address.
>
> **A service can say which code it is.** `GET /v1/health` carries a
> version-control build stamp, peers learn each other's, and an unknown or
> locally-modified build never compares equal to anything — so "we disagree"
> stays distinguishable from "we are different vintages". The deploy path reads
> the running build back after restarting and fails if it is not what it just
> installed.
>
> **A session can be driven, not just watched.** Beyond input and answers:
> `rename` (corroborated, announced so subscribers re-key), `discard` (removes
> unsent composer text without submitting it), and a `send` that can resume a
> delivery it could not confirm — submitting only text the service itself
> placed there, never text a human typed.
>
> **The screen no longer collapses failure into idle.** A session out of quota
> reports `quota_blocked`; one whose turn died on a transient error stays
> `idle` and carries `lastTurn`, because it is genuinely ready for input; one
> holding unsent text says so with `waitingOn` and an age. All three used to
> look identical — quiet, with an empty composer — which is how abandoned work
> went unnoticed.
>
> **State survives a restart.** Idempotency keys, the event epoch and cursor,
> and the driver's own session records are persisted atomically, which is what
> makes §12 reconciliation able to tell adopted from orphaned rather than
> calling everything orphaned. Verified: `adopted=24` across a restart.
>
> **The specification is still the primary artifact.** Building against it
> resolved seven of its nine recorded defects, added the types and operations it
> turned out to need, and produced a findings log of 53 measurements — every one of them a
> place the document or the code was wrong before something ran.

## What it owns

A **session**: a running agent process, on some machine, in some working
directory, with an identity, a model, and a state.

## What it must never own

Version control state, worktrees, issue trackers, work claims, planning, or any
judgement about whether work is finished.

The boundary is deliberate and load-bearing:

> **colab-fleet knows a session has a working directory.
> It does not know what a worktree is.**

A fleet layer that learns what an issue is has become a second supervisor, and
now two components believe they are in charge. Every field in this API is
tested against that sentence.

## Why it is shaped this way

Three properties fall out of the same decision:

- **Runtime independence** — a session driver is an interface, not a hardcoded
  subprocess. New agent runtimes plug in without touching callers.
- **Machine-to-machine is free** — a remote peer is *just another driver*. There
  is no separate federation feature to build.
- **Churn is contained** — when a runtime's API changes underneath you, the
  damage lands on one driver instead of everywhere.

That last one matters more than it looks. The value of the abstraction does not
depend on any particular runtime being good; it is what makes betting on one
affordable.

---

## Start here

Read in this order. The specs carry the reasoning; the code is a transcription
of them, not the other way round.

1. [`docs/spec/session-abstraction.md`](docs/spec/session-abstraction.md) — the
   domain model. If you read one thing, read §5.7 (absence vs failure) — it is
   the invariant the rest of the design serves.
2. [`docs/spec/api-http.md`](docs/spec/api-http.md) — the wire protocol.
3. [`doc.go`](doc.go) — an index of what transcription revealed, including the
   judgement calls that were left out of the spec on purpose.
4. [`docs/client-guide.md`](docs/client-guide.md) — **if you are writing a
   client, start here instead.** What to call, what you get back, and the
   handful of things you must handle. Every example is copied from a running
   service.
5. [`docs/adoption.md`](docs/adoption.md) — how an existing supervisor becomes
   a client of this, staged so each step is reversible. Read it if you are
   evaluating whether this is worth adopting; §2 is the precondition that
   surprised us.
6. [`docs/deploy.md`](docs/deploy.md) — how a merged commit reaches a running
   service. This is the procedure `exposure: self` in `.github/project.yml`
   depends on; read it before you next put a commit in front of anything.
7. [`docs/spec/checks.md`](docs/spec/checks.md) — a mechanical check that
   fails the build when a wire type grows a field the specs above don't name,
   what it does and doesn't catch, and the one field currently excepted from
   it on purpose.

If you are picking this up cold and want the short version: read the session
spec's **§14** (the defects, seven of nine now resolved — the open ones are
marked) and **Appendix A's closing section** (the one pattern that has now
recurred at five different altitudes).

The session spec is organised so you can tell current truth from history:
**§1–§13 are normative**, **§14 lists what the document requires but cannot
enforce** — read it before trusting any guarantee — and **Appendix A is the
findings log**, the measurements and bugs that produced the rules.

That appendix is kept rather than smoothed away on purpose: knowing a design was
wrong once, and how it was found out, is worth more than a clean document. A
reader who knows only a rule will restate it; a reader who knows how it was
violated will recognise the next instance.

## Layout

```
.                          wire and domain types only — importable by clients
internal/driver            the Driver interface and capability declaration
internal/drivers/stub      a driver that answers unsupported everywhere
internal/drivers/tmux      the first working driver — multiplexer + agent CLI
internal/drivers/remote    the second — an HTTP client to a peer (federation)
internal/service           registry, one-hop fan-out, HTTP routing
cmd/colab-fleetd           the binary
```

The root package deliberately holds nothing but types, so a third party writing
a client never has to import a driver.

## Build and run

Go 1.26 or newer, per `go.mod`. **No dependencies** — the standard library
covers all of it, and it should stay that way.

(The only language feature actually required is `http.ServeMux` routing
patterns, which landed in 1.22. The floor could likely drop, which would widen
who can build this; nobody has checked what else would break.)

```sh
go build ./...
go test ./...
go vet ./... && gofmt -l .

go run ./cmd/colab-fleetd
```

The binary binds loopback and requires a bearer token; there is no
unauthenticated mode, including in development. Configure via `FLEET_ADDR` and
`FLEET_TOKEN`.

This is the developer loop — running from source, on demand. Putting a merged
commit in front of a service that stays up is a different, documented
procedure: see [`docs/deploy.md`](docs/deploy.md).

## Which agent-CLI versions this is tested against

**A span, not a version: `2.1.220` through `2.1.223`.** Those are the versions
actually being driven on the machines this runs on, measured from the running
processes rather than from what is installed.

A single "tested against X" line would be true and misleading, because **a
session keeps the binary it was started with**. Long-lived sessions therefore
outlive upgrades, and one machine drives several versions at once. Measured on
two machines whose *installed* CLI is identical (`2.1.223` on both):

| | versions running concurrently |
|---|---|
| one machine | `2.1.223` ×48 · `2.1.222` ×21 |
| the other | `2.1.220` ×18 · `2.1.223` ×10 · `2.1.222` ×4 · `2.1.221` ×2 |

Four patch releases live at once on one box, the oldest three releases behind
what is installed. So **the installed version tells you very little about what
this driver is talking to**, and upgrading the CLI does not migrate the
sessions already running.

### Why this matters more here than it usually would

The driver does not call an API. It reads a **terminal UI**: a composer marker,
menu footers, dim (SGR 2) placeholder styling, spinner glyphs, a
running-versus-finished suffix. Every one of those is a rendering detail a patch
release may change, and a changed glyph does not raise an error — it silently
reclassifies. Detection is therefore structural wherever it can be, and no
single footer string is relied on alone.

### The status footer is LIVE STATE, not configuration and not version

Worth stating explicitly, because the natural assumption is wrong in a way that
sends people to the wrong fix.

The standing footer's tail varies between machines and between sessions on the
same machine. It is **not** explained by CLI version — the differing forms
appear under the same version — and it is **not** a settings difference. It is
composed from counts of things running right now. Three shapes observed on one
machine, at one instant, under one set of versions:

```
auto mode on (shift+tab to cycle) · ⇥ 3 agents
auto mode on · 1 monitor · ⇥ 3 agents
auto mode on · 2 monitors · ⇥ 3 agents
```

The trailing count is the machine-wide number of running **background agents**
(confirmed against the CLI's own `agents` listing: 3 background agents renders
`3 agents`), and a **monitors** segment appears alongside it — displacing the
generic hint when present.

Two consequences, and the second is the one people get wrong:

- It changes whenever a background agent or monitor starts or finishes, so it
  **can never be a sole anchor** for classification. Anything keyed on the
  footer tail is keyed on a number that moves under it.
- It **cannot be normalised away by aligning configuration**. There is no
  setting to match, because it is not a setting. Two machines with byte-identical
  configuration and the same CLI version will still render different tails
  whenever they are doing different amounts of work — which is most of the time.

### Running the checks against a live fleet

The scripts under `scripts/` drive a real service and, indirectly, the agent
CLI. One environmental fact will bite anything scripted:

**`claude` is not resolvable from a non-interactive shell.** It is installed
under a user-local `bin` that is added to `PATH` by the *interactive* startup
file, so a non-interactive login shell — which is what `ssh host '…'`, a cron
job, or a process manager gives you — does not see it. Measured on both machines
here; a plain `ssh` session gets `PATH=/usr/bin:/bin:/usr/sbin:/sbin` and the
command is simply not found.

The symptom is unhelpful: a session that starts and dies, leaving a dead pane
and no error anywhere a caller can read.

So anything scripted must either use an absolute path to the CLI or invoke it
through a **login and interactive** shell (`-lic`, not `-lc`). This is the same
distinction the driver itself has to make when it wraps a created session — see
`internal/drivers/tmux/environment.go`, which documents the measurement.

## Decided — pointers, not copies

Settled questions, with the reasoning where it lives. Reopen them on new
evidence, not on taste.

| Decision | Reasoning |
|---|---|
| Addressing is `(machine, id)`; no fleet-wide id | spec §7.1 |
| Peers are statically configured; no discovery | spec §7.2 |
| Fan-out is one hop deep; peers never recurse | spec §13.1 |
| A proxy relays a peer's source status, never re-synthesizes it | spec §13.2 |
| Every driver declares a mandatory deadline | spec §4.4 |
| Plural responses are envelopes, never bare arrays | spec §9 |
| Restart reconciles and adopts; it never destroys | spec §12 |
| Proxy topology, not redirect | spec §13 |
| A remote peer is just another driver | proven — spec §4.2 |
| Go, and zero dependencies | below |

**Go, zero dependencies.** Chosen at zero lines of code, on the reasoning that
language cost is lowest at the start and compounds afterwards. A static binary
removes an entire failure class — nothing to install on the target, no runtime
PATH to get right, no version skew at run time. The standard library covers
routing, HTTP, JSON and concurrency, so the dependency count should stay at
zero; adding one is a decision to argue for, not a convenience.

## Known gaps

Stated plainly so nobody rediscovers them the expensive way.

- **Adoption has a precondition this repository cannot discharge.** The service
  can dispatch an agent to another machine long before the surrounding system
  can safely let it edit anything there: repository state is a non-goal (§1),
  so nothing here prevents two machines editing one working tree, or two
  supervisors claiming one piece of work. Measured, not theorised. See
  [`docs/adoption.md`](docs/adoption.md) §2 — it is the one thing that must be
  answered before a supervisor's *write* path is cut over.
- **There are no metrics.** Subprocess spawn cost is known to degrade with host
  load — 8× idle on a machine at load average 63 (F19) — and nothing measures
  it in production.
- **Deadline composition across a hop is unspecified.** Bounded in practice by
  §13.1's one-hop rule, so it is a correctness gap rather than a live hazard.
  See spec §14 D7.
- **Capability declaration cannot say "I don't know yet."** `Capabilities()` is
  synchronous and infallible — fine for a driver describing itself, impossible
  for one describing a peer across a network. An unreached peer is
  indistinguishable from a peer that genuinely supports nothing. Both degrade
  safely, so the cost is diagnostic rather than correctness: a misconfigured
  peer looks like a minimal one forever. This is §5.7 a third time; see spec
  §14 D3.
- **A subscription's authority is the service's, not the subscriber's.** A
  multiplexed stream serves many callers at once and outlives any of them, so
  there is no single "original caller" whose credential it could present — the
  assumption §13's rule was written on. The service subscribes to peers as
  itself, over a read path only, which bounds the widening to reads but does
  not remove it: a subscriber sees a peer under the service's authority rather
  than its own. See spec §14 D9.
- **Auth has no rotation or expiry.** Credentials are per principal with
  per-verb grants and an audited outcome, and enrolment is now a command
  (`colab-fleetd principal add`) that mints a token and validates grants before
  writing. What is still missing is the rest of a lifecycle: nothing expires, a
  compromised token is revoked by editing a file, and there is no way to change
  a principal's grants except by removing and re-adding it.
- **`SourceState` has no member for "reachable but unsupported"** — currently
  squeezed into `degraded`.
- **Enumeration cost is the real scaling risk, not the network** — and the fix
  is structural rather than incremental. Re-measured on a host running 22 live
  sessions:

  | approach | spawns | wall clock |
  |---|---|---|
  | per-session capture loop (what the incumbent does) | N+1 (23) | 119 ms |
  | one batched invocation | 1 | 18 ms |

  The multiplexer accepts a command sequence in a single invocation, so a full
  fleet view costs a constant number of spawns regardless of session count —
  ~5 ms per session becomes ~0.15 ms per session, and the curve stops being a
  curve. `List` returns everything in one call for this reason.

## Where this is going

1. ~~**A first working driver.**~~ Done, and it earned its keep: it amended
   four sections of the specification and found one defect the specification
   cannot fix on its own (see Known gaps).
2. ~~**Event subscription.**~~ Done, over control mode. The substrate turned
   out to scope content notifications per attached session while broadcasting
   lifecycle notifications fleet-wide, so one always-on client covers sessions
   appearing and disappearing and per-session clients are opened only on
   demand. Notifications are used as change *triggers*; the reads still go
   through the same batched enumeration, so exactly one code path decides what
   a status means.
3. ~~**A second driver — a remote peer.**~~ Done, and it held. It also found a
   different *class* of problem than the first: where the local driver exposed
   places the model was imprecise, the remote one exposed places the interface
   has no room for a concept it requires — caller authority, and "capabilities
   unknown". §5.4's corroboration gap is also visibly worse here, exactly as
   predicted: this driver has no sightings of its own to corroborate against.
4. ~~**Settle the three wire-shape decisions.**~~ Done, all three: caller
   authority is an argument of every operation (§2.6), `startedAt` corroborates
   a destroy (§5.4), and a filter can name sessions (§5.5). Naming turned out
   to be a cost parameter as much as a correctness one — on this substrate,
   watching costs a connection per session, so a subscriber that describes
   instead of naming is charged for the difference.
5. ~~**Answering, not just observing.**~~ Done. A session can be lost to a
   question nobody can reach, and `send` cannot answer one by construction —
   the property that makes it safe for messages makes it useless for control.
   §3 gained `respond`: enumerated options, a nonce so an answer cannot land on
   a question that has changed underneath it, and verification that the prompt
   actually cleared.
6. **A supervisor as a client**, replacing direct terminal-multiplexer access.
   This is where the value actually lands: until it happens, this is a second
   implementation of session management rather than a replacement for one.
   Planned in [`docs/adoption.md`](docs/adoption.md) — including the one
   precondition that cannot be discharged from inside this repository.

## License

[MIT](LICENSE). Copy it, run it, change it — the adoption path in
[`docs/adoption.md`](docs/adoption.md) exists for people who are not us, and a
documented adoption path with no licence grants them nothing.
