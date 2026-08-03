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

> **Status: one working driver.** A driver over a terminal multiplexer running
> an interactive agent CLI now implements the read path and the write path, and
> has been exercised against 22 concurrent live sessions. It reports a full
> fleet view — with differentiated per-session state — in ~30ms, using a
> constant number of subprocess spawns rather than one per session.
>
> Event subscription is live too, over the substrate's own push channel — no
> polling anywhere, and cost proportional to subscribers rather than to
> sessions. Verified end to end: a session created after subscribing was
> reported within half a second, and its death reported after it.
>
> Federation remains unproven: the design's central claim is that a remote peer
> is just another driver, and until a second driver exists that claim is
> untested. **The specification is still the primary artifact**, and building
> this driver amended six sections of it.

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

Both specs carry **amendment notes** where implementation contradicted them.
Those notes are kept rather than smoothed over: knowing a design was wrong once,
and how it was found out, is worth more than a clean document.

## Layout

```
.                          wire and domain types only — importable by clients
internal/driver            the Driver interface and capability declaration
internal/drivers/stub      a driver that answers unsupported everywhere
internal/drivers/tmux      the first working driver — multiplexer + agent CLI
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
| Go, and zero dependencies | below |

**Go, zero dependencies.** Chosen at zero lines of code, on the reasoning that
language cost is lowest at the start and compounds afterwards. A static binary
removes an entire failure class — nothing to install on the target, no runtime
PATH to get right, no version skew at run time. The standard library covers
routing, HTTP, JSON and concurrency, so the dependency count should stay at
zero; adding one is a decision to argue for, not a convenience.

## Known gaps

Stated plainly so nobody rediscovers them the expensive way.

- **§5.4 cannot be satisfied at the signature §3 gives `close`.** The rule
  requires corroborating an independent attribute before destroying a session,
  but `close(ref)` gives a driver nothing to corroborate *against* — a
  `SessionRef` carries no attribute the caller observed. A driver can close the
  window between **its own** last sighting and the destroy; nothing at this
  signature closes the window between **the caller's** sighting and the
  destroy, which is the long one. Proposed fix (a corroborating attribute on
  `SessionRef`) is recorded in spec §5.4 and deliberately **not applied** —
  it changes a wire type and deserves a decision.
- **Idempotency keys do not survive a service restart**, on a driver that
  correctly declares `supportsResume: true`. Sessions survive; keys are in
  memory. A caller retrying a `create` across a restart therefore gets the
  §10 disaster — two sessions in one working directory — from a driver that
  looks compliant. `supportsResume` answers a question about sessions and was
  being read as answering one about keys. See spec §10's amendment.
- **The write path has never run against a live session.** `send`, `create`,
  `interrupt` and `close` are implemented and unit-tested, and §2.4's refusal
  now fires on real captured screens — but every live exercise so far has been
  read-only by construction, because the sessions available to test against are
  somebody's actual work. The refusal logic is no longer prose; the delivery
  path still is.
- **`subscribe`'s filter cannot name a session.** It can only describe one by
  working-directory prefix. That is a proxy for identity, not identity — and
  here it is also a cost parameter, because watching a session's content costs
  a connection per session on this substrate. A caller wanting one session must
  ask for a directory and hope. See spec §5.5's amendment.
- **Auth has no lifecycle.** A static bearer token is specified; issuance,
  rotation and scoping are not. This is the largest unaddressed surface.
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
3. **A second driver — a remote peer.** This is the real test: the design says a
   remote peer is just another driver, and two implementations is the cheapest
   way to find out whether the interface actually holds. Note that the first
   driver has already exposed a hazard the second will feel more sharply —
   §5.4's corroboration gap is worse across a network, where the caller's
   sighting and the destroy are separated by a round trip rather than a
   function call.
4. **A supervisor as a client**, replacing direct terminal-multiplexer access.

## License

TBD.
