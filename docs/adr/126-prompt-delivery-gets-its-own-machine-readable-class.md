# 126 — extend WaitingReason with a third value, and give PromptDelivery its own class field

## Context

#125 made a create's pending prompt explain itself in prose: `PromptEvidence`
is live, updated every time the reason a prompt has not landed yet changes.
#126 measured that prose is all a caller has ever had for this, on any of
seven distinct causes found stopping a session doing its work in one day on
two machines — a folder-trust dialog, a tool-permission dialog, no composer
painted at all, a composer already holding a stranded delivery, input
rejected for size (already fixed, #114), a runtime dying mid-turn, and a
control surface that never reached this service at all. All seven read
identically through the API as "the session did not start". Nobody could
branch on any of them without parsing English a driver is free to reword.

The issue asked, at minimum, for three classes a caller can tell apart
without reading prose: a dialog waiting for a human, not ready yet, and
something already occupying the composer — while preserving the existing
discipline exactly: absent means unclassified, never a guess, and a caller
must never treat an unclassified prompt as safe to auto-answer.

Two of the three were already covered. `WaitingReason` already has
`WaitingPrompt` for a dialog, and `SessionPrompt.Kind` already tells a
folder-trust dialog from a tool-permission one apart in practice (via
`classifyPromptKind`) — kept exactly as-is; #126 explicitly asked for that
discipline to continue, not be duplicated into a second vocabulary. The third
— no composer painted at all — had a status-level answer for a session in
general (`StatusStarting`), but no answer for `PromptDelivery` specifically,
because `PromptDelivery`'s own pending state is legitimately reachable while
`Status` reads `idle` or `starting` or anything else (session-abstraction.md
§2.11 already documents the measured harm of conflating the two: a session
correctly reading `idle` while its prompt is still undelivered).

## Decision

**Add `WaitingStarting` to the existing `WaitingReason` closed vocabulary**,
alongside `WaitingPrompt` and `WaitingUnsentInput`, naming "no composer
painted yet, nothing to be blocked on". It is documented to be used only on
`PromptDelivery.WaitingOn` (below), never on `SessionState.WaitingOn`, which
already has `StatusStarting` for the identical fact and is documented to
apply only when `Status` is `waiting_input`.

**Give `PromptDelivery` its own field, `WaitingOn WaitingReason`**, populated
from the SAME classification that produces `Evidence`, written in the SAME
call (`notePromptPending`), so the two can never drift into disagreeing about
one wait. `promptReadiness`'s `readinessCheck` gained a `waitingOn` field
alongside its existing `reason` string, filled by the identical branch that
builds the prose:

| `readinessCheck` shape | `waitingOn` |
|---|---|
| a dialog is on screen | `WaitingPrompt` |
| composer not found (`!found`) | `WaitingStarting` |
| composer holds other text | `WaitingUnsentInput` |
| ready, or session gone/unclassified | empty (unclassified) |

This directly reverses one line of #125's own "Alternatives rejected" —
"a separate `PendingReason` field instead of reusing `PromptEvidence`...
rejected as needless surface area" — for a reason that ADR could not have
had: #125 only needed a live PROSE diagnosis; #126 is the specific complaint
that prose alone is not enough, filed as its own issue with its own
measurement. The calculus is different, not reversed by mistake.

## Alternatives rejected

- **A second, dialog-specific enum splitting `WaitingPrompt` into "trust
  dialog" vs "capability dialog".** Rejected: `SessionPrompt.Kind` already
  answers this, in practice, for exactly the two causes the issue names
  (`PromptFolderTrust`/`PromptSettingsTrust` vs `PromptToolPermission`). A
  second vocabulary naming the same distinction would give a caller two
  places to check for one answer, and the issue itself says to keep using
  `Kind` rather than replace it.
- **Splitting `WaitingUnsentInput` into "a human's text" vs "this driver's own
  stranded delivery"** (the issue's cause 4). Rejected for `PromptDelivery`
  specifically: at the point `promptReadiness` runs, this driver has not yet
  delivered anything into the brand-new session a create's own prompt targets
  — nothing sent means nothing can yet be *this driver's own* residue — so the
  two causes are not confusable here the way they could be for an
  already-delivered-to session's general `WaitingOn`. Worth revisiting if a
  future issue measures the general (non-create-time) case actually needing
  it; not invented speculatively here.
- **Reusing `SessionState.WaitingOn` for the pending-prompt case.**
  session-abstraction.md §2.11 already rejected this for #125's `Evidence`
  and the same argument applies unchanged to the class: that field is
  documented to mean something only when `Status` is `waiting_input`, and the
  measured harm is a session correctly reading `idle` while a prompt is still
  pending. Writing it there would either misstate the present status or break
  the field's own contract.
- **A brand-new type distinct from `WaitingReason`** (e.g. a
  `PromptWaitingReason` with its own three values). Rejected as needless
  surface area for no additional expressive power: the vocabulary a caller
  needs to answer "why is this thing not ready" is the same shape whether the
  "thing" is a session or a still-pending delivery, and a caller already
  familiar with `SessionState.WaitingOn`'s two values gets the third one free
  rather than having to learn a second, parallel type.
- **Validating `PromptDelivery.WaitingOn` (a closed-set `MarshalJSON`, the way
  `Outcome`/`Status`/`Confidence` validate).** Rejected for consistency with
  the type it extends: `WaitingReason` itself has no such validation today
  (`SessionState.WaitingOn` accepts any string), and adding one only on the
  new field would make the two fields sharing one Go type behave
  inconsistently for no measured reason.

## Consequences

- `createRecord.PromptWaitingOn` is a new persisted field (`create-record`
  store document) — additive, `omitempty`, so an existing on-disk record from
  before this change decodes fine with it absent (matches `PromptOutcome`'s
  own shape).
- `docs/spec/session-abstraction.md` §2.11's `PromptDelivery` block and prose
  gained the field; `TestSpecTypeBlocksMatchGoFields` enforces that the two
  cannot drift apart again.
- Not addressed here, deliberately: causes 5 (already fixed, #114), 6 (a
  fact about the last turn, not about what a caller is waiting on now —
  already has `TurnEnd`), and 7 (a different layer's failure to reach this
  service at all, not this driver's to classify) from #126's own catalogue.
  The issue itself says its seven are evidence the number is greater than
  two, not a closed list to fully cover in one change.
