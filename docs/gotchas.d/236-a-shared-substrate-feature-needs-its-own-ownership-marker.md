# A feature scoped to "sessions this driver created" needs an explicit marker, not an implicit assumption

**Issue:** #236

## What happened

#235's ruling on exit-capture said "only for sessions that ended without a
service close" — scoped, by its own comment on `Create`, to sessions this
driver started. But `reapDeadRows` had no way to tell those apart from any
other dead pane the enumeration happened to see, because this driver's
multiplexer server is explicitly documented (on `Create`) to sometimes host
sessions other tools started too. The implementation read the whole
enumeration's `pane_dead` field and acted on every row it was true for — a
scope that was accidentally "everything on this multiplexer server", not
"everything this driver owns", and nobody had to write a bug for the gap to
open: it was there from the first line, found only by re-reading the shipped
code against its own ruling.

The consequence was live: an operator (or another tool) setting
`remain-on-exit` on its own session, for its own reasons, had this driver
kill that session, capture its screen, and report an exit it never owned —
the instant that session's own process happened to exit.

## The rule

**A feature whose scope is "things I created" on a shared substrate cannot be
enforced by filtering the substrate's own enumeration alone — the substrate
has no concept of who created what.** It needs an explicit marker written at
creation time and read back at the point that scope is enforced. Here: a
tmux user option (`@colab-managed`) set once, at `Create`, checked by
`reapDeadRows` before ever acting on a dead pane — carried through
`enumerate`'s existing format string (one more `#{…}` field, no new
invocation — see #235's own gotcha on `List`'s constant-spawn contract) so
the marker costs nothing extra to read.

Generalizes past this driver: any code that walks a shared resource
(processes, files, sessions on a multiplexer, rows in someone else's table)
and acts on a subset "we created" needs that ownership recorded somewhere the
walk can check, not inferred from "well, we're usually the only thing
running here."

## What to check when reviewing a similar feature

- Does the ruling/spec say "sessions/rows/processes *this service* created"?
- Does the code that acts on them filter by anything other than a
  content property of the resource itself (dead, expired, matching a
  pattern)? If the filter is purely content-based, and the substrate is
  documented anywhere as shared, that is the gap #236 found.
