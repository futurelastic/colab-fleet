# "Writes stopped landing after a restart, reads stayed correct" — what to rule out first

**Issue:** #124 (a pre-existing tmux session's composer stopped responding to
`send-keys`/Discard after this service's own process restarted, while
`capture-pane`/`list-panes` kept reading it correctly)

## What this is NOT, and why — checked so the next investigator doesn't re-derive it

A report shaped "X worked before a restart of THIS SERVICE and stopped
after" invites the assumption that the service held some connection or cache
that the restart dropped. For this driver, specifically, that assumption
does not hold, because of how it talks to the multiplexer:

- **No held connection to lose.** Every tmux call (`enumerate`, `send-keys`,
  `capture-pane`, …) is a fresh subprocess against the OS-level tmux server
  via `d.run(ctx, d.bin, ...)`. The tmux server is a separate, long-running
  process that this service's own restart does not touch — there is nothing
  "held" to re-establish.
- **No cached pane id.** `paneID` is re-queried fresh from `tmux list-panes`
  on every single call (`enumerate`) — never persisted or memoised across
  calls, so a restart cannot leave a stale one behind.
- **Not a dead pane.** `Send`/`Respond` already refuse on `target.dead`, and
  `classify.go`'s `alive`-gated branch reports a terminal `StatusDead`
  rather than `waiting_input` for a genuinely dead pane. #124's report showed
  `waiting_input` throughout, which rules this out directly.
- **Not the `#87` futile-clear map (`d.futile`).** That map is in-memory
  only, *by design* (see its field comment on `Driver`) — a restart forgetting
  it is the intended behaviour, and it does not gate a session's FIRST clear
  attempt in any case, only a repeat against the identical residue.
- **Not a nonce collision.** `paintedMarkers`/`confirmLanded` markers use
  `d.nonce()` (`randomNonce` by default) — cryptographically random per call,
  not a restart-deterministic sequence that could re-collide with a marker
  left over from a previous process.

## What IS already restart-aware, and where

`stampSinceLocked` (tmux.go) already detects and reports, in `State`/`List`'s
own `Evidence` string, when a status's `since` was carried from a record
persisted **before this driver instance started** — the literal source of
the phrase `"(age carried from before this service restarted)"`. That
mechanism was already correct and already answered #124's own diagnostic
question ("does this predate the restart?") for a `State()`/`List()` read.
What it did NOT do — closed by #124's fix for `Discard`, and by #131's for
`Send` — is surface through the failure messages an operator actually acts
on; before those fixes, confirming the correlation required a separate
`State()` read and manual cross-referencing (see
`restoredWaitingInputSince`/`restartNote`/`withRestartNote`/
`withRestartNoteReason`). Both `Discard`'s 409s and `Send`'s
`OutcomeUnknown` receipts now carry it.

## What is still open

Whether a service restart is causally *why* writes stop landing against a
pre-existing session remains an open question — #124/#131's fixes make the
correlation observable at the point of failure, on both `Discard` and
`Send`, they do not confirm or explain a mechanism. Closing that needs a
live reproduction against an affected host, which no session working this
issue has had access to (#131). If it recurs, the diagnostic message these
fixes added is where to start: it will say whether the session's
`waiting_input` state predates this service's current process, without a
caller having to derive that by hand a second time.

#131 also carries a second open point this file does not cover: a session
in this condition still presents as fully controllable rather than
reporting a degraded (readable-but-unwritable) state before a write is
attempted. That is explicitly the fallback for the causal question above
being *confirmed* — it is not, so no heuristic for it has been added here.
