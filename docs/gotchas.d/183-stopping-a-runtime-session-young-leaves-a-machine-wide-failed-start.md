# Stopping a runtime session before it is about ten seconds old leaves a machine-wide failed-start record, and two of them switch a rendering mode off for every session on the machine

**Issue:** #183 (found while measuring a candidate runtime build for
`colab-fleetd compat`; nothing in this repository's code showed it)

## What happened

Exploratory sessions were started against a candidate build and stopped within
a few seconds of boot. The next launch on the machine printed a notice that the
runtime's fullscreen renderer "didn't finish starting last time" and fell back
to its classic renderer. Reading the runtime's own state file showed why.

The runtime records every launch in a machine-wide state file, keyed by the
process id, about one second after it starts. Once the launch has been alive for
roughly ten seconds it deletes that record, and it also clears the running count
of failed starts. A launch whose process is gone before then is counted as a
**failed start** by the *next* launch, whichever session that is. Enough
consecutive failed starts for one runtime version (the threshold is two) switch
the fullscreen renderer off for that version on the whole machine, durably.

Measured, not inferred: the record appeared one second after a session started
and was cleared, together with the strike count, at eleven. A short-lived
session had left one strike behind; a longer one cleared it again.

## Why it matters here

Anything that starts a runtime session and stops it quickly — a test harness, a
probe, a script that boots one to look at it — can silently change how **every
real session on the machine** renders. There is no error at the time, and the
effect is on other people's sessions. The screen grammar this service's
classifiers read is exactly what changes with the renderer.

## What to do

Let a session you started reach about fifteen seconds of age before you stop it.
Judge it by **age**, not by reading the runtime's private state, so the rule does
not depend on a key name that may change. `colab-fleetd compat` does this in its
teardown (`compatSettleAge`) and waits only when it has to: a full run lasts
minutes and never waits.

The one case this cannot cover is the process being killed outright, which runs
no cleanup; at worst one failed start is left behind and the next healthy launch
clears it. The full account, and why the rule is by age, is in
[`docs/compat.md`](../compat.md), *A session must not be killed young*.
