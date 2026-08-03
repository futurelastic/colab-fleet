# Session abstraction — specification

**Status: two drivers satisfy this interface** — one local, over a terminal
multiplexer running an interactive agent CLI; one remote, an HTTP client to a
peer service. That was the threshold this document set for itself, and it is
met: no claim here is now unexercised prose.

§4.2's central claim survived. A remote peer really is just another driver,
proven end to end with no special case in either service — a caller asked a
service holding no local drivers, which proxied through the remote driver, over
HTTP, into a second service, into the multiplexer driver, and back with 22 real
sessions. `confidence: inferred` survived the round trip rather than being
flattened to `observed`.

Five things did **not** survive contact, and they are collected in **§14, Open
defects**. Read that section before relying on any guarantee in this document:
one of the five is a security defect, and its symptom is that everything
appears to work.

### How to read this

- **§1–§13 are normative.** They state the design as it is now, in the present
  tense. Where a rule is known to be unenforceable, the section carries a short
  blockquote naming the defect — the rule is left in place because it is still
  what a driver should do.
- **§14 is the list of things this document requires but cannot enforce.** Each
  entry states the rule, why it fails, what that costs, and a proposed fix.
  A proposed fix is not a decision.
- **Appendix A is how any of this was learned.** Measurements, the bugs that
  taught the rules, and the reasoning behind decisions that look arbitrary from
  the outside. Sections above point into it by finding number (F1, F2, …).

The separation is deliberate. Earlier revisions kept each discovery inline as
an amendment, which preserved the reasoning but grew until a third of the
document was archaeology and a first-time reader could not tell current truth
from a record of having been wrong. Nothing was deleted in the consolidation —
the narratives moved to Appendix A, and the rules they produced moved into the
body.

---

## 1. Scope

`colab-fleet` owns **sessions**. It creates them, delivers input to them,
reports their state, and destroys them. It does this identically whether the
session runs on the local machine or a peer.

### Non-goals

Explicitly outside this layer, permanently:

- version-control state, branches, worktrees
- issue trackers, work claims, assignment
- planning, scheduling, or deciding what should be worked on
- any judgement about whether work is complete or correct

A supervisor built on top of this layer owns all of the above. If a field in
this API would only make sense to something that understood those concepts, the
field is wrong.

---

## 2. Core model

### 2.1 SessionSpec

What a caller must supply to start a session.

```
SessionSpec {
  machine    : MachineId          // which host should run this
  runtime    : RuntimeId          // which driver
  cwd        : AbsolutePath       // working directory
  agent?     : AgentId            // named persona/config
  model?     : string
  effort?    : string
  name?      : string             // human-facing label
  prompt?    : string             // initial input
  contextRef?: AbsolutePath       // see §5.3 — never inline, never argv
}
```

`agent`, `model` and `effort` are **hints**, not guarantees. A driver that
cannot honour one must say so at creation rather than silently substituting a
default; see §4.3.

### 2.2 SessionRef

```
SessionRef {
  machine : MachineId
  id      : string        // opaque, scoped to (machine, runtime)
  name?   : string
}
```

Ids are **machine-scoped and potentially recyclable**. A caller must never
treat an id as a globally unique identity, and must never act destructively on
an id match alone — see §5.4.

### 2.3 SessionState

```
SessionState {
  status     : Status
  confidence : "observed" | "inferred"
  evidence   : string             // human-readable provenance
  since?     : Timestamp | null
}

Status =
  | "starting"        // spawned, not yet accepting input
  | "working"         // actively producing
  | "waiting_input"   // blocked on a human or caller
  | "idle"            // alive, finished, awaiting more work
  | "quota_blocked"   // alive but refused by its provider
  | "dead"            // process gone
  | "unknown"         // driver cannot determine — a real answer, not an error
```

Three rules govern this type, and they are the point of the whole
specification:

**`unknown` is a valid answer.** Some runtimes genuinely cannot report their
own state. A driver must return `unknown` rather than guessing, and callers
must handle it as an ordinary case rather than a fault.

**`confidence` separates knowing from guessing.** A driver that reads a
structured status from an API reports `observed`. A driver that infers state
from terminal output, process tables, or file mtimes reports `inferred`. Both
are legitimate. Collapsing the distinction is how a precise runtime gets
flattened to an imprecise one's accuracy — the interface would then destroy the
exact advantage it exists to expose.

**`evidence` is for humans.** It carries whatever the driver actually saw. It
is never parsed by callers, and its format is not stable.

Two further rules follow from the first, and they are normative rather than
advisory — a driver that violates either produces confident wrong answers:

