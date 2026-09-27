# A fixed per-actor wait budget does not scale with how many actors are waiting together

**Issue:** #221 (same family as #207/#214/#186)

## What happened

`TestModuleSend_ManySessionsConcurrent` (`internal/drivers/tmux`) creates 6
sessions, then waits for each one's module lane to read live, one at a time:

```go
for _, id := range ids {
    r.waitLive(id)
}
```

`waitLive` is `waitFor` with a fixed 15s bound. The test failed once in a full
`go test -race ./...` run — `timed out waiting for the session's lane to read
live`, at 22.52s — on a host with a load average around 10; it passed alone
(2.8s) and the whole package passed on a re-run (158.7s).

All 6 lanes attach **concurrently** — `moduleHost.launched` starts an async
`attachPoll` goroutine per session the moment it is created, all 6 racing for
the same `moduleHost.mu` and the fake multiplexer's own lock. The per-lane
wait bound (15s) was sized for one lane contending with nothing; it does not
grow when 6 are contending with each other and the rest of a loaded `-race`
run for the same locks. This is the same shape as #214 (a wait bound sized
for a fake, handed to something slower) and #186 (a wall-clock assertion that
is really measuring host scheduling, not the property under test) — just with
the "slower thing" being *N concurrent actors* instead of one real process or
one clock read.

## The rule

A wait whose real cost scales with **how many of it are happening at once**
needs a budget that scales too, not the package's ordinary constant.

- Added `waitForDeadline` (`tmux_test.go`) — `waitFor` with the deadline
  supplied rather than assumed, so a caller with a different real budget does
  not have to reimplement the poll loop.
- Added `waitAllLive` (`modulelane_rig_test.go`) — waits for **all** `n` lanes
  under **one** deadline of `n * waitForTimeout`, checking every lane's
  condition inside the same poll loop rather than looping `waitLive` `n`
  times with `n` independent 15s windows. A healthy run still returns in well
  under a second; only the ceiling nothing should hit got wider.

## What is still not covered

Any other test that loops a fixed-bound `waitFor` over a collection of
concurrently-progressing actors has the same shape and was not audited here;
`TestModuleSend_ManySessionsConcurrent` was the one that actually reddened.
