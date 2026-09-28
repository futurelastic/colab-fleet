# 235 — capture a session's own exit before removing it, inside List's existing read

**Issue:** #235 (blocked by #234, which landed first)

## Context

A session whose runtime exits on its own within seconds of `Create`, before
its first turn, left no evidence: the pane and the multiplexer session vanish
together the instant the process dies, the runtime never got far enough to
write a transcript, and the service's own history record only ever gets to
say `closedBy: absent` — "this id was missing from a later listing", nothing
about why. Measured 2026-09-28: a session absent 15 seconds after create, with
no transcript and nothing that would have explained it except the pane's own
last screen — already gone by the time anyone thought to look.

Lucy's ruling on the issue: the exit status goes on the history record and is
readable through `GET /v1/sessions/closed`; the last N lines of the pane are
written to a capped, private file on that machine, and the record carries only
the file's path — this service serves no screen content over HTTP today, and
this issue does not change that.

## Decision

### 1. `remain-on-exit`, set per session, not globally

`Create` now runs `set-option -t <name> remain-on-exit on` in a **separate**
tmux invocation right after `new-session` succeeds. `remain-on-exit` is a
WINDOW option; targeting the session by name scopes it to that session's own
(only) window. Never `-g` — this driver's multiplexer server can host other
tools' sessions too (the compat harness's own `-g` usage is a different,
isolated server, and is not a precedent for the production one).

Chaining `new-session ... ; set-option ... remain-on-exit on` into ONE
invocation was considered and rejected: tmux fails the whole client call if
ANY chained command fails, so a `set-option` hiccup would make an
already-successfully-created session get reported as a failed create,
abandoning the idempotency reservation over a session that now exists
orphaned — a worse failure than the race two separate calls leave open. Two
calls leave a real but narrow gap: a process that exits in the few
milliseconds between them is not caught. Accepted, because the issue's own
measurement is about processes that survive seconds, not milliseconds.

### 2. Detection reuses `List`'s own enumeration — no second listing, no second capture

The obvious shape — a dedicated `reapExited` that runs its own `list-panes -f
pane_dead` at the top of every `List` — was implemented first and rejected:
`driver.Driver.List` has a **constant-spawn contract**
(`TestListCostsConstantSpawns`, `TestListNoServerIsCompleteAndEmpty`), and an
extra top-level invocation broke it immediately — every `List` call now cost
one more subprocess spawn, on every machine, forever, to cover an outcome
that is rare by construction.

The fix: `enumerate`'s existing batched listing already reads `pane_dead`, and
now also reads `pane_dead_status` (`paneRow.deadStatus`) in the same
invocation — one more `#{…}` field, not one more call. `reapDeadRows(ctx,
rows, captures)` runs once, inside `List`, right after `enumerate` returns,
using the screen `captures[r.paneID]` that the SAME enumeration's batched
capture-pane already fetched for every row, dead or not. A dying pane's
"last screen" is therefore never a hand-rolled second `capture-pane` call
either — which also keeps `saveExitScreen` off `TestClassifierCapturesAreBuiltInOnePlace`'s
allow-list, a source-level gate on every literal `"capture-pane"` in this
package.

Net cost in the ordinary case (nothing dead): **zero** extra invocations.
Killing a genuinely dead session (`killCorroborated`, the same path `Close`
uses) is the one non-constant cost, and it is meant to be — it only runs when
something is actually dead, and the alternative is the pane lingering under
`remain-on-exit` forever.

### 3. The driver queues; the service drains and tombstones

`DrainExits() []driver.CapturedExit` implements a new optional capability,
`driver.ExitReporter` — mirroring `driver.ClosedLister`'s shape for the
opposite direction (a LOCAL driver reporting outward, not a peer driver
answering a forwarded read). `Service.ListSessions` drains it right after
calling a local driver's `List`, on every call, filtered or not: a captured
exit is a positive, exact fact the driver has already acted on (the session
is gone from the runtime, permanently), unlike absence, which only a
**complete** read may conclude (§5.7).

`historyStore.observe` takes the drained exits as a new parameter and settles
each one — deleting it from `seen` and tombstoning it with the new
`fleet.ClosedByExit` — **before** the ordinary present/absent bookkeeping ever
sees the id. Without that ordering, a session the driver already removed
(and which therefore cannot appear in `items` this round) would fall through
to the generic `absent` tombstone the complete-listing scan writes for
everything else it cannot explain — the exact case an `ExitReporter` exists
to do better than.

## Alternatives considered

**A `pane-died` tmux hook instead of polling `pane_dead` on every `List`.**
The issue's own text offers this as an alternative mechanism. Rejected for
this pass: a hook needs somewhere to run a command from, which on this
driver's bare-exec-capable, dependency-free posture means either shelling out
to this same binary (re-deriving most of `reapDeadRows` anyway, invoked by
tmux instead of by `List`) or standing up a control-mode listener
permanently for every session rather than on demand (`subscribe.go`'s own
architecture deliberately opens content clients only when something
subscribes, for descriptor-budget reasons documented there). Piggybacking on
`List`'s existing poll cadence costs nothing new when nothing is dead and
reuses infrastructure this driver already depends on. Revisit if a caller
needs exit capture to be faster than "the next time something lists" — today
nothing does.

**Serve the captured screen over the API, gated by a flag.** Rejected by
Lucy's ruling directly: "The service serves no screen content over HTTP
today, and this issue does not change that." Pane text can hold anything the
runtime printed; putting it on the API is a new data-exposure surface that is
a maintainer's call, not a triage one.

## Consequences

- New wire field: `ClosedSession.exit` (`fleet.SessionExit`: `status`, `at`,
  `screenPath`), present only for `closedBy: "exit"`. Documented in
  `docs/api.md` and `docs/spec/api-http.md`.
- `paneRow` gains `deadStatus`; `enumerate`'s format string and `parseRows`'
  field count both moved from 7 to 8. The one fake multiplexer test fixture
  (`fakeSession`/`fakeMux.exec`'s `list-panes` case, `tmux_test.go`) was
  updated to match — no other test builds a raw `list-panes` row by hand.
- `go test -race ./...` and, gated by `FLEET_TMUX_INTEGRATION=1`, a new live
  test (`TestLiveExitIsCapturedBeforeThePaneIsGone`) against a real private
  multiplexer server: a session whose command exits non-zero after printing a
  line is captured with that status and that line (read back from
  `screenPath`), and no leftover multiplexer session survives it.
- `killCorroborated`'s existing per-session cleanup (`d.observed`,
  `d.stranded`, `d.delivered`, `d.mods.closeLane`) applies unchanged to a
  reaped session — it is not special-cased.

## Reopen when

A caller needs exit capture faster than the next `List` naturally provides —
that is the trigger to revisit the `pane-died` hook alternative above.