**Reach `unknown` from unrecognised evidence, not only from absent evidence.**
When a driver sees a signal it does not recognise, the answer is `unknown`.
"No positive evidence of working" must never decay to `idle`: a wrong `idle`
for a session that is working is silent, and an `unknown` is not.

**Fail toward `unknown`, never toward the plausible answer.** §5.6 states this
for capabilities; it holds field by field.

> Why `inferred` is load-bearing rather than decorative on a real substrate,
> and how these two rules were earned: Appendix A, F6.

### 2.4 DeliveryReceipt

```
DeliveryReceipt {
  outcome : "submitted" | "queued" | "refused" | "unknown"
  reason? : string
}
```

Input delivery is **not** fire-and-forget, and not a boolean.

- `submitted` — the driver confirmed the agent received it
- `queued` — accepted by the driver, submission unconfirmed
- `refused` — the driver declined; `reason` explains why
- `unknown` — sent, outcome unverifiable

`refused` is the important one. A driver is expected to protect a session from
input that would corrupt it — for example, injecting text into a prompt that
already holds unsent input the human typed. That protection belongs in the
contract, not in each caller's memory of a past incident.

### 2.5 Ack

```
Ack {
  accepted : boolean
}
```

`interrupt` and `close` express intent only (§3; api-http.md §3.3's 202
Accepted). Confirmation of what actually happened arrives later as a state
change on the event stream (§4), never as this call's return value. An `Ack`
carrying a status of its own would be a driver promising synchronous
completion it may not be able to deliver, which §5.6 forbids.

> Origin: Appendix A, F2.

---

## 3. Operations

```
create(spec)                  -> SessionRef
send(ref, text, opts?)        -> DeliveryReceipt
state(ref)                    -> SessionState
interrupt(ref)                -> Ack
close(ref)                    -> Ack
list(filter?)                 -> Collection<Session>
subscribe(filter?)            -> EventStream
```

`subscribe` is not optional garnish. Federated callers must be able to learn
about state changes without polling — see §5.5.

> The table above once read `list(filter?) -> SessionRef[]`. Why a bare array
> cannot satisfy §9's envelope rule or §13.2's adopt-don't-resynthesize rule:
> Appendix A, F3.

---

## 4. Drivers

A driver implements the operations above for one runtime on one machine.

### 4.1 Local drivers

Wrap whatever actually runs an agent: a terminal multiplexer session, a managed
subprocess, a runtime with its own HTTP server.

### 4.2 The remote driver

A driver whose implementation is an HTTP client to a peer `colab-fleet`.

This is the entire federation design. Cross-machine operation is not a feature
layered on top of the abstraction — it is one implementation of it. If the
interface cannot express "a session on another machine," the interface is
wrong, and that is the cheapest available test of it.

### 4.3 Capability declaration

Drivers differ in what they can do. A driver declares its capabilities, and
callers must degrade rather than assume:

```
DriverCapabilities {
  observesState   : boolean   // can report status without inference
  confirmsDelivery: boolean   // can distinguish submitted from queued
  supportsResume  : boolean   // sessions survive a service restart
  supportsPin     : { model: boolean, effort: boolean, agent: boolean }
  deadlineMs      : number    // declared upper bound on any single call
}
```

A driver must never silently emulate a capability it lacks.

> **Open defect D3.** This declaration is synchronous and infallible, which a
> remote driver cannot be — its answer lives on the peer, behind a network.
> And `DriverCapabilities` has no way to say "the peer has not answered yet":
> that value is currently indistinguishable from "supports nothing". See §14.

### 4.4 Every driver declares a deadline

**`deadlineMs` is mandatory. A driver that can block without a bound is a
specification violation, not a slow driver.**

This was found empirically rather than reasoned about, and it is the sharpest
lesson the first two drivers taught: the interface is symmetric, but **the
hazard profile underneath it is not.**

A local driver talks to a subprocess. If that hangs it is rare, local, and
diagnosable. A remote driver talks to a machine that may be powered off,
firewalled, or — worst of all — *stopped mid-syscall*, which is neither alive
nor dead. Measured directly: a caller with no deadline, querying a peer that had
been SIGSTOPped, was **still blocked with no result after seven seconds** and
would have waited indefinitely.

Note carefully what did *not* save us there: no mainstream language's HTTP
client defaults to a finite timeout. The protection cannot come from the
runtime, and therefore has to come from the contract.

Earlier drafts described `unreachable` as an **outcome** while saying nothing
about how a caller ever *reaches* that outcome. An outcome nobody is obliged to
produce is not a guarantee — it is a hope. Hence:

