# A fact about a conversation, memoised under the process, outlives the conversation

**Issue:** #203 (follows #202, which is the same rule one level up)

## What happened

#202 made the conversation memo follow the process in a pane: a different pid, or
the same pid with a different measured start time, drops it. That left one way for
the memo to be wrong under a process it still matches: the runtime starting a new
conversation inside the *same* process (`/clear`). The pid and the start time do
not move; only the `sessionId` in the runtime's per-process record does. Measured
on a test rig: the record rewritten in place, same pid, same start time — the next
listing still said `known: true` with the old id, and its evidence quoted a record
that no longer said so.

The send path was never affected. It compares the memo with the live record on
every send and lets the live record win. The listing, and anything else that reads
the memo without going through that cross-check, was.

## The rule

A memo of a fact that a running process **states about itself** is checked against
that statement on every hit, not only when the process changes. The comparison
needs no subprocess: the memo already holds the start time the answer was
corroborated with, and the record carries its own — so a record with the same
start time is the memo's own process saying it changed, and a record with any
other start time is somebody else's and no evidence either way.

That is a file read per session per listing. It was chosen over accepting a bounded
staleness because the read is small and the listing's contract — a constant number
of subprocess spawns — is untouched; a stale id, in a field whose whole point is
that it is not a guess, was not worth the saving.

Everything short of a corroborated difference is absence of evidence: no record,
an unreadable one, one naming another working directory, one whose id is not the
runtime's own shape, a memo with no start time to compare. None of them drops the
answer — treating a thin read as a change would trade a stale answer for an
intermittent unknown one.

Dropping the memo retires the old id, for the same reason as in #202: the name
still points at the record the session's first conversation titled, which is still
there and still looks like the only candidate.

## A measurement that was wrong on the way in

The #202 write-up said the listing had not measured the process's start time. It
had: it asked the OS for it to corroborate the record, then threw it away. Keeping
it is what makes this check free, and it is worth checking what an earlier step has
already paid for before deciding a later one has to pay again.
