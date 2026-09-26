package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// labelDriver is the smallest driver that behaves like a substrate for
// colab-fleet #153's purposes: sessions exist after Create, carry a
// startedAt, appear in List, can be renamed, closed, or — out from under the
// service — replaced by a new session under the same id.
type labelDriver struct {
	stub.Driver
	mu       sync.Mutex
	sessions map[string]fleet.Session
	n        int
	epoch    time.Time
}

func newLabelDriver() *labelDriver {
	return &labelDriver{
		Driver:   stub.Driver{DeadlineMs: 2000},
		sessions: map[string]fleet.Session{},
		epoch:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (d *labelDriver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := fmt.Sprintf("s%d", d.n)
	ts := d.epoch.Add(time.Duration(d.n) * time.Second)
	s := fleet.Session{
		SessionRef: fleet.SessionRef{Machine: spec.Machine, ID: id},
		Cwd:        spec.Cwd, StartedAt: &ts,
		State: fleet.ObservedState(fleet.StatusIdle, "labelDriver fixture", nil),
	}
	d.sessions[id] = s
	return s, nil
}

func (d *labelDriver) List(ctx context.Context, req fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	items := make([]fleet.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		items = append(items, s)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return fleet.NewCollection(items, []fleet.SourceStatus{{Machine: "test-machine", Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (d *labelDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[ref.ID]; ok {
		return s.State, nil
	}
	return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
}

func (d *labelDriver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[ref.ID]
	delete(d.sessions, ref.ID)
	s.ID = to
	d.sessions[to] = s
	return fleet.RenameAck{Accepted: true}, nil
}

func (d *labelDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, ref.ID)
	return fleet.Ack{Accepted: true}, nil
}

// recycle replaces a session with a NEW one under the same id — §5.4's
// recycled id, the case a label must not survive.
func (d *labelDriver) recycle(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[id]
	ts := s.StartedAt.Add(time.Hour)
	s.StartedAt = &ts
	d.sessions[id] = s
}

// vanish removes a session without going through the service.
func (d *labelDriver) vanish(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, id)
}

func labelServer(t *testing.T, svc *Service, d *labelDriver, cfg Config) *httptest.Server {
	t.Helper()
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, cfg))
	t.Cleanup(srv.Close)
	return srv
}

func tokenCfg() Config { return Config{Token: testToken, AllowLocalMutations: true} }

// call performs one request and returns status and raw body.
func labelCall(t *testing.T, method, u, token string, body any, header map[string]string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

var createSeq int

func create(t *testing.T, srv *httptest.Server, token string, labels map[string]string) (int, []byte) {
	t.Helper()
	createSeq++
	body := map[string]any{"cwd": "/work"}
	if labels != nil {
		body["labels"] = labels
	}
	return labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", token, body,
		map[string]string{"Idempotency-Key": fmt.Sprintf("k-%d", createSeq)})
}

func decodeSession(t *testing.T, raw []byte) fleet.Session {
	t.Helper()
	var s fleet.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decoding session %s: %v", raw, err)
	}
	return s
}

func listLocal(t *testing.T, srv *httptest.Server, query string) (int, []fleet.Session, []byte) {
	t.Helper()
	code, raw := labelCall(t, http.MethodGet, srv.URL+"/v1/sessions?scope=local"+query, testToken, nil, nil)
	if code != http.StatusOK {
		return code, nil, raw
	}
	var col fleet.Collection[fleet.Session]
	if err := json.Unmarshal(raw, &col); err != nil {
		t.Fatalf("decoding list %s: %v", raw, err)
	}
	return code, col.Items(), raw
}

func sameLabels(a, b map[string]string) bool {
	return len(a) == len(b) && fleet.MatchesLabels(a, b)
}

// Acceptance: create with labels → the 201 and every later read carry them.
func TestLabels_CreateCarriesThemOnEveryRead(t *testing.T) {
	srv := labelServer(t, New("test-machine"), newLabelDriver(), tokenCfg())
	want := map[string]string{"issue": "153", "type": "code"}

	code, raw := create(t, srv, testToken, want)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, raw)
	}
	sess := decodeSession(t, raw)
	if !sameLabels(sess.Labels, want) {
		t.Fatalf("201 labels = %v, want %v", sess.Labels, want)
	}

	code, raw = labelCall(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/"+sess.ID, testToken, nil, nil)
	if code != http.StatusOK || !sameLabels(decodeSession(t, raw).Labels, want) {
		t.Fatalf("single GET = %d %s, want labels %v", code, raw, want)
	}

	_, items, _ := listLocal(t, srv, "")
	if len(items) != 1 || !sameLabels(items[0].Labels, want) {
		t.Fatalf("list items = %+v, want one carrying %v", items, want)
	}
}

// Acceptance: a session created without labels reads `labels: {}` — not null,
// not absent.
func TestLabels_AbsentReadsAsAnEmptyObject(t *testing.T) {
	srv := labelServer(t, New("test-machine"), newLabelDriver(), tokenCfg())
	code, raw := create(t, srv, testToken, nil)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, raw)
	}
	for name, body := range map[string][]byte{"create": raw} {
		if !bytes.Contains(body, []byte(`"labels":{}`)) || bytes.Contains(body, []byte(`"labels":null`)) {
			t.Errorf("%s body %s: want \"labels\":{}", name, body)
		}
	}
	_, _, listRaw := listLocal(t, srv, "")
	if !bytes.Contains(listRaw, []byte(`"labels":{}`)) || bytes.Contains(listRaw, []byte(`"labels":null`)) {
		t.Errorf("list body %s: want \"labels\":{}", listRaw)
	}
}

