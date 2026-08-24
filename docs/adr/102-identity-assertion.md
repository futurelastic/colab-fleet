# ADR: identity drift becomes a wire field, not only prose

**Issue:** #102 (the state-side half of #97/#96, `group:identity-on-the-wire`)
**Status:** decided

## Context

#97 measured a rename that returned `202 accepted`, read back correct for
roughly half an hour, then silently reverted — id, name and attach target all
restored — with nothing on the event stream or in any read saying so for that
whole window. #97's own fix closed the *detection* half: `List` now compares
what it just enumerated against a durable asserted-identity record
(`sessionRecord.Name`, keyed and matched by `(pane, created)` so a rename does
not orphan it), reports a mismatch, and repairs it, bounded, on a later read
(`internal/drivers/tmux/reconcile.go`, `docs/adr/97-identity-in-record.md`).

It did not close the *reachability* half. The only place the mismatch
surfaced was a sentence appended to `SessionState.Evidence` —
`"this machine last asserted %q for this session and the runtime now carries
%q"` — and `session-abstraction.md` §2.3 is explicit that `Evidence` fields
are prose to display, never to parse. A caller that wants to act on drift
programmatically had nothing it was allowed to read. #97's own ADR filed this
gap as a follow-up rather than folding it in, because it needed a root
`internal/`-package change and a wire addition outside that fix's scope
fence.

## Decision

**`fleet.Session` gains `IdentityAssertion` (`identity.go`), populated
entirely from facts the tmux driver's durable record already holds — no new
detection.** It follows the absent / unresolved / resolved discipline
`ResumeOutcome` and `PinOutcome` already established for this API (§5.7), but
with **four** states rather than three — one more, for a reason specific to
this field rather than symmetry with `RuntimeSurfaceRef`:

| shape | means |
|---|---|
| field absent | this machine has asserted no identity for this session at all — adopted, foreign, or a driver with no state store |
| `drifted` absent | an identity WAS asserted, and no read has yet matched the record to a live run |
| `drifted: false` | settled, as of THIS read: the runtime carries exactly what was asserted |
| `drifted: true` + `carried` | this read found them disagreeing — #97's defect, now machine-readable |

The third state is real, not invented for symmetry: `Create`/`Rename` write
`Name` the instant an identity resolves, before `Pane`/`Created` — the pairing
`identityDrift`'s own `indexByPaneCreated` matches on — are known.
`indexByPaneCreated` deliberately skips a record with an empty `Pane`, so a
just-written stub cannot yet be told apart from a drifted one. Reporting that
as "no drift" would be exactly the collapse §5.7 forbids; it clears on the
very next `List`, once `noteSessionSet` fills the pairing in.

**One prose sentence, two channels.** The existing `Evidence` format string
was extracted into `driftSentence` (`reconcile.go`) so `SessionState.Evidence`
and `IdentityAssertion.Evidence` are computed from one function and can never
disagree about the same read — verified by a cross-check assertion in
`TestIdentityAssertionReportsDriftStructurally`.

**Populated in `List`, and that alone covers both wire endpoints.** The
single-session HTTP GET (`internal/service/http.go`) answers from `List`, not
from `State` — `State` returns a bare `SessionState`, has no drift-detection
path of its own, and falls through to it only when a session has vanished
from the listing entirely, a case where an identity assertion is moot by
construction. `State`, the `driver.Driver` interface, and the other two
drivers (`opencode`, `remote`) are therefore untouched.

**`Create`, and an idempotent replay of one, report the uncorroborated
state.** Nothing has been read back live at that point, so none of `Create`'s
three return sites (fresh path, idempotency-key replay, adopted-pending
recovery) may claim more. All three now share `identityAssertionForCreate`,
which reads the durable record rather than reconstructing the value from the
caller's own arguments — the same "one fact, not two that can drift apart"
property `noteCreateRecord`'s own comment already states for `pins`/
`runtimeSurface`/`promptDelivery`.

### Why no `contested` state

