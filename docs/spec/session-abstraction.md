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

Things that did **not** survive contact are collected in **§14, Open defects**.
Read that section before relying on any guarantee in this document.

The one security-shaped defect among them — caller authority having nowhere to
travel, whose symptom was that everything appeared to work — **is now fixed**
(§2.6, §6). Fixing it also proved the cross-machine write path end to end.
Four remain open, and deployment added a fifth.

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

### 2.6 Request

```
Request {
  caller : Caller
  expect : Expectation
}

Caller {
  principal  : string       // who is asking — audit trail (§6)
  credential : string       // authority to present onward when proxying (§13)
}

Expectation {
  startedAt? : Timestamp    // the session start time the CALLER observed (§5.4)
}
```

The caller-side context of an operation: everything about *who is asking and
what they believe*, as opposed to what they are asking about. It is a
parameter of every operation in §3.

**Why one type rather than separate parameters.** Two defects in this document
turned out to be the same defect. §13 needed operations to carry the caller's
authority; §5.4 needed them to carry the caller's observation. Neither was
expressible, for the same reason — the operations took domain arguments only.
Both were then moved out of band, where one produced a silent security defect
and the other produced a rule nobody could enforce.

So caller-side context is one parameter with room to grow. The next thing a
caller must tell an operation is a field here, not another break of every
signature in §3.

`credential` is what a remote driver presents to a peer. **A driver that finds
it empty refuses**, and never substitutes its own — the strongest form of that
rule, which the first remote driver adopts, is to hold no credential at all.

`expect` is optional, and its absence is meaningful. A caller that supplies
`startedAt` gets §5.4's real guarantee: *destroy the session I looked at.* A
caller that omits it gets whatever weaker check a driver can offer from its own
sightings — and must be **told which it got** when the operation refuses, so a
weak check is never mistaken for a strong one.

**What does not belong here:** deadlines, which the context already carries and
§4.4 already governs — a second field would be a second source of truth about
the same fact. And anything about the *target*, which stays an argument.

### 2.7 SessionPrompt

```
SessionPrompt {
  question?: string
  options  : string[]      // in order, 1-based when referenced
  selected?: number        // the highlighted option
  nonce    : string        // changes when the prompt changes
}
```

The question a session is blocked on, carried **on `SessionState`** so every
path that reports state reports the question too — a single read, a listing,
and an event. A subscriber learns that a session blocked and what it is asking
in one message rather than having to turn around and ask.

**Options are enumerated, not described.** Evidence prose naming the
highlighted option explains a state; it does not let a caller act on one. Two
boot prompts observed on one fleet:

```
❯ 1. Yes, I trust this folder        ❯ 1. No, exit
  2. No, continue without these        2. Yes, I accept
```

Same shape, same footer, and the safe answer is at a different index. A caller
accepting the highlighted default proceeds in one case and kills the session in
the other. Enumerating is what makes an answer a choice rather than a guess.

**Options are recognised by their numbering, not their wording**, so a prompt
from a future release is enumerated without a new matcher — otherwise every new
screen needs new code, which is how this class of stall stays permanently one
release behind.

**`nonce` exists because an option index is not an option.** A caller reads a
prompt, shows it to a human, and answers seconds or minutes later; in between,
the session may be showing a different question in the same place. Answering by
index would answer that one instead. Supplying the nonce turns a stale answer
into a refusal — §5.4's "a proxy for identity is not identity", in a third
operation.

**Parsing is bounded.** The screen is written by an agent that can print
anything, so an index parsed from it is untrusted input. See Appendix A, F35.

### 2.7a Response

```
Response {
  choice? : number     // 1-based option; absent means the highlighted default
  cancel? : boolean    // dismiss rather than answer
}
```

What `respond` (§3) delivers. Absent `choice` means "accept whatever is
highlighted", which is what a human pressing Enter gets and what a caller
usually means. `cancel` exists because a caller that likes none of the options
needs a way to say so other than picking one anyway.

### 2.8 AttachHint

```
AttachHint {
  kind      : string      // "multiplexer"; unknown kinds are unsupported, not guessed
  target?   : string      // the substrate's own handle for this session
  command?  : string[]    // argv, run ON THIS SESSION'S MACHINE, to take over
  readOnly? : string[]    // the same attachment, without a keyboard
  shared?   : boolean     // attaching does not evict another viewer
}
```

Optional on `Session`. Absent means the driver has no answer, which is a real
answer (§5.7) and the correct one for a substrate with no interactive
attachment.

**Why this is in the model at all.** Every other thing a supervisor does to a
session — read it, drive it, answer it, end it — is expressible without knowing
what the substrate is. One was not: giving a *human* a terminal. A supervisor
that still shells out to a multiplexer for that has not been freed of the
substrate, it has been freed of it everywhere except where its users touch.
This is the difference between a driver boundary and a leak.

**Why a hint and not an operation.** There is deliberately no `attach` in §3.
Attaching gives a terminal to a person, and no person is on the far end of this
API — an HTTP request is. A service that "attached" could only attach something
of its own, which is either useless or an impersonation.

**Why local argv and no remote form.** The service knows the machine it runs
on; it does not know how a caller reaches that machine. Synthesising a remote
invocation would assert a network topology it cannot see — the same reason §7.2
requires a peer's address to be one the operator confirmed rather than the
peer's own idea of its name. The client composes remoteness, because the client
is the one that knows it.

Argv rather than a command string, because session ids are operator-chosen and
routinely contain emoji and spaces; a string invites interpolation into a shell
and the quoting bug that follows.

**`readOnly` is not a nicety.** A supervisor offering "watch" and "take over"
as the same button will corrupt somebody's session by leaning on a keyboard.
A client that cannot tell the two apart offers the dangerous one.

---

## 3. Operations

```
create(req, spec)              -> SessionRef
send(req, ref, text, opts?)    -> DeliveryReceipt
respond(req, ref, response)    -> DeliveryReceipt
state(req, ref)                -> SessionState
interrupt(req, ref)            -> Ack
close(req, ref)                -> Ack
list(req, filter?)             -> Collection<Session>
subscribe(req, filter?)        -> EventStream
```

`respond` answers a prompt a session is blocked on, and is a separate
operation rather than a flag on `send` for a reason that is not stylistic:
`send` must guarantee it never produces a keystroke, so that a message
containing something like `C-c` cannot interrupt the session receiving it. That
guarantee is what makes a prompt unanswerable by `send`. A driver must refuse
`respond` when nothing is being asked — a keypress delivered to a session that
is not at a prompt is consumed by whatever it was doing.

`Response` names a **choice**, not a key (§2.7). §5.1 says the interface
expresses questions rather than mechanisms, and "press Enter" would bind every
future driver to this substrate's idea of confirmation.

