package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// End-to-end coverage for #56: the driver-level upgrade from the runtime's
// own on-disk record, exercised through List/State exactly as the polling
// loop and a targeted read call them — not just latestAPIError in
// isolation (runtimerecord_test.go already covers that unit).

// writeConversationWithAPIError lays down a record the same way writeRecord
// (conversation_test.go) does — title first, dated so conversation.go's
// identity match resolves — then appends one assistant entry carrying the
// runtime's own report of a refusal or a clean turn, at a caller-chosen
// timestamp.
func writeConversationWithAPIError(t *testing.T, root, cwd, id, title string, began time.Time, apiErrorLine map[string]any) {
	t.Helper()
	dir := filepath.Join(root, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []any{
		map[string]any{"type": "custom-title", "customTitle": title, "sessionId": id},
		map[string]any{"type": "mode", "mode": "normal", "sessionId": id},
		map[string]any{"type": "user", "sessionId": id, "timestamp": began.UTC().Format(time.RFC3339Nano)},
		apiErrorLine,
	}
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rateLimitEntry(id string, at time.Time, resetHint string) map[string]any {
	return map[string]any{
		"type":              "assistant",
		"sessionId":         id,
		"timestamp":         at.UTC().Format(time.RFC3339Nano),
		"error":             "rate_limit",
		"isApiErrorMessage": true,
		"apiErrorStatus":    429,
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "You've hit your session limit · resets " + resetHint},
			},
		},
	}
}

func serverErrorEntry(id string, at time.Time) map[string]any {
	return map[string]any{
		"type":              "assistant",
		"sessionId":         id,
		"timestamp":         at.UTC().Format(time.RFC3339Nano),
		"error":             "server_error",
		"isApiErrorMessage": true,
		"apiErrorStatus":    529,
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "API Error: 529 Overloaded. This is a server-side issue, " +
					"usually temporary — try again in a moment. If it persists, check https://status.claude.com."},
			},
		},
	}
}

// turnDurationEntry is the runtime's own, unasked, structural turn-boundary
// marker — colab-fleet #111's whole provenance argument rests on this being
// a DIFFERENT kind of entry from every one of the api-error/clean-turn
// entries above: none of them carries anything the agent chose to say.
func turnDurationEntry(id string, at time.Time) map[string]any {
	return map[string]any{
		"type":      "system",
		"subtype":   "turn_duration",
		"sessionId": id,
		"timestamp": at.UTC().Format(time.RFC3339Nano),
	}
}

func cleanTurnEntry(id string, at time.Time) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"sessionId": id,
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Done."},
			},
		},
	}
}

func sessionOf(t *testing.T, col fleet.Collection[fleet.Session], id string) fleet.Session {
	t.Helper()
	for _, s := range col.Items() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %q not in listing", id)
	return fleet.Session{}
}

// The headline fix: Since is the refusal's own timestamp, not the moment
// this driver's poll happened to run — which, in this fixture, is a full
// two hours later than the refusal itself.
func TestQuotaSinceComesFromTheRuntimesOwnRecord(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your session limit · resets 9:50pm (Asia/Saigon)\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		rateLimitEntry("conv-beta", refusedAt, "9:50pm (Asia/Saigon)"))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	beta := sessionOf(t, col, "beta")
	if beta.State.Status != fleet.StatusQuotaBlocked {
		t.Fatalf("status = %s, want quota_blocked", beta.State.Status)
	}
	if beta.State.Quota == nil {
		t.Fatal("no quota block on a blocked session")
	}
	if !beta.State.Quota.Since.Equal(refusedAt) {
		t.Errorf("since = %v, want the record's own refusal time %v (not the read time)",
			beta.State.Quota.Since, refusedAt)
	}
	if !strings.Contains(beta.State.Evidence, "the runtime's own record of the refusal") {
		t.Errorf("evidence does not say since came from the record: %q", beta.State.Evidence)
	}
}

