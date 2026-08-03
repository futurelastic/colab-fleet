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

> **Status: skeleton.** The types and the HTTP routing exist; no working driver
> does. Every operation is served by a driver that answers `unsupported`, so
> what exists proves the interface shape and the wire error contract — not any
> actual session management. **The specification is still the primary artifact.**

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

- **`send` and `DeliveryReceipt` are unvalidated.** The two-machine exercise that
  tested the rest was read-only by construction, so the refusal semantics in
  §2.4 are still prose that has never run.
- **Auth has no lifecycle.** A static bearer token is specified; issuance,
  rotation and scoping are not. This is the largest unaddressed surface.
- **`SourceState` has no member for "reachable but unsupported"** — currently
  squeezed into `degraded`.
- **Enumeration cost is the real scaling risk, not the network.** Measured on a
  terminal-multiplexer host: per-session introspection dominated by roughly two
  subprocess spawns per session, so listing ~80 sessions cost about a second
  while the network round trip cost a third of that. Any driver written as
  one-query-per-session will not scale past a few dozen. `List` returns
  everything in one call for this reason.

## Where this is going

1. **A first working driver.** Until one exists, every claim here is
   unfalsified rather than proven.
2. **A second driver — a remote peer.** This is the real test: the design says a
   remote peer is just another driver, and two implementations is the cheapest
   way to find out whether the interface actually holds.
3. **A supervisor as a client**, replacing direct terminal-multiplexer access.

## License

TBD.