Every operation carries a `Request` (§2.6), reads included. A driver cannot
compile without deciding what to do with it, which is the point: the rule it
serves — §13's "a proxy presents the original caller's authority, never its
own" — is unenforceable if the operations have nowhere to carry a principal.

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
  source          : "observed" | "assumed"
  observedAt?     : Timestamp | null
}
```

**A declaration carries its own provenance.** `observed` means the driver these
describe reported them — a local driver is always this, since it is describing
itself. `assumed` means nobody has reported them and the values are a
conservative floor.

The distinction is not decoration. Every flag false means "this driver supports
nothing"; a peer that has never answered produces exactly that value, meaning
"nobody has told me anything". Without `source` those are one value, and a
permanently misconfigured peer is indistinguishable from a deliberately minimal
one — §5.7 in its fourth location.

`observedAt` is when the declaration was obtained. Freshness is left for the
caller to judge rather than collapsed into a boolean, for the same reason §11
reports clocks instead of deciding about skew: the component that has the
information is rarely the one that knows what counts as too old.

A caller acting on a capability **must** consult `source`. Reading `assumed`
values as an answer is how a temporarily unreachable peer gets treated as a
permanently incapable one.

A driver must never silently emulate a capability it lacks.

> **Partly resolved (D3).** `source` now separates "nobody has told me" from
> "supports nothing". What remains is that the declaration is still synchronous
> and infallible, so a remote driver cannot *fetch* it in the course of
> answering — it can only report what it happens to have cached. See §14 D3.

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

**The operand this rule needs is `Request.Expect.StartedAt` (§2.6).** A caller
quotes the start time it observed; the driver compares the live session against
that, and refuses on mismatch. This closes the window between the *caller's*
observation and the destroy — the long one, the one a human is standing inside
of, and the one that contains a round trip when the session is on another
machine.

A caller may omit it, and then a driver falls back to comparing against its own
last sighting. That is strictly weaker: it proves only that nothing changed
since the *driver* looked. A driver applying the weak check must say so when it
refuses.

A proxy forwards the expectation and corroborates nothing itself. Checking on
the relaying machine would compare against something a third party believes,
which puts one more layer between the caller's observation and the destroy —
the opposite of what this section asks for. This was open defect D2; see
Appendix A, F16.

### 5.5 State and events, never polling

Federated callers may be many network round-trips away. An API that requires
polling to stay current becomes unusable at exactly the distance federation is
for. State is readable on demand; changes are pushed.

**A subscription filter can name sessions, not only describe them.** On a
substrate that charges one connection per watched session, granularity is a
cost parameter rather than a convenience: a caller that can only say
"everything under this directory" makes a driver attach to every match, while a
caller that can name what it means attaches to one.

Selectors narrow and compose with AND, matching the rule plural reads already
follow, so a caller does not carry two conventions.

Naming an id inherits §5.4's recyclability — a subscription to an id that dies
and is recreated will carry events for the new session. That is safe only
because the discontinuity is **announced**: the subscriber sees `session.closed`
then `session.created` and can tell. A stream that silently swapped subjects
would be §7.3's silent gap in another costume. This was open defect D4; see
Appendix A, F22.

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

**Caller authority is a parameter of every operation (§2.6, §3).** Requirement
3 above and §13's "proxying does not launder authorization" are enforceable
because the authority cannot be omitted: a driver does not compile without it,
and a proxy that holds no credential of its own has nothing to substitute.
This was open defect D1; how it failed before, and what proving the fix
required, is Appendix A, F14.

**Authorization is per principal, per verb.** Each caller — peer or client —
presents its own credential and holds a set of grants: `read`, and one per
mutating verb, plus `relay` for having a mutation forwarded to a peer on its
behalf. Grants default to none, because requirement 3's default is denied.

That granularity is what requirement 3 asked for and a shared secret could not
express: "may watch my sessions" and "may kill my sessions" are exactly the
distinction an operator wants when opening a machine to a peer at all, and one
mutate bit forces them together. `relay` keeps §14 D6's host/client split, now
per caller rather than per service.

**Requirement 4 becomes implementable at the same moment.** An audit trail
wants an actor, and the best a shared token can name is an address — which
answers *where from* and never *who*. With principals the actor is a name, and
a relayed request names both the original asker and the machine that relayed
it, because a line reading "the peer did it" cannot answer who asked the peer.

**How a proxied request presents authority, revised.** §13 requires the
original caller's authority to reach the peer. Under one shared secret that was
literal — forward the caller's credential, and the peer accepts it because it
is the peer's credential too. Per-peer credentials remove that coincidence: a
caller's token means nothing on another machine.

So a proxied request carries authority in two parts. The relaying machine
authenticates as **itself**, with the credential it holds on that peer, and
that is what the peer authorizes. The original principal travels **as an
assertion**, which the peer records. The peer trusts that assertion exactly as
far as it trusts the relay, which is the honest bound: a relay can never obtain
more than it was granted, whatever principal it names, and what the assertion
buys is the audit trail requirement 4 asks for. See Appendix A, F27.

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
- ~~**Backpressure.**~~ Answered by implementing it, and the rest of the design
  left only one option: a subscriber that cannot keep up is **marked and
  resynced**, never silently skipped. Dropping quietly would hand a subscriber
  a hole it has no way to detect, which is §7.3's silent gap; blocking would
  let one slow reader stall the machine's whole event plane. Retention is a
  bounded window, and falling off it is announced like any other gap.
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

**It must not restart on every read.** A driver that stamps the current time
each time it looks makes `since` useless — it would always read "just now", and
the field's only purpose is duration. A driver therefore carries the timestamp
forward while the status is unchanged and resets it when the status changes.

**Duration is the passive discriminator for a class of stall that otherwise
requires touching the session.** A pane holding unsent input looks identical
whether a human is mid-sentence or the pane has stopped accepting input
entirely — and the correct policy for the first ("do not evict, an operator has
text pending") becomes permanent for the second, because no human can come
back to a pane that ignores typing. A sibling project measured exactly that:
fourteen hours of the same unsent line behind a veto that never expired.

Its discriminator was to type a character and see whether it appeared, which is
not something to do to a live session. `since` gives the same answer without
touching anything: text unchanged for hours is not a sentence somebody is still
composing. See Appendix A, F34.

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

**A recycled id is two facts, not one.** An id that is remembered but now holds
a session started at a different time means the recorded session vanished AND a
new one is present under its name. Reporting only "adopted" attributes one
session's history to another — §5.4's recyclability, arriving in a third
operation.

**Records are written when the set changes, not on every read.** A read happens
on every event trigger; persisting each would turn a cheap enumeration into a
write amplifier for no benefit, since what reconciliation needs — which sessions
exist and when they were first seen — only changes when one appears or leaves.

See Appendix A, F30 for the ordering bug that made an earlier version report
everything as adopted, always.

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

**The local half of this exists; the federated half does not.** A service now
streams its own drivers' events with §7.3's cursor, epoch, retention and
resync. A peer's events do not yet arrive, because a remote driver cannot
subscribe — see §14 D8. Until it can, the event plane delivers push exactly
where §5.5 says it matters least.

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

### D1 — Caller authority has nowhere to travel · §6, §13 — **RESOLVED**

Kept in place, rather than deleted, so the numbering the code cites stays
stable and so the entry that once said "this cannot be enforced" now says how
it was.

`Caller` (§2.6) is a parameter of every operation in §3. A driver cannot
compile without deciding what to do with it, and the first remote driver holds
no credential of its own — so the confused-deputy fallback is not merely
forbidden, it is unrepresentable. Reads are included, because "which sessions
exist, in which directories, on which machine" is exactly the reconnaissance an
unauthorized caller wants.

Full account, including what the fix cost and what proving it required:
Appendix A, F14.

### D2 — `close` cannot corroborate · §5.4 — **RESOLVED**

Kept in place so the numbering the code cites stays stable.

`Request.Expect.StartedAt` (§2.6) is the missing operand: the caller quotes
the start time it observed, and a driver refuses when the live session
disagrees. Omitting it is permitted and yields an explicitly weaker check
against the driver's own last sighting, named as such in any refusal.

D1 and D2 were the same defect wearing different clothes — an operation
needing caller-side context with nowhere to carry it — and the envelope in
§2.6 is the fix for the class rather than for either instance. See
Appendix A, F16.

### D3 — Capability declaration is synchronous and infallible · §4.3 — **NARROWED**

The half that is fixed: `DriverCapabilities.source` (§4.3) distinguishes
`observed` from `assumed`, so an unreached peer is no longer indistinguishable
from a peer that genuinely supports nothing. That was the part with real cost,
because it made a misconfiguration look permanent and unremarkable. See
Appendix A, F21.

The half that remains: `capabilities()` still cannot fail and cannot take a
context, so a remote driver can only ever report a cache. Something out of band
must populate it, and until something does, every answer is `assumed` — which
is now honest, but still not the peer's answer.

The consequence is concrete and visible in D7: a proxy derives its deadline
from the peer's declared one, so until the peer's capabilities are known the
proxy uses a floor it has no reason to believe.

- **Proposed fix:** capability discovery becomes an operation like any other
  cross-machine question — fallible, context-taking, and refreshable — rather
  than a property read.

### D4 — `subscribe`'s filter cannot name a session · §5.5 — **RESOLVED**

Kept for numbering stability.

A filter may now name session ids as well as describe a working-directory
prefix, and the two compose with AND. Measured on the first driver: naming two
sessions out of forty that share a directory opens three connections
(lifecycle plus one each) rather than forty-one.

The residual limitation is not this defect. A subscription still cannot span
machines, because no service implements the event stream and the remote driver
answers `unsupported` — so a filter naming sessions narrows what one machine
watches, not what a fleet does. That is the event plane's missing federation
design, not the filter's shape.

### D5 — Idempotency retention does not outlive the service · §10 — **RESOLVED**

Keys are durable when a state directory is configured, so a caller retrying a
`create` across a restart receives the session it already has rather than a
second one.

**Intent is recorded before the side effect**, which is the part worth stating.
Persisting a key after starting the session closes the restart window and
leaves a narrower one: crash in between, and a retry starts a second agent in
the same working directory — the same disaster through a smaller door. So a key
is reserved first, and completed with the resulting reference afterwards.

A reservation found at startup means exactly one thing, and the response is to
look rather than assume. If a session matching the recorded name **and working
directory** exists, it is adopted and the record completed; if nothing matches,
the create did not take effect and proceeding is safe because there is nothing
to duplicate. Both branches are safe, which is the property worth having.
Matching on the name alone would adopt a recycled name — §5.4's lesson, in a
new operation.

In-memory remains a legitimate configuration for a throwaway instance. What is
no longer possible is a service that looks durable and is not: an unreadable
key table is fatal at startup rather than absorbed into an empty one, because
starting fresh silently discards precisely what §10 exists to keep.

### D6 — Mutation permission cannot distinguish host from client · §6 — **RESOLVED**

Two independent grants, decided by whether the request targets this machine or
a peer:

- **host** — may mutate sessions on this machine (what this host exposes);
- **relay** — may forward a mutation to a peer (what this instance may do as a
  client, which exposes nothing here — the peer takes the risk, and the peer
  has its own gate).

Both default closed. The configuration that was previously unreachable, and is
the one a fleet actually wants, is now expressible: a hardened host that is
still a full-featured client.

Found by deploying, not by reasoning — the first cross-machine mutation was
refused by the wrong machine. See Appendix A, F18.

### D7 — Deadline composition across a hop is unspecified · §4.4

§4.4 makes a deadline mandatory and governs a *caller* shortening a driver's
declared bound: "a caller may supply a shorter deadline; never a longer one."
It says nothing about what happens when the caller is itself a proxy.

The gap has a sharp edge. If a proxy waits less time than a peer has declared
it may take, the proxy abandons calls the peer would have completed — and
reports each one as `unreachable`. A machine that answered is described as one
that did not, which is §5.7's confusion produced by a timer rather than by a
missing field.

Measured, not reasoned: a peer declaring 5s, behind a proxy waiting 3s, on a
host loaded enough that a single subprocess spawn cost over a second. The peer
was healthy and answering throughout.

- **Mitigation, implemented:** a remote driver treats its configured deadline
  as a *floor*, and once the peer's capabilities are known waits at least the
  peer's declared deadline plus a transit margin. Before they are known it can
  only use the floor — which is one more consequence of D3, capability
  declaration being unable to say "not yet known".
- **A second edge, found while fixing the first:** the bootstrap is circular.
  The deadline a proxy should enforce is derived from the peer's declared one,
  but learning it requires a call — and bounding that call by the too-short
  floor is exactly what the derivation exists to correct. Resolved by treating
  capability discovery as out-of-band metadata that honours only the caller's
  context: §4.4 governs *session operations*, whose point is that they must
  not block unboundedly, and a probe whose purpose is to discover bounds is
  not one of them.
- **Still open:** the general rule. A fleet more than two machines deep, or
  one where a peer raises its deadline at runtime, needs deadline composition
  stated in this document rather than implemented in one driver. Note also
  that a chain of proxies would need each hop's budget to *shrink*, which is
  the opposite direction from the one hop case — the two requirements are not
  obviously reconcilable, and §13.1's one-hop rule is currently what keeps the
  question from arising.

### D8 — Events do not cross machines · §5.5, §13 — **RESOLVED**

A relayed event keeps the originating machine and the origin's own
`(cursor, epoch)` as provenance, and receives the relaying service's cursor for
local ordering. So "resume from cursor N" is never ambiguous about whose N,
while a caller that later talks to that peer directly can still resume there
rather than refetch. This is the same "adopt what the peer said, add only what
you are uniquely positioned to know" split §13.2 uses for source status and F20
for error kinds.

A proxied subscription asks the peer for `scope=local`, and a service serving a
`scope=local` subscription neither streams from its own peers nor delivers peer
events. §13.1 applies to subscriptions, and violating it here is worse than in
a unary call: two mutually-configured machines would each hold an open stream
to the other indefinitely, and a long-lived loop does not announce itself the
way a failed request does.

An interrupted peer stream is announced with a `source.status` before any
reconnection, then resumed from the last cursor seen. Reconnecting quietly
would leave a caller unable to distinguish "this peer has nothing to say" from
"we stopped listening". See Appendix A, F25.

### D9 — A shared stream cannot present per-caller authority · §6, §13 — **RESOLVED**

The observation stands and is now bounded rather than unbounded.

A multiplexed subscription still has many callers at once and outlives any of
them, so it cannot present any one caller's authority; the service subscribes
to peers as itself. What has changed is what "itself" means. It is now a
distinct principal with its own credential and its own grants, so a peer can
grant this service read access to its events without granting it anything else,
and without that being the same authority every caller holds.

Under one shared token the widening was total and invisible: "as the service"
and "as any caller" were the same string. It is now explicit, bounded by the
grants that principal was given, and visible in the peer's audit log.

Residual, and inherent rather than fixable: a subscriber reads a peer under the
service's authority rather than its own. Multiplexing means the stream cannot
be per-caller without becoming per-caller streams, which reintroduces duplicate
events with competing cursors (see the event plane's design). An operator who
needs per-caller peer reads must give that caller its own service, which is a
real answer even if it is not a cheap one.

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

### Phase 4 — fixing the security defect, and deploying

**F14 · Caller authority became a parameter, and the fix is the type rather
than the policy.** D1's failure had a specific shape: authority travelled in an
out-of-band context value, a service could forget to attach it, and a remote
driver missing it reached for the one credential it certainly had — its own.
The request succeeded. The tests passed. Authorization silently widened.

Two changes, and the second matters more than the first. Authority is now an
argument of every §3 operation, so it cannot be omitted. And the remote driver
was stripped of any credential of its own, so there is nothing left to
substitute — a policy a driver could get wrong became a property of the type.

The compiler is the enforcement: changing the interface broke every driver, the
service, and every test in one pass, and each break was a place that had to
decide what authority it was acting under. That is exactly what an out-of-band
value cannot do.

Proven on two machines afterwards, because a fix to an authorization path that
has never carried a real request is a hypothesis. Input sent from one machine
landed in a pane on the other; a session was destroyed across the network; and
§5.4's corroboration refused an id the far side had never observed, which is
the protection working at the exact distance it is hardest to get right.

**F15 · The reverse direction needed a bind change, not a config change.** One
machine bound loopback, so it was unreachable regardless of how the other was
configured — a reminder that "can A call B" and "can B call A" are independent
facts, and only one of them had been tested.

Making both machines peers of each other also produced the first real test of
§13.1. With a genuine 2-cycle in the configuration, a `scope=local` query
returns one source and does not forward; a fleet query from either side returns
identical counts. The document admits the first spike "avoided this only by
being a star" — this one did not avoid it.

**F16 · Two defects, one shape, one fix.** D1 (authority) and D2
(corroboration) were logged separately and read as unrelated: one a security
problem, one a correctness problem. Fixing D1 by adding a parameter made the
similarity obvious — both were *an operation needing caller-side context with
nowhere to carry it*, and both had been "solved" by moving the value out of
band, where D1 could be silently forgotten and D2 could not be enforced at all.

So the second fix generalised the first rather than repeating it: one envelope
(§2.6) carrying authority now and expectation next, with room for whatever the
third instance turns out to need. The test that matters is the one that was
previously unwritable — a driver whose *own* sighting is current and would pass
its weak check must still refuse when the *caller* is quoting a session that no
longer exists at that id.

Two smaller things fell out. Reads had to start returning `startedAt`, because
a caller cannot quote a value it was never given — a guarantee is only as
reachable as the data needed to invoke it. And a proxy had to be made to
forward the expectation rather than evaluate it, since corroborating on the
relaying machine would insert a third party's belief between the caller's
observation and the destroy.

**F17 · Deadlines were deliberately left out of the envelope.** They were in
the first sketch and removed on the same principle the design applies
elsewhere: the context already carries them, §4.4 already governs them, and a
second field would be a second source of truth free to disagree with the first
— the failure §9's `complete` and §13.2's source status both exist to prevent.
Recorded because "we considered it and did not" is worth more than silence when
the next reader wonders why the obvious field is missing.

**F18 · The first cross-machine mutation was refused by the wrong machine.**
Sending input from one host to a session on another returned "this instance is
configured read-only" — from the *relaying* machine, which was not being asked
to mutate anything of its own.

One flag had been governing two questions: what this host exposes, and what
this instance may do as a client. The only way to relay was to open this
machine's own sessions to mutation, which is precisely backwards — the machine
taking no risk had to accept all of it.

Worth recording as a category, not an incident: a permission that reads
naturally as one sentence ("may this service mutate?") can still be two
questions, and deployment is what separates them. No amount of reading the
specification produced this; one `curl` did.

**F19 · Subprocess spawn cost is not constant; it degrades with load.** The
enumeration measurements in F10 were taken on an idle machine. On a peer
carrying 79 agent sessions at load average 63, the raw multiplexer work still
took 0.24s — but the same read through the service took 1.87s. The difference
is fork/exec latency under load, not the driver.

Two consequences. It strengthens the case for minimising spawns rather than
weakening it: the machines that most need fleet visibility are exactly the busy
ones, and that is where per-session spawning would be most catastrophic. And it
is what exposed D7 — deadlines tuned against an idle machine are not deadlines
at all.

**F20 · A proxy was quietly downgrading its peer's error classification.** The
peer correctly answered `conflict` for a destroy whose expectation was stale
(§5.4). The relaying service re-derived a kind from the Go error it held and
produced `invalid` — telling the caller to fix its syntax when what it should
do is re-read and decide.

This is §13.2 — "adopt a peer's SourceStatus; never re-synthesize it" — applied
to errors, and nobody had noticed the rule generalised. The peer had already
classified the failure; a second opinion downstream can only lose information.
A proxying service now relays a classified error verbatim.

Three instances of the same rule now exist (source status, error kind, and the
peer's `count`), which is enough to state it generally: **a proxy relays what a
peer said about itself and derives nothing.** The reachability of a peer is the
proxy's observation to make; everything the peer reported about its own answer
is the peer's.

**F21 · §5.7, found for the fourth time, and fixed by copying §2.3 rather than
inventing.** Capability declaration had the same collapse the design had
already solved twice: an all-false value meaning both "supports nothing" and
"nobody has said". The fix borrows the shape §2.3 uses for session state —
a value plus its provenance plus when it was obtained — instead of designing a
third mechanism for the same problem.

That is worth stating as a working rule. When this design meets absence again,
the answer is not a new type; it is `(value, provenance, observed-at)`, because
that trio is what the two previous instances converged on independently.

The immediate payoff was in D7's fix, which derives a proxy's deadline from its
peer's. Before provenance, "the peer declares 0ms" and "we have never asked"
were the same reading, and the derivation could not tell whether it was
applying a floor because the peer was minimal or because nobody had checked.

**F22 · Naming a thing costs less than describing it, when watching is
metered.** The filter originally carried only a working-directory prefix, which
reads like a reasonable minimum until you notice what a driver must do with it:
attach to every session that matches, because it cannot know which one the
caller actually meant. Forty sessions sharing a directory cost forty
connections to serve a caller interested in one.

The general form is worth keeping. **Where an interface offers only a
descriptive selector, the implementation must satisfy the description — and
pays for the gap between what the caller said and what the caller wanted.** An
identifying selector closes that gap. This is §5.4's lesson ("a proxy for
identity is not identity") arriving in a second operation, where it costs
connections rather than correctness.

One consequence had to be reasoned about rather than measured: naming an id
inherits recyclability, so a subscription can silently change subject when an
id is reused. It is acceptable here only because the stream announces the
change — closed, then created — which is the same property §7.3 demands of
reconnection. Had the stream not already been obliged to announce, this fix
would have introduced a silent gap while closing a cost problem.

**F23 · Two harness faults found while proving F22, both of the same kind.**
Neither was in the driver, and both would have quietly devalued the tests that
guard it.

A data race in the fake multiplexer: the test goroutine mutated it while a live
subscription's engine goroutine read it. Latent for as long as subscriptions
have been tested, and surfaced only when a new test shifted the timing. Every
subscription test was therefore trustworthy by luck rather than by
construction.

And an equality assertion across two reads of a live machine — federated count
versus direct count — on a host where sessions are created and destroyed while
the test runs. It failed once, passed on retry, and that is the worst outcome
available: a test that fails at random teaches people to ignore failures, which
costs more than the test was ever worth.

Recorded because the pattern generalises past this repository: **when a test
asserts on a moving system, decide what must hold and assert that, not what
happened to be true when it was written.** What matters here is that federation
carries sessions faithfully; that the fleet stands still is not a property
anyone claimed.

**F24 · Implementing the stream answered two questions the document had left
open, and both answers were forced rather than chosen.**

The SSE framing question — does `kind` travel as the `event:` line, a JSON
property, or both — turned out to have no defensible single answer. `event:`
is what makes a browser `EventSource` able to listen by kind; the JSON property
is what spares every other client from parsing SSE framing to learn what it
received. Picking one makes the stream awkward for half its consumers, so it
carries both, plus the cursor as `id:` so a reconnecting browser sends
Last-Event-ID without any client code. Redundancy chosen deliberately, at the
cost of a short string per event.

Backpressure (§7a) had exactly one answer consistent with the rest of the
design. Dropping silently hands a subscriber a hole it cannot detect, which is
the failure this specification is organised against; blocking lets one slow
reader stall the machine's event plane. So a subscriber that overflows is
marked and resynced — the same announcement §7.3 already required for a cursor
that falls off the retained window.

Neither answer required a judgement call. Both were determined by rules already
written down, which is the clearest sign so far that the design has become
self-consistent enough to decide things on its own.

**F25 · D1's rule met a case it did not anticipate, and the failure was
silent.** The hub's peer pump called `subscribe` with a system request carrying
no credential; the remote driver correctly refused, the pump returned, and the
event plane was local-only with nothing logged and every test passing. The
first symptom was a live cross-machine subscription that simply produced
nothing.

The lesson is not "add a credential". It is that **a rule phrased in terms of
"the original caller" quietly assumes one caller per request**, and a
multiplexed stream violates that assumption without violating the words. See
D9.

**F26 · A field was added to the event type and forgotten in its wire form.**
`Origin` existed in memory, survived every unit test, and vanished at the
encoder — the live cross-machine test showed events arriving correctly
attributed with `origin: null`.

The separate wire envelope exists precisely so the stream's shape is stated in
one place, and it still drifted, because "stated in one place" only helps if
something checks the two against each other. There is now a test that reads a
frame off the wire and looks for the field. Worth generalising: **a type that
mirrors another needs a test that crosses the boundary between them, or it
mirrors it only until someone edits one side.**

**F27 · D1 was right about where authority must travel, and its mechanism only
worked by coincidence.** Forwarding the caller's literal credential to a peer
was correct under one shared secret — and that shared secret was exactly what
§6 requirement 3 needed removed. Introducing per-peer credentials therefore
broke the fix for D1, which had been verified working across machines a few
hours earlier.

The rule survived; the implementation did not. Authority now travels as
transport identity plus an asserted principal, which is what "present the
original caller's authority" has to mean once the caller's credential is not
meaningful on the far machine.

Worth generalising, because it is easy to mistake one for the other: **a fix
verified end to end proves the mechanism worked under the conditions that
existed, not that the mechanism is what the rule requires.** The conditions
here were a deployment convenience nobody had chosen deliberately, and removing
it invalidated a working, tested, deployed path.

**F28 · The refusal that protects a session can also strand it, and the same
function failed both ways.** §2.4's refusal exists so input is never
concatenated into a message a human was still typing. Its detector reads a
terminal, and a detector that reads terminals is wrong in two directions with
very different consequences:

- **False positive** — text that is not pending input is read as pending, so
  every send to that session is refused, permanently, for text nobody typed.
  The session simply stops responding to its supervisor, with no error anywhere
  and a reason that names input that does not exist.
- **False negative** — real pending input is missed, and the next delivery
  concatenates into a half-typed message. Invisible when it happens.

Both were live. A selection menu marks its highlighted option with the same
glyph as the composer prompt, so a session sitting on a menu read as holding
input — found by running the detector across every session on a real machine
rather than by reasoning about it. And the fix for that (requiring the composer
to be fenced by rules on both sides) then depended on recognising a fence whose
label can be longer than its dashes, where two successive thresholds failed on
real screens.

The rule that survived is deliberately generous about what counts as a fence,
because the two errors are not symmetric: **a refused send is visible and
recoverable; a corrupted message is neither.** Where a detector must be wrong,
it should be wrong in the direction somebody notices.

Recorded at length because the failure mode generalises past this driver: any
component that infers intent from a rendered interface will eventually read
that interface's own furniture as content, and the first symptom is a
correctly-functioning system that has quietly stopped doing anything.

**F29 · Persisting the epoch is only honest if the cursor persists with it.**
§7.3's epoch tells a subscriber whether its cursors still mean anything. The
obvious way to stop every restart resyncing every subscriber is to keep the
epoch — and doing only that would be a lie, because a service reusing numbers
it had already issued is worse than one announcing a new instance.

So the cursor high-water mark is persisted alongside. The retained event
*window* deliberately is not: a subscriber resuming from an old cursor still
gets `resync_required`, but now with the truthful reason — the sequence
continued, this service simply cannot replay that far back. Persisting the
window would buy transparent restarts at the cost of durably storing every
event, a much larger mechanism than the problem justifies.

Verified across a real restart: same epoch, and the next events issued cursors
3 and 4 rather than starting again at 1.

The general shape is worth keeping. **Durability decisions come in sets.** A
field that identifies a sequence and a field that positions you within it are
one decision wearing two names, and persisting either alone produces a service
that describes itself incorrectly.

**F30 · Reconciliation read the records its own first read had just written.**
An ordinary read records the live set when it changes. Reconciliation enumerated
first and loaded records afterwards, so it compared the world against a snapshot
it had itself produced moments earlier: every session adopted, nothing ever
orphaned or vanished.

The classification still ran. It still produced an answer. The answer was that
everything was fine, always — which is the shape of failure this project keeps
meeting: not an error, but a confident report built on evidence the reporter
manufactured.

Fixed by reading what was remembered before looking at what exists. Worth
stating as a rule, because the same trap is available anywhere state is both
read and written on a common path: **a process that compares "before" against
"after" must capture "before" prior to anything that can write it — including
its own instrumentation.**

### Phase 5 — answering, and driving a live agent on another machine

The read path was federated and the write path worked locally. What remained
was the case the whole layer exists for: an operator on one machine starting an
agent on another, and getting it past every question it asks before it will do
any work. Every finding below came from attempting exactly that.

**F31 · A session can be lost to a dialog nobody can reach, and there were
three of them.** In one working session a supervisor met a folder-trust
question on every newly created session, a resume-from-summary question on a
session being reattached, and a menu inside a running conversation. None could
be answered, because `send` is built to guarantee it never produces a
keystroke — the property that makes it safe for messages is exactly what makes
it useless for control.

The consequence was not a degraded session but an unreachable one: an agent
could be started and then never got past its first question. §3 gained
`respond` for this.

Two detection failures compounded it. The menu detector knew one footer
(`Enter to select`) and both real prompts used another (`Enter to confirm`), so
they classified as `unknown` — which reads as "cannot determine" rather than
"blocked on a human", and a supervisor waits forever on something that will
never move by itself. And a fresh session renders a **placeholder hint** in its
composer, which the composer detector read as typed input, refusing every send
to a session nobody had ever spoken to.

The placeholder is separable only by how it is painted: the hint is rendered
dim (SGR 2) and typed input is not. Matching the hint's words would have
repeated the spinner-verb mistake — prose in an interface with no compatibility
contract. **Where an interface distinguishes two things visually, the
distinction is in the rendering, and reading the text instead is guessing.**

**F32 · Create manufactured the stuck session it exists to avoid.** §2.1 lets a
spec carry an initial prompt; §4.4 bounds every call by the driver's declared
deadline. On a runtime that takes far longer to paint its interface than any
sane deadline, those two requirements do not fit — so Create delivered
immediately, the paste landed, and the submit keystroke was swallowed during
startup.

The prompt then sat unsent in the composer, indistinguishable from a human's
half-typed message, and every later send was refused to protect text the
session had put there itself. Delivery now happens after Create returns,
bounded, and only once the interface is ready — and stops if a prompt is
waiting, because clicking through a trust question is a consent decision a
driver must not make on a caller's behalf.

**F33 · A sibling project had already measured this family, and two of its
findings were bugs here.** A supervisor built on the same substrate has been
tracking "text arrived but was never submitted" for months. Reading its issue
tracker was worth more than any amount of further testing, because it had
counted things a single session cannot: eight stranded operator instructions in
one day, and 37 of 39 panes fleet-wide holding the same unsent line.

Two of its results applied directly:

- **Submitting immediately after delivering loses a race.** The submit can win,
  the prompt is submitted empty, and the text lands afterwards — where it sits
  unsent forever. Delivery is now confirmed on screen before submitting, and a
  failure to confirm is reported as `unknown` naming the stranded text rather
  than silently dropped.
- **`Enter` is not reliably the same as `C-m`.** The same pane, seconds apart,
  ignored `Enter` and submitted on `C-m`. Both are "the same character" in
  principle; only one has been observed to work when the other did not.

Also worth recording: that project found a prompt whose highlighted default is
`No, exit`. A caller that reflexively accepts the default would kill the
session it was trying to start — which is why §2.7's `choice` is explicit, and
why §2.3's evidence now names the highlighted option rather than merely
reporting that something is blocked.

**The general lesson is about where to look.** A design can be argued about
indefinitely; a system that has been run in anger has counted its failures. When
one exists next door, its bug tracker is evidence, and evidence outranks
reasoning.

**F34 · The field that had been filled with nil the whole time was the answer.**
`since` existed in §2.3 from the first draft and every driver passed nil, because
nothing had needed it. It turns out to resolve a stall that a sibling project
could only diagnose by typing into the pane to see whether characters appeared.

Duration distinguishes "an operator is mid-sentence" from "this pane stopped
accepting input", and those demand opposite responses while presenting
identically in a single reading. One reading cannot tell them apart; two
readings and a clock can.

Two things worth carrying forward. **A spec field that every implementation
fills with nil is not necessarily unnecessary — it may be unused because
nothing has yet needed the question it answers.** And the cheapest new signal
is usually not a new probe but a second look: this needed no extra call, no
extra permission, and nothing done to the session at all.

**F35 · A parser over screen content allocated without a bound, and hung the
service.** `parsePrompt` padded its option list up to whatever index it read,
so a transcript line reading `1000000. something` allocated a million entries.
The live service stopped answering — including its own health endpoint — and it
looked like a network fault until loopback proved otherwise.

The pane is written by an agent that can print anything. Anything parsed out of
it is attacker-influenced input in the general case and arbitrary input in every
case, and the code treated a number on screen as a size. Both the index and the
digit count are now bounded.

Worth stating because the same shape recurs wherever a system reads a rendered
interface: **a value parsed from a display is input, and sizing an allocation
from input is the oldest bug there is.** It is easy to forget when the "input"
looks like a menu.

**F36 · Binding only to a tunnel interface makes a service unreachable from its
own machine.** When the VPN dropped, the service was still listening on its
tunnel address and answering nothing — not even to a client on the same host,
because the route to that address went with the tunnel. It presented exactly
like a hung process: `launchctl` showed it alive, `lsof` showed it listening,
and every request timed out.

§6.1 says exposure beyond loopback is explicit configuration, and that remains
right. What is missing is that a service which can only be reached over a
tunnel has no local fallback when the tunnel is what failed — so diagnosing it
requires knowing to try a different address, which is precisely what nobody
thinks to do while it looks like the process is wedged.

*Fixed:* loopback is now bound automatically, on the configured port, whenever
the configured addresses do not already cover it. The general form is worth
stating, because it is not about tunnels: **a service must remain reachable
over a path that cannot be taken down by the failure being diagnosed.**
Configuring an interface is a statement about who ELSE may reach the service,
never about whether its own machine may.

**F37 · Footer matching was always going to lose, and four variants proved it.**
The prompt detector recognised menus by their footer text. One runtime produced
`Enter to select · Tab/Arrow keys to navigate`, then `Enter to confirm · Esc to
cancel` on two different boot screens, then `Esc to cancel · Tab to amend` on a
tool-permission dialog. Each new screen needed a new matcher, which is how this
class of stall stays permanently one release behind the thing it watches.

What every variant shares is the question itself: a run of numbered options
near the bottom with one marked as highlighted. Detection is now structural
first, with the footer kept as a second signal for the case structure misses —
a long menu whose highlighted marker has scrolled above the captured window.

Neither signal alone is sufficient, and that is the point: **when an interface
has no compatibility contract, match what the interface is FOR rather than how
it happens to be decorated** — and keep the decoration as a fallback, because
the thing it is for can also fall off the edge of the screen.

**F38 · The endpoint a caller reads before destroying did not return what
destroying requires.** A single-session read returned only an id and a state,
because the driver operation behind it returns a state. But §5.4's strong
corroboration needs the caller to quote back the session's start time — and
that field was absent from exactly the response a caller would read first.

F16 already said a guarantee is only as reachable as the data needed to invoke
it. It said so about listings, and the same omission reappeared one endpoint
over. **A rule learned about one surface is not learned until it is checked on
every surface that could break it.**

### Phase 6 — the preconditions for being adopted

Planning a supervisor's migration onto this service produced a short list of
things that had to be true first. None was a missing feature; each was a way
the service could be wrong without being able to say so.

**F39 · A participant that cannot state which code it is running turns every
disagreement into a mystery.** Two machines in one fleet silently ran different
builds. The older one still had a bug the newer had fixed, and the symptom — a
session stranded at a question the newer code answers — made no sense against
the source anyone was reading. The entire diagnosis was spent looking for a
defect that had already been fixed.

Every surface reported health, and each was right by its own standard: the
service was running, answering, and correct for the code it happened to be.
What no surface could express is the distinction that mattered. **"We disagree"
and "we are different vintages" need opposite responses — the first is a bug,
the second is a deploy** — and nothing in the API could tell them apart.

`GET /v1/health` now carries a build identity, and a peer's is learned on the
same probe that learns its deadline. Two details are load-bearing:

- The stamp comes from version control, not a hand-maintained constant. A
  constant records what somebody remembered to bump.
- **An unknown or locally-modified build never compares equal to anything,
  including an identical-looking counterpart.** This is §5.7 again, and the
  asymmetry is deliberate: this comparison exists to raise a warning, and a
  false "same" suppresses precisely the warning worth having. A false
  "different" costs a log line.

The comparison also names *why* it failed, because "different revisions" sends
an operator looking for a lagging deploy, and saying that about a comparison
that could not be made wastes the same diagnosis this finding is about.

**F40 · An unverified deploy is a deploy that can silently not have happened.**
F39's skew was produced by cross-compiling and copying a binary by hand. The
copy never failed loudly; what happened is that a service kept serving the old
binary afterwards, and nothing checked.

The deploy path now asks the running service what it is, *after* restarting it,
and fails if the answer is not what was just installed. It also refuses to
build from a modified tree by default — a binary with no identity cannot
participate in F39's check at all, so shipping one quietly disables the
mechanism that catches the problem.

Worth stating generally: **a deployment step that does not read back the
deployed state is a copy, not a deploy.** The failure mode is never the loud
one.

**F41 · The largest single source of `unknown` was settled by looking twice.**
A fleet-wide read across two machines returned 91 sessions, of which 10 were
`unknown` — and every one carried the same evidence: *no spinner line; composer
present and empty.*

That branch was written deliberately. From one capture, a session sitting at a
fresh prompt and a turn that began too recently to have painted its spinner are
byte-for-byte identical, and §5.6 says degrade rather than emulate. The
classifier was right to refuse.

But the refusal was permanent, and 11% of a fleet reading `unknown` is not a
safe default — it is the state a supervisor's rescue ladder triggers on, so
honest uncertainty at rest becomes intervention against sessions that are
merely idle.

What settles it is not a better screen-reading rule but a **second look**. A
turn that had just begun paints within a second; a screen unchanged thirty
seconds later was not mid-anything. The driver already remembers each pane
between observations — that is where §8's `since` comes from — so the
resolution costs no extra capture and never touches the session. F34's lesson
repeated: the answer was in a field that already existed.

Three details are load-bearing:

- **It resolves only toward less activity.** A screen that CHANGED between
  observations is left `unknown`, not called `working`. Content moves for
  reasons other than a turn, and the wrong direction here interrupts a session
  that was doing nothing.
- **A first sighting still answers `unknown`.** The floor is unchanged; only a
  comparison can lower it.
- **A failed capture yields no fingerprint.** Otherwise two consecutive
  failures compare equal and get read as a stable screen — F5's driver
  malfunction laundered into an observation about the session.

The general form: **when a single sample is genuinely ambiguous, the fix is
usually another sample rather than a cleverer reading of the first** — and the
sample you need is often one you already took.

**F42 · The glyph was an animation frame, and matching it was matching one
frame of five.** F41's fix left a residue of `unknown` on one machine, so the
next step was to open a pane and look. It was 21 minutes into a turn, with a
running status line on screen, and the classifier could not see it: the line
began with `✽` and the detector matched `✻`.

A sweep of every session on that machine found **five glyphs in use at the same
instant** — `✻ ✽ ✢ ✶ ✳` — because the leading character is animated. So a
session's status line was legible or invisible depending on which frame the
capture happened to catch, at random, refreshing several times a second. 16% of
one machine's sessions were `unknown` for this reason alone, and the number
would have moved on its own between any two readings.

This is F37 again, one level down, and the repetition is the point: **the
footer was decoration, and so was the glyph.** What the line is FOR is
announcing a turn, and the parts that carry that meaning — the ellipsis for
running, `for <duration>` for finished — were already being matched. The glyph
only ever needed to be recognised as *a symbol rather than text*.

Widening it immediately produced a second bug worth recording, because it is
the cost of every loosened matcher: the composer's own `❯` is a symbol too, and
sits below the status line. The old scan stopped at the first symbol-led line
it met and reported "found nothing usable", so **every screen went from
one-frame-in-five detection to none at all.** The tests caught it; the fix was
to scan for the status line's *shape* rather than stopping at the first
candidate. Chrome is full of symbols — `❯`, `⏵⏵`, `▸`, `⎿` — and a matcher
loose enough to survive an animation must not treat the first symbol it meets
as decisive.

Worth stating as a rule for anything that reads an interface with no
compatibility contract:

> **Every constant you match against is a bet about what will not change.
> Prefer the ones that carry meaning — a structure, a role — over the ones that
> carry style, because style is exactly what a UI is free to animate.**

**F43 · The attach hint immediately justified itself: the two machines do not
agree on where the binary is.** The first fleet-wide read carrying §2.8 hints
showed one machine answering `/opt/homebrew/bin/tmux` and the other
`/usr/local/bin/tmux` — different package-manager prefixes, because the two
hosts are different architectures.

A client composing its own attach command would have had to know that, per
machine, and would have been silently wrong on one of them the moment the fleet
stopped being homogeneous. The service knows because it is *on* that machine
and already had to resolve the binary to run at all.

Which is the general argument for putting this in the model rather than leaving
it to callers: **the facts a client would have to hardcode are exactly the
facts the machine already knows about itself.** Every one it hardcodes is a
place the fleet is not allowed to be heterogeneous.

**F44 · There are two absences, and one answer was being given for both.**
Writing the consumer-facing guide meant probing every endpoint as a client
would, and a single-session read for an id that does not exist answered
`200 dead`.

`dead` is a claim about history — it existed, and it ended. For an id the
machine has never had, there is no history, so the claim is manufactured. A
caller that mistypes an id would be told its session had **died**, which is
both false and alarming in a way that invites the wrong follow-up.

The driver could always tell the difference and was not asked to: it remembers
what it has seen, which is the same memory §8's `since` and §12's
reconciliation are built on. Seen and now absent is `dead`; never seen is
`not_found`.

Note where this was found. Not by a test, not by the implementation, but by
**writing the documentation for someone else** — and the same exercise is what
surfaced F38 one endpoint over. Explaining an interface to a stranger exercises
it differently from building it, because the builder knows which ids exist.

The original test had encoded the old behaviour under the name "absence is an
answer, not an error". That principle was never wrong; it was applied to a
question with two answers as if it had one — which is §5.7 turned inward, on a
rule §5.7 itself produced.

**F45 · Writing the client guide found three defects, and none of them were
findable from inside.** F44 was one. The other two were the same shape:

- **The nonce was undocumented on the wire.** `respond` accepts a nonce that
  makes an answer refuse rather than land on a question that changed underneath
  it — the entire protection §2.7's design exists for — and the HTTP document
  showed `{ "choice": 1 }` and never mentioned it. Every client written from
  that document would have been unprotected, and nothing would have failed
  until the day it mattered. `Response` also carried no JSON tags: decoding
  worked because Go matches field names case-insensitively, so the omission was
  invisible from the server while making it the one type in the package that
  would MARSHAL as `Choice`.
- **A peer's runtime id was reported as the empty string.** The peer names it
  in the very row the driver reads its capabilities from; the value was
  discarded and a placeholder `""` shipped. A client cannot use `?runtime=` to
  disambiguate a session on a peer if a peer's runtime is never reported.

Each was invisible from the inside for the same reason. **The implementer knows
which ids exist, which fields are load-bearing, and what the server will accept
— so the implementer never sends the request that exposes the gap.** Writing
the guide meant calling the API as a stranger: every endpoint, with wrong
inputs, reading only what came back.

The generalisation, which is now three findings deep (F38, F44, this):

> **Documentation for a consumer is a test suite that runs against the parts of
> a design tests do not reach — the affordances.** A test asserts that what you
> called does what you meant. A guide has to state what a stranger should call
> and what they will get, and the sentences that cannot be written truthfully
> are the defects.

**F46 · The client guide was tested by having someone build from it, twice.**
F45 argued that documentation is a test suite for the affordances. That claim
was itself testable, so it was tested: an agent was given the guide, forbidden
from reading any other file in the repository or calling the service, and asked
to implement a session-management client. It reported per-function confidence
and, more usefully, every question the guide had failed to answer.

The first run failed in one specific place. `create` — the operation the client
existed for — scored *low confidence*, because the guide said "see below" and
had no below. The implementer reconstructed a request body from the *read*
shape and guessed field names. Two of the guesses were wrong in the worst
available way: the server ignores unknown fields, so the create would have
half-worked, producing sessions named by the driver instead of by the caller,
with nothing failing.

The same run also found that a client had no documented way to learn **which
machine it is**. `/v1/machines` carries a `self` flag; the guide never showed
the response. The implementer therefore routed *every* attach — including local
sessions — through SSH to the machine it was already running on.

The guide was corrected and a second, independent implementer given the same
task. `create` moved from low confidence to "no gaps"; the attach path used
`self`; the code ran against the live service and listed 99 sessions, handled
emoji ids, distinguished alive from gone, and surfaced a permission error
verbatim.

Three things this technique is good at, which review is not:

1. **It finds absences.** A reviewer reads what is present. An implementer
   stops at what is missing, and has to say so.
2. **It grades by confidence, not correctness.** "I did this and I am not sure"
   locates a weak passage precisely; a correct-looking implementation hides it.
3. **It is honest about the reader's ignorance**, which the author cannot
   simulate. The author knows which ids exist.

What it does not test is judgement about the *host* language. The generated
client declared `local path=` in zsh, where `path` is tied to `PATH`, and
destroyed its own environment inside every request — a bug the guide could not
have prevented and should not try to.

**F47 · A subscription exhausted the machine it was watching, and the
incumbent supervisor read the result as "everything is gone".** A forgotten
client — one `curl` that outlived the shell that started it — held a
fleet-wide subscription for two hours. Unfiltered, so the driver opened one
control client per session: 62 of them on a 69-session host.

The cost was not paid where it was incurred. Each client is a connection to a
multiplexer server that launchers, supervisors and a human's terminal also use,
and that server has one descriptor budget shared by everything. It reached 262
descriptors and began refusing new clients. Every subsequent connection —
including a plain `list-sessions` — failed with *"server exited unexpectedly"*,
while all 69 sessions and their agents were alive and healthy.

**The incumbent supervisor then logged: `MASS-VANISH BURST — 67 sessions gone
in one tick with no paired kill`.** It had asked, been refused, and recorded
the refusal as an observation about the world. Its own guard — a threshold on
implausible disappearance — is the only reason it paused instead of reaping 67
live sessions.

Three lessons, and the middle one is uncomfortable.

**1. This is §5.7 with real consequences, and it is the strongest evidence in
this document.** Asked the same question in the same conditions, this service
answered `unreachable` carrying the peer's own error text, and its client
printed *"sessions there are NOT shown and are NOT known to be gone"*. The
distinction this specification is built around is not academic: one system
concluded the machine was empty, the other concluded it could not see. Only a
threshold heuristic stopped the first from acting on it.

**2. The design that caused it was already documented, and documenting a cost
is not bounding it.** §5.5's "a vague subscriber pays" was written, measured
(26 clients, 26 MB, released on disconnect) and published in the client guide.
Every word was true and it still took the machine down, because *"the
subscriber pays"* was false: **the subscriber pays in a resource the machine
shares with everything else on it.** A cost borne by a shared substrate is not
a cost, it is a hazard, and hazards need bounds rather than documentation.

**3. Bound the accumulation, not the snapshot.** The obvious cap is on the
initial pass. The path that matters over hours is the one that attaches to
sessions appearing *later*: it bounds the fleet's accumulated history rather
than its size at any instant, and capping only the first pass would have made
the leak slower instead of impossible.

The cap is 16 per subscription, and it costs correctness nothing, because
notifications here are triggers rather than data: any one of them causes a full
enumerate-and-diff across every session. Verified while capped — a session
created afterwards, outside the watched set, still produced `created`, `state`
and `closed`. What degrades is latency on quiet sessions, and the cap is
logged rather than silently applied.

> **A limit that only appears in documentation is a limit the system does not
> have.** If exceeding it damages something outside the component, the
> component must refuse, not describe.

### The pattern worth naming

§5.7 — *absence and failure are different answers* — has now been discovered
independently at five different altitudes:

1. **A status field.** A driver that cannot determine state must say `unknown`,
   not guess (§2.3).
2. **A plural response.** A machine that did not answer contributes a
   `SourceStatus`, not an absence from `items` (§9).
3. **Inside a driver.** A pane that could not be read is not a pane that was
   read and found empty (F5).
4. **A capability declaration.** A peer that has not answered is not a peer
   that supports nothing (D3).
5. **A build identity.** A peer whose build is unstamped is not a peer whose
   build matches — and here the rule had to be enforced in a *predicate*
   rather than a field, because the tempting default was to let an unknown
   compare equal and stay quiet (F39).

Five instances, found separately, each initially looking like a local detail.
The generalisation is worth stating as a design rule for anything added later:

> **Every field in this API that can be absent needs to distinguish
> absent-because-no from absent-because-unknown — and if it cannot, it will
> eventually report someone's ignorance as a fact about the world.**
