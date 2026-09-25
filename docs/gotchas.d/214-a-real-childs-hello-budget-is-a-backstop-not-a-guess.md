# A real child's hello budget is a backstop, not a guess — and a dead wait blames the wrong clock

**Issue:** #214 (the second bite of #207, mechanism 4)

## What happened

Two real-process tests failed by timeout in a full `go test -race ./...` run and
passed alone (`ok … 16s`), on a host where the whole suite ran in parallel:

- `process_test.go:89: timed out after 15s waiting for the real child to say hello`
- `process_test.go:204: timed out after 15s waiting for the module`

Both wait on `c.Usable` with a 15 s bound, so 15 s read like the budget that ran
out. It was not. The bound that decides is the client's own **`HelloTimeout`**,
which the real-process rig had set to 5 s after #207 ("a cold test binary under
`-race` is slow to start"). A missed hello is terminal — the client refuses the
child and the module stays `StateDisabled` until the daemon restarts — so after
5 s `Usable` can never become true, the remaining 10 s of the wait is a dead
wait, and the failure names the wait's bound instead of the budget that was
spent.

Two budgets were sized for an in-memory fake and handed to a real process:

1. **Hello (5 s).** One slow start is enough. Reproduced deterministically, with
   the same two messages at the same two lines, by making the child start 6 s
   late (a `sleep 6` ahead of the exec in the wrapper `modtest.Install` writes).
   Status afterwards: `disabled`, reason `hello refused: no hello line within 5s`.
2. **Request deadline (2 s).** The first health probe after the hello has to be
   answered inside `Deadlines.Default`; two misses in a row mark the child broken
   and it is restarted — into another hello and another first probe. A child
   whose health answer takes 2.5 s never becomes usable and never disables
   either, so it waits out the whole bound. Reproduced with
   `OpScript{DelayMs: 2500}` on `health`. The same 2 s also bounded the
   `Send` the first test makes to the live child.

## The rule

Give a real process a budget that is a **backstop** — long enough that the host
cannot be the reason it is missed — and hang the wait on the event, not on a
number.

- `realClient` sets `HelloTimeout` to 60 s and the request deadlines to 90 s;
  a test that passes never waits for either.
- The waits on a real child's start go through `waitChild` / `waitUsable`, whose
  outer bound (90 s) is longer than the hello budget, and which fail **at once**
  with the client's own reason when the client disables the module — nothing
  can make the wait's condition true after that, so waiting out the bound only
  hides the reason. The first check of a mutant is now legible: with the hello
  budget put back to 5 s the same two tests fail after 5 s, saying
  `the client disabled the module for good: hello refused: no hello line within 5s`.
- A budget that the code under test enforces itself and never retries (the hello)
  is the one to size first; the test's own wait bound only has to exceed it.

## What is still not covered

Waits that are still short bounds on the daemon's own reaction once the child is
up (the 10 s for an in-flight request to fail after a kill, the 5 s follow-up
waits in the same file) are events in this process rather than in the child, and
have not been observed to fail; they were left as they were.
