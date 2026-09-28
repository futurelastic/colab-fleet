# A second `list-panes` (or hand-rolled `capture-pane`) call inside `List` breaks its constant-spawn contract

**Issue:** #235

## What happened

Adding exit-capture support (a session whose own process exited needs its
status and last screen recorded before the multiplexer drops it) was first
implemented as a standalone `reapExited(ctx)`, called at the top of `List`,
issuing its own `list-panes -f pane_dead` invocation. It also captured a
dying pane's screen with a hand-rolled `capture-pane` call.

`go test ./...` immediately caught both:

- `TestListCostsConstantSpawns` — "List made 3 subprocess calls for 42
  sessions; must be constant (2), not proportional": the extra `list-panes`
  call is a THIRD invocation on every single `List`, forever, to cover an
  outcome (a dead pane) that is rare by construction.
- `TestListNoServerIsCompleteAndEmpty` — "want 1 subprocess, got 2": the same
  extra call fires even when there is no multiplexer server to talk to.
- `TestListToleratesPaneVanishingMidCapture` — failed for an unrelated-looking
  reason (a session reported missing that should have been present). The real
  cause: several tests mock `d.run` by call-count/sequence, and an extra call
  inserted before `enumerate`'s own shifts every later call's index, so the
  mock hands back the wrong canned response to the wrong question.
- `TestClassifierCapturesAreBuiltInOnePlace` — a source-level scan that fails
  on ANY function containing the literal `"capture-pane"` that is not on its
  allow-list (`classifyCaptureArgs`, the one definition of the classifier's
  capture shape). The hand-rolled call in `saveExitScreen` tripped it
  immediately.

## The rule

`driver.Driver.List` has a **constant-spawn contract**: cost must not scale
with session count, and — this is the part a new feature can violate without
touching the chunking logic at all — must not scale with the number of
*calls* either. Anything added to `List` has to reuse what `enumerate`
already fetched in its ONE batched invocation, never add a second listing or
a second capture, however small. In this case: `enumerate`'s format string
gained one more `#{…}` field (`pane_dead_status`) in the SAME call, and the
per-pane screen a dying session needs was read from `captures[r.paneID]` —
the batched capture-pane result `enumerate` already fetched for every row,
dead or not — rather than captured again by hand.

Net effect: the ordinary case (nothing dead) costs zero extra invocations,
and per-pane screen capture never needed a second `"capture-pane"` literal
that would have needed its own allow-list entry.

## What is still not covered

Anything genuinely rare enough to justify its own extra invocation (this
issue's own `killCorroborated` call for a session actually confirmed dead) is
fine — the contract is about the ORDINARY path, not about forbidding every
conditional extra call ever. The trap is specifically adding an invocation
that runs on EVERY `List`, dead pane or not.
