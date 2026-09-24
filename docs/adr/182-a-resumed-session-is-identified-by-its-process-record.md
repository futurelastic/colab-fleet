# ADR 182 — A resumed session is identified by its process record, not by elimination

**Status:** accepted (2026-09-24)
**Issue:** colab-fleet #182 · builds on #180 (the per-process record reader) · epic #181

## Context

A session's `conversation` was found by elimination: the records in the session's
working directory that carry the session's name, minus those that began before the
session existed. The date rule is a fact, not a heuristic — a conversation runs
inside a session, so an older record cannot be its own.

It is a fact about a session that *started* a conversation. A session that
*continued* one runs inside a record that is older than itself, so the rule
eliminates exactly the right answer. Measured on one machine's live fleet, 18 of 75
sessions read `known: false`, nearly all resumed, all with the same evidence — "all
N records carrying this session's name were created before this session existed".
One more read ambiguous. Every consumer that keys on the conversation identifier
could not reach those sessions.

A second class the name can never answer: a session created without remote control
is given no name (`claudeCodeCommand` passes it only inside that branch), so it
writes no title and has nothing to match.

The runtime writes one small record per running process, filed under the pid, that
names the conversation the process is in. #180 introduced its reader for a different
purpose — confirming a delivery from the session's transcript.

## Decision

Identify the conversation from that record, corroborated the way #180 already
corroborates it, and keep the name-and-date derivation as the fallback.

- **One resolver, not two.** The reader, the working-directory check and the
  start-time corroboration are #180's, factored into `processRecordFor` and
  `corroborateProcessRecord` so the delivery path and the listing cannot disagree
  about what a record said. Nothing was copied.
- **Corroboration is exact.** The record's start time must equal the start time of
  the process running under that pid now. Both are whole-second text, so there is
  nothing finer for a tolerance to absorb, and any tolerance is a window in which a
  reused pid's stale record passes. A pid the kernel recycled leaves a record that is
  as well-formed as a live one; the start time is the only thing that tells them apart.
- **The record is read before the OS is asked.** A session whose runtime wrote no
  record costs one failed file open and no `ps`. A fleet's listing is dominated by such
  questions.
- **Both sources are asked when the record is usable, and disagreement is reported.**
  If the record and the name-based derivation name different conversations the answer
  is `known: false` and the evidence names both. Neither is chosen: two independent
  sources that disagree are a finding, not a tie. Where the derivation cannot answer
  (every candidate predates the session; several are possible; none carries the name)
  the record answers.
- **An unusable record is never silent.** Missing, unreadable, incomplete, naming
  another working directory, a non-UUID identifier, or a start time that does not
  match: the answer is the derivation's, and its evidence says why the record was not
  used.
- **The identifier is checked twice.** The reader accepts only a UUID-shaped
  identifier; the store separately refuses an identifier that is not a single path
  element or whose path would leave the record root, so the second check does not
  depend on the first staying in front of it.
- **Successes are remembered, refusals are not** — as before. The OS is asked once
  for the life of a session, not once per listing.

### Considered: a new `source` value

The wire label stays `derived` and the evidence names the rule that answered
("per-process record, start time corroborated"). `ConversationSource` is a closed set
decoded strictly, and this service federates between builds of different vintages: a
member an older peer does not know does not degrade one field on that peer, it fails
the whole session read. A distinct label is a later change that has to land on every
peer's decoder first.

## Consequences

- Resumed sessions and unnamed sessions resolve. Fresh sessions resolve to the same
  identifier as before, with the derivation's own evidence first.
- A session that shares its name and directory with an unrelated later record now reads
  as a conflict where it used to read as a confident, possibly wrong, answer.
- The listing does not notice a runtime that regenerates its own identifier under an
  unchanged pane (a `/clear`): a cached success stays for the life of the session, as it
  always did. The transcript path cross-checks the cache against the live record on every
  send (#180); the listing does not, and re-reading a record per session per listing would
  reintroduce the per-session cost this design avoids.
- The evidence is prose for humans. A consumer that needs to tell the two rules apart
  programmatically has no field for it until the label above is added.
