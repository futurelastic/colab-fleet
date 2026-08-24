# ADR: `discard` cannot be made atomic; fix liveness instead

**Issue:** #87
**Status:** decided

## Context

`discard` clears unsent composer text by repeatedly sending a line-kill
keystroke and re-capturing the pane to verify progress, bounded by a short
window. A timed-out pass can leave the composer holding neither the
original text nor nothing — a partially-cleared, "damaged" state — and a
caller that retried exactly as a prior refusal's own message told it to
could see zero further progress across several consecutive calls.

## Decision

**Do not attempt to make `discard` atomic** (full clear or full rollback to
the originally-verified text). Fix the liveness problem instead: bound how
long a single pass keeps pressing once it has evidence of a stall, and
remember — in memory, per session, corroborated on id + cwd + residue
digest — that a specific residue has already survived one full exhausted
pass, so a repeat call against the identical residue is refused before
pressing again rather than repeating a promise ("safe to retry") that the
prior pass already disproved.

## Alternatives considered

**Rollback via re-paste.** On a timed-out, partially-cleared pass, compute
the delta removed and re-paste it back (the driver already has a
paste-buffer primitive used for delivering text) to restore the
originally-verified digest, turning "damaged" into "safely restored, retry
away." Rejected: the driver never has the caller's original bytes to
restore. The text it reads back for verification purposes is a *lossy*
reconstruction — wrapped lines get joined and trimmed, and a large paste
collapses into an opaque summary placeholder rather than the text it
represents — so a "restore" would insert a mangled reconstruction, or in
the collapsed-paste case, insert the placeholder's own label in place of
the content it stood for. The existing paste-delivery confirmation logic
also isn't reusable as rollback confirmation: it is built to tell a fresh
delivery apart from residue already sitting there, which is exactly the
state a rollback would be operating against.

**A stronger/alternate keystroke escalation** (a different key sequence
tried when the default one stalls). Rejected for lack of any way to verify
a specific sequence helps against the actual terminal this drives, and
because the one escalation candidate available on this substrate had
already been tried and measured not to help, per this code's own
pre-existing history.

## Consequences

- A composer that reaches a genuinely damaged state can still require a
  human, or closing the session — this is unchanged and was always true.
- What changes: the driver no longer manufactures *more* damage chasing a
  full window once it has good evidence a pass has stalled, and it no
  longer tells a caller a repeat of an already-proven-futile action is
  worth doing.
