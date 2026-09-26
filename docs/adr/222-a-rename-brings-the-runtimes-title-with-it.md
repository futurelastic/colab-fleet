# ADR 222 — A rename brings the runtime's title with it

**Status:** accepted (2026-09-27)
**Issue:** colab-fleet #222

## Context

`POST …/{id}/rename` changes the multiplexer session's own idea of its name.
On the tmux driver's one measured runtime (Claude Code), the process inside
the pane keeps a SEPARATE idea of its own title — the transcript's
`custom-title` entry — and a rename through this API left it untouched.

Measured on a live fleet: a rename accepted (`202`) and changed the
multiplexer name; seconds later a different principal renamed the session
back to its old id. The cause was a client that reconciles the multiplexer
name from the runtime's OWN title and "repairs" a disagreement it sees —
this API's own rename had never been told to the runtime, so the
disagreement was real, and the "repair" was actually a revert of a rename
that had genuinely happened.

## Decision

**A rename brings the runtime's title along, as a second, separately
reported half — never folded into the id half, and never allowed to delay
or fail it.**

1. **`rename` returns `fleet.RenameAck`, not the bare `fleet.Ack`.** `Ack`'s
   own doctrine (§2.5) forbids a status of its own, because `interrupt`/
   `close` are pure intent — confirmation arrives later, only, on the event
   stream. Rename's id half does not have that shape: by the time a driver's
   `Rename` returns, the multiplexer-level change has already happened or it
   has not. So the id half stays a plain `accepted` boolean, and the title
   half is a genuinely separate fact, `RenameAck.Title *TitleSync`, with an
   honest `pending` state for exactly the case `Ack`'s doctrine exists to
   guard against — nothing here promises synchronous completion it cannot
   deliver.
2. **`TitleSync` is a new, closed vocabulary — `synced` / `pending` /
   `failed` / `not_applicable` — not a reuse of `Outcome`.** `Outcome`'s
   `queued` already means "delivered, confirmed by the SCREEN in the worst
   case" for the terminal path, and this feature exists specifically to
   never call a title synced on screen evidence. `TitleSync.Receipt` still
   carries the underlying `/rename <name>` delivery's own `DeliveryReceipt`
   verbatim, so "the same rules as `/input`" (busy/stranded composer
   refusals, in particular) are visible to a caller directly, not
   re-derived from `Status` alone.
3. **The id half and the title half are split across two calls, sequenced
   by the SERVICE, not the driver.** `Driver.Rename` (tmux) does only the
   multiplexer-level rename and never sets `Title` itself.
   `internal/service/http.go`'s `handleRename` calls it, publishes
   `session.renamed` (unchanged — still immediately after the id half, so
   the announcement stays exactly as timely as before this existed), and
   only THEN calls a new optional driver capability,
   `driver.TitleSyncer.SyncTitle`, targeting the session's NEW id. A driver
   that does not implement it gets `TitleNotApplicable` from the service —
   the ONLY place that value is ever produced.
