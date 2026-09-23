# A fleet read that gives up on a peer at a round number is usually the caller's own deadline, not a constant here

**Issue:** #174 (a peer intermittently answering a fleet read in just over
2 s was reported unreachable dozens of times an hour, with a ~2.001 s cliff)

## What happened

The report named a "fixed 2 s peer budget". There is no 2 s deadline in the
read path. The two constants that look right are both wrong:

- `defaultDeadlineMs` (3000) is the remote driver's **floor**, and
  `WithDeadline` sets that floor, not a ceiling.
- `margin` (2 s) is the transit allowance **added** to a peer's declared
  deadline once its capabilities are known. A multiplexer-backed peer
  declares 30 s, so the driver's own bound is then 32 s.

The service gives each peer `min(driver bound, caller deadline)`. The
caller's deadline comes from its `Fleet-Deadline-Ms` header, and §4.4 lets a
caller shorten any call below the driver's floor. So a cliff at exactly
2.00X s means a caller sent `Fleet-Deadline-Ms: 2000`. The error text agrees:
`context deadline exceeded`, not `context canceled`. A client that simply hung
up would read "canceled".

Reproduced on one machine against a peer that accepts connections and never
answers: sending the header with value 2000 reproduces the reported message
`no answer from <peer> after 2.001s` byte for byte. Without the header the
same read gives up at 3 s, the floor.

Tuning `margin` or `defaultDeadlineMs` would have changed nothing. Both only
move a bound that is already above the caller's.

## Why nobody could see it from the daemon

Until #174 the read path never logged at all. `remote.do` built the error
with its measured latency, and `List` folded it into a `SourceStatus` and
returned it as data. The one "no answer from" line an operator did find in
the daemon log came from the startup capability probe, which is a different
code path. The miss rate could only be read off the consumers.

## What to do instead

Every failed peer call now logs one line from the remote driver:

```
remote: peer call failed machine=<m> op="GET /v1/sessions" after=2.001s budget=2s bound=3s on_behalf_of=<principal> kind=deadline err=context deadline exceeded
```

Read `budget` against `bound` before tuning anything:

- `budget < bound`: the caller shortened the deadline. The fix, if one is
  wanted, belongs to the caller named in `on_behalf_of`, not to this service.
- `budget == bound`: this driver's own limit fired. Only then are the
  constants relevant.

`kind=transport` (refused, DNS, reset) is a dead or unreachable peer, not a
slow one.

The line is deliberately unsuppressed. Suppression is how this stayed
invisible.
