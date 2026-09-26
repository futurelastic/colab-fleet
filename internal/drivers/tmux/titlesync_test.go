package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// titleSyncSession is transcriptSession (slashcommand_test.go) plus the one
// thing these tests need that it does not expose: the transcript's own
// path, so a test can read the runtime's LAST custom-title back directly
// (the reconciler tests) rather than only through a Send call's own
// confirmation. Kept as its own helper rather than widening
// transcriptSession's return shape, which every existing call site already
// destructures positionally.
func titleSyncSession(t *testing.T) (d *Driver, f *fakeMux, ref fleet.SessionRef, recordOnSubmit func(map[string]any), convPath string) {
	t.Helper()
	root := t.TempDir()
	const cwd, name = "/work/alpha", "alpha💬"
	dir := filepath.Join(root, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(dir, "conv-1.jsonl")
	line := func(v map[string]any) string {
		v["timestamp"] = time.Now().Format(time.RFC3339Nano)
		v["sessionId"] = "conv-1"
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	if err := os.WriteFile(conv, []byte(line(map[string]any{"type": "custom-title", "customTitle": name})), 0o600); err != nil {
		t.Fatal(err)
	}
	f = twoSessions()
	var mu sync.Mutex
	var queued []map[string]any
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if isSubmitKeystroke(args) {
			mu.Lock()
			entries := queued
			queued = nil
			mu.Unlock()
			for _, v := range entries {
				appendLine(t, conv, strings.TrimSuffix(line(v), "\n"))
			}
		}
		return out, err
	}
	d = New("testbox", withExec(run), withNonce(func() string { return testNonce }),
		withClock(time.Now), WithRecordRoot(root))
	recordOnSubmit = func(v map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		queued = append(queued, v)
	}
	return d, f, fleet.SessionRef{Machine: "testbox", ID: name}, recordOnSubmit, conv
}

// startedAtFixture is twoSessions()'s "alpha💬" own created time — the value
// every Rename call below corroborates against, the same convention
// TestRenameCorroboratesLikeClose already uses.
func startedAtFixture() time.Time { return time.Unix(1785600000, 0) }

func withStartedAt(req fleet.Request, t time.Time) fleet.Request {
	req.Expect.StartedAt = &t
	return req
}

// TestSyncTitleBringsTheRuntimeTitleToTheNewID is colab-fleet#222's own
// done-when #1: a rename through the API leaves the runtime reporting the
// new title, confirmed against a recorded transcript.
func TestSyncTitleBringsTheRuntimeTitleToTheNewID(t *testing.T) {
	d, f, ref, recordOnSubmit, conv := titleSyncSession(t)
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"

	if _, err := d.Rename(context.Background(), req, ref, to); err != nil {
		t.Fatalf("rename: %v", err)
	}

	recordOnSubmit(localCommandEcho("rename", to))
	recordOnSubmit(localCommandResult("rename", to))
	recordOnSubmit(map[string]any{"type": "custom-title", "customTitle": to})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got := d.SyncTitle(ctx, req, fleet.SessionRef{Machine: "testbox", ID: to})

	if got.Status != fleet.TitleSynced {
		t.Fatalf("status = %s, evidence = %q, want synced", got.Status, got.Evidence)
	}
	if got.Receipt == nil || got.Receipt.Outcome != fleet.OutcomeQueued {
		t.Fatalf("receipt = %+v, want a queued (transcript-confirmed) outcome", got.Receipt)
	}
	if len(f.pasteLog) != 1 || f.pasteLog[0] != "/rename "+to {
		t.Fatalf("pasted %v, want exactly one paste of %q", f.pasteLog, "/rename "+to)
	}
	scan, err := transcriptTitleScan(conv, 0, "/rename "+to)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !scan.sawTitle || scan.title != to {
		t.Fatalf("the transcript's own last custom-title is %q (saw=%v), want %q", scan.title, scan.sawTitle, to)
	}
}

// TestSyncTitleControlCharacterFailsWithoutSending: an unusable name is
// refused before anything is enumerated or pasted — never a claim that a
// delivery was attempted.
func TestSyncTitleControlCharacterFailsWithoutSending(t *testing.T) {
	d := newTestDriver(twoSessions())
	got := d.SyncTitle(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha\x00💬"})
	if got.Status != fleet.TitleFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.Receipt != nil {
		t.Fatalf("receipt = %+v, want nil — nothing should have been sent", got.Receipt)
	}
}

// TestSyncTitleDoesNotOverwriteAPersonsDraft: the composer already holds
// unsent text nobody typed on this driver's behalf. The rename itself
// already succeeded; the title half must refuse, exactly as an ordinary
// /input would, and paste nothing.
func TestSyncTitleDoesNotOverwriteAPersonsDraft(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = fixtureUnsent // "alpha💬"'s pane, a human draft sitting there
	d := newTestDriver(f)
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"
	if _, err := d.Rename(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, to); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got := d.SyncTitle(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: to})
	if got.Status != fleet.TitleFailed {
		t.Fatalf("status = %s, evidence = %q, want failed", got.Status, got.Evidence)
	}
	if got.Receipt == nil || got.Receipt.Outcome != fleet.OutcomeRefused {
		t.Fatalf("receipt = %+v, want a refused outcome", got.Receipt)
	}
	if len(f.pasteLog) != 0 {
		t.Fatalf("pasted %v, want nothing — a person's draft must never be overwritten", f.pasteLog)
	}
}

// TestSyncTitleReportsFailedWhenADifferentTitleWasRecorded: the runtime
// recorded SOME custom-title after this delivery, but not the one asked
// for. Never confirmed as synced on the strength of the command alone.
func TestSyncTitleReportsFailedWhenADifferentTitleWasRecorded(t *testing.T) {
	d, _, ref, recordOnSubmit, conv := titleSyncSession(t)
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"
	if _, err := d.Rename(context.Background(), req, ref, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	recordOnSubmit(localCommandEcho("rename", to))
	recordOnSubmit(localCommandResult("rename", to))
	recordOnSubmit(map[string]any{"type": "custom-title", "customTitle": "something-else"})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got := d.SyncTitle(ctx, req, fleet.SessionRef{Machine: "testbox", ID: to})
	if got.Status != fleet.TitleFailed {
		t.Fatalf("status = %s, evidence = %q, want failed", got.Status, got.Evidence)
	}
	if !strings.Contains(got.Evidence, "something-else") {
		t.Fatalf("evidence %q should name the title actually recorded", got.Evidence)
	}
	scan, _ := transcriptTitleScan(conv, 0, "/rename "+to)
	if scan.title != "something-else" {
		t.Fatalf("setup sanity: transcript title = %q", scan.title)
	}
}

// TestSyncTitlePendingWhenTheCommandRanButNoTitleFollowed: the runtime
// confirmed it ran /rename (colab-fleet#187's own local_command evidence),
// but no custom-title entry followed within the confirmation window — this
// repo has not measured whether/when that ever happens, so the honest
// answer is pending, never a guessed synced or failed. A short caller
// deadline keeps the test from paying the full confirmation window twice
// over (once inside Send, once inside SyncTitle's own poll).
func TestSyncTitlePendingWhenTheCommandRanButNoTitleFollowed(t *testing.T) {
	d, _, ref, recordOnSubmit, _ := titleSyncSession(t)
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"
	if _, err := d.Rename(context.Background(), req, ref, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	recordOnSubmit(localCommandEcho("rename", to))
	recordOnSubmit(localCommandResult("rename", to))
	// Deliberately no custom-title entry queued.

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	got := d.SyncTitle(ctx, req, fleet.SessionRef{Machine: "testbox", ID: to})
	if got.Status != fleet.TitlePending {
		t.Fatalf("status = %s, evidence = %q, want pending", got.Status, got.Evidence)
	}
	if !strings.Contains(got.Evidence, "ran") {
		t.Fatalf("evidence %q should say the command ran but no title followed", got.Evidence)
	}
}

// TestSyncTitlePendingWhenNoTranscriptCanBeResolved: no record root
// configured at all (this driver's off-by-default contract, WithRecordRoot)
// — SyncTitle still sends the command (it may well land), but can prove
// nothing about the runtime's title, and says so rather than guessing
// either way.
func TestSyncTitlePendingWhenNoTranscriptCanBeResolved(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f) // no WithRecordRoot: d.conversations == nil
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"
	if _, err := d.Rename(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got := d.SyncTitle(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: to})
	if got.Status != fleet.TitlePending {
		t.Fatalf("status = %s, evidence = %q, want pending", got.Status, got.Evidence)
	}
	if len(f.pasteLog) != 1 {
		t.Fatalf("pasted %v, want exactly one attempt — this driver still tries even with nothing to confirm against", f.pasteLog)
	}
}

// TestTranscriptTitleScan is a pure unit test of the scanner: the LAST
// custom-title after the offset wins, an entry before the offset is
// ignored, and a matching local_command entry sets ran independently of
// whether any title was ever recorded.
func TestTranscriptTitleScan(t *testing.T) {
	writeConv := func(t *testing.T, before int, entries ...map[string]any) (string, int64) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "conv.jsonl")
		var b strings.Builder
		var offset int64
		for i, e := range entries {
			if i == before {
				offset = int64(b.Len())
			}
			line, _ := json.Marshal(e)
			b.Write(line)
			b.WriteByte('\n')
		}
		if before >= len(entries) {
			offset = int64(b.Len())
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return path, offset
	}

	t.Run("last title wins", func(t *testing.T) {
		path, offset := writeConv(t, 0,
			map[string]any{"type": "custom-title", "customTitle": "first"},
			map[string]any{"type": "custom-title", "customTitle": "second"})
		got, err := transcriptTitleScan(path, offset, "")
		if err != nil || !got.sawTitle || got.title != "second" {
			t.Fatalf("got %+v (err %v), want title=second", got, err)
		}
	})

	t.Run("pre-offset entries are ignored", func(t *testing.T) {
		path, offset := writeConv(t, 1,
			map[string]any{"type": "custom-title", "customTitle": "before-offset"})
		got, err := transcriptTitleScan(path, offset, "")
		if err != nil || got.sawTitle {
			t.Fatalf("got %+v (err %v), want no title seen", got, err)
		}
	})

	t.Run("a matching local_command entry sets ran", func(t *testing.T) {
		path, offset := writeConv(t, 0, localCommandResult("rename", "x"))
		got, err := transcriptTitleScan(path, offset, "/rename x")
		if err != nil || !got.ran || got.sawTitle {
			t.Fatalf("got %+v (err %v), want ran=true sawTitle=false", got, err)
		}
	})
}

// TestRenameSharesTheComposerLockWithTheNewID proves aliasComposerLock's own
// job: an operation already holding the OLD id's lock and a fresh attempt
// on the NEW id contend on the SAME mutex, without Rename itself ever
// blocking on it.
func TestRenameSharesTheComposerLockWithTheNewID(t *testing.T) {
	d := newTestDriver(twoSessions())
	req := withStartedAt(testCaller, startedAtFixture())
	const to = "alpha-renamed💬"

	unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha💬")
	if !ok {
		t.Fatal("setup: could not take the old id's lock")
	}

	renameDone := make(chan struct{})
	go func() {
		if _, err := d.Rename(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, to); err != nil {
			t.Errorf("rename: %v", err)
		}
		close(renameDone)
	}()
	select {
	case <-renameDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Rename must not block on the composer lock")
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := d.lockComposerOpsCtx(shortCtx, to); ok {
		t.Fatal("the new id's lock should be held too, via the alias — it must not have been free")
	}

	unlock()
	unlock2, ok := d.lockComposerOpsCtx(context.Background(), to)
	if !ok {
		t.Fatal("once the old id's holder released, the new id's (aliased) lock should be free")
	}
	unlock2()
}

// TestASessionCommandLeavesTheTurnsDenominatorAlone: a session-management
// command produces no agent turn, so it must never mark the #111 delivery
// mark; an ordinary message still does.
func TestASessionCommandLeavesTheTurnsDenominatorAlone(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	if _, err := d.Send(context.Background(), testCaller, ref, "/rename ignored-target", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, ok := d.deliveryMarkFor("alpha💬", "/work/alpha"); ok {
		t.Fatal("a session-management command must not set the delivery mark")
	}

	if _, err := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, ok := d.deliveryMarkFor("alpha💬", "/work/alpha"); !ok {
		t.Fatal("an ordinary message must still set the delivery mark")
	}
}

// reconcilerRevert is the "fake reconciler" colab-fleet#222's own done-when
// #2 asks for: a client that trusts the runtime's OWN title over the
// multiplexer's, and renames back on disagreement. Returns whether it
// reverted anything this pass.
func reconcilerRevert(t *testing.T, d *Driver, req fleet.Request, convPath string, currentID *string) bool {
	t.Helper()
	scan, err := transcriptTitleScan(convPath, 0, "")
	if err != nil {
		t.Fatalf("reconciler: reading the transcript: %v", err)
	}
	if !scan.sawTitle || scan.title == *currentID {
		return false
	}
	if _, err := d.Rename(context.Background(), req, fleet.SessionRef{Machine: "testbox", ID: *currentID}, scan.title); err != nil {
		t.Fatalf("reconciler: reverting: %v", err)
	}
	*currentID = scan.title
	return true
}

// TestAReconcilerTrustingTheRuntimeTitleDoesNotRevertAnAPIRename is
// colab-fleet#222's done-when #2. The "reproduces" subtest is the negative
// control this repo's own gotchas file (#212) asks for: without it, a
// passing "fixed" subtest would be equally consistent with "the reconciler
// never actually checks anything".
func TestAReconcilerTrustingTheRuntimeTitleDoesNotRevertAnAPIRename(t *testing.T) {
	const to = "alpha-renamed💬"

	t.Run("reproduces #222: Rename alone reverts", func(t *testing.T) {
		d, _, ref, _, conv := titleSyncSession(t)
		req := withStartedAt(testCaller, startedAtFixture())
		if _, err := d.Rename(context.Background(), req, ref, to); err != nil {
			t.Fatalf("rename: %v", err)
		}
		current := to
		if !reconcilerRevert(t, d, req, conv, &current) {
			t.Fatal("the reconciler should have found a disagreement and reverted")
		}
		if current != "alpha💬" {
			t.Fatalf("current = %q, want reverted to the original name", current)
		}
	})

	t.Run("fixed: Rename + SyncTitle survives 5 reconciler intervals", func(t *testing.T) {
		d, _, ref, recordOnSubmit, conv := titleSyncSession(t)
		req := withStartedAt(testCaller, startedAtFixture())
		if _, err := d.Rename(context.Background(), req, ref, to); err != nil {
			t.Fatalf("rename: %v", err)
		}
		recordOnSubmit(localCommandEcho("rename", to))
		recordOnSubmit(localCommandResult("rename", to))
		recordOnSubmit(map[string]any{"type": "custom-title", "customTitle": to})

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got := d.SyncTitle(ctx, req, fleet.SessionRef{Machine: "testbox", ID: to})
		cancel()
		if got.Status != fleet.TitleSynced {
			t.Fatalf("status = %s, evidence = %q, want synced before the reconciler runs at all", got.Status, got.Evidence)
		}

		current := to
		for i := 0; i < 5; i++ {
			if reconcilerRevert(t, d, req, conv, &current) {
				t.Fatalf("reconciler interval %d reverted the rename; it must see no disagreement", i)
			}
		}
		if current != to {
			t.Fatalf("current = %q, want %q — unchanged across every interval", current, to)
		}
	})
}
