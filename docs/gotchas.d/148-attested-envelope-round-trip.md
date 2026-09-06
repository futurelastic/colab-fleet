# A delivery path with no reply channel may only report what it can prove

**Issue:** #148 (found after inbox delivery became the default and a fleet's
sends began succeeding on paper while sessions sat untouched for days)

## What happened

The receiving runtime's inbound policy defaults to **mode parity**: it accepts a
message whose sender asserts a permission-mode class matching its own, and
**holds** everything else — including every message from a sender that asserts no
class at all, whenever the receiver runs with permission prompts bypassed.

Held is not queued. The message is parked for a human, never handed to the model,
and dropped when the receiver's hold deadline passes.

This service asserted nothing. An unattended session is normally the bypassed
kind. So every send to one was held, and every send was reported `delivered`,
because this path advertises that it does not confirm delivery and there was
nothing to confirm against. Three days of data: **206 sends reported delivered
that were not**, 62 that landed, one session unreachable for three and a half
days.

The failure also compounds. Each held message paints a multi-line notice in the
terminal; the notices accumulate; once they push the composer past the capture
window the driver can no longer read it, correctly refuses to act on what it
cannot see, and the documented escape — wait for the composer to shrink back —
never arrives on an unattended session with a retrying caller. Holds blind the
driver, and a blinded driver cannot clear the holds.

## The rule going forward

**A delivery path that cannot read a receipt may report only what it can prove,
and must decline the path entirely when it cannot prove enough.**

Concretely, three habits this cost us:

1. **A capability is not partial.** If one fact the path needs is missing — here,
   which class the target runs in — the path is unavailable, not degraded. Using
   it anyway and reporting success is the bug. Falling back to the slower path is
   the fix.
2. **Symmetric rules cannot be satisfied by a constant.** Parity accepts a
   *matching* class, so asserting one fixed class relocates the failure rather
   than removing it. Anything mirrored must be supplied per target, never
   compiled in.
3. **Where a peer validates by rebuilding and comparing bytes, be provably
   correct or refuse.** The receiver parses the envelope, rebuilds it, and
   compares byte for byte; any difference discards the envelope silently. A
   wrapper that is *usually* right fails in the silent direction. We refuse any
   body carrying a rune that could make the rebuild differ, rather than escape it
   correctly — the escaper needs lookahead this language's regexp engine lacks,
   and a subtly wrong scanner would fail exactly the way the original bug did.

## Re-deriving this if it stops working

The envelope shape, the class vocabulary, and the rune set are facts about **one
version of one runtime**, transcribed into code — not a standard, and nothing
notifies us if they change. Because there is no reply channel, a changed envelope
does not raise an error: sends simply stop being honoured and messages are held
again, silently, exactly as before this fix.

So if delivery regresses with no error anywhere, **suspect the envelope first.**
Re-derive it by reading the receiving runtime's own inbound-policy and envelope
code on a machine that has one; the working notes that never leave the machine
say where to look. Check, in order:

- the attribute **order** — the receiver's rebuild is byte-comparing, so order is
  load-bearing, not cosmetic;
- the **class vocabulary** — a closed two-value set, validated on both sides;
- the **rune set** the escaper triggers on, which is what makes the round-trip
  provable;
- whether the receiver still consults the asserted class at all — that
  consultation sits behind a remotely-controlled feature gate, and if it is off
  the correct assertion is ignored and holds resume.

The unit tests pin the exact bytes and re-parse them with a transcription of the
receiver's own grammar. If the runtime changes, those tests keep passing while
reality diverges — they prove we build what we *believe* the receiver wants, not
what it currently wants. That is the limit of what is testable without a reply
channel, and it is why this file exists.
