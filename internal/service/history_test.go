package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// Tests for colab-fleet #179: a bounded record of sessions that ended.
// They reuse labels_test.go's labelDriver, which already models everything
// needed here: create, list, close, rename, a recycled id, and a session
// vanishing out from under the service.

func listClosed(t *testing.T, srv *httptest.Server, query string) (fleet.Collection[fleet.ClosedSession], []byte) {
	t.Helper()
	code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions/closed?scope=local"+query, testToken, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/sessions/closed: %d %s", code, raw)
	}
	var col fleet.Collection[fleet.ClosedSession]
	if err := json.Unmarshal(raw, &col); err != nil {
		t.Fatalf("decoding closed list %s: %v", raw, err)
	}
	return col, raw
}

func closeSession(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	code, raw := labelCall(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/"+id, testToken, nil, nil)
	if code != http.StatusAccepted {
		t.Fatalf("close %s: %d %s", id, code, raw)
	}
}

// The issue's oracle: create, close, restart — the closed read still returns
// it with its start and end, and the live list is unchanged.
func TestClosed_SurvivesRestartWithStartAndEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewWithState("test-machine", st)
	if err != nil {
		t.Fatal(err)
	}
	d := newLabelDriver()
	srv := labelServer(t, svc, d, tokenCfg())

	code, raw := create(t, srv, testToken, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	sess := decodeSession(t, raw)
	before := time.Now()
	closeSession(t, srv, sess.ID)
	after := time.Now()

	// Restart: a fresh service over the same state directory, same driver.
	st2, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := NewWithState("test-machine", st2)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := labelServer(t, svc2, d, tokenCfg())

	col, body := listClosed(t, srv2, "")
	if len(col.Items()) != 1 || !col.Complete() {
		t.Fatalf("want one closed session, complete; got %s", body)
	}
	got := col.Items()[0]
	if got.ID != sess.ID || got.Machine != "test-machine" || got.Runtime != "fake" || got.Cwd != "/work" {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(*sess.StartedAt) {
		t.Errorf("startedAt = %v, want %v", got.StartedAt, sess.StartedAt)
	}
	if got.ClosedAt.Before(before) || got.ClosedAt.After(after) {
		t.Errorf("closedAt %v outside the close call [%v, %v]", got.ClosedAt, before, after)
	}
	if got.ClosedBy != fleet.ClosedByClose {
		t.Errorf("closedBy = %q, want close", got.ClosedBy)
	}

	// The live list is unchanged: the closed session is not in it.
	_, live, _ := listLocal(t, srv2, "")
	if len(live) != 0 {
		t.Errorf("live list should be empty, got %+v", live)
	}
}

// A session that ends on its own is recorded by the next complete listing,
// with its closedAt reported as an upper bound next to lastSeenAt.
func TestClosed_VanishedIsRecordedByACompleteListing(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)
	listLocal(t, srv, "")

	d.vanish(sess.ID)

	// A filtered listing proves nothing about absence (§5.7).
	listLocal(t, srv, "&cwdPrefix=/work")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a filtered listing ended a session: %s", body)
	}

	listLocal(t, srv, "")
	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want one closed session, got %s", body)
	}
	got := col.Items()[0]
	if got.ClosedBy != fleet.ClosedByAbsent {
		t.Errorf("closedBy = %q, want absent", got.ClosedBy)
	}
	if got.ClosedAt.Before(got.LastSeenAt) {
		t.Errorf("closedAt %v before lastSeenAt %v", got.ClosedAt, got.LastSeenAt)
	}
	if got.Evidence == "" {
		t.Error("an inferred end must say how it was learned")
	}

	// Recorded once, not on every later listing.
	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 1 {
		t.Fatalf("re-listing duplicated the record: %s", body)
	}
}