- Every driver declares `deadlineMs` and honours it.
- Exceeding it produces `unreachable` with the elapsed time as evidence.
- A caller may supply a shorter deadline; never a longer one.

---

## 5. Design rules

These are stated as rules because each one was learned by violating it.

### 5.1 Express questions, not mechanisms

The interface says `state()`, never `readScreen()`; `send()`, never
`typeKeys()`. Mechanism-shaped interfaces bind every future driver to the first
driver's substrate.

### 5.2 Uncertainty travels

If a driver cannot determine something, that must survive all the way to the
caller. An interface that forces a boolean where the truth is a guess produces
confident wrong answers, which are worse than admitted ignorance.

### 5.3 Context by reference, never by argv

Session context is passed as a path (`contextRef`), never inlined into a
command line.

Rationale, generally applicable: process command lines are a shared namespace.
Anything that matches processes by name — a cleanup command, a monitoring
script, an agent tidying up after itself — can match a session whose argv
merely *contains* the string it was hunting for, and terminate it. Passing
context by file removes the session from that namespace entirely.

### 5.4 Ids are recyclable; require consensus before destruction

Before any destructive operation, a driver must confirm that the session at an
id is the session the caller meant — by corroborating at least one independent
attribute (working directory, start time, name). Matching an id alone is not
identification.

> **Open defect D2 — this rule is not enforceable at the signature §3 gives
> `close`.** A `SessionRef` carries no attribute the caller observed, so a
> driver has nothing to corroborate the live session *against*. A conforming
> driver closes the window between its own last sighting and the destroy, and
> refuses an id it has never observed; nothing at this signature closes the
> window between the *caller's* sighting and the destroy, which is the long
> one. See §14.

### 5.5 State and events, never polling

Federated callers may be many network round-trips away. An API that requires
polling to stay current becomes unusable at exactly the distance federation is
for. State is readable on demand; changes are pushed.

> **Open defect D4.** §3 writes `subscribe(filter?)` without saying what a
> filter can express. On a substrate that charges one connection per watched
> session, that decides whether a subscription costs O(subscribers) or
> O(sessions) — so filter granularity is a cost parameter, not a convenience.
> See §14.

### 5.6 Degrade, never emulate

A driver that cannot observe state reports `inferred` or `unknown`. It does not
manufacture a plausible `observed`. Emulation makes capability differences
invisible at precisely the layer built to expose them.

### 5.7 Absence and failure are different answers

**A failed read must never render as an empty result.**

"There are no sessions on that machine" and "I could not reach that machine to
ask" are different facts with opposite implications, and they are trivially
easy to collapse into the same empty list — at which point every caller
downstream draws a confident conclusion from a failure.

This is the general form of the `unknown` status in §2.3, and it governs every
plural response in this API. It is why §9 forbids returning a bare array from
any operation that spans machines: there is nowhere in a bare array to say
*"and one source didn't answer."*

**This rule applies inside a driver, not only across machines.** The wording
above is about sources and envelopes, but the same collapse is available
between a driver and a single session, and it manufactures the same confident
wrong answer. A driver must distinguish **"I read this and could not tell"**
from **"I failed to read this."** Both may surface as `unknown`, but they must
not carry the same `evidence` — that field is the only place the difference can
survive (§2.3).

The general form, worth stating once: *a component that cannot report its own
failure to observe will report its ignorance as the world's.*

> How this was found — a driver that could read nothing returned a complete,
> error-free view of 22 sessions and passed its entire test suite:
> Appendix A, F5.

---

## 6. Authorization

Where a session runs is no longer a physical constraint, so it must become an
explicit one.

A single-machine fleet has an accidental safety property: a supervisor can only
destroy sessions on its own host, because it has no way to reach any other.
Federation removes that property. It has to be rebuilt deliberately rather than
mourned.

**Requirements:**

1. **Bind narrowly.** Default to loopback. Exposure beyond it is explicit
   configuration, never a side effect of enabling federation.
2. **Authenticate peers.** No unauthenticated network surface, ever. A service
   that can start processes and read files is a remote-execution surface
   regardless of intent.
3. **Separate read from destroy.** Peer authorization is per-verb. `list` and
   `state` from a peer may be permitted by default; `close`, `interrupt` and
   `create` are opt-in per peer.
4. **Log every remote-originated mutation** — actor, verb, target, outcome.
   This is the audit trail that replaces "it could only ever have been me."

