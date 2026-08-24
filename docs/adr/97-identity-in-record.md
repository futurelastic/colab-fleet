# ADR: session identity lives in a durable record, not re-derived from the string

**Issues:** #96, #97 (`group:identity-in-record`)
**Status:** decided

## Context

Two measured defects, traced back to the same mechanical cause neither issue
named on its own: this driver's tmux backend has exactly one source of
session identity — `enumerate()` — and every consumer re-derives identity
from it fresh, on every read, with nothing durable in between.

- **#97:** a rename returned `202 accepted`, read back correct for roughly
  half an hour, then reverted with no request asking for it — id, name and
  attach target all back to the old value. Every session on the machine
  briefly read `status: unknown` at the moment of the revert, and a
  credential-store timestamp advanced at the same moment.
- **#96:** `applyMarker` cannot tell "this name already carries the marker"
  apart from "this name coincidentally ends in the same characters" when the
  marker is drawn from the same alphabet as an ordinary name (the fleet's
  actual markers are non-ASCII and do not hit this, but the ambiguity is real
  for any caller that is not). It resolves the ambiguity by guessing
  "already applied" — the safer of two bad guesses, because the alternative
  reopens an unbounded name-growth bug an earlier fix (#90) closed — but it
  is still a guess.

Both are the same shape: a fact this driver itself decided (I appended this
marker; I renamed this session to this name) is not written down anywhere
that survives a re-enumeration, so a later read cannot tell "the runtime
genuinely disagrees" from "I never recorded what I expected in the first
place."

### What the crux investigation established, and what it did not

#97 asked, explicitly, whether a rename ever reaches the tmux server at all.
Reading this driver's own code answers half of that: `List` builds every
session's reported id from a fresh `enumerate()` on every call, and nothing
caches a listing anywhere in this service — so a read returning the new name
for roughly thirty-five minutes is direct evidence that the multiplexer
itself carried the new name for that long. `Rename` issues a real
`rename-session` command and only reports success on a zero exit. A grep of
the whole tree finds exactly one `rename-session` call site (this one) and
nothing else in this service that could plausibly move a name back.

What that does **not** establish is what un-rendered the name. This service
does not appear to be the actor, but nothing here can rule out an actor
outside this codebase entirely — some other process on the machine, sharing
the multiplexer, asserting its own idea of that session's name. The design
below does not need that question answered, which is the point: it converges
correctly whichever of the two ways #97's own "not established" section
named turns out to be true.

## Decision

**Add an asserted-identity record to the durable session record
(`sessionRecord`, `internal/drivers/tmux/reconcile.go`), keyed so a rename
does not orphan it, and make every identity-adjacent operation read from
it instead of guessing from the current string.**

- `sessionRecord` gains `Pane`, `Name`, `Marker`, `MarkerApplied`,
  `Reasserts`, `NameAssertedAt`. `Pane` (paired with the already-present
  `Created`) identifies a session **run** rather than a session **name** —
  the same pairing `conversationKey` already uses, for the same reason: it
  survives a rename where the record's own map key does not.
- `Create` writes `Name`/`Marker`/`MarkerApplied` the instant it resolves a
  name — before there is any way yet to tell what becomes of it, the same
  discipline `noteCreateRecord` (#84/#85/#86) already established for the
  create response.
- `Rename` writes the new `Name` **durably**, re-keyed to the new name, the
  moment its own `rename-session` call succeeds — closing the literal gap
  #97 measured: until this change, `Rename` only updated an in-memory map,
  so nothing this service had wrote down ever expected the session to be
  named the new thing.
- `List` compares what `enumerate()` just found against what was last
  asserted, matching by `(Pane, Created)` rather than by name. A mismatch is
  reported in `State.Evidence` on the read that finds it — so the *read*
  stops silently agreeing with a name that did not hold, which was #97's
  actual complaint, distinct from "the rename itself failed" — and repaired
  by re-issuing `rename-session`, bounded at `maxNameReasserts` (2) attempts
  before the driver reports it contested and stops, the same "a repair
  already proven not to hold is not retried forever" rule `discardProvenFutile`
  already applies to a composer clear that will not move.
- `applyMarker` becomes a tri-state function (`markerState`:
  `markerUnknown` / `markerApplied` / `markerAbsent`). `markerApplied` and
  `markerAbsent` answer #96 exactly, from the record, with no string
  comparison. `markerUnknown` — no record to answer from — reproduces the
  original heuristic byte-for-byte, so a session this driver did not itself
  resolve a name for (adopted, foreign, or a cold store) behaves exactly as
  before.

### Why repair is a read-path side effect, not the same call's own response

A `List` call that first notices a mismatch reports what is genuinely live
at that instant (`enumerate()` just read it) plus the evidence sentence
saying it disagrees with what this driver expected — it does not rewrite its
own already-built response to claim an identity the multiplexer had not
confirmed to this exact call. The repair it issues takes effect for the
*next* read. This is more honest than pretending same-call convergence, and
it is still bounded, still durable across a restart (`Reasserts` lives in the
persisted record, not an in-memory map — a second actor that keeps
re-asserting an old name does not get a fresh budget of attempts merely
because this service restarted), and it still closes #97's actual defect: a
caller reading the *next* response after a drift either sees the repaired
name, or — if genuinely contested — the same evidence sentence again rather
than silent agreement.

## Alternatives considered

**Poll the multiplexer periodically and repair proactively**, rather than
piggy-backing repair on `List`. Rejected: this driver has no background loop
today, and adding one for a defect that only matters when a caller is
actually reading is disproportionate — a session nobody is watching does not
need its name enforced faster than the next time somebody looks.

**Refuse to guess at all when `markerUnknown`** — treat "no record" as an
error rather than falling back to the old heuristic. Rejected: it would make
every adopted or foreign session (the common case after any restart, until
this record accumulates history) refuse to resolve a name at all, which is
strictly worse than the ambiguity it replaces.

**Keep the "record and string disagree → take the safe answer" fallback**
for `markerAbsent`, mirroring `markerUnknown`'s caution. Rejected during
implementation: once the record says a marker is confirmed absent for this
exact string, that *is* the exact answer #96 asks for. Re-consulting the
string after the record has already answered reintroduces the ambiguity the
record exists to remove.

## Consequences

- A drift that is genuinely contested (another live session already holds
  the wanted name) is reported and left alone rather than fought over —
  visible via `State.Evidence` and a counter
  (`identity.contested`), never silent.
- `Rename` still applies none of `naming.go`'s sanitize/number rules to the
  caller-supplied target — unchanged scope, noted as an open question below,
  not fixed here.
- Two related gaps are explicitly **not** closed by this change, because
  closing them needs a file outside this fix's scope fence
  (`internal/service/http.go`) or a wire change (`internal/`'s public
  `fleet` package): a machine-readable field carrying "what this machine
  asserted vs what the runtime carries" (today: prose in `Evidence` only),
  and `session.renamed` being emitted at accept-time rather than at
  confirmed-durable time — so a rename that never holds still emits the
  event #97 itself observed nothing said anything was wrong. Both are filed
  as follow-ups.

## Open questions

- What actually reverted the name in #97's own measurement is still
  unestablished. The design does not depend on the answer, but if it
  recurs and is ever pinned down precisely, this ADR's Context section is
  the place to record it.
- Whether a read-path repair is the right layer for this at all, versus
  something closer to `Reconcile`'s own startup-time role, given
  `internal/drivers/tmux/subscribe.go`'s recorded ruling that a similar
  machine-level fact (credential generation) is report-only, never repaired.
  Raised in the wrap for whoever reviews this to weigh in on.
