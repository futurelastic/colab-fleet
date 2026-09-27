# A shared fixed timeout in a table-driven test is only as tight as its neediest case

**Issue:** #225

## What happened

`TestClient_HelloRefused/OversizeLine` (`internal/delivery/modclient`) failed
trunk CI twice in ~12 h under `go test -race`, both times on a commit that
never touched this package: `no hello line within 150ms` — which is the
`Silence` subtest's own expected reason, not `OversizeLine`'s (`"line
limit"`). It passed alone and on re-run.

All six `TestClient_HelloRefused` subtests shared one `cfg.HelloTimeout`
(150ms). For five of them that number only has to outlast parsing a few bytes
of JSON — effectively free. `OversizeLine` is different: it sends a 5 MB raw
line, which the fake's in-memory pipe and the client's `ReadLine` move through
dozens of synchronous Read/Write handoffs (a 64 KB `bufio.Reader`), not a
constant-time parse. Under `-race`, with the rest of the suite also running,
that transfer itself can pass 150ms — so the client's own `HelloTimeout` timer
wins the race against the pipe transfer, and the module is refused for
"no hello line" before the oversize check is ever reached.

## The rule

A fixed-duration knob (timeout, deadline, sleep) shared across a
table-driven test's cases is sized correctly only for whichever case needs the
*least* real time. A case that does meaningfully more real work — a large
payload, more round trips, more contention — needs its own, larger budget; do
not assume the table's shared value covers it just because the test passed
locally or in a quiet CI run.

This is the same lesson as #214 (`docs/gotchas.d/214-…`), arriving from the
opposite direction: #214 was a *real subprocess* being slow to start under
`-race`, handed a budget sized for an in-memory fake. Here it is an
*in-memory fake* doing real, non-trivial I/O work, handed a budget sized for
the tiny cases sitting next to it in the same table. Either way, the fix is
the same shape: give the outlier case its own generous backstop (here, 5s
instead of 150ms, with the test's `waitFor` bound scaled to match) rather than
loosening the shared constant for every case, which would only weaken the
`Silence` case's actual test of "the timer really does fire."

## What is still not covered

The other five `HelloRefused` subtests keep the tight 150ms window. If a
future subtest is added that also has to move a non-trivial payload before
the client sees a complete hello line, it needs the same per-case treatment —
nothing here makes that automatic.
