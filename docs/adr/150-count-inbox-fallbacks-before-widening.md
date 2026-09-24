# 150 — count the inbox path's fallbacks before widening the attestation rule

## Context

#148 made the inbox send path attest the target's permission-mode class, and
refuse to attest — falling back to the terminal path — whenever the body holds
any of sixteen opening-bracket lookalike runes. That refusal is provably safe:
without a lookalike the receiver's escaper is the identity function, so its
byte-for-byte rebuild check always passes. It is also broader than the receiver
needs, because the escaper only fires on a lookalike that begins something
resembling the closing tag. #150 asks whether to implement that narrower
trigger, and states its own prerequisite: measure the refusal rate first.

There was nothing to measure from. This service keeps no log of send texts,
and `sendViaInbox` returns the same `ok=false` for every fallback — no
resolver answer, no class, an unattestable body, a failed dial, a failed
write — so a refused attestation left no trace of why.

A second fact makes a naive counter misleading. `Attest` checks the class
before the body. While an index still omits classes, every send refuses on the
class first, and a counter of body refusals alone reads near zero — not because
bodies are clean, but because the question is never reached.

## Decision

Count every exit of the inbox send path, on the existing `counterSet` registry
(`internal/drivers/tmux/counters.go`), which `GET /v1/health` already exposes
under `counters`, keyed by runtime:

| Counter | Incremented when |
|---|---|
| `inbox.attempted` | a resolver is configured, so the inbox path is tried |
| `inbox.fallback_identity_unresolved` | the target's process identity cannot be resolved |
| `inbox.error_identity_resolve` | resolving identity fails for any other reason (returned as an error) |
| `inbox.fallback_resolver_error` | the resolver itself errors |
| `inbox.fallback_resolver_declined` | the resolver has no inbox for this target |
| `inbox.refused_identity_unverified` | identity verification fails just before the write (final, no fallback) |
| `inbox.fallback_no_mode_class` | attestation refused: no valid class |
| `inbox.fallback_body_unattestable` | attestation refused: class valid, body holds a lookalike |
| `inbox.fallback_dial_failed` | the dial fails |
| `inbox.fallback_write_failed` | the write fails |
| `inbox.written` | the envelope was written (not "delivered": nothing observes delivery, #144) |
| `inbox.attest_checked` | a send reached attestation |
| `inbox.attest_body_lookalike` | a send reached attestation and its body holds a lookalike, whatever its class |

`inbox.attempted` equals the sum of the ten exit counters beneath it, and
`inbox.attest_checked` equals the sum of the last five exits. The number #150's
decision reads is

    r = inbox.attest_body_lookalike / inbox.attest_checked

summed over every machine, because it is independent of the class rollout.
`inbox.fallback_body_unattestable` is today's actual cost, which the class
rollout will grow toward `inbox.attest_body_lookalike`.

The body rule moves into one exported predicate, `inboxclient.BodyAttestable`,
which `Attest` itself calls. A test pins the contract
`Attest ok == class.Valid() && BodyAttestable(body)`, which is what lets the
caller name the refusal's reason. A widened rule later changes the predicate
and the counter follows it for free.

## Alternatives rejected

- **Give `Attest` a reason return value.** Touches every caller and test, and
  puts a diagnostic into the function the round-trip proof is written about.
- **Re-run the rune check at the call site.** A second copy of the rule, free
  to drift from the one that actually refuses.
- **Count only the attestation refusals.** A low `attest_checked` could then not
  be told apart from a resolver that declines or an identity that fails.
- **Count the call with no resolver configured.** A machine with no inbox at all
  would publish zeros for a measurement it never took.
- **Log send texts to classify refusals exactly.** This service deliberately
  keeps no record of message content.

## Consequences

- `r` is an upper bound on what widening could recover: it counts every body the
  current rule refuses, not only those the narrower trigger would accept. That
  is sufficient for a close decision; a widen decision is a separate plan.
- The counters live in memory and reset on restart. A reading covers the window
  since that machine's `startedAt`, and a restart inside a measurement window
  discards the sample.
- Nothing here changes which sends are attested, what is written, or the order
  of the checks.

## Addendum (#184): two more exits, and what "written" became

The exit list above is the one #150 shipped. `docs/adr/184-route-by-sender.md`
changed it in three ways, and the invariants were kept:

- **`inbox.fallback_no_transcript`** — a new exit, before the dial: the session's
  transcript could not be located, so a delivery could not be confirmed.
- **`inbox.unknown_partial_write`** — a new exit: a write that put *some* bytes on
  the socket and failed. It is never a fallback.
- **`inbox.fallback_write_failed`** now means a write that put **no** byte on the
  connection. It is the only write failure that falls back.
- **`inbox.written`** is a complete write and is split, as sub-counters and not
  exits, into `inbox.confirmed` (the receiver's transcript recorded the message;
  itself split into `inbox.confirmed_by_envelope` and
  `inbox.confirmed_by_origin_body`) and `inbox.unconfirmed`. `written = confirmed
  + unconfirmed`.

`inbox.attempted` therefore equals the sum of **twelve** exits, exactly as it
equalled ten. `inbox.attest_checked` no longer has the tidy identity it had: it is
the sum of the last seven exits — no_mode_class, body_unattestable,
no_transcript, dial_failed, write_failed, unknown_partial_write and written —
**plus** the sends the *second* identity verification refused, because
attestation now runs between the two verifications. Those are counted in
`inbox.refused_identity_unverified` together with the first verification's
refusals, which never reached attestation, and the two cannot be told apart. The
number #150's decision reads, `attest_body_lookalike / attest_checked`, is not
affected: both are counted at the same point.

The route counters (`route.*`) are a separate family, one count per `Send`.

