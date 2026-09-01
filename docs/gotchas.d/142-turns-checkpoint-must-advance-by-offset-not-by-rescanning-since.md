# A liveness counter memoised as "count as of file size S" must advance by reading forward from S, never by re-deriving from the original timestamp

**Issue:** #142 (found polling `state.turns` on a session running one real,
several-minute turn)

## What happened

`turnsFor`'s `turns` count (#111) is memoised per delivery as a
checkpoint — `{Count, Size}`: the count last computed, and the record
file's size at the moment it was computed. Once `Size` is set, a later poll
that finds the file has grown re-derives the count by re-scanning the last
`recordTailBytes` (256KiB) of the file and checking whether that window can
be *proven* to reach back to the original delivery timestamp
(`turnsSince`'s own honesty rule — this part is correct and unchanged).

A single turn producing enough real content (~4-5 minutes of tool use, in
the field report that found this) is large enough to push more than 256KiB
onto the record between two polls. Once that happens, the tail window can no
longer be shown to reach back to the *original* delivery timestamp, so the
honest re-scan returns "unresolvable" — correctly, by its own contract — and
the caller fell back to the **stale cached count**, permanently, for the
rest of that delivery mark's life. The fresh turn boundary that proves the
turn completed was sitting a few bytes from EOF, comfortably inside that
same tail window — the bug was never in what the window could see, it was
in anchoring the proof to a timestamp from long before the window's own
horizon instead of to the last point already vouched for.

## The rule going forward

**A checkpointed count only ever needs to prove it reaches back to its own
checkpoint — never back to the original starting point that checkpoint was
itself derived from.** Once `Count` is known-good as of file size `S`,
counting forward from byte offset `S` to EOF is provably complete no matter
how large the file has grown since, with no timestamp or window-size proof
needed at all (`turnsSinceOffset`, alongside `turnsSince` in
`internal/drivers/tmux/runtimerecord.go`). Re-deriving "since the beginning"
on every read is not just wasted work, it is where an honesty guard that is
individually correct (never guess past what you can prove) combines with a
memo that never gets to use its own prior proof, and produces a value that
is silently frozen forever.

This generalizes past this one field: any "count/sum since X, memoised as a
running total plus a resume point" should resume from the memoised point,
not recompute the window against the *original* X, once the two have
diverged enough that recomputing from X can no longer be shown to be
complete.

## The check, if you hit the same symptom

A monotonically-increasing counter derived from a growing log, gated by an
honesty check against a fixed window, that appears to freeze permanently
once the source has grown past some size since the last delivery/reset: look
for whether the "resume from here" fact (a byte offset, a sequence number, a
cursor) is being tracked at all — and if it is, whether the recompute path
actually uses it, or silently falls back to re-deriving from the original
starting point every time.
