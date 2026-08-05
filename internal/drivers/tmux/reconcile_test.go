package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

func listAll() driver.ListFilter { return driver.ListFilter{} }

func statMod(t *testing.T, dir string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, sessionsFileName+".json"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// §12 requires three classifications. The previous implementation could
// produce exactly one, because with no records everything is orphaned by
// definition — it looked implemented and could not do its job.
func TestReconcileDistinguishesAdoptedOrphanedAndVanished(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	// First run: nothing is remembered, so everything found is orphaned.
	first := stateDriver(t, f, dir)
	got, err := first.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orphaned) != 2 || len(got.Adopted) != 0 || len(got.Vanished) != 0 {
		t.Fatalf("first run: %s — everything should be orphaned on a cold start", got)
	}
	for _, s := range got.Orphaned {
		if s.State.Evidence == "" {
			t.Error("§12 requires orphans surfaced with identifying evidence")
		}
	}

	// Second run, same sessions: now they are remembered.
	second := stateDriver(t, f, dir)
	got, err = second.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Adopted) != 2 || len(got.Orphaned) != 0 {
		t.Fatalf("second run: %s — remembered sessions should be adopted", got)
	}

	// One disappears while nothing is watching.
	f.dropLastSession()
	third := stateDriver(t, f, dir)
	got, err = third.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vanished) != 1 {
		t.Fatalf("third run: %s — a remembered session that is gone must be vanished", got)
	}
	v := got.Vanished[0]
	if v.State.Status != fleet.StatusDead {
		t.Errorf("vanished session reported %q, want dead", v.State.Status)
	}
	if v.State.Evidence == "" {
		t.Error("§12 wants evidence that it disappeared while unobserved")
	}
}

// A recycled id is two facts, not one. Reporting only "adopted" would
// attribute one session's history to another (§5.4).
func TestRecycledIdIsBothVanishedAndOrphaned(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	if _, err := stateDriver(t, f, dir).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same name, different start time: a new session wearing an old name.
	f.mu.Lock()
	f.sessions[0].created = 1785699999
	f.mu.Unlock()

	got, err := stateDriver(t, f, dir).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var vanished, orphaned bool
	for _, s := range got.Vanished {
		if s.ID == "alpha💬" {
			vanished = true
		}
	}
	for _, s := range got.Orphaned {
		if s.ID == "alpha💬" {
			orphaned = true
		}
	}
	if !vanished || !orphaned {
		t.Errorf("recycled id produced vanished=%v orphaned=%v; both are true and "+
			"reporting one hides the other", vanished, orphaned)
	}
}

// §12 rule 4 is absolute. Reconciliation must never destroy, and the safest
// guarantee is that the code has no way to.
func TestReconcileDestroysNothing(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)
	if _, err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("reconciliation destroyed a session — §12 rule 4 is absolute")
		}
	}
}

// Records are written when the set changes, not on every read: a read happens
// on every event trigger, and persisting each would be a write amplifier.
func TestRecordsAreWrittenOnChangeNotOnEveryRead(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)
	ctx := context.Background()

	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	before := statMod(t, dir)
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := d.List(ctx, testCaller, listAll()); err != nil {
			t.Fatal(err)
		}
	}
	if after := statMod(t, dir); !after.Equal(before) {
		t.Error("an unchanged session set rewrote the records file on every read")
	}

	f.addSession(fakeSession{name: "delta", paneID: "%40", cwd: "/w", pid: 9, created: 1785600099},
		idleFixtureFor("delta"))
	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}
	if after := statMod(t, dir); after.Equal(before) {
		t.Error("a changed session set did not update the records file")
	}
}

// `since` is the discriminator this service added so nobody has to probe a
// pane to tell "somebody is mid-thought" from "nobody is coming back" (F34).
// It resets on restart unless the value survives — and restarts are how this
// service is deployed, so a 14-hour stall silently reads as minutes old.
func TestSinceSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	f.captures["%2"] = fixtureUnsent // beta holds unsent operator text

	// A service instance sees it, some time ago.
	early := time.Unix(1785600100, 0)
	d1 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return early }))
	if _, err := d1.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// A NEW instance — same store, no in-memory history — reads it much later.
	late := early.Add(14 * time.Hour)
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return late }))
	col, err := d2.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, s := range col.Items() {
		if s.ID != "beta" {
			continue
		}
		found = true
		if s.State.Since == nil {
			t.Fatal("no since at all")
		}
		age := late.Sub(*s.State.Since)
		if age < 13*time.Hour {
			t.Errorf("age = %s, want ~14h — the restart reset it, which is the bug", age)
		}
		// §5.2: an age this instance did not measure must not pass as one it did.
		if !strings.Contains(s.State.Evidence, "restart") {
			t.Errorf("evidence does not say the age is second-hand: %q", s.State.Evidence)
		}
	}
	if !found {
		t.Fatal("beta missing from the listing")
	}
}

// The provenance must survive being cached, not just the first read. It was
// computed correctly and then dropped on the floor: the observation did not
// carry it, so every read after the first presented a second-hand age as one
// this instance had measured — §5.2's exact prohibition, reintroduced by the
// fix for it.
func TestCarriedAgeStaysMarkedOnLaterReads(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent

	early := time.Unix(1785600100, 0)
	d1 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return early }))
	if _, err := d1.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	late := early.Add(3 * time.Hour)
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return late }))

	for read := 1; read <= 3; read++ {
		col, err := d2.List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range col.Items() {
			if s.ID != "beta" {
				continue
			}
			if !strings.Contains(s.State.Evidence, "restart") {
				t.Fatalf("read %d lost the provenance: %q", read, s.State.Evidence)
			}
		}
	}
}
