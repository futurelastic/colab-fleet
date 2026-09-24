# ADR: closed sessions get a bounded, service-kept record — not a persisted event window

**Issue:** #179
**Status:** decided

## Context

`GET /v1/sessions` lists live sessions only, and the event window that carries
`session.closed` is in memory, unpersisted, and advances only while something
is subscribed (`internal/service/service.go`, `eventSequence`). A consumer asking
"which sessions ran here in the last N days, and when did each end?" had to keep
its own copy of session state — the second copy of the truth this service
exists to make unnecessary. #179 asked for one tombstone per closed session,
kept for a bounded time.

## Decision 1 — the service keeps the record, fed by a "seen" map

The service, not a driver, keeps it (`internal/service/history.go`), in one
state document holding two halves: a small record of each live local session
as last seen, and the tombstones. Same reasoning as the label store (#153): no
substrate has a place for it, and every driver would do it the same way.

The "seen" half exists because a tombstone must describe a session that can no
longer be asked about. Persisting it is what lets a restart record a session
that ended while the service was down, with its metadata intact.

## Decision 2 — what counts as an end

- A close through this service that the driver accepted: `closedBy: close`,
  exact. Withdrawn if a later read finds the same run (id and `startedAt`)
  still there.
- Absence from a **complete, unfiltered** local listing, or an id now naming a
  session with a different `startedAt`: `closedBy: absent`. `closedAt` is when
  the absence was observed and is reported next to `lastSeenAt`, never as the
  moment of death (§5.2).
- **A `session.closed` event is not an end.** A rename also retires the old id
  on the stream. Instead, a local `session.closed` triggers a background
  re-listing (single-flight, one trailing rerun), which settles rename vs end
  with the carry rule and gives a timely `closedAt` whenever a stream is live.
- **Rename carry:** a seen id missing from a complete listing is carried to a
  newly-listed session of the same runtime only when exactly one carries its
  `startedAt`. Ambiguity carries nothing. This matters in practice: the
  multiplexer reports start time to the second, and two sessions created in
  the same second were measured sharing one `startedAt`.

## Decision 3 — a sibling route, not `?state=closed`

`GET /v1/sessions/closed`, beside `/v1/sessions/watch`. The issue offered both.
A query parameter on the existing route would be silently ignored by a peer on
an older build, which would answer with its **live** sessions — indistinguishable
from a closed list when it is empty, and wrong when it is not. A new route makes
that peer answer a bare `404`, which the remote driver already recognises
(`routeMissing`) and reports as a `degraded` source.

## Decision 4 — an atomic document, not an append-only file

The issue suggested an append-only file pruned on write. Pruning rewrites the
file anyway, so the state package's atomic write-and-rename is used instead —
the one persistence mechanism this codebase has, readable by an operator
during an incident. Closes are rare, so rewrite cost is negligible.

## Consequences

- With no subscriber and no listing, an end is noticed at the next read or
  restart; `closedAt` is then late, and says so through `closedBy: absent` and
  `lastSeenAt`. No periodic sweep was added: it would enumerate every pane on
  a timer for a question no one may be asking.
- Retention (`closedRetentionDays`, default 14) prunes on every write and
  filters on every read, so an idle service never returns an expired record.
- Seen records not sighted within the retention period are dropped; that can
  only be a runtime no longer registered.
