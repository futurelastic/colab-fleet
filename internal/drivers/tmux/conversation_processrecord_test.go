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
)

// #182: a session's conversation, identified from the runtime's own
// per-process record instead of by elimination on the session's name.

const (
	// resumedConv is the conversation a resumed session continues: a record the
	// runtime filed under a UUID, well before this session existed.
	resumedConv = "0a0b0c0d-0000-4000-8000-00000000abc1"
	// otherConv is a second, different conversation in the same directory.
	otherConv = "1b1c1d1e-1111-4111-8111-11111111def2"
)

// processStart is the one instant the fake `ps` reports for the session's
// process. The runtime writes the same instant into its record in UTC, so the
// two are rendered in different zones to model the real boundary.
var processStart = time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)

// countingPS is fakePS that counts how many times the OS was asked.
type countingPS struct {
	inner *fakePS
	mu    sync.Mutex
	n     int
}

func (c *countingPS) exec(ctx context.Context, bin string, args ...string) ([]byte, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.inner.exec(ctx, bin, args...)
}

func (c *countingPS) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// conversationRig is twoSessions with both record stores and a fake `ps`. The
// first session ("alpha💬", pid 100) is the one these tests are about.
type conversationRig struct {
	t        *testing.T
	mux      *fakeMux
	ps       *countingPS
	records  string
	sessions string
}

func newConversationRig(t *testing.T) *conversationRig {
	t.Helper()
	r := &conversationRig{
		t:        t,
		mux:      twoSessions(),
		ps:       &countingPS{inner: &fakePS{}},
		records:  t.TempDir(),
		sessions: t.TempDir(),
	}
	r.ps.inner.set(100, processStart)
	r.ps.inner.set(200, processStart)
	return r
}

func (r *conversationRig) driver() *Driver {
	return New("testbox",
		withExec(r.mux.exec),
		withPSExec(r.ps.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(r.records),
		WithProcessSessionsRoot(r.sessions),
	)
}

// processRecord lays down the runtime's per-process record for pid: what the
// runtime wrote about itself, with its start time in UTC.
func (r *conversationRig) processRecord(pid int, sessionID, cwd string, started time.Time) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"pid": pid, "sessionId": sessionID, "cwd": cwd,
		"procStart": started.UTC().Format(psStartTimeLayout),
	})
	if err := os.WriteFile(filepath.Join(r.sessions, itoa(pid)+".json"), raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// untitledRecord lays down a conversation record with no title line: what a
// session created without a name leaves behind.
func (r *conversationRig) untitledRecord(cwd, id string, began time.Time) {
	r.t.Helper()
	dir := filepath.Join(r.records, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "timestamp": began.UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(raw, '\n'), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *conversationRig) alpha() *fleet.ConversationRef {
	r.t.Helper()
	return conversationOf(r.t, r.driver(), "alpha💬")
}

// The headline case. The record the session continues began three days before
// the session did, so the date rule rules it out and the name-based derivation
// says none of them can be this session's. The runtime's own record for the
// running process names it.
func TestConversationResumedSessionResolvesFromThePerProcessRecord(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	got := r.alpha()
	if got == nil || !got.Known {
		t.Fatalf("a resumed session with a corroborated per-process record must resolve, got %+v", got)
	}
	if got.ID != resumedConv {
		t.Errorf("resolved to %q, want the conversation the process is in, %q", got.ID, resumedConv)
	}
	if !strings.Contains(got.Evidence, "per-process record, start time corroborated") {
		t.Errorf("evidence must name the rule that answered, got %q", got.Evidence)
	}
	if got.Source != fleet.ConversationDerived {
		t.Errorf("Source is a closed set an older peer cannot decode past; it must stay %q, got %q", fleet.ConversationDerived, got.Source)
	}
}

// The precondition, so the case above proves something: without the
// per-process record this very fixture is the reported failure.
func TestConversationResumedSessionIsUnknownWithoutThePerProcessRecord(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))

	got := r.alpha()
	if got == nil || got.Known {
		t.Fatalf("setup: this is the case that reads known:false today, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "created before this session existed") {
		t.Errorf("setup: expected the reported evidence, got %q", got.Evidence)
	}
	if !strings.Contains(got.Evidence, "no per-process record") {
		t.Errorf("a fallback must say why the record was not used: %q", got.Evidence)
	}
}

// A session created without a name writes no title, so nothing keyed on the
// name can ever find it. The per-process record does not care.
func TestConversationSessionWithoutATitleResolvesFromThePerProcessRecord(t *testing.T) {
	r := newConversationRig(t)
	r.untitledRecord("/work/alpha", resumedConv, sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	got := r.alpha()
	if got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("a session with no title record must resolve from its per-process record, got %+v", got)
	}
}

// Both sources answer and agree: the same conversation, now with a second
// witness. This is what an ordinary fresh session reads once the per-process
// source is configured.
func TestConversationSourcesThatAgreeResolveToTheSameConversation(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	got := r.alpha()
	if got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("agreeing sources must resolve, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "same conversation") {
		t.Errorf("evidence should say the derivation agreed: %q", got.Evidence)
	}
}

// Two independent sources, two answers: report it, choose neither.
func TestConversationDisagreementIsReportedNeverPicked(t *testing.T) {
	r := newConversationRig(t)
	// The name-based derivation finds exactly one possible record...
	writeRecord(t, r.records, "/work/alpha", otherConv, "alpha💬", sessionStart.Add(4*time.Second))
	// ...and the runtime's own record for the process names a different one.
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	got := r.alpha()
	if got == nil {
		t.Fatal("a lookup happened; it must be reported")
	}
	if got.Known || got.ID != "" {
		t.Fatalf("disagreeing sources must not resolve, got %+v", got)
	}
	for _, id := range []string{resumedConv, otherConv} {
		if !strings.Contains(got.Evidence, id) {
			t.Errorf("the evidence must name both conversations; %q is missing from %q", id, got.Evidence)
		}
	}
}

// A reused pid: the record on file was written by an EARLIER process and is
// exactly as well-formed as a live one. Its start time is the only thing that
// tells them apart, and any difference at all must refuse.
func TestConversationStartTimeMismatchNeverResolvesFromTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"one second later", time.Second},
		{"one second earlier", -time.Second},
		{"a minute later", time.Minute},
		{"an hour earlier", -time.Hour},
		{"a day later", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newConversationRig(t)
			// The name-based derivation cannot answer, so a record that were
			// trusted here would be the only thing resolving this session.
			writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
			r.processRecord(100, resumedConv, "/work/alpha", processStart.Add(tc.offset))

			got := r.alpha()
			if got == nil || got.Known {
				t.Fatalf("a record from a different process generation must not resolve, got %+v", got)
			}
			if !strings.Contains(got.Evidence, "different time") {
				t.Errorf("the refusal must say the start times differ: %q", got.Evidence)
			}
		})
	}

	t.Run("the same instant resolves", func(t *testing.T) {
		r := newConversationRig(t)
		writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
		r.processRecord(100, resumedConv, "/work/alpha", processStart)
		if got := r.alpha(); got == nil || !got.Known {
			t.Fatalf("control: the same start time must resolve, got %+v", got)
		}
	})
}

