# A timestamp layout with no zone field is only ever safe within one process's own local reads — never across a serialization boundary

**Issue:** #147 (found immediately after #146 shipped, by testing the assertion
#146 asserted instead of measured)

## What happened

#146 bound each inbox-index record to one process run by having the index
carry the exact textual layout `ps -o lstart=` produces (`"Mon Jan _2
15:04:05 2006"`), reasoning that this was "a field copy compared exactly. No
tolerance, no format conversion, no clock reasoning." That reasoning was
checked against the **format** of one sample — never against a **value**
compared to the live process table.

The layout carries no zone field. Parsing it is `time.ParseInLocation(layout,
s, time.Local)` on both sides of the comparison, which is correct when both
strings are genuinely produced by `ps` on the machine doing the comparing —
but the index's copy of that string was written by a separate,
machine-local mechanism that renders in **UTC**, not local time. Parsing that
UTC-rendered text as if it were local silently produced the wrong instant, by
exactly the writer's UTC offset (9 hours, measured), on **every** comparison,
permanently — not intermittently, not on pid reuse.

**Measured cost:** comparing, for every live session on one machine, the
index's copy of a start time against a fresh resolve of the same process:
`identical: 0, differ: 52` — all 52 off by the identical nine-hour offset.

## Why it was invisible

The failure was also silent by construction, not just by accident:

- a mismatch is returned as an error from the resolver
- the caller (`sendViaInbox`) treats a resolver error as capability-absent —
  correct behavior for a genuinely stale/recycled-pid record, the case this
  comparison exists to catch
- so every send fell back to the pane path and succeeded
- while the capability-advertised flag (`deliversToInbox`) stayed **true**,
  because it answers "is a resolver wired", not "does it ever resolve"

An operator watching this system would see the capability advertised,
messages delivering normally, and no error anywhere. The feature would be
off, and nothing would say so — the same shape #122 (never wired) and #143
(framing never validated against the real receiver) already failed in, for
this same subsystem.

## The fix

The index side now carries an RFC 3339 (zone-bearing) timestamp instead of
the bare `ps -o lstart=` layout, and the comparison parses both sides into
real `time.Time` instants (`time.Time.Equal`) rather than reparsing
zone-blind text. The identity side is untouched — `ResolveProcessIdentity`
still parses genuine `ps` output via `time.ParseInLocation(...,
time.Local)`, which remains correct because that text is always produced and
consumed by the same machine, in the same call. Only the value that crosses
a writer/reader boundary changed shape.

A writer still emitting the old bare layout now gets a loud parse error
(`time.Parse(time.RFC3339, ...)` fails on ps-lstart text) instead of a
silent, permanently-wrong comparison — the format itself makes "the contract
changed and you haven't updated" detectable, where the old one made it
undetectable.

## The rule going forward

**A timestamp layout with no zone field is a same-process, same-machine
value only.** The moment a timestamp is written to a file, sent over a
socket, or read back by any process/machine other than the one that just
asked the OS for it, it needs a zone-bearing format (RFC 3339, or an epoch
value) — never a copy of whatever `time.Format` layout happened to be
convenient locally. This applies even when both sides use the *identical*
parse call: identical code does not make a comparison correct if the two
values it's comparing were rendered in different zones. Before reusing an
exported time-parsing helper for a *new* caller, ask where the string it
will be fed actually comes from — not just whether the layout matches.