// Same shape, but with NO record store configured (the ordinary case for a
// test driver, and for any substrate this feature was never pointed at).
// The fallback must stay exactly what it was before #56: first sighting,
// honestly labelled as such rather than presented as the refusal time.
func TestQuotaSinceFallsBackHonestlyWithNoRecordStore(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your session limit · resets 9:50pm (Asia/Saigon)\n" +
		rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	beta := sessionOf(t, col, "beta")
	if beta.State.Quota == nil {
		t.Fatal("no quota block on a blocked session")
	}
	if !strings.Contains(beta.State.Evidence, "could not confirm it") {
		t.Errorf("evidence does not admit the record could not confirm since: %q", beta.State.Evidence)
	}
	if strings.Contains(beta.State.Evidence, "the runtime's own record of the refusal") {
		t.Errorf("evidence claims record confirmation with no record store configured: %q", beta.State.Evidence)
	}
}

// #56's "coming out" rule: the record showing the account's most recent
// turn actually succeeded is durable, positive proof — sufficient on its
// own to clear the block, the same authority sawWorking already has,
// without needing some OTHER session to be caught mid-spinner.
func TestQuotaBlockClearsWhenTheRecordShowsTheAccountRecovered(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	// beta still shows the (now stale) notice; nothing else in the fleet is
	// observed working this cycle.
	f.captures["%2"] = "transcript\nYou've hit your session limit · resets 9:50pm (Asia/Saigon)\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	recoveredAt := sessionStart.Add(30 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		rateLimitEntry("conv-beta", refusedAt, "9:50pm (Asia/Saigon)"))
	// A later, successful turn supersedes the refusal — appended after the
	// helper's own single api-error line by writing the file a second time
	// through the same low-level append the helper uses.
	appendRecordLine(t, root, "/work/beta", "conv-beta", cleanTurnEntry("conv-beta", recoveredAt))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	beta := sessionOf(t, col, "beta")
	if beta.State.Status == fleet.StatusQuotaBlocked {
		t.Errorf("still blocked after the session's own record showed a later successful turn: %+v", beta.State)
	}
}

func appendRecordLine(t *testing.T, root, cwd, id string, line map[string]any) {
	t.Helper()
	path := filepath.Join(root, recordDirFor(cwd), id+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

// LastTurn's Retryable/Reason now come from the record's full text when a
// record resolves, rather than the screen's window-bounded scrape — same
// word rule (retryableWords), better source.
func TestLastTurnRetryableComesFromTheRuntimesOwnRecord(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	// alpha's screen shows a failed turn (idle + api-error banner).
	f.captures["%1"] = "  ⏺ API Error: 529 Overloaded. This is a server-side issue, usually\n" +
		"    temporary — try again in a moment. If it persists, check\n" +
		"    https://status.claude.com.\n" +
		"✻ Sautéed for 3m 24s\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	failedAt := sessionStart.Add(5 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		serverErrorEntry("conv-alpha", failedAt))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.LastTurn == nil {
		t.Fatal("expected a LastTurn footnote on a failed turn")
	}
	if alpha.State.LastTurn.Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", alpha.State.LastTurn.Outcome)
	}
	if !alpha.State.LastTurn.Retryable {
		t.Error("the runtime's own record calls this temporary; expected retryable")
	}
}

// The screen's window scan can find a STALE "api error" banner and read it
// as the last turn's outcome; the record proves the last turn actually
// succeeded, and #56 says the record wins on the way OUT of a failure the
// same way it wins on the way out of quota_blocked.
func TestLastTurnClearsWhenTheRecordShowsTheTurnActuallySucceeded(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "  ⏺ API Error: 529 Overloaded. This is a server-side issue, usually\n" +
		"    temporary — try again in a moment. If it persists, check\n" +
		"    https://status.claude.com.\n" +
		"✻ Sautéed for 3m 24s\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	failedAt := sessionStart.Add(5 * time.Minute)
	recoveredAt := sessionStart.Add(6 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		serverErrorEntry("conv-alpha", failedAt))
	appendRecordLine(t, root, "/work/alpha", "conv-alpha", cleanTurnEntry("conv-alpha", recoveredAt))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.LastTurn != nil {
		t.Errorf("LastTurn still set after the record showed a later successful turn: %+v", alpha.State.LastTurn)
	}
}