4. **`SyncTitle` delivers `"/rename " + name` through the ordinary `Send`
   pipeline**, forced onto the terminal route, unlabelled. The input guard
   already admits `/rename` for any caller (colab-fleet#180 L6); a busy or
   stranded composer refuses exactly as it would for `/input`, overwriting
   nothing.
5. **`synced` requires the runtime's own `custom-title` entry, read from its
   transcript — never the screen, and never the weaker evidence of the
   `/rename` command merely having run** (colab-fleet#187's local_command
   confirmation). The two are different facts: the command running proves
   the runtime accepted it; the title changing proves the fact this whole
   issue is about. A poll (`transcriptTitleScan`, bounded by the same
   `submitConfirmWindow`/`Interval` the ordinary send-confirmation path
   uses) tries to observe the second; short of that within the time
   available, the honest answer is `pending`, distinguished from a bare,
   unexplained non-confirmation by a counter
   (`title_sync.command_without_title`) when the command DID visibly run.
6. **The composer-serialisation lock is aliased across the rename**
   (`aliasComposerLock`, `terminalpath2_lock.go`): the old and new id name
   the same pane, and `SyncTitle`'s own delivery — a new, guaranteed side
   effect of every rename — must contend on the identical mutex as
   anything already in flight against the old id, or the exact concatenation
   hazard the lock exists to prevent (colab-fleet's own D5) reopens at the
   rename boundary. `Rename` itself never waits on this lock: a busy
   composer must not turn the id half into a failure.
7. **The conversation-memo is primed under the OLD name, before the
   multiplexer rename runs.** Name-and-date derivation
   (`internal/drivers/tmux/conversation.go`) matches a transcript whose
   FIRST `custom-title` is still the old name at that instant; the memo it
   populates is keyed on `(pane, created)`, which survives the rename
   untouched — so a title-sync lookup moments later, asking with the NEW
   name (which the transcript carries nowhere yet), still resolves.
8. **A session-management command never marks the #111 delivery mark.**
   `/rename`, `/rc` and `/remote-control` produce no agent turn, so marking
   `turns` for one would read as "a delivery was made and nothing has
   completed since" — a false work-lost signal. This was always
   theoretically true of a bare `/input` call; #222 made it a GUARANTEED
   side effect of every rename, so it stopped being a rare edge case worth
   leaving unfixed.

## Alternatives rejected

- **Add the status to the bare `Ack`.** Directly forbidden by §2.5's own
  doctrine — see decision 1.
- **Carry it on `session.renamed` instead of the response.** That event
  promises exactly one accept-time member plus exactly one later
  corroboration follow-up (colab-fleet#103); a title half would need a new,
  normative `EventKind` in a closed set, and would either delay the
  time-sensitive id announcement or arrive as a confusing THIRD event no
  existing subscriber expects. The reconciling client this issue is about
  does not need the STATUS on the stream — it needs the runtime's title to
  actually change, which decision 4 delivers directly.
- **A background retry queue for `pending`/`failed`.** Rejected the same
  way colab-fleet#97's own ADR (102) rejected a poller: this call already
  has a retry path that costs nothing new — renaming to the SAME name again
  re-attempts only the title half (`to == ref.ID` was always a documented
  no-op for the id half; §3). Building a second, timer-driven mechanism to
  do the same thing invites the two disagreeing.
- **Treat the local_command confirmation alone as `synced`.** Rejected in
  decision 5 — it is evidence the command ran, not evidence the title
  moved, and this issue exists because those two facts can disagree.
- **A full `corroborateTarget` refactor shared between `Rename` and
  `SyncTitle`.** `Rename`'s own corroboration (`req.Expect.StartedAt`, or
  the weak `d.observed` check) runs once, synchronously, moments before
  `SyncTitle` is even called by the service — re-deriving it a second time
  inside `SyncTitle` would mostly re-prove what was just proven. `SyncTitle`
  instead relies on `Send`'s own "no such session" refusal for the
  (extremely narrow) case the target vanished in between; this is a
  deliberate scope reduction from this issue's own design pass, not an
  oversight.
- **A one-in-Send retry on an `unknown` outcome before polling for the
  title.** Considered and dropped: `SyncTitle`'s own title-scan poll already
  gives a delayed `custom-title` write extra chances within the same
  confirmation window, so a second, separate retry layer inside `SyncTitle`
  would mostly duplicate work `Send` (and the poll) already do.

## Consequences

- Every rename through this API now has a real, if small, side effect on
  the session's composer: a `/rename <name>` local command lands in the
  conversation. Previously undocumented as absent — `docs/spec/*.md` never
  promised rename was composer-silent, and the pre-existing invariant that
  the multiplexer id, the remote-control binding and the runtime's own name
  are one string from birth (`internal/drivers/tmux/naming.go`) points the
  other way. Flagged here as a genuine, user-visible behavior change, not a
  regression.
- `202` latency on a local tmux rename grows to roughly one confirmation
  window on the pending/failed path, bounded by the caller's own deadline —
  the id half and its announcement are unaffected; only the response BODY's
  `title` field waits.
- **Genuinely unmeasured runtime behavior**: whether/when Claude Code writes
  a fresh `custom-title` entry after a PROGRAMMATIC `/rename`, at all, or
  under what timing relative to the local_command entry. This repo has
  measured only the local_command shape itself (#187), against real
  transcripts from a HUMAN-typed `/rename`. If the runtime never writes one
  for a programmatic delivery, every rename through this API will honestly
  read `pending` forever, and `title_sync.command_without_title`'s rate
  will show it. A follow-up compat check, modeled on the existing `E-NAME`
  check (`compat_send_checks.go`), is recommended but not built in this
  change — see the tracking issue this ADR's own wrap notes file.
- **Pre-existing adjacent defects, noticed but explicitly out of scope**,
  filed as follow-ups rather than fixed here: `Rename` does not sanitize
  `to` the way `Create` sanitizes a name (`naming.go`), so an announced id
  can differ from the multiplexer's real one; per-id driver state other
  than `d.observed` (stranded records, delivery marks, tombstones, the #184
  ledger) is not moved off the old id by a rename, so a stranded record
  under the old id is orphaned rather than migrated; colab-fleet#97's
  `reassertNames` does not itself sync titles, so a `failed` title plus a
  title-trusting client can still fight (bounded by `maxNameReasserts`) —
  the remedy is the same-name retry, decision 3's own escape hatch.
- A residual lock-alias race is accepted, not closed: an operation already
  blocked on a lock a DIFFERENT, just-closed session held under the name
  `to` before this rename is not covered (`aliasComposerLock`'s own doc
  comment). Narrow — it needs a close, a pending operation, and a rename
  into the freed name inside one short window — and the per-id lock table
  is never pruned regardless, so nothing distinguishes this from ordinary
  contention today.

## Reopen when

A live measurement shows whether/when `/rename` writes `custom-title` for a
programmatic delivery (the compat-check follow-up); the pending rate from
`title_sync.command_without_title` proves nonzero at scale; or a second
runtime gains title-syncing support and needs its own confirmation signal
(this ADR's decision 5 is Claude-Code-transcript-specific by construction).
