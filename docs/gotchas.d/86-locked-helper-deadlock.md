# Calling a locking helper from inside a loop that already holds the same mutex deadlocks silently

**Issue:** #86 (found while wiring per-session create-record lookups into the
local multiplexer driver's `List`)

## What happened

`List`'s per-session row-building loop already holds the driver's single
`sync.Mutex` (`d.mu`) for its whole body — a pre-existing pattern, not new
here. A new per-session record lookup was added inside that loop, and its
first version called the ordinary (locking) form of the helper:
`d.mu.Lock(); defer d.mu.Unlock(); ...`. Called from inside a block that
already held `d.mu`, that is an immediate self-deadlock — `sync.Mutex` is
not reentrant in Go, and there is no error, no panic, no timeout by default:
the goroutine just blocks forever.

**Measured cost of missing this by eye:** two full `go test ./...` runs hung
for 10+ minutes each before being traced and killed by hand (`ps aux | grep
test` + `kill -9` — this environment had no `timeout` binary to bound the
run automatically). Nothing in the test output indicated a deadlock
specifically; it just never finished.

## The fix

Split the helper into two: a locking wrapper for callers that do not already
hold the lock, and an unexported `*Locked` variant that assumes the caller
does. Use the `*Locked` form inside anything already running under `d.mu`.

This driver already had this exact pattern for other per-session lookups
(`sweepCreateRecordsLocked`, `stampSinceLocked`) and one documented near-miss
in the session-rename handler's own comment ("No locking here: exec() already
holds f.mu, and sync.Mutex is not reentrant — taking it again deadlocks the
whole suite, which is exactly what it did.") — this is the same failure
shape recurring, not a new one.

## The rule going forward

Before adding a call to any `func (d *Driver) somethingFor(...)` inside
`List`'s row loop (or any other block already holding `d.mu`), check whether
that function locks internally. If its name doesn't already say `Locked`,
assume it does lock, and either use a `*Locked` sibling or hoist the call
outside the locked section.