A driver giving up on a repair — the wanted name is live under another
session, or `maxNameReasserts` is already spent — is a fact about the
**repair policy**, decided by `reassertNames` *after* this response already
exists, not a fact about what this read observed. Folding it into a field
that describes an observation would report a future action as a present fact,
the same category of error `Session.Agent`/`.Model` are documented to avoid
(colab-fleet #84). It is also not stable: a name taken by another session
becomes free the moment that session closes. The operational need is met
elsewhere — the existing `identity.contested` counter, and evidence prose
naming how many times a repair was already attempted. A field added to this
published, normative wire contract later is additive; retracting one is not,
so the narrower shape is what shipped.

### Why no `source` field

`PinResult.Source` and `RuntimeSurfaceRef.Source` each fork a real question —
observed against declared, observed against derived. Here there is exactly
one source in every state: `asserted` comes from this machine's own durable
record, `carried` from the live enumeration in the same read. A field with
one possible value would teach a caller nothing and only invite a later
driver to invent a second meaning for it.

### Why `drifted: false` is not latched, unlike `runtimeSurface.known`

§2.13's `known: true` stays true for the life of the record once
corroborated — it names an address, not a health check. `drifted` is the
opposite: a claim about *this* read alone. The runtime's name for a session
can change again the moment after a read reports agreement, and it is the
*next* read that says so, never a cached verdict from an earlier one. Proven
by `TestIdentityAssertionReportsDriftStructurally`: the read that discovers a
drift reports `drifted: true`; the read after the driver's own repair (which
runs *after* the drifted response is already built, per #97) reports
`drifted: false` — a per-read observation, not a latch.

## Alternatives considered

**Three states, matching `ResumeOutcome`/`PinOutcome` exactly.** Rejected —
see "the third state is real" above. Collapsing the uncorroborated stub into
"absent" would misreport a session this machine just named as one it has no
opinion about; collapsing it into "drifted: false" would assert corroboration
that never happened.

**Expose `contested`/`reasserts` on the wire**, so a caller can distinguish a
repairable drift from one this driver has given up on. Rejected: see "why no
`contested` state" above. Revisit only if a caller demonstrates a structural
need with a measurement behind it — a new issue, not a retrofit of this one.

**Thread `identityAssertionFor`'s index through `identityDrift`'s own
signature**, rather than a second `indexByPaneCreated` call in `List`.
Rejected: `identityDrift`'s output also drives `reassertNames`, covered by
`TestIdentityReassertStopsOnceContested` and
`TestReassertRefusesWhenTheNameIsTaken`. Reshaping it to serve a second
reader risked those tests as a moving target instead of the untouched control
they are meant to be; one extra pass over a small in-memory map costs
nothing.

## Consequences

- A caller can now branch on identity drift without pattern-matching prose —
  `session.identityAssertion.drifted` — while `state.evidence` keeps carrying
  the same sentence, byte-identical, for a human reading the response
  directly.
- The uncorroborated state is easy to "simplify away" by a future edit that
  collapses `identityAssertionFor` to two branches instead of three. Guarded
  by `TestIdentityAssertionUncorroboratedAtCreate`, which would start failing
  the moment that collapse happened.
- `sessionFactsFor`'s three call sites inside `Create` are now four
  (`pins`/`runtimeSurface`/`promptDelivery`/`identityAssertion`) that must
  each be threaded through all three of `Create`'s return literals. A future
  `Session`-level field added to only one of the three will silently regress
  the other two to absent on an idempotent replay — `go test -race ./...`
  catches this immediately (`TestIdempotencyKeysSurviveARestart`,
  `TestCreateIsIdempotentPerKey`), but only if the full suite runs.
- #103's coupling concern, as filed, did not materialize: #103 shipped its
  own mechanism over the event plane
  (`docs/adr/103-rename-corroboration.md`), independent of this record.
  `SessionRenamed`/`RenameCorroboration`/`internal/service/rename_corroboration.go`
  are untouched by this change. Feeding this durable record into that
  in-memory bookkeeping so it survives a restart remains #103's own listed
  open question, not resettled here.

## Open questions

- Whether `assertedAt` earns its permanent place on the wire. Included
  because it is the only way a caller reproduces #97's own timing
  measurement (accepted at T, correct at T+30m, reverted by T+35m) without
  keeping a poll history, and it costs nothing — already durable
  (`sessionRecord.NameAssertedAt`), already optional. Free to drop before
  wide adoption if it turns out unused; not free after.
