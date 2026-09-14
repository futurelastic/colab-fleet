# A new request on the peer probe must be excluded in two places, or it re-probes itself and races the tests

**Issue:** #154 (adding `GET /v1/whoami` to the remote driver's capability probe)

## The trap

The remote driver re-probes a peer opportunistically: any request that gets a
domain answer calls `noteSuccessfulContact`, which starts `RefreshCapabilities`
in a goroutine when the cached capabilities are unseen or stale. The probe's
OWN requests are excluded from that hook by path in `do()` — otherwise a probe
triggers a probe.

The fake peer in `internal/drivers/remote/remote_test.go` (`peerServing`)
carries a matching exclusion: it does not record probe paths into the test's
shared `capture`, because the background probe runs concurrently with the
operation under test and writing the same struct from both goroutines is a
data race — one that `go test -race` catches only intermittently, and one that
also silently overwrites what the test meant to capture.

Adding a request to the probe without updating **both** lists brings the race
back, and nothing fails deterministically.

## The rule

When `RefreshCapabilities` (or anything it calls) gains a request:

1. add its path to the exclusion in `do()` — note the path may carry a query
   string, so match by prefix (`/v1/whoami?peer=…`);
2. add it to `probing` in `peerServing`.

Both lists currently hold `/v1/runtimes`, `/v1/health` and `/v1/whoami`.
