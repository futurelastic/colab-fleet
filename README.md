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
> Event subscription is live over the substrate's own push channel and served
> as SSE with cursors, epoch, retention and announced resync — no polling
> anywhere, and watching costs what the subscribers actually asked for. Verified end to end: a session created after subscribing was
> reported within half a second, and its death reported after it.
>
> **Federation is proven.** A second driver — an HTTP client to a peer service —
> satisfies the same interface, and the whole path runs end to end: a caller
> asks a service that holds no local drivers, which proxies through the remote
> driver, over HTTP, into a second service, into the multiplexer driver, and
> back with 22 real sessions in ~30ms. `confidence: inferred` survives the round
> trip rather than being flattened. Neither service needed a special case, and
> the proxying one never learns its peer is remote.
>
> **The specification is still the primary artifact**, and building these two
> drivers amended eight sections of it.

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

- **Idempotency keys do not survive a service restart**, on a driver that
  correctly declares `supportsResume: true`. Sessions survive; keys are in
  memory. A caller retrying a `create` across a restart therefore gets the
  §10 disaster — two sessions in one working directory — from a driver that
  looks compliant. `supportsResume` answers a question about sessions and was
  being read as answering one about keys. See spec §10 and §14 D5.
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
  ask for a directory and hope. See spec §14 D4.
- **The operations have nowhere to carry the caller's authority — and the
  natural fallback is a security bug.** §13 requires a proxying service to
  present the *original caller's* credentials, never its own. No operation in
  §3 takes a principal, so the only channel is an out-of-band context value
  that a service can silently forget. A remote driver missing it will reach for
  the credential it definitely has — its own — and then everything works,
  every test passes, and every machine is a confused deputy for every other.
  Note the asymmetry: every other gap here fails toward a wrong answer somebody
  eventually notices; this one fails toward a **correct-looking answer with the
  authorization quietly widened.** The current driver refuses rather than
  substituting, which is a driver declining to do something the interface
  cannot stop it doing. See spec §14 D1.
- **Capability declaration cannot say "I don't know yet."** `Capabilities()` is
  synchronous and infallible — fine for a driver describing itself, impossible
  for one describing a peer across a network. An unreached peer is
  indistinguishable from a peer that genuinely supports nothing. Both degrade
  safely, so the cost is diagnostic rather than correctness: a misconfigured
  peer looks like a minimal one forever. This is §5.7 a third time; see spec
  spec §14 D3.
- **Events do not cross machines.** A service streams its own drivers' events;
  a peer's do not arrive, because the remote driver cannot subscribe. That
  inverts §5.5's requirement — push works only for callers who are already
  local. The blocker is a design decision, not code: whose cursor and epoch a
  relayed event carries, and how one stream expresses "resync this source
  only". See spec §14 D8.
- **Auth has no lifecycle.** A static bearer token is specified; issuance,
  rotation and scoping are not.
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
4. **Settle the three wire-shape decisions** now that two implementations exist
   to weigh them against: caller authority as an operation parameter (§6),
   a corroborating attribute on `SessionRef` (§5.4), and a filter that can name
   sessions (§5.5).
5. **A supervisor as a client**, replacing direct terminal-multiplexer access.
   This is where the value actually lands: until it happens, this is a second
   implementation of session management rather than a replacement for one.

## License

TBD.
