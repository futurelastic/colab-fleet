# Session abstraction — specification

**Status:** draft. Nothing here has been implemented; nothing here has been
proven by a second driver yet. Treat every claim as a proposal until at least
two drivers satisfy it.

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

---

## 3. Operations

```
create(spec)                  -> SessionRef
send(ref, text, opts?)        -> DeliveryReceipt
state(ref)                    -> SessionState
interrupt(ref)                -> Ack
close(ref)                    -> Ack
list(filter?)                 -> SessionRef[]
subscribe(filter?)            -> EventStream
```

`subscribe` is not optional garnish. Federated callers must be able to learn
about state changes without polling — see §5.5.

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
}
```

A driver must never silently emulate a capability it lacks.

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

### 5.5 State and events, never polling

Federated callers may be many network round-trips away. An API that requires
polling to stay current becomes unusable at exactly the distance federation is
for. State is readable on demand; changes are pushed.

### 5.6 Degrade, never emulate

A driver that cannot observe state reports `inferred` or `unknown`. It does not
manufacture a plausible `observed`. Emulation makes capability differences
invisible at precisely the layer built to expose them.

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

## 7. Open questions

- **Session naming across machines.** Is a fleet-wide name required, or is
  `(machine, id)` sufficient addressing?
- **Peer discovery.** Static configuration or announcement? Static is safer and
  probably sufficient for small fleets.
- **Event durability.** If a subscriber is disconnected during a state change,
  is the event lost, or replayable from a cursor?
- **Partial fleet visibility.** When a peer is unreachable, does `list()` return
  a partial set, or fail? A partial set that looks complete is the more
  dangerous of the two — cf. §5.2.
- **Restart semantics.** When the service restarts, are running sessions
  adopted, or orphaned? A driver may not get a choice.