// State (the single-session, targeted read) applies the same upgrade as
// List — it has no pre-resolved Conversation to reuse, so it must do its
// own lookup rather than silently skip the upgrade.
func TestStateAppliesTheSameLastTurnUpgradeAsList(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "  ⏺ API Error: 529 Overloaded. This is a server-side issue, usually\n" +
		"    temporary — try again in a moment. If it persists, check\n" +
		"    https://status.claude.com.\n" +
		"✻ Sautéed for 3m 24s\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	failedAt := sessionStart.Add(5 * time.Minute)
	recoveredAt := sessionStart.Add(6 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		serverErrorEntry("conv-alpha", failedAt))
	appendRecordLine(t, root, "/work/alpha", "conv-alpha", cleanTurnEntry("conv-alpha", recoveredAt))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }),
		WithRecordRoot(root))

	st, err := d.State(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if st.LastTurn != nil {
		t.Errorf("State did not apply the record upgrade: %+v", st.LastTurn)
	}
}

// The correction on #56 found that "restart and the memory is gone" was
// already stale — quota has survived a restart since 0cadd98, via the same
// state store as everything else this driver persists. What was still true,
// and what this pins: a record-confirmed Since (and the fact that it WAS
// record-confirmed, so the evidence stays honest) must survive that same
// restart, not just the timestamp on its own.
func TestQuotaSinceAndItsProvenanceSurviveARestart(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your session limit · resets 9:50pm (Asia/Saigon)\n" +
		rule + "\n❯ \n" + rule + "\n"

	root := t.TempDir()
	refusedAt := sessionStart.Add(10 * time.Minute)
	writeConversationWithAPIError(t, root, "/work/beta", "conv-beta", "beta", sessionStart,
		rateLimitEntry("conv-beta", refusedAt, "9:50pm (Asia/Saigon)"))

	d1 := New("testbox", withExec(f.exec), WithState(store), WithRecordRoot(root),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(2 * time.Hour) }))
	if _, err := d1.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// A NEW driver instance — same store, no in-memory history, and this
	// time with NO record root: the restarted process should still know
	// Since was record-confirmed, without having to re-resolve the record.
	d2 := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(20 * time.Hour) }))
	col, err := d2.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	beta := sessionOf(t, col, "beta")
	if beta.State.Quota == nil {
		t.Fatal("quota block did not survive the restart")
	}
	if !beta.State.Quota.Since.Equal(refusedAt) {
		t.Errorf("since = %v, want the original refusal time %v to survive the restart",
			beta.State.Quota.Since, refusedAt)
	}
	if !strings.Contains(beta.State.Evidence, "the runtime's own record of the refusal") {
		t.Errorf("evidence lost its record-confirmed provenance after the restart: %q", beta.State.Evidence)
	}
}

// The persisted shape changed to carry sinceObserved (#56). A store already
// holding a bare QuotaBlock — written by any build before this one — must
// still load: quotaPersisted embeds QuotaBlock rather than nesting it
// exactly so an old file's flat "since"/"resetHint" keys land in the same
// place a new file's do. Written directly rather than via saveQuotaLocked,
// which would only prove the NEW code can read its own new shape.
func TestOldFlatQuotaFileStillLoads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	oldRefusedAt := sessionStart.Add(-3 * time.Hour)
	old := fleet.QuotaBlock{Since: oldRefusedAt, ResetHint: "9pm"}
	if err := store.Save("quota", old); err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	f.captures["%1"] = idleFixtureFor("alpha")
	f.captures["%2"] = idleFixtureFor("beta")
	d := New("testbox", withExec(f.exec), WithState(store),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return sessionStart.Add(1 * time.Hour) }))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status != fleet.StatusQuotaBlocked {
			t.Errorf("%s: an old-format persisted block was silently dropped, status = %s", s.ID, s.State.Status)
			continue
		}
		if s.State.Quota == nil || !s.State.Quota.Since.Equal(oldRefusedAt) {
			t.Errorf("%s: since = %+v, want the old file's own %v preserved", s.ID, s.State.Quota, oldRefusedAt)
		}
		if !strings.Contains(s.State.Evidence, "could not confirm it") {
			t.Errorf("%s: an old block with no sinceObserved bit must read as unconfirmed, got %q", s.ID, s.State.Evidence)
		}
	}
}

