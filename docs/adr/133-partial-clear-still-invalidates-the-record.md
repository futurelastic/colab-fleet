# ADR: a partial clear still invalidates the stranded record

**Issue:** #133 (§2), following on from #132
**Status:** decided

## Context

#132's own third ask, carried forward rather than resolved: if the composer
cannot be fully emptied, should the driver either (a) leave the ORIGINAL
delivery intact so `resumeIfStranded` still works, or (b) make the NEW,
partly-cleared state itself recoverable? What shipped in #132 removed the
single most common cause of an unclearable composer — a blank row the
line-kill structurally could not cross — but did not change what happens when
a clear pass genuinely makes partial progress and then stops: the record's
`ComposerDigest` still describes the composer as it was BEFORE that pass, not
as it is now, and neither `Discard`'s own retry nor `replaceIfStranded`'s
digest check will act again without a fresh corroborated digest.

Two things are true today that were not true when #132's own report was
written, and they change how much this question is still worth:

1. **`resumeIfStranded` was never blocked by this at all.** It corroborates
   whose delivery is sitting in the composer from this driver's OWN record
   (`strandedMatches`, text-based) plus a fresh `confirmLanded`/
   `confirmSubmitted` read — never against `record.ComposerDigest`. A partial
   clear changes what `Discard`/`replaceIfStranded` will accept next, not
   whether a human's own retry-and-finish path is reachable.
2. **The escape hatch is a real endpoint now, not prose in a refusal
   message.** `Discard` takes `expectDigest` from the caller, not from any
   internal record — a caller who reads the session (one `List`/`Get` call)
   gets the composer's CURRENT digest and can `discard?expect=<that digest>`
   directly, corroborating against the residue as it actually stands rather
   than against a stale strand.

So the field shape #132's report opened with — a composer stranded with "no
combination of `resumeIfStranded`, `replaceIfStranded`, or a plain retry could
reach the composer again" — no longer describes the current code: (1) was
never true of `resumeIfStranded`, and (2) closes the other two paths in one
extra round-trip.

## Decision

**Keep a partial clear invalidating `record.ComposerDigest` for
`replaceIfStranded`'s purposes. Do not make the driver self-heal the record
against the fresh residue.**

Concretely, `tryReplaceStranded`'s existing behavior stays: on a partial clear
(`moved=true, didClear=false`), the record is kept but its digest is left
exactly as it was recorded at strand time — so a second `replaceIfStranded`
call against the SAME record is refused, pointed at `discard?expect=<digest>`
instead of pressing more keys.

### Why not self-heal

The alternative — updating `record.ComposerDigest` to the residue's own fresh
digest the moment a partial clear observes it, so the very next
`replaceIfStranded` call could press again without a caller-driven `discard`
round-trip first — trades a small ergonomic win for a real safety cost: it
would let a SECOND clear pass spend more destructive keystrokes against text
the CALLER never freshly corroborated between attempts. That is exactly the
pattern `Discard`'s own futility refusal (#87) exists to prevent for identical
residue pressed against by the same caller twice in a row — the difference
here is only that the corroboration gap spans two different calls
(`replaceIfStranded`, then `replaceIfStranded` again) rather than one call's
internal retry loop. The digest gate forcing a caller to read before acting
again is the same discipline, not an accident of implementation.

### Why not leave the ORIGINAL delivery resumable instead

The other half of #132's original either/or — leave the record pointing at
what was ORIGINALLY delivered, ignoring that a clear pass touched the
composer at all — is worse, not neutral: `resumeIfStranded` against a record
whose text no longer matches what a partial clear left behind would press
Space+C-m into a composer holding neither the original delivery nor nothing,
submitting a residue nobody asked to submit. §2.4's whole discipline is not
delivering into a composer whose actual content is not corroborated; a record
that quietly stopped describing reality is the same hazard from the other
direction.

### What actually changed since #132's filing, restated as the reason this
### is closed as "no code change" rather than reopened as a gap

`resumeIfStranded` already does the RIGHT thing here today without needing
`record.ComposerDigest` at all, and `discard?expect=<digest>` is a complete,
already-shipped way to reset a residue for `replaceIfStranded` to try again
from. Nothing in this driver currently leaves a caller with zero path back —
#132's own severity-lowering observation ("the flag set is no longer closed
with no exit") is confirmed here as still holding, not merely asserted.

## Consequences

- No code change from this ADR. `tryReplaceStranded`'s digest check and
  `Discard`'s own retry contract are exactly as #129/#132 left them.
- A caller whose `replaceIfStranded` call partially clears a composer and then
  wants to try again must read the session and retry through
  `discard?expect=<the fresh composerDigest>` first — documented behavior, not
  a dead end.
- If a future measurement shows this round-trip is a real operational cost
  (as opposed to a theoretical one), self-healing the record is the
  documented alternative this ADR rejected and can be revisited against that
  evidence — this is a decision made deliberately, not a default nobody
  looked at, and the tradeoff it is weighed against is recorded above rather
  than left to be re-derived.
