package tmux

import (
	"context"
	"sync"
	"sync/atomic"
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
	// preempt counts respond calls waiting for a session's lock (#180 M4).
	// A send holding the lock polls it at its safe points — before the
	// paste is confirmed, never after Enter — and gives the lock up rather
	// than keep a respond to the very dialog it cannot get past waiting.
	preempt map[string]*atomic.Int32
}

// composerBusyPrefix opens every refusal for a composer lock the caller's
// deadline ran out waiting for. It is stable and documented: the condition
// is transient, and a consumer that treats refused as final can recognise
// this one as retryable by its prefix.
const composerBusyPrefix = "composer busy, retryable: "

// composerBusyReason words that refusal for an operation.
func composerBusyReason(extra string) string {
	return composerBusyPrefix + "another delivery, respond or discard held this session's " +
		"composer until the caller's own deadline ran out; nothing was done — retry" + extra
}

// preemptCounter returns the pre-emption counter for id.
func (d *Driver) preemptCounter(id string) *atomic.Int32 {
	d.composerLocks.mu.Lock()
	defer d.composerLocks.mu.Unlock()
	if d.composerLocks.preempt == nil {
		d.composerLocks.preempt = map[string]*atomic.Int32{}
	}
	c, ok := d.composerLocks.preempt[id]
	if !ok {
		c = &atomic.Int32{}
		d.composerLocks.preempt[id] = c
	}
	return c
}

// lockComposerPreempting is lockComposerOpsCtx for a caller that takes
// priority (Respond): it asks the holder to give the lock up at its next
// safe point, then waits for it like anyone else.
func (d *Driver) lockComposerPreempting(ctx context.Context, id string) (unlock func(), ok bool) {
	c := d.preemptCounter(id)
	c.Add(1)
	defer c.Add(-1)
	return d.lockComposerOpsCtx(ctx, id)
}

// preempted reports whether a priority caller is waiting for id's lock.
func (d *Driver) preempted(id string) bool {
	if id == "" {
		return false
	}
	return d.preemptCounter(id).Load() > 0
}

// aliasComposerLock makes `to` share the SAME composer-serialisation mutex
// `from` already has — creating one for `from` if it does not exist yet —
// so an operation still addressed to the OLD id and a delivery this
// service is about to make against the NEW id (colab-fleet #222's
// title-sync, run immediately after a rename) contend on the identical
// lock rather than two independent ones. Both ids name the same underlying
// pane at the moment this is called from Rename, and this table is keyed
// by id (this file's own package doc), so without the alias they would
// otherwise never know about each other.
//
// Called from Rename BEFORE the multiplexer-level rename runs. Rename
// itself never blocks on this lock — a busy composer must not turn a
// rename into a failure — so this only ever widens what a LATER call
// (Send, Respond, Discard, Keys) contends against; it never adds a wait
// here.
//
// A residual gap, accepted rather than closed: an operation already
// blocked on a lock `to` held before THIS rename — left by a different,
// just-closed session that previously used the same name — is not covered.
// That needs a close of `to`, a pending operation on it, and a rename INTO
// `to` all inside the same narrow window; the pre-existing per-id lock
// table is never pruned (see this file's own doc comment on why), so nothing
// here can tell that case apart from an ordinary live contention. Filed as
// a known limitation rather than engineered around.
func (d *Driver) aliasComposerLock(from, to string) {
	if from == to {
		return
	}
	d.composerLocks.mu.Lock()
	defer d.composerLocks.mu.Unlock()
	if d.composerLocks.locks == nil {
		d.composerLocks.locks = map[string]*sync.Mutex{}
	}
	mu, ok := d.composerLocks.locks[from]
	if !ok {
		mu = &sync.Mutex{}
		d.composerLocks.locks[from] = mu
	}
	d.composerLocks.locks[to] = mu

	// #180 M4's preemption counter follows the same alias, for the same
	// reason: a Respond waiting to preempt the OLD id's holder must still
	// see it after the rename retargets delivery to the NEW id.
	if d.composerLocks.preempt == nil {
		d.composerLocks.preempt = map[string]*atomic.Int32{}
	}
	c, ok := d.composerLocks.preempt[from]
	if !ok {
		c = &atomic.Int32{}
		d.composerLocks.preempt[from] = c
	}
	d.composerLocks.preempt[to] = c
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