// End-to-end coverage for #111: `turns`, counted from the runtime's own
// record and published through List/State exactly as #56's LastTurn
// upgrade already is — deliveryMark_test.go covers the mark itself in
// isolation; this exercises the full pipeline a real caller reads.

// The headline case: two completed turns after the delivery this driver
// itself made, surviving a restart (delivery mark AND record both resolve
// from a second driver instance, same state store, same record root).
func TestTurnsCountsCompletedTurnsSinceTheDelivery(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	deliveredAt := sessionStart.Add(1 * time.Hour)

	d1 := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt }), WithState(store))
	got, err := d1.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"do the thing", driver.SendOptions{Submit: false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("setup: send was refused: %s", got.Reason)
	}

	root := t.TempDir()
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		turnDurationEntry("conv-alpha", deliveredAt.Add(-1*time.Minute))) // before delivery: not counted
	appendRecordLine(t, root, "/work/alpha", "conv-alpha", turnDurationEntry("conv-alpha", deliveredAt.Add(1*time.Minute)))
	appendRecordLine(t, root, "/work/alpha", "conv-alpha", turnDurationEntry("conv-alpha", deliveredAt.Add(2*time.Minute)))

	d2 := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt.Add(1 * time.Hour) }),
		WithState(store), WithRecordRoot(root))

	col, err := d2.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.Turns == nil {
		t.Fatal("turns absent after two turns completed since the delivery")
	}
	if *alpha.State.Turns != 2 {
		t.Fatalf("turns = %d, want 2", *alpha.State.Turns)
	}
}

// #111's whole reason to exist: `turns: 0` alongside `status: idle` is what
// a session whose prompt never ran looks like, reachable and distinguishable
// from a session that was never dispatched at all (which reports `turns`
// absent — the next test).
func TestTurnsZeroAlongsideIdleIsThePromptNeverRanSignal(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	deliveredAt := sessionStart.Add(1 * time.Hour)
	root := t.TempDir()

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt }), WithRecordRoot(root))
	if _, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"do the thing", driver.SendOptions{Submit: false}); err != nil {
		t.Fatal(err)
	}

	// The record exists and resolves, but nothing in it happened AFTER the
	// delivery — the window still reaches back far enough to answer
	// honestly (the user/title lines land at/after sessionStart, well
	// before `deliveredAt`), so this must read 0, not absent.
	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		cleanTurnEntry("conv-alpha", sessionStart.Add(1*time.Second)))

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.Turns == nil {
		t.Fatal("turns absent — want 0, a positive finding, not an unresolved absence")
	}
	if *alpha.State.Turns != 0 {
		t.Fatalf("turns = %d, want 0", *alpha.State.Turns)
	}
	if alpha.State.Status != fleet.StatusIdle {
		t.Fatalf("status = %s, want idle — turns:0 alongside idle is the signal this field exists to make readable", alpha.State.Status)
	}
}

// No delivery mark at all — this driver never sent anything into this
// session — must report `turns` absent, never a guessed 0. A session this
// driver has simply never touched costs nothing extra here either: no
// delivery mark means no record is even opened.
func TestTurnsAbsentWhenThisDriverNeverDelivered(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := sessionOf(t, col, "alpha💬")
	if alpha.State.Turns != nil {
		t.Fatalf("turns = %v, want absent — no delivery mark exists for this session", *alpha.State.Turns)
	}
}

// State (the single-session, targeted read) applies the same #111 upgrade
// as List — mirrors TestStateAppliesTheSameLastTurnUpgradeAsList.
func TestStateAppliesTheSameTurnsUpgradeAsList(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	deliveredAt := sessionStart.Add(1 * time.Hour)
	root := t.TempDir()

	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt }), WithRecordRoot(root))
	if _, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"do the thing", driver.SendOptions{Submit: false}); err != nil {
		t.Fatal(err)
	}

	writeConversationWithAPIError(t, root, "/work/alpha", "conv-alpha", "alpha💬", sessionStart,
		turnDurationEntry("conv-alpha", deliveredAt.Add(1*time.Minute)))

	st, err := d.State(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Turns == nil {
		t.Fatal("State did not apply the #111 upgrade")
	}
	if *st.Turns != 1 {
		t.Fatalf("turns = %d, want 1", *st.Turns)
	}
}
