# A fact about a process, memoised under the pane's identity, outlives the process

**Issue:** #202

## What happened

A session's runtime was relaunched inside the same multiplexer session, under a
new conversation. For as long as the service kept running, the session record
went on naming the old conversation — and its evidence string went on claiming
"verified same process generation" for a pid that no longer existed. Observed
still stale about half an hour after the relaunch, while a send to the same
session, made in the same minute, had already resolved the new process and
quoted its pid and conversation in the receipt.

The conversation memo was keyed on the pane and the session's creation time. A
pane, its creation time and the session's name are exactly what a relaunch
leaves alone; the process in the pane is what it replaces. Two paths answered
one question from two identities, and only the one that re-resolved the process
each call was right.

## The rule

A memo of a fact about **the process in a pane** is keyed on the process (pid,
and start time where somebody already measured it), not on the pane. The pid is
free — the multiplexer reports it on every read — so comparing it costs nothing;
the start time costs a `ps`, so it is compared only where a caller has it. An
unknown pid is absence of evidence, never a change.

Dropping the memo is not the whole fix. The fallback derivation reads the record
the session's *first* process titled, which is still there after the relaunch
and still looks like the only candidate, so it hands the old id straight back on
the next read. Keep the ids that replaced runs held, and refuse a name-derived
answer that names one of them.

## What is still not covered

A relaunch made before the service started was never observed, so there is
nothing to retire. And a runtime that regenerates its conversation id under the
same process (the same pid, the same start time) is not a replacement by this
rule at all — see #203.
