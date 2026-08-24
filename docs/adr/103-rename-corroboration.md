# ADR: `session.renamed` gets a corroboration follow-up, not a later single fire

**Issue:** #103 (the event-side half of #97/#96, `group:identity-in-record`)
**Status:** decided

## Context

#97 measured a rename that returned `202 accepted`, read back correct for
roughly half an hour, then silently reverted — id, name and attach target all
restored — with nothing on the event stream or in any read saying so for that
whole window. #97's own fix closed the *read* side: `List` now detects the
drift against a durable asserted-identity record, reports it, and repairs it
(`internal/drivers/tmux/reconcile.go`, `docs/adr/97-identity-in-record.md`).

It did not close the *event* side, because it could not: `session.renamed` is
published from `internal/service/http.go`, a file #97's own branch did not
touch — held by a sibling session for the whole of that work
(`fix/create-response-contract-84-85-86`) — and the fix landed with this
follow-up filed rather than folded in.

`session.renamed` still fires exactly once, at accept time, from the HTTP
handler, the moment `Rename` returns success. A subscriber that re-keys on it
— which `spec/api-http.md` tells it to — holds a name the service itself has
not yet, and may never, independently confirm.

## Decision

**`session.renamed` now fires twice for a rename that this service watches
long enough to have an opinion about: once at accept, always, and once more,
always, once it does.** The second carries a `Corroboration` field
(`fleet.RenameCorroboration`) with one of three values — `corroborated`,
`contested`, `unconfirmed` — never omitted. See `session.go`'s doc comments on
`RenameCorroboration` and `internal/service/rename_corroboration.go` for the
mechanism; this ADR is about why that shape and not an alternative one.

### Why a follow-up event, not a delayed single fire

The alternative the issue itself named — hold `session.renamed` until the
rename is corroborated durable, instead of firing at accept — is more
truthful about what it claims, and it is also later: #97's own measurement
puts a genuine revert as far out as half an hour. A caller that renamed a
session and is now waiting to be told about it, correctly, waits half an hour
for an event that today arrives instantly. That is a worse trade than keeping
the fast signal and adding a second, slower one: the first told what the
caller already knows (their own request succeeded), the second tells them
something they could not otherwise learn without polling.

### Why the corroboration mechanism is watching events, not calling a driver

#97's own ADR rejected a background poller inside the tmux driver for the
identical shape of problem this fix could have reintroduced from the other
side: "adding one for a defect that only matters when a caller is actually
reading is disproportionate." A poller here — `internal/service` calling
`List`/`Get` on a timer to check whether a rename held — is the same
disproportionate machinery wearing a different hat, and it duplicates work
the driver's own edge-triggered re-enumeration
(`internal/drivers/tmux/subscribe.go`) already does whenever anything
changes.

So this does not call a driver at all. It watches `session.created` and
`session.closed` — events this service already publishes, sourced from that
same edge-triggered re-enumeration — for the specific signature #97 measured:
the renamed id stops resolving, and the old id's identity, matched by
`StartedAt` and never by name alone (§5.4), comes back. Everything the
corroboration mechanism knows, it learns from the event plane it is already
part of.

### Why `contested` requires the old identity to reappear, not just the new one to close

A `session.closed` for the renamed id alone is exactly as consistent with an
ordinary `DELETE` of the freshly-renamed session as it is with an
unattributable revert. Reporting either of those as `contested` would be
inventing certainty this service does not have — the same discipline #97's
own ADR applied to *what reverted the name*, left as an open question rather
than guessed at. `contested` is reserved for the one signature that is
actually unambiguous: the OLD id's identity — `StartedAt` included, per §5.4's
standing rule that an id match alone is never identity — reappearing after the
new one stopped resolving. A closed event with no such reappearance resolves
to `unconfirmed`, honestly short of the stronger claim.

### Why a feed gap forces `unconfirmed` rather than defaulting to `corroborated`

If this service's own view of the event stream had a hole during the
corroboration window — a `source.status` degradation, a `control.resync` —
"corroborated" would be a claim it did not earn: it did not watch
continuously, so it cannot swear nothing happened in the gap. §5.7's rule
("known false, with the evidence for it" is not the same claim as "known
true") applies here exactly as it does to every other place this codebase
already uses it: silence about a hole is worse than admitting one.

## Consequences

- `SessionRenamed` gains one field on the wire, `corroboration`
  (`RenameCorroboration`, always present) — additive, no existing consumer
  breaks by ignoring it.
- A caller that wants today's exact behaviour needs no change: the first
  `session.renamed` for a `to` still arrives at the same moment it always did,
  carrying `"accepted"`.
- This bookkeeping is **in-memory**, scoped to one `hub`, and does not survive
  a restart of this process. A rename whose corroboration window was still
  open at restart gets no follow-up event — silently, which is the exact
  failure this file otherwise exists to prevent. Closing that needs the
  durable, machine-readable identity record #102 asks for; deliberately not
  attempted here (#102 is its own open issue, unresolved as of this fix).
- The corroboration window (60 minutes) is sized off one measurement (#97's
  ~35-minute revert) with margin, not off a distribution. If a revert is ever
  measured taking longer, this is the value to revisit.

## Alternatives considered

**Wait for corroboration before firing at all**, rather than firing at accept
and following up. Rejected above (see "Why a follow-up event, not a delayed
single fire") — it trades an instant, honest-if-provisional signal for a slow
one, for callers who mostly want the fast one.

**Poll the driver for the new id's continued existence.** Rejected: the exact
disproportionate-machinery argument #97's own ADR already made against a
background repair loop, applied to a background confirmation loop instead —
and it duplicates work the driver's own edge-triggered enumeration already
does.

**Treat any `session.closed` for the renamed id as `contested`.** Rejected: it
cannot be told apart from an ordinary `DELETE`, and reporting a delete as a
contested identity would be a false claim, not a conservative one — the
direction this design is careful never to be wrong in.

## Open questions

- Whether corroboration bookkeeping belongs somewhere durable instead of
  in-memory is #102's question, not resettled here.
- Whether 60 minutes is the right window at all, versus something derived
  from the driver's own `maxNameReasserts` timing, is unmeasured — noted for
  whoever next has a second data point.