// A stale record must not override a derivation that IS sound: when the record
// is refused, the answer is the name-based one, exactly as before.
func TestConversationRefusedRecordFallsBackToTheNameBasedAnswer(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", otherConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart.Add(time.Hour))

	got := r.alpha()
	if got == nil || !got.Known || got.ID != otherConv {
		t.Fatalf("a refused record must leave the derivation's answer alone, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "not used") {
		t.Errorf("a fallback must say the record was not used: %q", got.Evidence)
	}
}

// The id from a per-process record becomes part of a path. Anything but the
// runtime's own UUID shape never resolves — least of all one that climbs out
// of the record root.
func TestConversationUnusableIdInThePerProcessRecordNeverResolves(t *testing.T) {
	for _, bad := range []string{
		"conv-1",
		"../../outside",
		"../../../../etc/passwd",
		"0a0b0c0d-0000-4000-8000-00000000abc1/../../x",
		"0a0b0c0d-0000-4000-8000-00000000abcg", // right length, not hex
		"0a0b0c0d000040008000000000000abc1",    // no separators
	} {
		t.Run(bad, func(t *testing.T) {
			r := newConversationRig(t)
			writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
			r.processRecord(100, bad, "/work/alpha", processStart)

			got := r.alpha()
			if got == nil || got.Known {
				t.Fatalf("an id of %q must never resolve, got %+v", bad, got)
			}
		})
	}
}

// The store's own guard, independent of the UUID check in front of it: a
// per-process answer whose path would leave the record root is refused even if
// the id slipped past everything above.
func TestConversationStoreRefusesAPerProcessIdThatEscapesTheRecordRoot(t *testing.T) {
	root := t.TempDir()
	s := newConversationStore(root)
	escaping := func() liveConversation {
		return liveConversation{id: "../../../outside", evidence: "test"}
	}
	got := s.lookup(conversationKey{pane: "%1", created: sessionStart}, "/work/alpha", "alpha💬", sessionStart, escaping)
	if got == nil || got.Known {
		t.Fatalf("an id that climbs out of the record root must never resolve, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "outside the record root") {
		t.Errorf("the refusal must say why: %q", got.Evidence)
	}
}

// A record naming another directory says nothing about this session, whatever
// else it holds.
func TestConversationPerProcessRecordForAnotherDirectoryIsNotUsed(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/elsewhere", processStart)

	got := r.alpha()
	if got == nil || got.Known {
		t.Fatalf("a record naming another working directory must not resolve, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "different working directory") {
		t.Errorf("the refusal must say why: %q", got.Evidence)
	}
}

// A fresh session, whose runtime wrote no record we can use, resolves exactly
// as it did: same identifier, same label, and the derivation's own evidence
// first.
func TestConversationFreshSessionResolvesAsBeforeWhenNoRecordIsThere(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))

	got := r.alpha()
	if got == nil || !got.Known || got.ID != resumedConv || got.Source != fleet.ConversationDerived {
		t.Fatalf("a fresh session must resolve as today, got %+v", got)
	}
	const derivedEvidence = "the only record in this session's working directory carrying the name this service gave the session"
	if !strings.HasPrefix(got.Evidence, derivedEvidence) {
		t.Errorf("the derivation's own evidence must lead, got %q", got.Evidence)
	}

	// Configured off, the answer is byte-for-byte what it always was.
	plain := New("testbox", withExec(r.mux.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(r.records))
	if got := conversationOf(t, plain, "alpha💬"); got == nil || got.Evidence != derivedEvidence {
		t.Errorf("with no per-process root the evidence must be untouched, got %+v", got)
	}
}

// A session whose runtime wrote no record must not cost the OS a question: the
// record is read first, and `ps` is asked only for one worth corroborating.
func TestConversationNoPerProcessRecordCostsNoProcessQuery(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	_ = r.alpha()
	if n := r.ps.calls(); n != 0 {
		t.Fatalf("asked the OS %d times for sessions with no per-process record, want 0", n)
	}
}

// A per-process answer is remembered like every other success: the OS is asked
// once for the life of the session, not once per listing.
func TestConversationPerProcessAnswerIsRememberedNotRepeated(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	for i := 0; i < 3; i++ {
		if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known {
			t.Fatalf("listing %d: %+v", i, got)
		}
	}
	if n := r.ps.calls(); n != 1 {
		t.Fatalf("three listings asked the OS %d times, want 1", n)
	}
}

// A refusal is not remembered: the record for a session created moments ago is
// not written yet, and caching "no" would make that permanent.
func TestConversationRefusalIsRetriedOnceTheRecordAppears(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.Known {
		t.Fatalf("precondition: no record yet, got %+v", got)
	}
	r.processRecord(100, resumedConv, "/work/alpha", processStart)
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("the record appeared; the next listing must resolve, got %+v", got)
	}
}

// State builds its own reply and does its own lookup. It must hand the pane's
// pid to it, or a resumed session resolves in List and not here.
func TestStateUpgradeReachesAResumedSessionsRecord(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	// The resumed conversation's most recent turn succeeded.
	appendLine(t, filepath.Join(r.records, recordDirFor("/work/alpha"), resumedConv+".jsonl"),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-08-03T00:00:00Z"}`)
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	flagged := fleet.SessionState{LastTurn: &fleet.TurnEnd{Outcome: "failed", Reason: "the screen's guess"}}
	got := d.upgradeLastTurnFromRecord(context.Background(), flagged, "/work/alpha", "alpha💬", time.Unix(1785600000, 0), "%1", 100)
	if got.LastTurn != nil {
		t.Fatalf("the record says the last turn succeeded; the screen's failure must be cleared, got %+v", got.LastTurn)
	}
}

// The send path and the listing share one cache, so they must agree about a
// disagreement: a first lookup that finds the two sources naming different
// conversations does not resolve, and the transcript to watch is the one the
// RUNNING process says it is writing.
func TestResolveTranscriptSourceUsesLiveIdentityWhenSourcesConflict(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", "conv-1", "alpha💬", sessionStart.Add(4*time.Second))
	writeRecord(t, r.records, "/work/alpha", otherConv, "someone-else", sessionStart.Add(8*time.Second))
	r.processRecord(100, otherConv, "/work/alpha", processStart)

	d := r.driver()
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	target := &paneRow{session: "alpha💬", paneID: "%1", cwd: "/work/alpha", pid: 100, created: time.Unix(1785600000, 0)}

	src, ok := d.resolveTranscriptSource(context.Background(), ref, target)
	if !ok {
		t.Fatal("resolveTranscriptSource did not resolve")
	}
	if want := d.conversations.recordPath("/work/alpha", otherConv); src.path != want {
		t.Errorf("watching %q, want the live process's conversation %q", src.path, want)
	}
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.Known {
		t.Errorf("the listing must report the same disagreement, got %+v", got)
	}
}
