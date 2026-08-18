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
)

// writeRecord lays down one record file the way the runtime does: a title
// record first, carrying the session name and the identifier the file is
// filed under, then a timestamped line that dates it.
func writeRecord(t *testing.T, root, cwd, id, title string, began time.Time) {
	t.Helper()
	dir := filepath.Join(root, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []any{
		map[string]any{"type": "custom-title", "customTitle": title, "sessionId": id},
		map[string]any{"type": "mode", "mode": "normal", "sessionId": id},
		map[string]any{"type": "user", "sessionId": id, "timestamp": began.UTC().Format(time.RFC3339Nano)},
	}
	var b strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sessionStart is the fake multiplexer's creation time for twoSessions'
// first session, as a real time — every record in these tests is dated
// relative to it.
var sessionStart = time.Unix(1785600000, 0)

func conversationOf(t *testing.T, d *Driver, id string) *fleet.ConversationRef {
	t.Helper()
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Items() {
		if s.ID == id {
			return s.Conversation
		}
	}
	t.Fatalf("session %q not in listing", id)
	return nil
}

// The distinction the whole field exists for. A driver that was never pointed
// at a record store has not discovered that a session has no record — it has
// not looked, and saying otherwise turns a missing configuration into a
// finding about somebody's session (§5.7).
func TestConversationLookupUnconfiguredIsNotAFailedLookup(t *testing.T) {
	d := newTestDriver(twoSessions())
	if got := conversationOf(t, d, "alpha💬"); got != nil {
		t.Fatalf("a driver with no record root must report nothing at all, got %+v", got)
	}

	d = New("testbox", withExec(twoSessions().exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(t.TempDir()))
	got := conversationOf(t, d, "alpha💬")
	if got == nil {
		t.Fatal("a driver that DID look must say so, even when it found nothing")
	}
	if got.Known {
		t.Fatalf("nothing was recorded, so nothing can be known: %+v", got)
	}
	if got.Evidence == "" {
		t.Error("a failed lookup must carry the reason it failed")
	}
}

func TestConversationResolvesByTheNameThisServicePassed(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", "rec-alpha", "alpha💬", sessionStart.Add(4*time.Second))
	// A record for another session in the same directory: same store, wrong
	// name, and it must not be picked up.
	writeRecord(t, root, "/work/alpha", "rec-other", "somebody-else", sessionStart.Add(time.Minute))

	d := New("testbox", withExec(twoSessions().exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known {
		t.Fatalf("the one record carrying this session's name should resolve, got %+v", got)
	}
	if got.ID != "rec-alpha" {
		t.Errorf("resolved to %q, want the record filed under rec-alpha", got.ID)
	}
	if got.Source != fleet.ConversationDerived {
		t.Errorf("a matched value must be labelled derived, got %q", got.Source)
	}
}

// The refusal that matters. A session name is reused by convention here — one
// name was measured on 27 records in a single directory — so several records
// carrying it is the normal case, not the exotic one.
func TestConversationRefusesWhenSeveralRecordsCouldBeThisConversation(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", "rec-one", "alpha💬", sessionStart.Add(time.Minute))
	writeRecord(t, root, "/work/alpha", "rec-two", "alpha💬", sessionStart.Add(2*time.Hour))

	d := New("testbox", withExec(twoSessions().exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	got := conversationOf(t, d, "alpha💬")
	if got == nil {
		t.Fatal("a lookup happened; it must be reported")
	}
	if got.Known {
		t.Fatalf("two records could be this conversation; naming one is a guess, got %q", got.ID)
	}
	if !strings.Contains(got.Evidence, "2") {
		t.Errorf("the refusal must say how many candidates it would not choose between: %q", got.Evidence)
	}
}

// Not a heuristic: a conversation runs inside the session, so a record created
// before the session existed cannot be that session's. Eliminating the
// impossible is what turns the common ambiguous case into a single answer
// without ever picking between two possible ones.
func TestConversationRulesOutRecordsOlderThanTheSession(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", "rec-old", "alpha💬", sessionStart.Add(-72*time.Hour))
	writeRecord(t, root, "/work/alpha", "rec-older", "alpha💬", sessionStart.Add(-10*time.Minute))
	writeRecord(t, root, "/work/alpha", "rec-live", "alpha💬", sessionStart.Add(3*time.Second))

	d := New("testbox", withExec(twoSessions().exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known {
		t.Fatalf("two of three candidates are impossible, leaving one: %+v", got)
	}
	if got.ID != "rec-live" {
		t.Errorf("resolved to %q, want rec-live", got.ID)
	}
	if !strings.Contains(got.Evidence, "2") {
		t.Errorf("evidence should say the answer survived an elimination, not that it was unique: %q", got.Evidence)
	}
}

func TestConversationSaysSoWhenEveryCandidatePredatesTheSession(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", "rec-old", "alpha💬", sessionStart.Add(-72*time.Hour))

	d := New("testbox", withExec(twoSessions().exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	got := conversationOf(t, d, "alpha💬")
	if got == nil || got.Known {
		t.Fatalf("the only candidate cannot be this session's record: %+v", got)
	}
	if !strings.Contains(got.Evidence, "before") {
		t.Errorf("this failure must be distinguishable from finding nothing at all: %q", got.Evidence)
	}
}

// A rename changes the multiplexer's session name; the title already written
// into the record is immutable history. So the name key stops matching, and a
// driver that re-derived on every read would LOSE an identifier it had already
// established.
func TestConversationSurvivesARename(t *testing.T) {
	root := t.TempDir()
	f := twoSessions()
	writeRecord(t, root, "/work/alpha", "rec-alpha", "alpha💬", sessionStart.Add(4*time.Second))

	d := New("testbox", withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(root))

	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known {
		t.Fatalf("precondition: it must resolve before the rename, got %+v", got)
	}

	f.mu.Lock()
	f.sessions[0].name = "alpha-renamed💬"
	f.mu.Unlock()

	got := conversationOf(t, d, "alpha-renamed💬")
	if got == nil || !got.Known || got.ID != "rec-alpha" {
		t.Fatalf("the identifier must survive the rename that broke its key, got %+v", got)
	}
}
