# 125 — hold the initial prompt indefinitely, bounded by the session's own lifetime; always report a live reason

## Context

A session created with a prompt can be not-ready-yet at delivery time —
parked on a startup dialog awaiting a keypress most commonly, slow startup
too. #86/#124 originally fixed the two failures this caused: the prompt being
discarded the moment any dialog appeared, and the delivery outcome staying
unresolved (in one field observation, indefinitely) instead of reaching a
terminal value. That fix (commit `7e19a6a`) waited through a blocking dialog
for a fixed 90s window, then gave up and resolved `unknown`.

#125's own newest comment replaced that design with two sharper requirements,
from the person who hit this live:

1. **Try as hard as possible to deliver**, rather than racing an arbitrary
   timer against an unknown human response time. A session parked on a dialog
   might be answered in ninety seconds or in ten minutes — this service has
   no way to predict which, and discarding the caller's entire instruction
   because a guessed number expired is the wrong trade, especially against a
   session that is very much still alive and waiting.
2. **Always have an answer to WHY the prompt has not landed yet, available
   DURING the wait** — not only once delivery is abandoned. A design that
   retries harder but stays silent trades one invisible failure (a session
   that quietly did nothing) for another (a service that quietly waits) — a
   human staring at either sees the same nothing.

Left explicitly to the implementer: whether "as hard as possible" is
unbounded, or bounded by something meaningful rather than an invented
duration — with the choice defended here.

## Decision

**Bound: the session's own lifetime, not a duration.** `settleNewSession`'s
poll loop no longer carries a `context.WithTimeout`. It keeps polling for as
long as the session exists, and stops only on one of three real events:

- the prompt is delivered (unchanged from `7e19a6a`);
- the driver's own pre-write guard refuses it outright (unchanged);
- the session itself is confirmed **gone** — absent from this driver's own
  enumeration, or present with its process already exited (tmux's own `dead`
  flag) — over `sessionGoneConfirmations` (2) consecutive polls, so a single
  listing race (already a documented, transient shape elsewhere in this file)
  is not mistaken for a closed session.

"The session's own lifetime" was the implementer's own suggestion in the
issue, and it fits this driver exactly: it already knows how to tell a live
session from a dead one (used everywhere else — `Close`, `List`, the classify
path), so no new liveness signal had to be invented, and the bound is
*real* — nothing here ever waits longer than the thing it is waiting on. A
prompt for a session a human is still actively answering a dialog on, ten
minutes in, is still worth delivering; a prompt for a session that no longer
exists never will be, and there is nothing left to try harder against.

**Live evidence: `notePromptPending`, distinct from `notePromptDelivered`.**
Every poll that finds the session still not ready computes a specific reason
— "still starting: the interface has not painted a composer yet", "parked on
a `folder-trust` dialog awaiting a keypress (\"…question…\")", "the composer
already holds other text" — and writes it onto the create record's existing
`PromptEvidence` field, *only when it changed* from the last write. Reading
the field is unchanged (`promptDeliveryFor`): while `PromptOutcome` is still
empty, `PromptEvidence` is the live diagnosis rather than a static sentence;
once `PromptOutcome` is set, `PromptEvidence` is the terminal receipt, exactly
as `7e19a6a` already had it. One field carries both eras; `notePromptPending`
refuses to write once `PromptOutcome` is set, so a stale poll racing behind
the terminal write can never blend a pending reason into a resolved one.

## Alternatives rejected

- **A longer fixed window (e.g. 10 or 30 minutes).** Still a guess, just a
  more generous one — the issue's own point is that no fixed number is
  right, because the answer depends on a human this service cannot see.
- **Truly unbounded, with no liveness check at all.** Rejected because it
  is not actually bounded by anything: a session's tmux pane can be killed
  by something other than this driver (a human, a crash, an unrelated
  cleanup), and a poll loop with no way to notice would leak forever,
  polling a target that no longer exists. The session's own lifetime is the
  bound *because* it is checked, not merely assumed.
- **A separate `PendingReason` field instead of reusing `PromptEvidence`.**
  Rejected as needless surface area: `PromptDelivery.Evidence` (the public
  wire type, `delivery.go`) is already documented as carrying prose in every
  state, pending included — a second field would either duplicate that or
  leave one of the two unpopulated depending on state, which is exactly the
  kind of drift-prone shape this repo's own `sessionFactsFor`/
  `pinOutcomeFor` comments warn against elsewhere.
- **Writing the live evidence on every poll, unconditionally.** Rejected on
  cost: a session parked on a human-answered dialog for ten minutes polls
  ~400 times at `promptPollInterval`; writing the state store that often for
  a value that has not changed is waste this fix does not need to introduce
  to satisfy the requirement, which is about a reader always finding a
  current answer, not about the write frequency.
- **Requiring only ONE missed poll before declaring a session gone.**
  Rejected because "a pane can vanish between listing and capture" is already
  a documented transient race in this file (`enumerate`'s own callers guard
  against it); one miss is not enough evidence to discard a real delivery.

## Consequences

- No more `promptDeliveryWindow`/`withPromptDeliveryWindow` — a fixed-window
  test seam is meaningless once there is no fixed window; tests that
  exercised the old timeout now exercise the session-gone bound directly
  (`internal/drivers/tmux/createrecord_test.go`'s "the session is confirmed
  gone before it was ever ready").
- A session genuinely parked on an unanswered dialog forever (nobody ever
  answers it, and nobody ever closes the session either) now holds a
  `settleNewSession` goroutine open for as long as that session exists,
  where the old design gave up after 90s. This is the trade the issue asked
  for explicitly — the cost is one goroutine and one poll loop per
  still-pending initial prompt, not unbounded resource growth, since it
  still ends the moment the session does.
- `counterInitialPromptSessionGone` is new, alongside the existing
  `counterInitialPromptRetried`/`counterInitialPromptStranded` — a nonzero
  rate says sessions are dying before their initial prompt lands, a
  different signal from an ordinary delivery strand and worth telling apart
  from it (same reasoning #104 gives for splitting confirm signals).
- Not addressed here: a `colab-fleetd` process restart while a
  `settleNewSession` goroutine is mid-poll still loses that goroutine — this
  was already true of the 90s-bounded design (a restart inside the window
  killed it identically) and remains a real gap in both, not a regression
  introduced by removing the timer. A durable record left pending across a
  restart is a plausible root cause for the very divergence #124/#125
  measured across two machines, and is worth its own issue rather than being
  folded into this one.
