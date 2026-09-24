package tmux

import (
	"context"
	"testing"
	"time"
)

// TestLockComposerOpsSerialisesTheSameSessionId is D4's own regression test:
// "two concurrent /input calls merged into one user turn". A second caller
// targeting the SAME session id must not proceed until the first releases —
// proven here by ordering markers appended under the lock, deterministically
// (via channels, not a sleep-and-hope race).
func TestLockComposerOpsSerialisesTheSameSessionId(t *testing.T) {
	d := &Driver{}

	firstHasLock := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondAcquired := make(chan struct{})

	go func() {
		unlock := d.lockComposerOps("alpha")
		close(firstHasLock)
		<-releaseFirst
		unlock()
	}()

	<-firstHasLock

	go func() {
		unlock := d.lockComposerOps("alpha")
		close(secondAcquired)
		unlock()
	}()

	select {
	case <-secondAcquired:
		t.Fatal("a second caller for the SAME session id acquired the lock while the first still held it")
	case <-time.After(50 * time.Millisecond):
		// Expected: still blocked.
	}

	close(releaseFirst)

	select {
	case <-secondAcquired:
		// Expected: unblocks once the first releases.
	case <-time.After(2 * time.Second):
		t.Fatal("the second caller never acquired the lock after the first released it")
	}
}

// TestLockComposerOpsDoesNotSerialiseDifferentSessionIds is D4's own scope
// note: per-id locking, not one driver-wide lock — an unrelated session must
// never wait on this one.
func TestLockComposerOpsDoesNotSerialiseDifferentSessionIds(t *testing.T) {
	d := &Driver{}

	unlockA := d.lockComposerOps("alpha")
	defer unlockA()

	done := make(chan struct{})
	go func() {
		unlockB := d.lockComposerOps("beta")
		unlockB()
		close(done)
	}()

	select {
	case <-done:
		// Expected: a different session id never blocks on "alpha"'s lock.
	case <-time.After(2 * time.Second):
		t.Fatal("a caller for a DIFFERENT session id blocked on an unrelated session's lock")
	}
}

// TestLockComposerOpsCountsContentionOnlyWhenItHappens exercises the
// observability half: counterComposerLockContended must fire exactly once
// for the caller that actually had to wait, and not at all for the
// uncontended acquisitions around it.
func TestLockComposerOpsCountsContentionOnlyWhenItHappens(t *testing.T) {
	d := &Driver{}

	unlock1 := d.lockComposerOps("alpha")
	unlock1()
	if got := d.counters.Snapshot()[counterComposerLockContended]; got != 0 {
		t.Fatalf("counterComposerLockContended = %d after an uncontended acquisition, want 0", got)
	}

	firstHasLock := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondDone := make(chan struct{})

	go func() {
		unlock := d.lockComposerOps("alpha")
		close(firstHasLock)
		<-releaseFirst
		unlock()
	}()
	<-firstHasLock

	go func() {
		unlock := d.lockComposerOps("alpha")
		unlock()
		close(secondDone)
	}()

	// Give the second goroutine time to actually reach and block on the
	// lock before releasing the first — otherwise the assertion below could
	// pass on a scheduling accident rather than genuine contention.
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)

	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquisition never completed")
	}

	if got := d.counters.Snapshot()[counterComposerLockContended]; got != 1 {
		t.Errorf("counterComposerLockContended = %d, want 1", got)
	}
}

// TestLockComposerOpsCtxRespectsCallerDeadline is the review's regression
// test for D4's own lock ignoring the caller's deadline: a plain `mu.Lock()`
// blocks a caller out no matter how short a deadline it declared, which the
// review measured directly (a 100ms Keys(Escape) call blocked 3.87s behind
// an unconfirmable send). A caller whose own ctx expires while waiting must
// get a fast, honest "busy" answer instead of being held past its own
// declared patience.
func TestLockComposerOpsCtxRespectsCallerDeadline(t *testing.T) {
	d := &Driver{}

	firstHasLock := make(chan struct{})
	releaseFirst := make(chan struct{})
	go func() {
		unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha")
		if !ok {
			t.Error("the first, uncontended acquisition must succeed")
			return
		}
		close(firstHasLock)
		<-releaseFirst
		unlock()
	}()
	<-firstHasLock
	defer close(releaseFirst)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, ok := d.lockComposerOpsCtx(ctx, "alpha")
	elapsed := time.Since(start)

	if ok {
		t.Fatal("lockComposerOpsCtx succeeded even though the caller's deadline should have run out first")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("lockComposerOpsCtx took %s to refuse — a caller's own short deadline must not be "+
			"held hostage by an unrelated, still-in-progress call on the same session", elapsed)
	}
	if got := d.counters.Snapshot()[counterComposerLockDeadlineExceeded]; got != 1 {
		t.Errorf("counterComposerLockDeadlineExceeded = %d, want 1", got)
	}
}

// TestLockComposerOpsCtxEventuallyReleasesTheMutexAfterATimedOutWaiter proves
// the losing waiter's own background goroutine (terminalpath2_lock.go) does
// not leak the mutex locked forever: once the first holder releases, a THIRD
// caller must still be able to acquire the same session id, even though the
// second caller gave up on it first.
func TestLockComposerOpsCtxEventuallyReleasesTheMutexAfterATimedOutWaiter(t *testing.T) {
	d := &Driver{}

	firstHasLock := make(chan struct{})
	releaseFirst := make(chan struct{})
	go func() {
		unlock, _ := d.lockComposerOpsCtx(context.Background(), "alpha")
		close(firstHasLock)
		<-releaseFirst
		unlock()
	}()
	<-firstHasLock

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := d.lockComposerOpsCtx(ctx, "alpha"); ok {
		t.Fatal("setup: the second caller should have timed out")
	}

	close(releaseFirst)

	done := make(chan struct{})
	go func() {
		unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha")
		if ok {
			unlock()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a third caller never acquired the lock — the timed-out waiter's own goroutine " +
			"must still release the mutex once it actually gets it")
	}
}
