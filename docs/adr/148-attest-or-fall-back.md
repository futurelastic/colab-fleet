# 148 — assert the receiver's permission-mode class, or do not claim the inbox path

## Context

Once inbox delivery became the default, `/input` began reporting success for
messages the receiving runtime never handed to its model.

The receiving runtime's inbound policy defaults to **mode parity**. Two facts
decide everything below:

- a message whose sender asserts a permission-mode class is accepted when that
  class equals the receiver's own, and **held** when it does not;
- a message whose sender asserts **no** class is accepted only when the receiver
  still prompts for permission, and **held** when the receiver runs with
  permission prompts bypassed.

Held is not queued. It means parked for a human's approval, never given to the
model, then dropped when the receiver's own hold deadline passes. The turn is
gone.

This service asserted no class, and an unattended session is normally the
bypassed kind. So every message to one was held. Measured over three days on a
live fleet after the inbox became the default: **206 sends reported delivered
that were not**, against 62 that landed, four sessions affected, one of them
unreachable for three and a half days. Two clean days at a 100% delivery rate
immediately precede the cliff.

Nothing in the receipt said so. This path advertises that it does not confirm
delivery, so a caller had no way to tell *delivered* from *held-then-dropped* —
and a caller told "delivered" does not retry, while a caller told nothing
retries forever. Both happened.

### Why the obvious fix is not available

The obvious fix is to report the hold: the vocabulary already exists, unused,
and the mapping is already one-for-one. It cannot be done. The runtime's status
frame is routed to a reply address that must be a socket bound inside the
receiver's own namespace with matching ownership. A plain external sender has
not bound one, so nothing comes back — confirming rather than contradicting the
earlier measurement that a fully successful delivery returns zero bytes over a
twelve-second window. That gap is its own issue and stays open; this ADR does
not close it and no code here produces a held outcome.

## Decision

**Assert the class the target runs in, and where it cannot be asserted, do not
use this path at all.**

### The class is per target, and mirrored — never a constant

Parity is **symmetric**. Asserting one fixed class is not a fix; it relocates
the failure, because a mismatch is held exactly as firmly as a missing
assertion. Assert "bypassed" everywhere and every prompting receiver begins
holding messages it accepts today.

So the class is a fact about each target that this service must be **told**. It
travels on the resolved address beside the socket and the token, supplied by the
same machine-local, operator-owned index those already come from. This service
is a conduit; it has no permission mode of its own, and mirroring the receiver
is the only assertion that is both honest and always accepted.

### Refuse the envelope rather than approximate it

The receiver does not merely parse the envelope carrying the class. It parses
it, **rebuilds it from what it parsed, and compares the rebuild to what arrived
byte for byte**. Any difference and the envelope is discarded wholesale: the
class is lost, the raw text reaches the model unwrapped, and the message is held
exactly as if nothing had been asserted.

A wrapper that is usually right is therefore worse than none — it fails in the
silent direction this whole issue is about. The rebuild's only transform is an
escaper, and that escaper cannot fire on a body containing no opening-bracket
lookalike, because its pattern must match one as its first character. Hence the
rule: **a body carrying none of those runes is provably byte-identical after the
rebuild; a body carrying any of them is refused outright.**

That is deliberately generous. Escaping such a body correctly is possible and is
not attempted here: the escaper's own pattern needs lookahead, which this
language's regexp engine does not have, and a hand-rolled scanner that is subtly
wrong reintroduces the silent failure. Refusing is provable; escaping would be
merely tested.

### An unattestable send falls back, and that is the whole fix

When the class is absent, unrecognised, or the text cannot be wrapped losslessly,
the inbox path reports itself unavailable and the terminal path carries the
message. This is ADR 119's own rule — *the honest response to half a capability
is the same as to none of it* — applied to a piece of the capability nobody knew
was part of it.

The fallback is the behaviour that predates the inbox default, and which the
same data records running at a 100% delivery rate. The cost is a slower
delivery. Reporting `delivered` without attesting is the bug; falling back is
the fix.

### An unrecognised class is an error; an absent one is not

Absent is the ordinary day-one state — no index writer emits the field yet — so
it resolves normally and is merely **counted**. Unrecognised is rejected, because
a wrong class is held as firmly as a missing one and would be held while this
service reported success. Guessing is the failure mode, not the fallback.

### Ordering

The attestation sits **after** the identity verification and **before** the
dial. Verification failing is a final refusal with no terminal fallback (ADR 119
explains why falling back there defeats the check). Checking attestability first
would have converted that refusal into a fallback for any send that merely
happened to be unattestable — switching off a security gate through an unrelated
door. Nothing is dialled for a send that cannot be attested.

## Consequences

- **The trust posture changes, narrowly, and on purpose.** For this service's own
  traffic, asserting the receiver's class has the same effect as an operator
  setting that receiver's inbound policy to accept — the workaround this
  replaces. It is strictly narrower: the workaround accepts unattested messages
  from *any* local process, whereas this asserts a class for one sender's
  traffic and leaves everything else held as before. That narrowing is the whole
  argument for doing it here rather than telling every operator to loosen a
  receiver-side setting, and it is a trust decision, not a bug fix.
- **Day one, nothing uses the inbox.** Until an index writer emits the field,
  every send falls back while the capability still advertises itself as wired.
  Intended, and it will read as a regression to anyone checking only that flag —
  hence the counter and the deployment note.
- **The receiver's consultation of the asserted class sits behind a
  remotely-controlled feature gate.** If it is ever off, holds resume with no
  sender-visible signal. Unmitigable from here, and the reason the terminal
  fallback must stay healthy rather than be treated as legacy.
- **The envelope changes how the message renders on the receiving side** — a
  labelled peer message rather than a plain turn. No test can assert that; it
  wants one human look after a deployment.
- **The transcribed rune set and envelope shape are facts about one version of
  one runtime.** If the envelope changes, sends stop being honoured and there is
  no signal, because there is no reply channel. `docs/gotchas.d` records how to
  re-derive them.

## Alternatives rejected

- **Report `held` as an outcome.** Not achievable — no reply address, see above.
  The vocabulary stays in the closed set, unproduced, rather than being faked by
  guessing at a clean write.
- **Assert a single compiled-in class.** Rejected: parity is symmetric, so this
  moves the failure to prompting receivers instead of removing it.
- **Escape the body faithfully instead of refusing it.** Rejected for now: the
  escaper needs lookahead this language's engine lacks, and a subtly wrong
  scanner fails silently in the same direction as the original bug. Refusal is
  provable; a follow-up may do better with the round-trip check as its oracle.
- **Keep using the inbox unattested and document the hazard.** Rejected: that is
  the bug, restated as a caveat nobody reads at the call site.
- **Relax the clipped-composer refusal so the cascade can be cleared.** Out of
  scope and its own design. That refusal is correct; what was missing was an
  operator-side lever, which is the capture window this branch also exposes.