> **Open defect D1 — the most serious in this document, and the only one whose
> failure mode is a security bug rather than a wrong answer.** Requirement 3
> above, and §13's "proxying does not launder authorization", cannot be
> enforced by the interface: no operation in §3 takes a principal, so the
> original caller's authority has nowhere to travel. See §14.

---

## 7. Resolved decisions

### 7.1 Addressing is `(machine, id)`

No fleet-wide identifier. A session is addressed by the machine that runs it
plus an id scoped to that machine. `name` is a human label — unique per machine
by convention, never an identifier, never used for routing.

*Rationale:* a fleet-wide id would need an allocator, and an allocator is a
single point of failure for the one operation that must keep working when a
peer is unreachable. `(machine, id)` needs no coordination at all.

### 7.2 Peers are statically configured

No announcement, no discovery protocol, no broadcast.

*Rationale:* automatic discovery means a machine can join the fleet without
anyone deciding it should — an anti-feature for something that starts
processes. For a fleet of a handful of machines, the configuration cost is
negligible and the audit trail is worth more than the convenience.

**Peer addresses are operator-verified, never inherited from a machine's own
idea of its name.** A hostname can resolve to different addresses depending on
who is asking — split-horizon DNS, overlay networks, and multi-homed hosts all
produce this, and it was observed on the first two machines this ran on. A peer
entry that stores a bare hostname and trusts the peer's self-resolution will
misconfigure silently, and present as an unreachable peer that pings fine.
Configuration stores an address the *operator* has confirmed reachable **from
the machine that will be doing the reaching**.

### 7.3 Events carry a cursor and an epoch

Each service instance assigns events a monotonic cursor and stamps them with an
**epoch** identifying the instance. A subscriber reconnects with its last
cursor. If the cursor is older than the retained buffer, or the epoch has
changed (the service restarted), the service returns `resync_required` and the
subscriber refetches state.

*Rationale:* the alternative — silently resuming from the oldest available
event — produces a subscriber that believes it has a complete history and does
not. Announced gaps are recoverable; silent gaps are not.

Two rules follow, both normative:

**Cursor and epoch are assigned by the service, never by a driver.** A driver
has access to neither, and two drivers under one service must not mint
competing sequences. A driver leaves them unset; the service stamps them on the
way out.

**The baseline snapshot is taken before `subscribe` returns.** If a
subscription takes its first reading asynchronously, everything occurring
between the call returning and that reading is folded into the baseline and
never reported — a gap that cannot even be announced, because no cursor covers
it and no epoch changed. Taking it synchronously makes the guarantee stateable:
every change after `subscribe` returns is either delivered or is a bug.

> Both were found the hard way: Appendix A, F8.

### 7.4 Plural responses are envelopes, never bare arrays

Any operation that spans machines returns per-source status alongside the data,
plus an explicit completeness flag. See §9.

### 7.5 Restart adopts; it never silently drops

On restart the service re-discovers sessions its drivers can still see and
adopts them. Anything it finds but cannot confidently identify is surfaced as
`unknown`, never dropped from listings and never destroyed. See §12.

## 7a. Still open

- **Input ordering under concurrency.** If two callers `send()` to one session
  simultaneously, is ordering defined, or is that the caller's problem?
- **Backpressure.** What happens when a subscriber cannot keep up with the
  event stream — drop, buffer, or disconnect with `resync_required`?
- **Whether `create` should be able to target "any machine"** under a policy,
  rather than requiring the caller to name one. Deferred: it needs a scheduler,
  and a scheduler is a supervisor concern (§1 non-goals).

---

## 8. Lifecycle

Legal transitions. Anything not listed is a driver bug.

```
        ┌──────────┐
        │ starting │
        └────┬─────┘
             ▼
  ┌──────► working ◄────────┐
  │          │  ▲           │
  │          ▼  │           │
  │    waiting_input        │
  │          │              │
  │          ▼              │
  └──────── idle ───────────┘
             │
             ▼
           dead
```

- `starting` → `working` | `idle` | `dead`
- `working` → `waiting_input` | `idle` | `quota_blocked` | `dead`
- `waiting_input` → `working` | `idle` | `dead`
- `idle` → `working` | `dead`
- `quota_blocked` → `working` | `idle` | `dead`
- `dead` → terminal. A `dead` session never becomes live again; a resumed
  session is a **new** session with a new id.

`unknown` is **outside** this machine. It may be entered from any state and
exited to any state, because it does not describe the session — it describes
the driver's knowledge of it. A caller must not infer that a transition
occurred merely because the state changed to or from `unknown`.

**`since` is the time the status was first observed to hold**, not the time it
began. For `inferred` states those differ, sometimes by a lot. A caller
computing "how long has this been stuck" is computing a lower bound, and should
present it as one.

