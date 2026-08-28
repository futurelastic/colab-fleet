# ADR: `resumeIfStranded`/`replaceIfStranded` clear an unrecorded composer too, instead of dead-ending at §2.4

**Issue:** #135
**Status:** decided

## Context

`send` refuses to append to a busy composer (§2.4). `resumeIfStranded`
completes a delivery the driver's own record shows it placed there;
`replaceIfStranded` (#112) clears such a record and delivers different text
in its place. Both require a **record** — the driver's own memory of having
put something in that composer — before either does anything but refuse.

When no record exists at all (the record never existed, or outlived
`strandedRetention`, 30 minutes), both flags previously fell straight through
to the original, unconditional refusal, regardless of either flag being set.
The only way out was three manual round trips: read the session for
`composerDigest`, call `discard?expect=<digest>`, then resend. Filed from a
coordinator session stuck on exactly this for close to an hour — long enough
to outlive `strandedRetention` mid-retry, one plausible route into the
no-record state the field report actually hit.

## Decision

**Fold `discard`'s own corroboration into `send` for these two opt-in flags,
even with no record.** When the composer holds content this driver cannot
attribute to a prior delivery of its own, and either flag is set, the driver
now reads the composer's own current content, clears it using that same read
as the proof nothing changed between "look" and "clear" (the identical
property `discard`'s `?expect=` digest enforces — not a weaker one), then
delivers THIS call's text in its place. Both flags converge on the same
action here, because there is no recorded "old delivery" for either to
distinguish resuming from replacing. A bare `send` with **neither** flag set
is untouched: it still refuses, with the original wording, unconditionally.

The `#87` futile-clear guard and the "moved but did not fully empty" honest
failure both apply exactly as they do for the record-backed path
(`tryReplaceStranded`) — reused verbatim (`futileClearAttempts`,
`clearComposer`), not reimplemented. No new stranded record is created on a
failed or partial clear here: this driver never delivered anything of its
own into an unrecorded composer, so there is nothing of "ours" left to
remember on a failure.

### Why this is not a loosening of §2.4, only a relocation

`discard`'s safety property was never "this driver knows whose text this
is" — Discard clears record-blind text unconditionally once its digest
check passes; it has never distinguished a human's draft from anything
else. Its actual property is narrower and structural: **the caller must
have looked at the composer before destroying it**, proven by quoting back
a digest from that look. Folding the read into `send` itself does not
remove that proof — it changes who performs the "look" and when: the driver
performs it inline, immediately before clearing, rather than requiring the
caller to have performed it via a separate `GET` some round trip earlier.
The window for "somebody typed here since I looked" that the digest guards
against is now sub-call rather than sub-request, which is a *smaller*
race, not a discarded one.

What actually changes is a different thing: **who is authorized to decide
"destroy whatever is here."** Before this issue, only a caller that had
separately read the composer and could quote its digest could authorize a
clear. After it, setting `resumeIfStranded`/`replaceIfStranded` on the
`send` call itself is that authorization — the caller is declaring "deliver
this regardless of what's stuck in the composer" as part of the same
request, rather than proving they inspected it first. That is a real
widening of what the flags are allowed to do, and it is the thing this ADR
is actually deciding, not the digest mechanics.

## Alternatives considered

**Leave the no-record case refusing unconditionally, and only improve the
refusal's wording (Ask 1 alone, without Ask 2).** Rejected as the primary
fix — it would have left the field incident's actual failure mode
unresolved: a caller that already knows it wants this text delivered
regardless of composer contents still pays three round trips every time
`strandedRetention` lapses mid-retry, which is exactly when a caller is
already retrying because something is stuck. Ask 1 (naming the exact
`discard?expect=<digest>` call in every refusal whose remedy is `discard`)
is implemented alongside this, not instead of it — it's the honest fallback
`send` still points a caller at when a clear-and-deliver attempt from this
same door itself fails to fully empty the composer.

**Gate the no-record door on some detectable "this is probably safe" signal**
(session age, absence of recent human activity, restart-correlation).
Rejected: every such signal is an inference from screen contents or timing,
which is exactly the category of evidence F49/§2.4 already distrust for this
decision (a collapsed multi-line paste cannot be compared byte for byte; a
composer redrawing a placeholder looks the same whether a human or a prior
delivery put it there). The one signal that IS trustworthy — the caller's
own opt-in on this call — is what this decision uses instead of inventing a
weaker, guessed one.

**Require a fresh, separate `GET` immediately before the internal clear, to
mirror the external `discard` flow exactly.** Rejected as pure overhead: the
composer read `send` already performs earlier in the same call (to decide
whether the busy-composer branch is even reached) is at least as fresh as a
second round trip would be, and demanding a second read of the same pane
microseconds apart proves nothing an extra network hop wouldn't already have
proven for free.

## Consequences

- A **pre-existing regression test**
  (`TestResumeSubmitsOnlyWhatThisDriverStranded/resume_never_submits_text_we_did_not_place`)
  asserted the old behavior directly — composer holds text with no relation
  to this driver, `resumeIfStranded` set, expected `refused`. Rewritten
  (`.../resume_clears_foreign_text_and_submits_only_what_this_driver_placed`)
  to assert the new, narrower property that actually still holds: the
  **foreign** text is never the thing submitted, only ever the caller's own
  new text on this same call. This is the one deliberate, intentional test
  behavior change in #135 — flagged here rather than left to be discovered
  by a future `git blame`.
- A caller that sets `resumeIfStranded` or `replaceIfStranded` against a
  session it does not actually control (or against a composer it has not
  actually decided to overwrite) can now destroy a genuine human draft in
  one call, with no separate look-first step enforced by the wire protocol.
  This was already possible in three calls before this issue (`read` →
  `discard?expect=` → `resend`); it is now possible in one. Callers that
  want the old, slower, look-first discipline still get it by simply never
  setting either flag and using `discard` directly.
- `docs/spec/api-http.md`, `docs/spec/session-abstraction.md`,
  `docs/api.md`, `docs/client-guide.md`, and `internal/driver/driver.go`'s
  `SendOptions` doc comments are updated alongside the code — the normative
  spec's own "only" wording for `resumeIfStranded` was a genuine
  overstatement of what is now true and needed correcting, not merely
  extending.