// A rename — through the service or behind its back — is the same run under
// a new id, not an end.
func TestClosed_RenameIsNotAnEnd(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	a := decodeSession(t, raw)
	_, raw = create(t, srv, testToken, nil)
	b := decodeSession(t, raw)
	listLocal(t, srv, "")

	code, body := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+a.ID+"/rename",
		testToken, map[string]any{"name": "renamed-a"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("rename: %d %s", code, body)
	}
	// Behind the service's back: the carry rule matches it by startedAt.
	if _, err := d.Rename(t.Context(), fleet.Request{}, fleet.SessionRef{ID: b.ID}, "renamed-b"); err != nil {
		t.Fatal(err)
	}
	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a rename was recorded as an end: %s", body)
	}

	// And the carried record is the one that ends later, under its new id.
	closeSession(t, srv, "renamed-b")
	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 || col.Items()[0].ID != "renamed-b" ||
		col.Items()[0].StartedAt == nil || !col.Items()[0].StartedAt.Equal(*b.StartedAt) {
		t.Fatalf("want renamed-b with b's startedAt, got %s", body)
	}
}

// §5.4: an id now held by a session that started later means the old one
// ended.
func TestClosed_RecycledIdEndsTheOldOccupant(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)
	listLocal(t, srv, "")

	d.recycle(sess.ID)
	listLocal(t, srv, "")

	col, body := listClosed(t, srv, "")
	if len(col.Items()) != 1 {
		t.Fatalf("want the old occupant recorded, got %s", body)
	}
	if got := col.Items()[0]; got.StartedAt == nil || !got.StartedAt.Equal(*sess.StartedAt) {
		t.Errorf("recorded the wrong run: %+v", got)
	}
	// The new occupant is live, not closed.
	if _, live, _ := listLocal(t, srv, ""); len(live) != 1 {
		t.Errorf("live list: %+v", live)
	}
}

// Past the retention window a record is gone — from reads immediately, and
// from the persisted document on the next write.
func TestClosed_RetentionAndSince(t *testing.T) {
	svc := New("test-machine")
	d := newLabelDriver()
	srv := labelServer(t, svc, d, tokenCfg())
	svc.SetClosedRetention(48 * time.Hour)

	clock := time.Now()
	svc.history.now = func() time.Time { return clock }

	_, raw := create(t, srv, testToken, nil)
	old := decodeSession(t, raw)
	closeSession(t, srv, old.ID)

	clock = clock.Add(24 * time.Hour)
	mid := clock
	_, raw = create(t, srv, testToken, nil)
	recent := decodeSession(t, raw)
	closeSession(t, srv, recent.ID)

	if col, body := listClosed(t, srv, ""); len(col.Items()) != 2 || col.Items()[0].ID != recent.ID {
		t.Fatalf("want both, newest first: %s", body)
	}
	since := mid.Add(-time.Minute).UTC().Format(time.RFC3339)
	if col, body := listClosed(t, srv, "&since="+since); len(col.Items()) != 1 || col.Items()[0].ID != recent.ID {
		t.Fatalf("since should keep only the recent one: %s", body)
	}

	clock = clock.Add(25*time.Hour + time.Minute) // old is now 49h past, recent 25h
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 1 || col.Items()[0].ID != recent.ID {
		t.Fatalf("the old record should have aged out: %s", body)
	}

	code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions/closed?since=yesterday", testToken, nil, nil)
	if code != http.StatusBadRequest {
		t.Errorf("a malformed since should be refused, got %d %s", code, raw)
	}
}

// A close the driver accepted but that did not take is withdrawn when the
// session is plainly still running.
func TestClosed_PrematureCloseIsWithdrawn(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	d.mu.Lock()
	keep := d.sessions[sess.ID]
	d.mu.Unlock()
	closeSession(t, srv, sess.ID)
	d.mu.Lock()
	d.sessions[sess.ID] = keep // the close "did not take"
	d.mu.Unlock()

	listLocal(t, srv, "")
	if col, body := listClosed(t, srv, ""); len(col.Items()) != 0 {
		t.Fatalf("a still-running session kept its tombstone: %s", body)
	}
}
