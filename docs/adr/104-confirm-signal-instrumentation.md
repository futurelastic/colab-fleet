# 104 — instrument confirmSubmitted's signals instead of capturing them

## Context

`confirmSubmitted` treats two independent things as confirming evidence: the
composer reading fully empty, or this delivery's own attributed marker count
falling below what it was pasted at. The second signal was added after the
first shipped and turned out insufficient (residue on the composer line can
keep it non-empty forever). #104 asked two questions about that state:

- is the second signal actually still live, or could real traffic route
  through only the first, leaving the second dead code nobody noticed; and
- is `submitConfirmWindow` (4s) actually the right budget, or an inherited
  number nobody has re-derived against how long a real submit takes to show.

A reframe on #104 narrowed the deliverable: both are answerable without a
live capture, which an agent session cannot take (driving the multiplexer
directly is refused by design) — the branch question by counting which
signal decided a given call, the window question (partly) by recording how
long deciding took.

## Decision

Count at the two return points inside `confirmSubmitted` itself, not at its
callers. There are two callers (an ordinary `Send`, and the
`resumeIfStranded` path) and duplicating the bookkeeping at each would risk
the exact kind of silent drift #104 is trying to make observable in the
first place.

Record latency as five **exclusive buckets**
(`<250ms`/`<500ms`/`<1s`/`<2s`/`<4s`), not a sum+count average. This repo
takes no third-party dependency, so there is no histogram library to reach
for — and a mean would hide the one thing this question is actually about: a
fat tail sitting near the 4s window is a reason to reconsider the budget even
when the average looks comfortable. The top bucket is capped at
`submitConfirmWindow` itself; a call that reaches the window without either
signal firing increments `submit_confirm.timeout` instead, never a "latency"
of unbounded length.

All of it rides the existing `counterSet` registry (`counters.go`, built for
#44) rather than a new type — a name and a count is already the established
shape for "the service noticing something about itself", and `Counters()`
already exposes the whole map through `driver.CounterReporter` with no new
wiring.

## Alternatives rejected

- **A histogram library.** Ruled out immediately by `stack:` in
  `.github/project.yml` — standard library only.
- **A running mean/stddev.** Cheaper to store than five buckets, but answers
  a different question than the one #104 asked; a comfortable mean is
  compatible with a tail that regularly eats the whole window.
- **Counting at the call sites instead of inside `confirmSubmitted`.** Rejected
  because it is the two-callers problem stated above: the same drift risk
  #104 is worried about for the confirming signals themselves.

## Consequences

- If `submit_confirm.by_marker_cleared` stays at zero across real traffic,
  that is the dead branch #104 suspected — found from a counter, not a
  capture, the same idiom #116 used for `identity.contested`'s sibling
  question.
- `submitConfirmWindow`'s size is still not re-derived here — that is
  deliberately left as a follow-up once these buckets have real traffic
  behind them. This ADR documents the instrument, not a new budget.
- The adversarial capture #104 originally asked about (forcing a swallowed
  keystroke and watching the window) remains open, and remains correctly
  blocked from an agent session.