---

## 9. Plural responses

Every operation that spans more than one machine returns:

```
Collection<T> {
  items    : T[]
  sources  : SourceStatus[]
  complete : boolean       // false if any source failed to answer
}

SourceStatus {
  machine   : MachineId
  status    : "ok" | "unreachable" | "unauthorized" | "degraded"
  count?    : number
  error?    : string
  observedAt: Timestamp
}
```

`complete` is redundant with `sources` and exists anyway, deliberately: the
common bug is a caller that never looks at `sources`. A single boolean at the
top level is hard to not notice, and a caller that ignores it has made a choice
rather than an oversight.

**Never** return `items: []` for a source that failed. An unreachable machine
contributes a `SourceStatus`, not an absence.

**`complete` is derived, never supplied.** It is true iff every
`SourceStatus.status` is `ok`. A caller-supplied boolean is exactly the kind of
value that drifts from what `sources` actually says — the same class of bug
this field exists to catch, one level up. `degraded` flips `complete` to false
on the same footing as `unreachable` and `unauthorized`: a degraded source's
data is present but not to be trusted at face value (§13.2), and treating it as
"answered cleanly" would reintroduce the confidence-flattening §5.6 forbids.

> Origin: Appendix A, F4.

---

## 10. Idempotency

`create` accepts a caller-supplied **idempotency key**. A driver that receives
a repeat key within the retention window returns the *existing* `SessionRef`
instead of creating a second session.

This is not a nicety. The failure it prevents is specific and expensive:

> A federated `create` times out in transit. The caller cannot distinguish "the
> session was never created" from "the session was created and the reply was
> lost", so it retries. Two agent sessions are now running in the same working
> directory, both writing to the same files, neither aware of the other.

Retention must outlive the caller's retry window. Keys are scoped per machine.

**Retention that does not outlive the *service* does not satisfy this rule**,
because a service restart falls inside the caller's retry window — and is one
very good reason a reply went missing in the first place. Either persist keys,
or declare that they are not persisted.

§4.3's `supportsResume` does **not** answer this question. It asks whether
*sessions* survive a restart, which is a different fact: a driver whose
sessions are owned by an external process can honestly declare
`supportsResume: true` while its idempotency keys live in memory and do not
survive at all.

> Known non-compliance in the current implementation, and how the two were
> conflated: §14 D5, Appendix A, F7.

---

## 11. Time

Every machine stamps timestamps in **its own clock**, and every response
carries that machine's current clock reading.

Callers compute skew rather than assuming synchronisation. No attempt is made
to establish a fleet-wide ordering of events across machines — there is no
requirement for one, and providing a fake one would be worse than providing
none.

Durations reported by a machine (`since`, silence timers) are computed
locally and are therefore internally consistent even when clocks disagree.
Comparing durations across machines is safe; comparing timestamps is not.

---

## 12. Adoption and restart

Sessions may outlive the service that manages them — a session inside a
terminal multiplexer survives the supervisor being restarted, upgraded, or
killed. The service must therefore treat startup as **reconciliation**, not
initialisation.

At startup, per driver:

1. Enumerate what actually exists on the machine.
2. Match against persisted records.
3. Classify each into one of:
   - **adopted** — matched with confidence; resumes normal management
   - **orphaned** — exists but no record; surfaced with `confidence: inferred`
     and whatever identifying evidence the driver has
   - **vanished** — record exists but nothing found; marked `dead` with
     evidence noting it disappeared while unobserved
4. Never destroy anything during reconciliation. A session the service cannot
   explain is a session for a human to look at, not one to clean up.

Rule 4 is absolute. Automated destruction during a phase whose entire premise
is *incomplete knowledge* is how a fleet eats its own work.

---

## 13. Federation topology

A client talks to **one** service — normally the one on its own machine — and
that service **proxies** to peers on the client's behalf.

The rejected alternative is redirection, where the service tells the client
which peer to ask and the client asks directly. Redirection is leaner, but it
pushes topology, authentication and reachability into every client, which
defeats the purpose: a supervisor should be able to ask about the fleet without
knowing its shape.

**Costs, accepted explicitly:**

- The local service becomes a dependency for remote operations. It is already a
  dependency for local ones.
- Latency is additive. Acceptable for control-plane calls; this is another
  reason §5.5 forbids polling.
- Events from peers are multiplexed through the local service's stream, and
  carry their originating machine.

**Proxying does not launder authorization.** A service forwarding a peer's
request presents the *original* caller's authority, never its own. Otherwise
every machine becomes a confused deputy for every other.

