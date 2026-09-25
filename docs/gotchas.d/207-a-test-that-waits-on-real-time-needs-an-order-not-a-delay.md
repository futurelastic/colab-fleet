# A test that waits on real time needs an order to hang off, not a delay

**Issue:** #207

## What happened

Trunk went red on every run, and a rerun did not help. The suite is run with
`go test -race ./...`; on a host with a load average between 16 and 55, tests
whose code was correct failed, then passed alone. The issue named three of them.
Fixing those, and running the whole gate afterwards, turned up more with the same
four mechanisms below (eight tests in all; two of them failed in that gate run).
Each mechanism was reproduced deterministically before anything was changed — the
same message at the same line, by making the test's guess lose.

1. **Two stamps of one clock, taken a few statements apart, asserted as one
   value.** A call's deadline was stamped when the call began and the `started`
   the miss log measures from just before the request was sent, so a couple of
   milliseconds on a slow runner printed a budget of `2.998s` where the test
   wanted `3s`. Reproduced with a clock that advances 2ms per read.
2. **A window that opens inside a call, while the test finishes its setup after
   the call returns.** The attach poll's real-time window opens inside `Create`;
   the rig adds the pane after `Create` returns. A stall between the two spends
   the whole window before the first attach, so "wait until the poll has attached
   twice and stopped" never came true and timed out at 15s: 4 of 6 runs.
   Reproduced by sleeping past the window before adding the pane.
3. **A timer standing in for an ordering.** A goroutine appended the runtime's
   transcript entry 30ms after starting; `Send` fixes its transcript offset
   *before* it presses the submit key. A stall longer than 30ms put the entry
   ahead of the offset, the transcript read as silent, and the screen fallback
   confirmed after the whole 4s window. The outcome and the reason still looked
   right — the fallback's reason also contains the word "transcript" — so the
   first assertion to fail was a counter. Reproduced by appending immediately.
4. **A real child process given the budget of an in-memory fake.** The child is
   a shell that re-execs the race-instrumented test binary; its start shows a
   tail (about a second, one start in twelve, against a median of 30ms). A missed
   hello is not retried — the client refuses the child and disables the module
   until the daemon restarts — so one slow start fails the test: 1 run in 10.

## The rule

Ask which **ordering** the test needs, and hang the event off the thing that
guarantees it. A delay is only a guess at an order.

- The entry lands after the offset and inside the window → append it in the
  call that delivers the submit key, not on a timer.
- Two reads of a clock must agree → freeze the clock in the test, and the
  assertion stays exact. A tolerance would only move the flake.
- A count of what fits in a real-time window is the scheduler's business. Assert
  what the window is *for* — the poll ends by itself, and stays ended — and let a
  test with no window own the retry.
- A real process gets a budget of its own, because nothing lets a test control
  how long the host takes to start one.

Check each fix against a mutant of the behaviour the test exists to protect
(make the enqueue shape unrecognisable; make the poll give up on the first miss)
and confirm something goes red. One attempt at a lower-bound assertion did not
catch the give-up-early mutant — the whole `Create` already took longer than the
window — and was dropped rather than left claiming more than it proved.

## What is still not covered

`TestModuleAttach_RecordMissingStopsAtTheWindow` can no longer fail because the
scheduler was slow, and by the same token it no longer fails for a poll that gives
up early; `TestModuleAttach_RecordMissingRetried` owns that. The rig still adds the
pane after `Create` returns, so a future test that tunes the attach window tightly
meets mechanism 2 again.
