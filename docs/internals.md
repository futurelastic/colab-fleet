# Internals — measurements, decisions and gaps

Engineering material that used to live in the README. It is kept in full,
because most of it is a measurement somebody made once and would otherwise
have to make again.

Read this if you are working **on** the service. If you are working **with**
it, you want [`api.md`](api.md) for the endpoint reference or
[`client-guide.md`](client-guide.md) for a walkthrough.

The normative documents are elsewhere and this file never overrides them:
[`spec/session-abstraction.md`](spec/session-abstraction.md) is the domain
model, [`spec/api-http.md`](spec/api-http.md) is the wire protocol.

---

## Layout

```
.                          wire and domain types only — importable by clients
internal/driver            the Driver interface and capability declaration
internal/drivers/stub      a driver that answers unsupported everywhere
internal/drivers/tmux      the first working driver — multiplexer + agent CLI
internal/drivers/remote    the second — an HTTP client to a peer (federation)
internal/drivers/opencode  the second LOCAL driver — a spawned subprocess,
                           the first able to declare observesState: true
internal/service           registry, one-hop fan-out, HTTP routing
cmd/colab-fleetd           the binary
```

The root package deliberately holds nothing but types, so a third party writing
a client never has to import a driver.


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