### 13.1 A proxied request asks for the peer's LOCAL view only

**Fan-out is one hop deep, always. Peers do not recurse.**

The service a client asks is responsible for querying every peer it knows. Each
peer answers for **itself alone** and never forwards further.

Without this rule, two mutually-configured peers each answer by asking the
other, and the result is an infinite loop or — more insidiously — a fleet that
merely double-counts and looks fine. The first spike avoided this only by being
a star, and by the implementer noticing they were avoiding it. A topology whose
correctness depends on nobody adding a second edge is not a design.

**Consequence, stated because it is a real limit rather than an oversight:**
with a partially-connected peer graph, different entry points yield different
views of the fleet. A fleet that wants every node to see everything must be
fully meshed in configuration. This is acceptable at the scale this system
targets, and it is preferable to recursion — recursion buys transitive
visibility at the cost of cycle detection, hop limits, and a distributed
join that no operator can reason about at three in the morning.

### 13.2 Adopt a peer's SourceStatus; never re-synthesize it

A peer answering locally returns an envelope containing exactly one
`SourceStatus` — its own. The proxying service **adopts that record** into its
own envelope.

It must not discard the peer's status and manufacture a fresh `"ok"` from the
mere fact that the call succeeded. A peer can answer promptly *and* report
itself `degraded`; flattening that into "ok, count N" produces a confident
envelope built on a self-declared unreliable source — §5.7's failure, one layer
in.

The reachability of a peer and the health of a peer are different facts. The
proxy observes the first and must **relay**, not overwrite, the second.

---

## 14. Open defects in this specification

Places where this document requires something it cannot enforce, or describes
something the interface has no room for. Every one was found by an
implementation, and none is fixed. They are listed here rather than left as
marginal notes because a reader who takes the sections above at face value will
believe guarantees that do not hold.

Each entry states the rule, why it cannot be satisfied, what that costs, and
the proposed fix. **A proposed fix is not a decision.** Each changes a shape
that two implementations and an HTTP surface already depend on, which is
exactly why they are written down instead of applied in passing.

### D1 — Caller authority has nowhere to travel · §6, §13

**Severity: this is the only defect here whose failure mode is a security bug
rather than a wrong answer.**

§6 requirement 3 and §13 both require a proxying service to present the
**original caller's** authority to a peer, never its own. Every operation in §3
takes a context and its domain arguments. None takes a principal.

So the caller's identity can only travel out of band — untyped, invisible at
the call site, impossible to require, silently absent when a service forgets to
attach it.

**The fallback is the vulnerability.** A remote driver missing the caller's
credentials will reach for the one credential it certainly has: its own
transport token. That works. The request succeeds. Tests pass. Authorization is
silently widened to whatever the proxy is allowed to do, and nothing anywhere
reports it — *the symptom of this bug is that everything works.*

Note the asymmetry with every other entry here: the others fail toward a wrong
answer somebody eventually notices. This one fails toward a correct-looking
answer.

- **Interim mitigation, implemented:** a remote driver refuses any mutating
  verb that arrives without caller authority, rather than substituting its own.
  This is a driver declining to do something the interface cannot stop it from
  doing. It is not a fix, and a driver written by somebody else will not do it.
- **Proposed fix:** caller authority becomes a parameter of the §3 operations,
  so a driver cannot compile without deciding what to do with it.
- **Cost of the fix:** every signature in §3, both drivers, the service, and
  api-http.md §5.

### D2 — `close` cannot corroborate · §5.4

§5.4 requires corroborating an independent attribute before any destructive
operation, because ids are recyclable. Corroboration needs two operands: what
the session looks like now, and what the caller believed it looked like. A
`SessionRef` carries machine, id and a human label. The second operand never
arrives.

What a conforming driver can do, and what both current drivers do:

- refuse outright on an id it has never observed, rather than destroying on an
  id match — the literal act §5.4 forbids;
- refuse when its own last sighting disagrees with the live session.

What no driver can do at this signature: close the window between the
**caller's** sighting and the destroy. That is the long window, and the one a
human is standing inside. It is worse across a network, where a round trip
separates the two rather than a function call — and a remote driver has no
sightings of its own to fall back on at all.

- **Proposed fix:** `SessionRef` (§2.2) gains an optional caller-observed start
  time, so `close` compares against the caller's belief instead of its own.
  Callers that omit it get today's weaker guarantee explicitly, rather than a
  strong-sounding rule that quietly degrades.
- **Cost of the fix:** a wire type, api-http.md §3.3, both drivers.

