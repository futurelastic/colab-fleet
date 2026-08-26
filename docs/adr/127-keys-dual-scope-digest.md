# ADR: `/keys` corroborates on the composer when the composer holds text,
the whole screen otherwise — not one scope for every call

**Issue:** #127
**Status:** decided

## Context

`GET` publishes two independent digests, both already existed before this
issue: `ScreenDigest` — the whole captured screen, always set when a capture
succeeds — and `ComposerDigest` — the composer's own text, set only while
`WaitingOn` is `WaitingUnsentInput`. `Discard` corroborates against the second
(`screenDigest(pending)`, same function, composer text as input). `Keys`
corroborated against the first (`screenDigest(text)`, whole screen) for every
call, unconditionally.

Those two never coincide once the composer holds anything, because the
composer is never the entire screen. A caller who reads `GET` while a
session's composer holds unsent text, and quotes back `ComposerDigest` — the
field `Discard`'s own error message tells it to use, and the only digest `GET`
publishes for that specific condition — was refused by `/keys` with a 409
naming a mismatch it could never resolve: the value it was refused for
(`screenDigest(text)`) is not a value `GET` hands out in that state under any
name a caller could discover without reading it back out of the error message
itself. `/discard`, pointed at the correct digest, hit a second, unrelated
failure (the clear keystroke not registering) — named here as out of scope,
tracked separately, not folded into this decision.

The dispatch brief posed the resolution as a binary: shrink `/keys` to
composer scope everywhere (matching `discard`, matching the field name
callers are already told), or argue that screen scope is genuinely required
and make the field name and 409 text say so. Neither answer alone survives
contact with `/keys`'s two real call shapes.

## Decision

**Dual scope, selected per call by what the composer holds at read time, not
a single global switch:**

- **Composer holds unsent text → corroborate on `screenDigest(pending)`** (the
  composer's own text) — the exact value `GET` publishes as `ComposerDigest`,
  and the exact function `Discard` already checks. Every key `/keys` could
  deliver into this state is refused immediately afterward regardless of
  which key was requested — the existing "composer holds unsent text; clear
  it with discard, or send it" business refusal, unchanged — so composer-scope
  corroboration costs nothing in this branch: it only ever has to prove the
  caller saw the composer, never anything else on screen, before that refusal
  fires for the right reason instead of a 409 it could never clear.
- **Composer empty (or absent) → corroborate on `screenDigest(text)`** (the
  whole screen), unchanged from before this issue. This is the branch `/keys`
  exists for — a screen the classifier could not parse, navigated by raw
  position (arrow keys on a dialog with no composer at all). A composer-scope
  digest here would be `screenDigest("")` on every call, a constant regardless
  of what the dialog actually shows — corroborating nothing, which is the
  exact relaxation the reporter was explicit must not happen.

Both values are already published unconditionally-in-their-own-right by
`GET` today (`ScreenDigest` always; `ComposerDigest` exactly when
`WaitingOn == WaitingUnsentInput`) — no new field, no wire change. The 409 and
the missing-digest error now each name the specific field the caller should
have quoted for the state the driver is actually in
(`?expect=<composerDigest>` vs `?expect=<screenDigest>`), so a caller is never
left inferring the right field from a live error message, only from the `GET`
response it already has.

## Alternatives considered

**Shrink `/keys` to composer scope unconditionally**, as the brief's smaller
option suggested. Rejected: it makes corroboration a no-op for `/keys`'s
primary use — an empty-composer dialog navigated by position — because the
composer-text digest of `""` never changes no matter what the dialog says.
That is a real weakening of the safety property dressed as a fix for the
reported bug, and the brief is explicit that corroboration must not be
relaxed to solve this.

**Expose both digests and require the caller to pick.** This is what already
happens at the wire level (`GET` publishes both), and pushing the choice onto
every caller is the option the original bug report itself argued against —
"the same trap moved rather than removed." The driver knows which one is
meaningful for the state it is actually in; making every caller re-derive
that from `WaitingOn` before calling `/keys` is strictly worse than the
driver applying the same rule GET's own field-presence already encodes.

**Two separate endpoints** (one composer-scoped, one screen-scoped). Rejected
as unnecessary ceremony: the two scopes are never both live for the same call
— a composer holding text always refuses regardless of scope, so there is no
call where a caller genuinely needs to choose. One endpoint that reads the
same state it is about to act on decides correctly every time.

## Consequences

- A caller that reads `GET` and quotes back whichever digest field it found —
  `ComposerDigest` when present, `ScreenDigest` otherwise — now gets either a
  real business refusal or a real success from `/keys`, never a corroboration
  409 it has no way to resolve. This is the acceptance criterion #127 measured
  against, proved directly in `keys_test.go`
  (`TestKeysRefusesWhenTheComposerHoldsUnsentText`,
  `TestKeysNamesComposerDigestWhenComposerHoldsText`).
- A caller that quotes the *wrong* field for the current state (e.g. an old
  `ScreenDigest` from before the composer picked up text) is still refused —
  correctly, because the world genuinely changed — and the 409 now names the
  field it should have used instead.
- `/discard`'s second, independent defect (a corroborated clear that reports
  "the clear keystroke did not register" against the correct digest) is
  unresolved by this change and remains open on #127 as a distinct problem —
  fixing the digest mismatch does not by itself restore the recovery path
  when discard's own keystroke delivery is what fails.
