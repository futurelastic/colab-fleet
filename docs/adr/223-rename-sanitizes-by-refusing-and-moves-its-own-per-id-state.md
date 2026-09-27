# 223 — rename refuses a name the multiplexer would mangle, and moves its own per-id state

**Issue:** #223 (follows #222; both gaps were noticed and explicitly deferred
in ADR 222 and ADR 97)

## Context

ADR 97 and ADR 222 each independently noticed the same two gaps in `Rename`
and explicitly left them unfixed, naming a scope fence rather than a decision
that they were fine as they were:

- **`Rename` never applies `naming.go`'s `sanitizeName`** to the caller's `to`,
  the way `Create`'s `resolveName` does to a requested name. The multiplexer
  silently mangles some characters — a `.` becomes `_` — so a rename to a name
  containing one succeeds, but the id this driver then announces (in
  `d.observed`, `session.renamed`, every downstream reader keyed on `to`) is
  not the id the multiplexer actually carries. Nothing had measured this
  causing an incident; it was noticed by symmetry with `Create`.
- **Per-id driver state other than `d.observed` is not moved off the old id.**
  `d.mods.rekey` (#185) already carries a delivery-module lane across a
  rename; `d.stranded`, `d.tombstones`, `d.unconfirmed` (the #184 cross-path
  ledger) and `d.delivered` (the #111 `turns` denominator) did not. A stranded
  (unsubmitted) composer record made under the old id was orphaned rather than
  migrated — a resume against the new id would not find it.

## Decision

### 1. Refuse, don't silently clean

`Rename` now computes `sanitizeName(to)` immediately after the empty-name and
self-rename checks, and refuses with a plain error naming the clean form when
it differs from `to` — before the multiplexer is touched, before any driver
state is read or written.

**This is a refusal, not `Create`'s silent clean-and-proceed.** The two
operations look symmetric but are not: `Create`'s requested name was never
authoritative on its own — `resolveName` may still renumber it on a
collision, so the caller was already reading the *actual* id back from the
returned `Session.SessionRef.ID`, not assuming it got what it asked for.
`Rename`'s `to` is client-facing top to bottom: `session.renamed` announces
it verbatim (api-http.md §4), and `RenameAck` (session-abstraction.md §2.5a)
carries no field a caller could read a "resolved" name back from. Silently
renaming to a *different* string than what was asked for would not close the
gap ADR 97/222 named — it would move it one layer up, from "this driver
disagrees with the multiplexer" to "this driver disagrees with its own
caller", and every caller downstream of `handleRename` (`publishRename`,
`labels.rekey`, `history.renamed`, the title-sync lookup) already assumes the
HTTP body's `name` — not some value `Rename` alone silently altered — is what
now applies.

Refusing is also the established idiom in this exact function already: the
code immediately below (the collision check) already reads "refuse a
collision rather than letting the multiplexer decide". Sanitization is the
same principle applied to shape instead of uniqueness, and needs no change to
`RenameAck`, no wire change, no `docs/spec` change beyond documenting the new
400 (api-http.md's rename section).

A useful side effect: `reassertNames` (#97) only ever reasserts a name this
driver itself previously accepted through `Rename`, so every name it retries
is now guaranteed already-clean — it can no longer attempt to re-assert a
name that would just get mangled again on the next attempt.

### 2. Move the four per-id records, mirroring `d.mods.rekey`

`rekeySessionState(from, to)` (`tmux.go`) runs once, under `d.mu`, right after
`d.mods.rekey` in `Rename`, and moves:

- `d.stranded[from]` → `d.stranded[to]` (overwrite — a singular "the current
  one" record, same shape as `d.observed`'s own move a few lines above it)
- `d.delivered[from]` → `d.delivered[to]` (overwrite, same reasoning)
- `d.tombstones[from]` → merged into `d.tombstones[to]`, trimmed to
  `tombstonesPerSession`
- `d.unconfirmed[from]` → merged into `d.unconfirmed[to]`, trimmed to
  `unconfirmedPerSession`

The two list-shaped records are **merged**, not overwritten, unlike the two
singular ones. Both lists hold evidence about several distinct past
deliveries, each self-corroborated by its own `Cwd` field (§5.4) rather than
by the id alone, so overwriting a new id's own list to make room for the old
id's would silently reopen the exact bugs those two ledgers exist to close —
a double terminal-delivery, or a composer clear/replace against someone
else's draft — for whichever session last held the new id. A collision here
is an edge case (the new id would have to have been a *different*, since-
closed session within the retention window), but merging costs nothing extra
this rename already isn't paying by holding `d.mu`.

## Alternatives considered

**Sanitize `to` silently and add a `name` field to `RenameAck`** so a caller
could read the actually-applied id back, the way `Create` already lets a
caller read `Session.SessionRef.ID`. Rejected for this change: it is a real
wire addition (`fleet.RenameAck`, its `UnmarshalJSON`, the remote driver's
proxying, `handleRename`'s downstream calls, `docs/spec`) for a defect nothing
has yet measured causing an incident, and ADR 97 already named "a wire change
in `internal/`'s public `fleet` package" as outside its own scope fence for
exactly this reason. Revisit if a real caller needs auto-clean-and-report
rather than refuse-and-retry — the shapes are not mutually exclusive, and
`RenameAck` gaining an optional field later is not itself a breaking change
(the same pattern `title` used when #222 added it).

**Leave list-shaped state unmerged, matching `d.mods.rekey`'s own plain
overwrite.** Rejected: unlike a delivery-module lane (one live thing per
session, by construction), `d.tombstones`/`d.unconfirmed` are specifically
retained history of several past events, corroborated per-entry rather than
by id. Overwriting would drop real evidence for a real, if rare, scenario
(renaming onto a very recently freed id) rather than a merely theoretical one.

**Also rekey `d.resumeIntents`, `d.conversationIntents`, `d.createRecords`,
`d.environments`, `d.futile`.** Investigated; each is plausibly the same
latent gap. Left out of this change: #223's own body names exactly the four
records above ("stranded records, delivery marks, tombstones, and the #184
ledger"), Lucy's acceptance of this issue frames items 1–2 as the correctness
gaps worth fixing now, and none of the five has a known caller that
addresses it by id *after* a rename the way `resumeIfStranded`, a retried
inbox send, or `turnsSince` do. Flagged here rather than silently matched, so
a future reader does not mistake the omission for "checked and found fine".

## Consequences

- `go test ./...` covers both fixes: `TestRenameRefusesANameTheMultiplexerWouldMangle`
  (a `.`, a `:`, and a leading `-`, plus a control case proving an
  already-clean name is unaffected) and `TestRenameMovesPerIDStateToTheNewID`
  (seeds all four records under the old id, asserts they read back under the
  new one and are gone from the old one).
- A rename to an unsanitary name now returns `400` (via `writeDriverError`'s
  default case) instead of `202` with a silently-different applied id.
  `docs/spec/api-http.md`'s rename section documents the new refusal.
- Items 3 and 4 of #223 are **not** touched by this change — see the issue's
  own comments for item 3's investigation finding (no code change needed: the
  id-half fight is already bounded by `maxNameReasserts`, and the title
  half's own documented remedy — re-POST the same name — is unconditional on
  whether the id half is currently contested) and the new issue filed for
  item 4's compat check.

## Reopen when

A real caller is found that needs the silently-cleaned name back rather than
a refusal to retry with — that is the trigger to revisit the rejected
`RenameAck.name` alternative above.
