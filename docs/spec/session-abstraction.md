# Session abstraction — specification

**Status:** draft. One working driver now exists (a terminal multiplexer
running an interactive agent CLI), so the claims below have been tested once
against a real substrate — 22 concurrent live sessions — and four of them did
not survive intact. Those are marked **Amendment (first working driver)** in
place: §5.4 is not implementable at the signature §3 gives it, §5.7 turns out
to govern the inside of a driver as well as the space between machines, §10's
retention rule is silently defeated by §4.3's `supportsResume` being read as
covering it, and §2.3's `inferred`/`unknown` machinery is load-bearing in ways
§2.3 undersells.

Nothing here has been proven by a **second** driver yet, which is the test
that matters for a federation design: until a remote peer satisfies this same
interface, every claim about machine-to-machine operation remains a proposal.

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

**Amendment (first working driver): `inferred` is doing more work than the
prose above suggests, and the first driver is the proof.**

The three rules read as prudent hedging until you build a driver that has to
obey them. This one infers a session's state by reading its terminal, and the
signal it must read to distinguish *working* from *idle* is the grammatical
tense of a **randomly chosen English verb**:

```
✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)   <- running
✻ Worked for 2m 7s                         <- finished
```

The verb is drawn at random from a large set per turn. The distinction is
carried entirely by the suffix's shape, in a terminal UI with no compatibility
contract, and it will break without warning and without erroring.

Three consequences, all of which the existing model already handles — which is
the actual finding:

1. `confidence: inferred` is not a formality on this substrate. It is the
   literal truth, and a caller that treats inferred and observed alike is
   trusting a coin-flip verb.
2. `unknown` must be reachable from *unrecognised evidence*, not only from
   absent evidence. When the spinner appears in a shape the driver does not
   know, the honest answer is `unknown` — not `idle`, which is what "no
   running spinner found" naively decays to. A wrong `idle` for a working
   session is silent; an `unknown` is not.
3. A driver must fail toward `unknown`, never toward the plausible answer.
   §5.6 says this about capabilities; it is equally true field by field.

Recorded because the value of §2.3 is easy to underestimate from the prose.
Its cost is one extra enum member and a nullable confidence; its benefit is
that a substrate this unreliable can be represented **honestly** rather than
being flattened into confident fiction at the interface boundary.

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

**Amendment (first Go transcription):** this type was named in §3's
operations table (`interrupt(ref) -> Ack`, `close(ref) -> Ack`) but never
given a shape, unlike `DeliveryReceipt` above. This is the shape settled on:
`interrupt` and `close` express intent only (§3; the HTTP wire's 202
Accepted, api-http.md §3.3). Confirmation of what actually happened arrives
later as a state change on the event stream (§4) — an `Ack` that promised
more would be a driver promising synchronous completion it may not be able
to deliver, which §5.6 forbids.

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

**Amendment (first Go transcription):** `list` was originally written here
as `-> SessionRef[]`, a bare array of refs. That pseudocode does not survive
being made to compile, for two reasons already stated elsewhere in this
document: §9 requires every plural response to be a `Collection` envelope
with `sources`, never a bare array — and this applies even to one machine
answering for itself alone (api-http.md §3.2: a `scope=local` response
still "carries exactly one `SourceStatus`"); and §13.2 requires a service
proxying a peer's answer to *adopt* that peer's own self-reported
`SourceStatus` rather than manufacture a fresh `"ok"`, which a driver
returning a bare slice has no way to do — there is nowhere in a slice to
carry a `SourceStatus` at all. The item type is `Session`, not
`SessionRef`, for the same reason §4.4's cost measurement matters here: a
batch operation whose natural shape is cheap must not force per-item
follow-up calls for state.

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

**Amendment (first working driver). This rule is not implementable at the
signature §3 gives `close`, and that is a defect in the interface, not in any
driver.**

`close(ref) -> Ack` hands the driver a `SessionRef`: machine, id, and a human
label. To corroborate, a driver needs two things — what the session looks like
now, and what the caller *believed* it looked like. It can read the first. The
second never reaches it. So "corroborate against an independent attribute"
has no second operand.

What a driver can do at this signature, and what the first one does:

- Keep its own record of each session it has observed, and refuse to destroy
  an id whose start time or working directory has changed since. This closes
  the window between **the driver's** observation and the destroy.
- Refuse outright on an id it has never observed, rather than destroying on an
  id match — which is the literal act this section forbids.

What no driver can do at this signature: close the window between **the
caller's** observation and the destroy. A supervisor that lists sessions,
pauses, and then closes one gets no protection, because the driver's own
sighting may have been refreshed during the pause. That window is the one that
matters — it is the long one, and it is the one a human is inside of.

**Proposed fix, not yet applied:** `SessionRef` (§2.2) gains an optional
corroborating attribute — the start time the caller observed — so `close` can
compare against the caller's belief instead of its own. Callers that omit it
get today's weaker guarantee, explicitly, rather than a strong-sounding rule
that silently degrades. This touches the wire type and therefore
api-http.md §3.3; it is recorded here rather than applied, because the
interface change deserves a decision rather than a patch.

### 5.5 State and events, never polling

Federated callers may be many network round-trips away. An API that requires
polling to stay current becomes unusable at exactly the distance federation is
for. State is readable on demand; changes are pushed.