### D3 — Capability declaration cannot say "unknown" · §4.3

`Capabilities()` is synchronous and infallible. For a driver describing itself
that is correct. For a driver describing a **peer**, the answer lives across a
network that may be down, and there is no honest synchronous value for "nobody
has told me yet."

The type cannot express it either. All-false already means "supports nothing";
an unreached peer produces exactly that. A caller consulting `/v1/runtimes` —
which api-http.md §3.1 says it MUST do before relying on a capability — cannot
distinguish a deliberately minimal peer from an unreachable one.

Both readings degrade identically, so the cost is **diagnostic rather than
correctness**: a permanently misconfigured peer looks like a minimal one,
forever, and nothing prompts anyone to look.

- **Proposed fix:** either `deadlineMs` becomes the only field a remote driver
  answers for itself — it describes the transport, not the peer — and the rest
  gain a third state; or capability declaration becomes fallible and
  context-taking like every other cross-machine question.

### D4 — `subscribe`'s filter cannot name a session · §5.5

§3 writes `subscribe(filter?)` and never says what a filter expresses. That
looked like a detail to settle later. It is not, because a substrate may charge
per subscribed session: measured on the first driver's substrate, content
notifications require one connection per watched session while lifecycle
notifications are fleet-wide and free.

A filter that can name sessions costs O(subscribers). One that can only
describe them by attribute costs O(sessions). The only mechanism currently
available is a working-directory prefix, which makes a caller wanting one
session ask for a directory and hope — a proxy for identity rather than
identity, which is D2's lesson in a different operation.

- **Proposed fix:** the filter can name session ids.
- **Cost of the fix:** the wire shape of `subscribe`, api-http.md §4.

### D5 — Idempotency retention does not outlive the service · §10

Unlike D1–D4 this is a **known non-compliance of the current implementation**,
not a gap in the interface. §10 as clarified is satisfiable; nothing satisfies
it yet.

The local driver's idempotency keys live in process memory. A caller retrying a
`create` across a service restart therefore receives §10's exact disaster — two
agents in one working directory — from a driver that correctly declares
`supportsResume: true`, because that flag answers a question about *sessions*.

- **Proposed fix:** persist keys, or declare that they are not persisted so a
  caller can compensate.

---

## Appendix A. Findings log

How the rules above were learned. This appendix exists because the reasoning is
worth more than the conclusions: a reader who knows only the rule will restate
it, while a reader who knows how it was violated will recognise the next
instance.

Kept deliberately, per this repository's standing preference — *knowing a
design was wrong once, and how it was found out, is worth more than a clean
document.*

### Phase 1 — transcribing the spec into types

Nothing ran yet. These are places the prose admitted more than one reading, or
where the document's own pseudocode did not survive being made to compile.

**F1 · A session id is scoped to `(machine, runtime)`, and the URL had no room
for the runtime.** Two runtimes on one machine may legally reuse an id;
`/machines/{machine}/sessions/{id}` cannot disambiguate. api-http.md gained an
optional `?runtime=` parameter, on that document's own rule that where the two
disagree, the abstraction wins and the wire document is the bug.

**F2 · `Ack` was named but never shaped.** §3's table returned it from
`interrupt` and `close`; unlike `DeliveryReceipt`, it was never defined. Shaped
to carry acceptance only — anything more would be a driver promising
synchronous completion it may not be able to deliver.

**F3 · `list` could not return a bare array.** §3 wrote `-> SessionRef[]`. Two
independent rules forbid it: §9 requires every plural response to be an
envelope with `sources`, and §13.2 requires a proxy to adopt a peer's own
`SourceStatus` — for which a slice has nowhere to put it. The item type became
`Session` rather than `SessionRef` for a third reason, confirmed later by
measurement (F10): a batch operation whose natural shape is cheap must not
force per-item follow-up calls.

**F4 · Nobody owned `complete`.** §9 said it was "false if any source failed to
answer" without saying who computes it, or whether `degraded` counts. Settled
as derived-never-supplied, with `degraded` flipping it false.

### Phase 2 — the first working driver

A local driver over a terminal multiplexer, developed against a machine running
22 concurrent live sessions.

**F5 · A driver that could read nothing reported a healthy fleet.** The batched
screen-capture markers were built from pane identifiers, and the command
emitting them passes its argument through `strftime` — which consumes `%`, the
character every pane identifier begins with. Every capture was misfiled, so
every session was classified from an empty string.

The driver then returned a complete, well-formed, **error-free** view of 22
sessions, all `unknown`, and passed its entire unit suite. Nothing anywhere
said *"the driver failed to read."* It said *"the sessions are unknowable"* — a
claim about the fleet rather than about itself, and false.

