# ADR 172 — Re-attach a content client on its death, not on every read

**Status:** accepted (2026-09-20)
**Issue:** colab-fleet #172 · builds on #170, #167

## Context

The event stream opens one always-on lifecycle control client, which sees every
session appear and disappear fleet-wide, plus one content client per watched
session, which is the only thing that sees that session's screen change. Content
clients are dialled in exactly two places: at subscribe time, and in the engine's
diff on the branch that handles a session it did not already know.

A content client dies with the session it is attached to. A session that exits and
is re-created under the same id **between two reads** is never observed to close,
so the id never leaves the engine's `known` map and the diff never takes that
branch. The session kept its place in the feed with no content client behind it.

This was invisible until #170. Before it, the dead client sat in the stream's
client map and the gap looked like a live attachment; after it, the state is
observable — a matching session in `known` with no entry in the map.

The cost is latency, not correctness. Notifications here are triggers, never data:
any notification drives one full enumerate-and-diff across every session, so the
fleet-wide lifecycle client still reported the re-created session's state changes.
They just waited for something else to speak.

## Options

- **A — re-attach on every read.** In the engine's diff, call the attach helper for
  every matching session seen, not only new ones. It returns early when an entry
  exists, so an attached session costs a map lookup. Its risk is a session that
  enumerates but cannot be attached: re-dialled on every pass, each dial behind a
  full enumeration, for as long as the subscription lives.
- **B — re-attach on a death.** The pump that drains a content client is the only
  thing that knows the client died. When it dies while the stream still holds it as
  the live client for that id, mark the id; the next read spends the mark and
  re-attaches once.

## Decision

**B**, with a bound of its own.

B carries one bit per observed death and spends it on the next read, so it cannot
be provoked into one dial per pass by a session that merely *enumerates*. It is
also immune to a failing dial, which opens no client, kills no pump, and therefore
leaves no mark — the exact case A has to defend against.

What B does **not** get for free, and what the issue only half-named: a client that
attaches and then dies at once produces one death per attempt, so "once per death"
degenerates into A's storm by the other route. So the budget is explicit:

- Three consecutive attempts that deliver nothing, then it stops and says so once.
- The budget is **renewable**, and what renews it is the only evidence available
  without consulting a clock: a client that consumed at least one notification
  demonstrably attached, so its eventual death is an ordinary session exit and costs
  nothing. The pump therefore reports whether it consumed any notification at all —
  any notification, not only one it forwarded as a trigger, since a client's filter
  decides what is interesting and a client that spoke and was ignored still attached.
- Giving up is self-terminating: with no client dialled there is no further death,
  so no further mark, so the log line is printed once per id.
- A session observed to close clears its bookkeeping entirely, so a reused session
  id — an ordinary thing for a launcher to produce — starts its next life with a full
  budget rather than a spent one.

Rejected in passing: measuring a client's lifetime to detect a flap. It needs a
clock the driver only has injected for tests, and a fixed test clock makes every
death read as instant. "Did it ever speak" needs no clock and is a stronger
statement anyway.

## Consequences

- **The client cap now bounds live clients rather than total dials.** A slot freed by
  a death can be refilled. This is a deliberate change to what the constant means over
  a subscription's life, and it is the reading the multiplexer server's descriptor
  budget — the thing the number was chosen against — actually cares about: a dead
  client holds no descriptor.
- It opens the cap to **no new competition**. Only a marked id re-attaches, so the
  freed slot goes back to the session that vacated it, never to whichever session the
  next enumeration happens to list first. A session skipped at subscribe time because
  the cap was full is still skipped; that is unchanged and out of scope here.
- A session that exhausts the budget keeps its fleet-wide triggers and loses only its
  own push latency — which is exactly the behaviour it had before this change, for
  that one session, and is now stated in the log rather than being silent.
- #170's own unit test had to be sharpened. It asserted the dead client's entry was
  **absent**; a replacement one coalesce window later makes that a test racing a timer.
  It now asserts identity — that the dead client is no longer the stream's client for
  that id — which is what #170 was really about.

## Reopen when

The give-up log appears in practice. It would mean a session enumerates while no
attachment to it survives, which this driver has never observed and has no
explanation for; the log line is the instrument for finding out, and the answer
would likely be a substrate fact worth its own issue rather than a bigger budget.
