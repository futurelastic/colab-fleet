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
> exercised against 22 concurrent live sessions. It reports a full fleet view —
> with differentiated per-session state — in ~30ms, using a constant number of
> subprocess spawns rather than one per session.
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
> **State survives a restart.** Idempotency keys, the event epoch and cursor,
> and the driver's own session records are persisted atomically, which is what
> makes §12 reconciliation able to tell adopted from orphaned rather than
> calling everything orphaned. Verified: `adopted=24` across a restart.
>
> **The specification is still the primary artifact.** Building against it
> resolved seven of its nine recorded defects, added three types it turned out
> to need, and produced a findings log of 38 measurements — every one of them a
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
4. [`docs/adoption.md`](docs/adoption.md) — how an existing supervisor becomes
   a client of this, staged so each step is reversible. Read it if you are
   evaluating whether this is worth adopting; §2 is the precondition that
   surprised us.

If you are picking this up cold and want the short version: read the session
spec's **§14** (five things that do not work) and **Appendix A's closing
section** (the one pattern that recurred at four different altitudes).

The session spec is organised so you can tell current truth from history:
**§1–§13 are normative**, **§14 lists what the document requires but cannot
enforce** — read it before trusting any guarantee, one of the five is a
security defect — and **Appendix A is the findings log**, the measurements and
bugs that produced the rules.

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
- **Nothing reports the build a service is running.** Two machines silently ran
  different builds, and the older one still had a bug fixed in the newer; the
  symptom was a stranded prompt that made no sense against the source. The
  health endpoint should carry a build identifier, and a peer whose build
  differs should be able to say so.
- **Binding is single-address.** A service bound only to a tunnel interface is
  unreachable from its own machine when that tunnel half-fails, and the symptom
  is indistinguishable from a wedged process. Loopback should always be bound
  alongside whatever else is. See spec F36.
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
- **Auth has no lifecycle.** Credentials are per principal with per-verb grants
  and an audit trail, but they are static: issuance, rotation and expiry are
  unspecified, and adding a principal is editing a config and restarting.
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

TBD.
