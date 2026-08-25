# ADR: `turns` is a liveness fact, measured against §5.8, not a result channel

**Issue:** #111
**Status:** decided

## Context

A caller that dispatches a session (colab-fleet #82's convention: create it
with `agent`/`prompt`, poll for `idle`/`waiting_input`, receive an answer via
the requester's own `input`) cannot tell two situations apart once the worker
goes quiet:

- the worker ran, decided nothing was warranted, or failed to follow the
  reply convention, and stopped: **it ran and produced nothing**;
- the worker never took a turn at all — the prompt never reached it, or the
  session never became receptive: **it never ran**.

Both read identically through every field this API reports today: `status:
idle`, `screenDigest`/`composerDigest` empty or absent, no pending prompt.
These call for opposite responses — ask the first what it concluded; re-
deliver the work or tear down the second — and a caller with no way to tell
them apart has to guess, expensively, in both directions.

Because this repo already ruled once, deliberately and at length, against
adding *any* new field a caller could read as "the session's answer" (#82,
kept by #107), a proposal to add a new field to `SessionState` has to clear
that bar explicitly before its design is worth discussing at all. This ADR
is that clearance, done first — the design section of the plan that shipped
alongside this decision assumes the reader has already read this.

## Decision

**Add `SessionState.Turns *int` (`turns`, §2.3): the number of agent turns
this driver has observed complete since the most recent prompt delivery it
made into this session.** Pointer, not a bare `int` — §5.7's rule applies
here exactly as it does everywhere else in this model: `0` is a positive
finding ("a delivery landed and nothing has completed since"), and absence is
a different fact ("this driver cannot count here"). A driver must never
report `0` in place of "could not tell".

### Why this clears §5.8, checked against all three premises it derives from

§5.8's rule is **provenance, not sensitivity**: report the runtime's own
structured account of its own condition; never content the session produced
for a reader. It rests on three independent precedents, and a fourth
proposal is supposed to be answered by checking against them, not
re-litigated from nothing. Taken in turn:

1. **`screenDigest` is a fingerprint, never the text.** A fingerprint is a
   function *of* content — change the content, the digest changes. A turn
   count is a function of the record's *structure*: how many
   `system`/`turn_duration` boundary markers the runtime appended. It is
   invariant under every possible change to what was actually said, which
   makes it **strictly less revealing** than a field this rule already
   permits, not merely comparably safe.
2. **`environment.names` carries variable names, never values.** `turns`
   carries neither a name nor a value belonging to the conversation; it
   carries a count of runtime-written events.
3. **§2.9's `ConversationRef` names a record the runtime wrote *unasked* —
   "an independent witness... the first source in this model that is not an
   echo."** That unprompted provenance is exactly what makes referencing it
   safe, and `turns` is counted from that same artefact.
   `internal/drivers/tmux/runtimerecord.go` already opens it, unasked by the
   agent, for `LastTurn` and `ControlChannel.reason`; `turns` adds one more
   read of a record this driver already trusts for the identical reason.
   Verified on a live record: the runtime appends
   `{"type":"system","subtype":"turn_duration","durationMs":…,"timestamp":…}`
   at each turn boundary, with no agent-authored field in it anywhere.
   Counting those entries inherits the provenance §2.9 already blessed
   without extending it to anything new.

### Checked against #82's own objections, not only its precedents

#82 raised two objections to a result slot independent of the exfiltration
concern, and both are worth checking by name rather than assumed to fail
because the shape rhymes:

- **"The field would publish a value this service was handed as a fact."**
  `turns` is not handed to the service by anyone. The agent cannot address it,
  choose it, or write to it — the driver *counts* it from a file the runtime
  writes without being asked. Nothing in its production path passes through
  the agent's intent, which is the property the pin/assertion/dictated-address
  trio (§2.13) was decided against, and the property this field never has.
- **"§5.7's absence/failure distinction is unbuildable at this layer."** For a
  result slot, "no result yet" versus "no result ever" is decided by the
  *dispatch brief*, which lives above this service entirely — the field would
  ship a permanently-unresolvable state at its centre. For `turns` the
  distinction resolves from something the **driver** observes: absent means
  this driver holds no delivery mark for the session, or could not read far
  enough back in the record to answer honestly. That is the same shape
  `ConversationRef`, `RuntimeSurfaceRef` and `IdentityAssertion` already have,
  and it is the exact test #82 itself set for a field to pass.

### The bandwidth objection, named so it does not need re-deriving

The remaining thing a skeptical reader reaches for: an agent could steer
`turns` by choosing to keep running or stop, so doesn't this leak *something*
about content after all? Two answers, together conclusive: first, §5.8's line
is provenance, not bandwidth — a field is not disqualified merely because an
agent's behaviour can influence it; second, the existing `status` field
already offers an identical channel today (an agent can choose to sit
`working` or settle to `idle`) at the same order-of-magnitude rate, and
nobody proposes removing `status`. `turns` adds no new leakage class beyond
what shipping `status` already accepted.

## What was deliberately left out, and why

- **`stopReason` (a closed category — `normal`/`error`/`limit`/`interrupted`)
  was proposed alongside `turns` and is NOT part of this decision.** Its
  `normal` member is the residual "none of the other three", and the only
  thing that distinguishes it from the others is *the agent deciding it was
  finished* — a session-authored judgement, laundered into a service-reported
  category. That is exactly the shape §5.8 forbids, on the one member that
  matters most to a caller. The other three members duplicate facts already
  on the wire (`error` is `LastTurn.Outcome`; `limit` is `status:
  quota_blocked` plus `Quota`), so the field adds surface without adding
  information a caller could not already derive, on top of carrying the one
  genuinely risky member. #107's ruling flagged this as needing separate
  treatment from `turns`; this ADR agrees and defers it rather than folding
  it in.
- **`lastActivityAt`** (the timestamp of the newest counted turn boundary) is
  nearly free to add alongside `turns` and is left out anyway. This is a
  public, normative, published wire contract — a one-field delta is a
  reviewable delta, and there is no caller need stated for it yet. Noted here
  as an obvious, cheap follow-up, not shipped unasked.

## Consequences

- `PromptDelivery.Outcome` (§2.11) can now resolve a case that was previously
  permanently stuck: on a substrate where receipt is not directly observable,
  `outcome` could rest at `queued`/`unknown` forever. A turn taken after
  delivery *is* observable receipt, so a caller reading `turns > 0` alongside
  a stuck `PromptDelivery` now has independent corroboration that the prompt
  landed, without this ADR touching `PromptDelivery`'s own structure.
- `turns` is tmux-only for now, `omitempty`, with no capability flag —
  precedent is `LastTurn` (screen/record-derived, no flag either) rather than
  `screenDigest` (gated behind `deliversRawKeys`). Absence already carries
  the honest meaning "this driver cannot count here"; a capability flag would
  only formalise what the field's own optionality already states.
- A future proposal to reopen §5.8, or #107's ruling on it, still has to name
  which of §5.8's three underlying premises it is challenging. This decision
  challenges none of them — it is one more field measured against the same
  standard, not a change to the standard.