// Acceptance: an over-size map is rejected invalid, naming the limit — and
// nothing is created.
func TestLabels_OverSizeIsRejectedNamingTheLimit(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i < fleet.MaxLabels+1; i++ {
		tooMany[fmt.Sprintf("k%02d", i)] = "v"
	}
	cases := map[string]struct {
		labels map[string]string
		names  string
	}{
		"too many keys": {tooMany, "16"},
		"long key":      {map[string]string{strings.Repeat("k", 129): "v"}, "128"},
		"long value":    {map[string]string{"k": strings.Repeat("v", 129)}, "128"},
		"separator":     {map[string]string{"a:b": "v"}, `":"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := newLabelDriver()
			srv := labelServer(t, New("test-machine"), d, tokenCfg())
			code, raw := create(t, srv, testToken, tc.labels)
			if code != http.StatusBadRequest {
				t.Fatalf("create = %d %s, want 400", code, raw)
			}
			var env fleet.ErrorEnvelope
			_ = json.Unmarshal(raw, &env)
			if env.Error.Kind != fleet.ErrorInvalid || !strings.Contains(env.Error.Message, tc.names) {
				t.Errorf("error = %+v, want invalid naming %s", env.Error, tc.names)
			}
			if d.n != 0 {
				t.Errorf("a refused create reached the driver %d time(s)", d.n)
			}
		})
	}
}

func postLabels(t *testing.T, srv *httptest.Server, token, id, query string, patch map[string]any) (int, []byte) {
	t.Helper()
	return labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+id+"/labels"+query, token,
		map[string]any{"labels": patch}, nil)
}

// Merge semantics, a null deletes, and a stale startedAt is refused the way
// rename refuses it — leaving the labels exactly as they were.
func TestLabels_WriteMergesDeletesAndCorroborates(t *testing.T) {
	srv := labelServer(t, New("test-machine"), newLabelDriver(), tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	if code, raw := postLabels(t, srv, testToken, sess.ID, "", map[string]any{"a": "1"}); code != http.StatusOK {
		t.Fatalf("first write = %d %s", code, raw)
	}
	code, raw := postLabels(t, srv, testToken, sess.ID, "", map[string]any{"b": "2", "a": nil})
	if code != http.StatusOK {
		t.Fatalf("second write = %d %s", code, raw)
	}
	if got := decodeSession(t, raw).Labels; !sameLabels(got, map[string]string{"b": "2"}) {
		t.Fatalf("after merge = %v, want {b:2}", got)
	}

	stale := "?startedAt=" + url.QueryEscape(sess.StartedAt.Add(-time.Minute).Format(time.RFC3339Nano))
	code, raw = postLabels(t, srv, testToken, sess.ID, stale, map[string]any{"c": "3"})
	if code != http.StatusConflict {
		t.Fatalf("stale startedAt = %d %s, want 409", code, raw)
	}
	_, raw = labelCall(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/"+sess.ID, testToken, nil, nil)
	if got := decodeSession(t, raw).Labels; !sameLabels(got, map[string]string{"b": "2"}) {
		t.Fatalf("a refused write changed the labels: %v", got)
	}

	fresh := "?startedAt=" + url.QueryEscape(sess.StartedAt.Format(time.RFC3339Nano))
	if code, raw := postLabels(t, srv, testToken, sess.ID, fresh, map[string]any{"c": "3"}); code != http.StatusOK {
		t.Fatalf("matching startedAt = %d %s, want 200", code, raw)
	}

	if code, raw := postLabels(t, srv, testToken, "no-such", "", map[string]any{"c": "3"}); code != http.StatusNotFound {
		t.Fatalf("unknown session = %d %s, want 404", code, raw)
	}
}

// The grant: a label write needs `label`; labels in a create body need only
// `create`.
func TestLabels_WriteNeedsTheLabelGrant(t *testing.T) {
	cfg := Config{Principals: []Principal{
		{Name: "creator", Token: "tok-create", Grants: []Grant{GrantRead, GrantCreate}},
		{Name: "labeller", Token: "tok-label", Grants: []Grant{GrantRead, GrantLabel}},
	}}
	srv := labelServer(t, New("test-machine"), newLabelDriver(), cfg)

	code, raw := create(t, srv, "tok-create", map[string]string{"issue": "153"})
	if code != http.StatusCreated {
		t.Fatalf("create with labels holding only create = %d %s", code, raw)
	}
	sess := decodeSession(t, raw)

	if code, raw := postLabels(t, srv, "tok-create", sess.ID, "", map[string]any{"x": "1"}); code != http.StatusUnauthorized {
		t.Fatalf("write without label grant = %d %s, want 401", code, raw)
	} else if !bytes.Contains(raw, []byte(`"label"`)) && !bytes.Contains(raw, []byte("label grant")) {
		t.Errorf("refusal %s does not name the label grant", raw)
	}
	if code, raw := postLabels(t, srv, "tok-label", sess.ID, "", map[string]any{"x": "1"}); code != http.StatusOK {
		t.Fatalf("write with label grant = %d %s, want 200", code, raw)
	}
}

// Acceptance: labels survive rename; a recycled id does not inherit them.
func TestLabels_SurviveRenameAndDieWithARecycledId(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	want := map[string]string{"issue": "153"}
	_, raw := create(t, srv, testToken, want)
	sess := decodeSession(t, raw)

	code, raw := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+sess.ID+"/rename", testToken,
		map[string]string{"name": "renamed"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("rename = %d %s", code, raw)
	}
	_, raw = labelCall(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/renamed", testToken, nil, nil)
	if got := decodeSession(t, raw).Labels; !sameLabels(got, want) {
		t.Fatalf("after rename labels = %v, want %v", got, want)
	}

	d.recycle("renamed")
	_, items, _ := listLocal(t, srv, "")
	if len(items) != 1 || len(items[0].Labels) != 0 {
		t.Fatalf("a recycled id inherited labels: %+v", items)
	}
}

// A rename the service did not see through — the driver's id changed without
// POST …/rename — is re-attached by startedAt on the next complete listing.
func TestLabels_ReattachAnOrphanByStartedAt(t *testing.T) {
	d := newLabelDriver()
	svc := New("test-machine")
	srv := labelServer(t, svc, d, tokenCfg())
	_, raw := create(t, srv, testToken, map[string]string{"issue": "153"})
	sess := decodeSession(t, raw)

	_, _ = d.Rename(context.Background(), fleet.Request{}, sess.SessionRef, "elsewhere")
	time.Sleep(2 * time.Millisecond) // the listing must start after the record's last write
	_, items, _ := listLocal(t, srv, "")
	if len(items) != 1 || items[0].ID != "elsewhere" || items[0].Labels["issue"] != "153" {
		t.Fatalf("orphan not re-attached by startedAt: %+v", items)
	}
}

// Records go when their session does: on close through the service, and on
// a complete listing that no longer contains them.
func TestLabels_AreForgottenWithTheirSession(t *testing.T) {
	d := newLabelDriver()
	svc := New("test-machine")
	srv := labelServer(t, svc, d, tokenCfg())

	_, raw := create(t, srv, testToken, map[string]string{"n": "1"})
	closed := decodeSession(t, raw)
	_, raw = create(t, srv, testToken, map[string]string{"n": "2"})
	vanished := decodeSession(t, raw)

	if code, raw := labelCall(t, http.MethodDelete, srv.URL+"/v1/machines/test-machine/sessions/"+closed.ID, testToken, nil, nil); code != http.StatusAccepted {
		t.Fatalf("close = %d %s", code, raw)
	}
	d.vanish(vanished.ID)

	// A FILTERED listing must not prune anything.
	time.Sleep(2 * time.Millisecond)
	listLocal(t, srv, "&label=n:2")
	if got := len(svc.labels.recs); got != 1 {
		t.Fatalf("after close + filtered list: %d records, want 1 (only the close prunes)", got)
	}
	listLocal(t, srv, "")
	if got := len(svc.labels.recs); got != 0 {
		t.Fatalf("after a complete listing: %d records, want 0", got)
	}
}

// Labels are durable: a restarted service on the same state directory still
// attaches them.
func TestLabels_SurviveARestart(t *testing.T) {
	dir := t.TempDir()
	d := newLabelDriver()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewWithState("test-machine", st)
	if err != nil {
		t.Fatal(err)
	}
	srv := labelServer(t, first, d, tokenCfg())
	if code, raw := create(t, srv, testToken, map[string]string{"issue": "153"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, raw)
	}
	srv.Close()

	st2, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWithState("test-machine", st2)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := labelServer(t, second, d, tokenCfg())
	_, items, _ := listLocal(t, srv2, "")
	if len(items) != 1 || items[0].Labels["issue"] != "153" {
		t.Fatalf("after restart: %+v, want the label back", items)
	}
}

// session.labels is announced at a labelled create and after every write,
// carrying the whole map.
func TestLabels_AreAnnouncedOnTheEventPlane(t *testing.T) {
	svc := New("test-machine")
	srv := labelServer(t, svc, newLabelDriver(), tokenCfg())
	_, raw := create(t, srv, testToken, map[string]string{"issue": "153"})
	sess := decodeSession(t, raw)
	postLabels(t, srv, testToken, sess.ID, "", map[string]any{"type": "code"})

	svc.events.mu.Lock()
	var got []fleet.SessionLabelsPayload
	for _, ev := range svc.events.ring {
		if ev.Kind == fleet.EventSessionLabels {
			got = append(got, ev.Payload.(fleet.SessionLabelsPayload))
		}
	}
	svc.events.mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("session.labels events = %d, want 2 (create + write)", len(got))
	}
	if got[0].Ref.ID != sess.ID || !sameLabels(got[0].Labels, map[string]string{"issue": "153"}) {
		t.Errorf("create event = %+v", got[0])
	}
	if !sameLabels(got[1].Labels, map[string]string{"issue": "153", "type": "code"}) {
		t.Errorf("write event carries %v, want the whole map", got[1].Labels)
	}
	if id, _ := eventTarget(fleet.Event{Payload: got[1]}); id != sess.ID {
		t.Errorf("eventTarget = %q, want %q so id filters apply", id, sess.ID)
	}
}

// The list filter: exact, AND across repeats, and malformed forms refused.
func TestLabels_ListFilter(t *testing.T) {
	srv := labelServer(t, New("test-machine"), newLabelDriver(), tokenCfg())
	create(t, srv, testToken, map[string]string{"issue": "1", "type": "code"})
	create(t, srv, testToken, map[string]string{"issue": "2", "type": "code"})
	create(t, srv, testToken, nil)

	if _, items, _ := listLocal(t, srv, "&label=issue:1"); len(items) != 1 || items[0].Labels["issue"] != "1" {
		t.Errorf("label=issue:1 → %+v", items)
	}
	if _, items, _ := listLocal(t, srv, "&label=type:code"); len(items) != 2 {
		t.Errorf("label=type:code → %d items, want 2", len(items))
	}
	if _, items, _ := listLocal(t, srv, "&label=type:code&label=issue:2"); len(items) != 1 {
		t.Errorf("AND of two pairs → %d items, want 1", len(items))
	}
	if _, items, _ := listLocal(t, srv, "&label=issue:3"); len(items) != 0 {
		t.Errorf("no match → %d items, want 0", len(items))
	}
	for _, bad := range []string{"&label=nocolon", "&label=:v", "&label=issue:1&label=issue:2"} {
		if code, _, raw := listLocal(t, srv, bad); code != http.StatusBadRequest {
			t.Errorf("%s → %d %s, want 400", bad, code, raw)
		}
	}
}