**Amendment (first working driver): the filter's granularity is a cost
parameter, not a convenience.**

§3 writes `subscribe(filter?)` and never says what a filter can express. That
looked like a detail to settle later. It is not, because a substrate may
charge per subscribed session.

Measured on the first driver's substrate: push notifications about a session's
*content* are delivered only to a client attached to that session, while
notifications about sessions *appearing and disappearing* are delivered
fleet-wide to any attached client. So lifecycle costs one connection total,
and content costs one connection per watched session. A subscription that can
be narrowed to the sessions a caller actually cares about costs
O(subscribers); one that cannot costs O(sessions).

⇒ A filter must be able to name **sessions**, not only describe them by
attribute. Describing them by working-directory prefix — the only mechanism
this interface currently offers — makes a caller that wants one session ask
for a directory and hope, which is a proxy for identity rather than identity
(§5.4's lesson, in a different operation).

Recorded rather than fixed, for the same reason as §5.4: it changes the wire
shape of `subscribe`, and that deserves a decision.

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

**Amendment (first working driver): this rule applies inside a driver, not
only across machines.**

Stated as above, §5.7 reads as a rule about federation — sources, machines,
envelopes. It is not. The same collapse is available between a driver and a
single session, and it is just as capable of manufacturing a confident wrong
answer.

Measured, not hypothesised. The first driver reads each session's state by
capturing its screen. A bug misfiled every capture, so every session was
classified from an empty string — and an empty screen and an unread screen
both produced `unknown`. The result: a driver that could not read a single
screen returned a complete, well-formed, error-free fleet view of 22 sessions,
and passed its whole unit suite. Nothing anywhere in the response said *"the
driver failed."* It said *"the sessions are unknowable,"* which is a claim
about the fleet rather than about the driver, and it is false.

⇒ A driver must distinguish **"I read this session and could not tell"** from
**"I failed to read this session."** Both may surface as `unknown` status, but
they must not carry the same `evidence`, because `evidence` is the only field
in which the difference can survive (§2.3).

The general form, worth stating once: *a component that cannot report its own
failure to observe will report its ignorance as the world's.*

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

**Amendment (first working driver), two parts.**

**A driver cannot fill these fields, and the type implies it should.** Cursor
and epoch are assigned per *service instance*, by this section's own wording.
A driver has access to neither, and two drivers under one service must not be
minting competing cursor sequences. So a driver must leave them unset and the
service must stamp them on the way out. This is easy to get wrong in the
direction that fails silently: a driver that helpfully invented a cursor would
produce a stream that looks correct until a subscriber reconnects, at which
point the resync logic compares values from different sequences and misses a
gap it was built to catch.

**"Silent gaps are not recoverable" has a second instance, at subscribe
time.** The rule above governs reconnection. The same failure is available at
the *start* of a subscription: if the initial state snapshot is taken
asynchronously, everything that happens between `subscribe` returning and that
snapshot is folded into the baseline and never reported. The subscriber holds
a stream it believes is complete, with a hole at the front. Nothing can
announce that gap, because nothing knows it occurred — no cursor covers it and
no epoch changed.

⇒ **The baseline snapshot must be taken before `subscribe` returns.** Then the
guarantee is stateable: every change after `subscribe` returns is either
delivered or is a bug. Found by writing the race and then watching a test
absorb the very change it was asserting on.

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

**Amendment (first Go transcription):** the prose above says `complete` is
"false if any source failed to answer" but does not say who computes it, or
whether a `degraded` source (answered, but reported itself unhealthy)
counts as a failure the same way `unreachable` (didn't answer at all) does.
Settled as follows: `complete` is **derived**, never independently
supplied — true iff every `SourceStatus.status` is `ok`. A caller-supplied
boolean is exactly the kind of value that can silently drift from what
`sources` actually says, which is the same class of bug this field exists
to catch, one level up. `degraded` also flips `complete` to `false`: a
degraded source's data is present but not to be trusted at face value
(§13.2), so treating it as "answered cleanly" would reintroduce the
confidence-flattening §5.6 forbids.

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

**Amendment (first working driver): retention and `supportsResume` are
different properties, and reading them as one produces the exact disaster
above on a driver that looks compliant.**

§4.3's `supportsResume` asks whether *sessions* survive a service restart. It
says nothing about whether *idempotency keys* do. The first driver makes the
gap concrete and is not unusual in doing so:

- Sessions survive, genuinely. The multiplexer owns them, not the service, so
  they outlive it being restarted, upgraded or killed. `supportsResume: true`
  is the honest declaration.
- Keys do not survive. They live in the service's memory.

So a caller retrying a `create` across a service restart — precisely the
partial-failure this section exists for, since a restart is one very good
reason a reply went missing — gets a **second session in the same working
directory**, from a driver that correctly declares `supportsResume: true`. The
capability declaration was read as covering both, and it covers one.

⇒ Retention that does not outlive the *service* does not satisfy "must
outlive the caller's retry window", because a service restart is inside that
window. Either persist keys, or declare that they are not persisted — a
driver must not leave a caller to infer key durability from a field about
session durability.

`send` is **not** idempotent and must not pretend to be — repeat delivery of
input is a legitimate caller intent. Callers needing exactly-once delivery must
read the `DeliveryReceipt` (§2.4) rather than retrying blindly.

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
