package tmux

import (
	"context"
	"sync"
)

// composerLocks is terminal-path-v2's fix for D5 ("no per-session
// serialisation: two concurrent /input calls merged into one user turn,
// both 'queued'"): one mutex per session id, held for the full duration of
// every composer-touching operation on that session — Send, Respond,
// Discard, Keys (the four callers this file's own doc comment on
// lockComposerOps names). Two callers targeting DIFFERENT sessions never
// contend; two callers racing the SAME session's composer are serialised
// into "second one waits, then sees whatever the first one left", which is
// an ordinary, honest outcome — never the silent merge D5 measured.
//
// A plain map keyed by session id, not sync.Map: this driver already holds
// far fewer live sessions than would make a mutex-guarded map's own lock a
// bottleneck (every other per-session table in this driver — d.observed,
// d.stranded, d.delivered — is exactly this shape, guarded by d.mu), and
// reusing that same discipline here keeps this file's own locking
// vocabulary consistent with the rest of the package rather than
// introducing a second one.
type composerLockTable struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lockComposerOps acquires (creating if needed) the mutex for one session id
// and returns the function that releases it. Every composer-touching entry
// point acquires this as its very first act and releases it via defer,
// before anything else in the call — including the #53/#112 guards Send
// already ran ahead of everything, which stay ahead of this lock too, since
// they decide on the request's own bytes and flags alone and never touch a
// pane (see Send's own doc comment on why those run first).
//
// # Why one lock per id rather than one lock for the whole driver
//
// A single driver-wide lock would serialise EVERY session's input through
// one queue, turning an unrelated session's slow confirm-and-poll loop
// (confirmLandedV2/confirmSubmittedV2, each bounded by submitConfirmWindow)
// into added latency for a caller that never touched that session. Per-id
// locking keeps D4's fix scoped to what D4 actually reported: two callers
// racing the SAME composer, not two callers using the fleet at the same
// moment.
//
// The lock table itself is never pruned. A session that closes leaves a
// harmless, never-again-contended *sync.Mutex behind; pruning it correctly
// (only once nothing holds it, and only once nothing can ever ask for it
// again) is more machinery than a handful of abandoned mutexes justifies —
// the same judgement call this driver already makes for d.observed entries
// of long-dead sessions.
func (d *Driver) lockComposerOps(id string) (unlock func()) {
	// Kept for callers with no caller-supplied deadline of their own to
	// respect (there are none left in this package — see lockComposerOpsCtx
	// below — but a bare, always-succeeds acquire is a one-line function
	// worth keeping separate from the ctx-aware one rather than folding a
	// context.Background() call into every remaining test that exercises
	// the table itself, see terminalpath2_lock_test.go).
	unlock, _ = d.lockComposerOpsCtx(context.Background(), id)
	return unlock
}

// lockComposerOpsCtx is lockComposerOps' review-fixed replacement: acquiring
// this driver's per-session composer lock used to block on a bare
// `mu.Lock()`, which does not look at the caller's own deadline at all. A
// caller retrying an unconfirmable send holds the lock for up to roughly two
// confirmation windows per attempt (confirmLandedV2 plus confirmSubmittedV2,
// each bounded by submitConfirmWindow) while every OTHER call against that
// same session's composer — critically, a Keys(Escape) meant to dismiss a
// dialog, or a Discard meant to clear a stuck line — queued behind it with
// no way to give up early. Measured: a 100ms-deadline Keys(Escape) call
// blocked 3.87s behind an unconfirmable send in the review's own test.
//
// ok=false means the caller's ctx was done before the lock could be
// acquired — a fast, honest "busy" refusal instead of an indefinite wait.
// This never means the SESSION is unusable, only that this particular call
// could not get a turn inside its own deadline; a retry is ordinary.
func (d *Driver) lockComposerOpsCtx(ctx context.Context, id string) (unlock func(), ok bool) {
	d.composerLocks.mu.Lock()
	if d.composerLocks.locks == nil {
		d.composerLocks.locks = map[string]*sync.Mutex{}
	}
	mu, exists := d.composerLocks.locks[id]
	if !exists {
		mu = &sync.Mutex{}
		d.composerLocks.locks[id] = mu
	}
	d.composerLocks.mu.Unlock()

	// A non-blocking probe first, purely so contention has a counter
	// (counterComposerLockContended) independent of how long the wait ends
	// up being — the same "count it even though it degrades gracefully"
	// discipline #44 already established for a retry (see counters.go).
	if mu.TryLock() {
		return mu.Unlock, true
	}
	d.counters.incr(counterComposerLockContended)

	// The blocking acquire happens on its own goroutine so this call can
	// still race it against ctx.Done() — sync.Mutex has no ctx-aware Lock of
	// its own. Losing the race leaves that goroutine's Lock() call pending;
	// it still runs to completion and takes the mutex eventually (a mutex
	// cannot be un-asked-for), so the goroutine unlocks it again right away
	// on the very next line rather than holding a lock this call already
	// gave up on and nobody will ever release otherwise.
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return mu.Unlock, true
	case <-ctx.Done():
		d.counters.incr(counterComposerLockDeadlineExceeded)
		go func() {
			<-acquired
			mu.Unlock()
		}()
		return func() {}, false
	}
}
