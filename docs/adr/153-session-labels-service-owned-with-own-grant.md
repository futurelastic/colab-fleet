# ADR: session labels are stored by the service, and written under their own grant

**Issues:** #153 (with #154, `group:record-fields`)
**Status:** decided

## Context

A fleet-wide consumer asking "is any machine already running a session for
unit of work X?" could only answer by encoding X into a session's name and
pattern-matching names — names that are mutable, collide across machines
(#19), and lose a session's type once a marker is creative (#90). #153 asked
for an opaque, bounded label map carried on the session record, writable at
create and afterwards, and filterable fleet-wide.

Two choices were not settled by the issue.

## Decision 1 — the service stores labels, not each driver

Labels live in a service-owned store (`internal/service/labels.go`), persisted
through the same state directory as the event cursor, keyed by
`(runtime, id)` and corroborated by `startedAt`.

- No substrate has a place for caller metadata, and no driver observes it.
  Per-driver storage would mean the same code once per runtime and a larger
  `Driver` interface for a fact every driver would handle identically.
- The local multiplexer driver's own per-create record expires after half an
  hour, which is the wrong lifetime for labels.
- `startedAt` is what stops a recycled id inheriting a stranger's labels
  (§5.4). A rename moves the record when it is accepted; a rename the service
  did not see through (a revert, a driver-adjusted name) is re-attached by
  `startedAt` on the next complete listing.
- Records are pruned only on close through the service, or against a
  complete, unfiltered, all-`ok` listing — never a filtered or failed one
  (§5.7) — and never a record newer than the listing's start, which would
  otherwise delete labels from a create that finished while the listing ran.

**Consequence:** a session closed *outside* the service keeps its record until
the next complete listing. Harmless (it can never attach to a different
session because of the `startedAt` check), and bounded.

## Decision 2 — a new `label` grant, not `rename`

The issue left this to the maintainer: reuse `rename` (smaller surface) or
mint `label`. Minted `label`.

- `rename` is defined as the power to change the handle every other caller
  addresses a session by. A label addresses nothing.
- The realistic writer of a label is often the session itself, once it knows
  its work. Under `rename` it would need the power to rename every session on
  its machine.
- A consumer that deliberately keeps cross-machine renames switched off would
  have to switch them on just to bind sessions to work — coupling two
  decisions an operator wants separate.
- Precedent: `keys` was split from `send` on the same "a distinct power gets
  its own grant" argument, and absent-means-denied keeps upgrades safe.

Labels sent **in a create body** need only `create`, like `name` and `marker`.

**Consequence:** the doctor's full-supervisor check deliberately excludes
`label` (as it excludes `relay`); counting it would turn every existing
supervisor principal into a warning on upgrade.

## Mixed versions

A peer on a build without labels decodes a create body and silently ignores
the field. So a relayed labelled create is refused `unsupported` **before**
anything is sent unless the peer's `/v1/health` reports `labels`; a filtered
list from such a peer is a `degraded` source with no items, never matches; and
its missing label route is `unsupported`, never `not_found`.

## Rejected

- **Labels as a driver capability.** Rejected for Decision 1's reasons.
- **Normalising `labels: null` to `{}` on every decode.** The nil map on a
  decoded session is the only signal that the answering service predates
  labels; `Session.MarshalJSON` guarantees `{}` on the way *out* instead.