This is §5.7 operating one level below where §5.7 states it, and it is why that
section now governs the inside of a driver too.

**F6 · The `working`/`idle` distinction rests on the tense of a randomly chosen
verb.** The runtime's interface signals a turn in progress with a spinner whose
verb is drawn at random per turn, distinguishing running from finished by that
verb's grammatical tense and suffix shape:

```
✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)   <- running
✻ Worked for 2m 7s                         <- finished
```

A driver keying on that is keying on the tense of a random English word in an
interface with no compatibility contract. It works today; it is one release
note away from being wrong, and wrong *silently*, because a missing spinner
reads exactly like a finished turn.

`confidence: inferred` is therefore not modesty on this substrate — it is the
literal truth, and §2.3's `unknown` earns its place as a first-class answer.

**F7 · `supportsResume: true` was honest while idempotency keys evaporated.**
Sessions here are owned by the multiplexer, so they genuinely survive a service
restart. Keys lived in memory. The capability flag was being read as covering
both. See D5.

**F8 · Two silent gaps in the event stream.** First: `Event` carries `cursor`
and `epoch`, which §7.3 assigns per service instance — a driver has neither,
and one that helpfully invented a cursor would produce a stream that looks
correct until a subscriber reconnects and the resync comparison misses the gap
it exists to catch. Second: taking the baseline snapshot inside the engine
goroutine let everything between `subscribe` returning and that snapshot be
absorbed into the baseline, unreported and *unannounceable*. Found by writing
the race and then watching a test absorb the very change it was asserting on.

**F9 · Push exists, but is scoped per attachment.** Measured on the substrate
rather than assumed:

| notification | delivered to a client attached elsewhere? |
|---|---|
| content (`%output`) for the attached session | yes |
| content for a sibling session | **no** |
| format subscription, per-pane | attached only |
| format subscription targeting a sibling's pane by id | **no** |
| session appearing / disappearing | **yes — fleet-wide** |

Content is per-attachment; lifecycle is global. One always-on client therefore
covers every session appearing and disappearing, while watching a session's
content costs a connection. That asymmetry is what makes filter granularity a
cost parameter (D4), and it is why notifications are used as change *triggers*
feeding the ordinary batched read rather than as a second interpretation of
screen bytes — two sources of truth about status would disagree only under
load.

**F10 · Enumeration cost is structural, not incremental.** On 22 live sessions:

| approach | subprocess spawns | wall clock |
|---|---|---|
| per-session capture loop | N+1 (23) | 119 ms |
| one batched invocation | 1 | 18 ms |

Constant in session count rather than linear — about 5 ms per session becomes
about 0.15 ms. This is why `list` returns everything in one call, and why a
driver that implements `list` by looping `state` has reproduced the cost the
interface exists to avoid.

### Phase 3 — the second driver

An HTTP client to a peer service, satisfying the same interface. It found a
different *class* of problem: where the first driver exposed places the model
was imprecise, this one exposed places the interface has **no room for a
concept it requires**.

**F11 · The confused-deputy fallback.** See D1. Worth restating once here
because of how it presents: the bug's symptom is that everything works.

**F12 · A remote driver cannot answer a synchronous question about a peer.**
See D3.

**F13 · What did survive, and it is the point of the exercise.** §4.2's claim —
that a remote peer is just another driver — held end to end, with no special
case in either service: a caller asked a service holding no local drivers,
which proxied through the remote driver, over HTTP, into a second service, into
the multiplexer driver, and back with 22 real sessions. `confidence: inferred`
survived the round trip rather than being flattened to `observed`, which is the
single easiest thing for a federation layer to destroy and the one §5.6 exists
to protect.

### The pattern worth naming

§5.7 — *absence and failure are different answers* — has now been discovered
independently at four different altitudes:

1. **A status field.** A driver that cannot determine state must say `unknown`,
   not guess (§2.3).
2. **A plural response.** A machine that did not answer contributes a
   `SourceStatus`, not an absence from `items` (§9).
3. **Inside a driver.** A pane that could not be read is not a pane that was
   read and found empty (F5).
4. **A capability declaration.** A peer that has not answered is not a peer
   that supports nothing (D3).

Four instances, found separately, each initially looking like a local detail.
The generalisation is worth stating as a design rule for anything added later:

> **Every field in this API that can be absent needs to distinguish
> absent-because-no from absent-because-unknown — and if it cannot, it will
> eventually report someone's ignorance as a fact about the world.**
