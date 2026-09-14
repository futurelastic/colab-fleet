package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	"github.com/godx-jp/colab-fleet/internal/service"
)

// Session labels across a federation hop (colab-fleet #153). The rules under
// test are all about the MIXED-VERSION case: a peer on a build that predates
// labels must never silently drop them on a create, never have its unfiltered
// answer counted as a filter's matches, and never have its missing route read
// as "no such session".

func healthJSON(withLabels bool) map[string]any {
	h := map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024}
	if withLabels {
		h["labels"] = fleet.SelfLabelLimits()
	}
	return h
}

func kindOf(err error) fleet.ErrorKind {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return ""
}

// labelledPeer serves /v1/health (with or without label support) and a
// create route that echoes the labels back, counting creates.
func labelledPeer(t *testing.T, withLabels bool) (*httptest.Server, *atomic.Int32, *string, *sync.Mutex) {
	t.Helper()
	var creates atomic.Int32
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			_ = json.NewEncoder(w).Encode(healthJSON(withLabels))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			lastBody = string(raw)
			mu.Unlock()
			var body struct {
				Labels map[string]string `json:"labels"`
			}
			_ = json.Unmarshal(raw, &body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fleet.Session{
				SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
				Labels:     body.Labels,
				State:      fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &creates, &lastBody, &mu
}

func TestCreateForwardsLabelsToAPeerThatCarriesThem(t *testing.T) {
	srv, creates, lastBody, mu := labelledPeer(t, true)
	d := New("peerbox", srv.URL)
	got, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", Labels: map[string]string{"issue": "153"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	mu.Lock()
	body := *lastBody
	mu.Unlock()
	if !strings.Contains(body, `"labels":{"issue":"153"}`) {
		t.Errorf("create body %s does not carry the labels", body)
	}
	if got.Labels["issue"] != "153" {
		t.Errorf("adopted session labels = %v", got.Labels)
	}
}

// The mixed-version rule for creates: refuse BEFORE any side effect, because
// an older peer would answer 201 and keep none of the labels.
func TestCreateRefusesALabelledCreateToAPeerThatWouldDropThem(t *testing.T) {
	srv, creates, _, _ := labelledPeer(t, false)
	d := New("peerbox", srv.URL)

	_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", Labels: map[string]string{"issue": "153"},
	})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if creates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
	}

	// An unlabelled create is unaffected: nothing to drop, nothing to check.
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Cwd: "/work"}); err != nil {
		t.Fatalf("unlabelled create: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatalf("unlabelled create did not reach the peer")
	}
}

func TestListForwardsTheLabelFilterInSortedOrder(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}}, nil), &rec)
	d := New("peerbox", srv.URL)
	if _, err := d.List(context.Background(), caller, driver.ListFilter{
		Labels: map[string]string{"type": "code", "issue": "153"},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "label=issue%3A153&label=type%3Acode") {
		t.Errorf("query = %q, want both label pairs, sorted", rec.query)
	}
}

// oldPeerSessions is how a build that predates labels writes a session: no
// `labels` key at all.
func oldPeerCollection(t *testing.T, machine fleet.MachineId) []byte {
	t.Helper()
	raw, err := json.Marshal(fleet.Session{
		SessionRef: fleet.SessionRef{Machine: machine, ID: "old-1"},
		Runtime:    "tmux", Cwd: "/work",
		State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	delete(item, "labels")
	out, err := json.Marshal(map[string]any{
		"items":    []any{item},
		"sources":  []fleet.SourceStatus{{Machine: machine, Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func oldPeer(t *testing.T, machine fleet.MachineId) *httptest.Server {
	t.Helper()
	body := oldPeerCollection(t, machine)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The mixed-version rule for reads: an older peer ignores `label=` and returns
// everything. That is neither a list of matches nor an empty list.
func TestFilteredListFromAPeerThatPredatesLabelsIsDegraded(t *testing.T) {
	d := New("oldbox", oldPeer(t, "oldbox").URL)

	got, err := d.List(context.Background(), caller, driver.ListFilter{Labels: map[string]string{"issue": "153"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items()) != 0 {
		t.Errorf("items = %+v, want none: an ignored filter produced them", got.Items())
	}
	if got.Complete() || len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceDegraded {
		t.Errorf("sources = %+v complete=%v, want one degraded source", got.Sources(), got.Complete())
	}

	unfiltered, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil || len(unfiltered.Items()) != 1 {
		t.Fatalf("an unfiltered list from the same peer must still work: %v %+v", err, unfiltered.Items())
	}
}

func TestLabelsWriteForwardsPatchAndStartedAt(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.Session{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
		Labels: map[string]string{"a": "1"}, State: fleet.ObservedState(fleet.StatusIdle, "fixture", nil)}, &rec)
	d := New("peerbox", srv.URL)
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	req := caller
	req.Expect.StartedAt = &started
	one := "1"
	got, err := d.Labels(context.Background(), req, fleet.SessionRef{Machine: "peerbox", ID: "s1"},
		map[string]*string{"a": &one, "b": nil})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/machines/peerbox/sessions/s1/labels" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if !strings.Contains(rec.query, "startedAt=2026-03-04T05%3A06%3A07Z") {
		t.Errorf("query = %q, want startedAt forwarded", rec.query)
	}
	if rec.body != "{\"labels\":{\"a\":\"1\",\"b\":null}}\n" && rec.body != `{"labels":{"a":"1","b":null}}` {
		t.Errorf("body = %q, want the patch with its null intact", rec.body)
	}
	if got.Labels["a"] != "1" {
		t.Errorf("adopted labels = %v", got.Labels)
	}
}

// A missing route is not a missing session.
func TestLabelsWriteToAPeerWithoutTheRouteIsUnsupported(t *testing.T) {
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(bare.Close)
	_, err := New("oldbox", bare.URL).Labels(context.Background(), caller, fleet.SessionRef{Machine: "oldbox", ID: "s1"}, map[string]*string{})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("bare 404 → %v, want unsupported", err)
	}

	enveloped := peerServing(t, 404, fleet.ErrorEnvelope{Error: fleet.Error{Kind: fleet.ErrorNotFound, Message: "gone"}}, nil)
	_, err = New("peerbox", enveloped.URL).Labels(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"}, map[string]*string{})
	if kindOf(err) != fleet.ErrorNotFound {
		t.Fatalf("enveloped 404 → %v, want not_found (the session really is gone)", err)
	}
}

// memDriver is a real-enough local driver for the federation tests below.
type memDriver struct {
	stub.Driver
	machine  fleet.MachineId
	mu       sync.Mutex
	sessions []fleet.Session
}

func (d *memDriver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ts := time.Date(2026, 1, 1, 0, 0, len(d.sessions), 0, time.UTC)
	s := fleet.Session{
		SessionRef: fleet.SessionRef{Machine: d.machine, ID: fmt.Sprintf("s%d", len(d.sessions)+1)},
		Cwd:        spec.Cwd, StartedAt: &ts,
		State: fleet.ObservedState(fleet.StatusIdle, "memDriver fixture", nil),
	}
	d.sessions = append(d.sessions, s)
	return s, nil
}

func (d *memDriver) List(ctx context.Context, req fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fleet.NewCollection(append([]fleet.Session(nil), d.sessions...),
		[]fleet.SourceStatus{{Machine: d.machine, Status: fleet.SourceOK, ObservedAt: time.Now()}})
}

func (d *memDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.sessions {
		if s.ID == ref.ID {
			return s.State, nil
		}
	}
	return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
}

// Acceptance: GET /v1/sessions?scope=fleet&label=k:v returns exactly the
// sessions carrying that pair from every reachable machine, and complete:false
// when a source could not answer — including a peer too old to apply it.
func TestFederatedLabelFilterIsFleetWide(t *testing.T) {
	const token = "federation-token"
	base := peerService(t, "peerbox", "mem", &memDriver{machine: "peerbox"}, token, true)
	rd := New("peerbox", base, WithDeadline(2*time.Second))
	ctx := context.Background()

	for i, labels := range []map[string]string{{"issue": "153"}, {"issue": "154"}, nil} {
		if _, err := rd.Create(ctx, fleetCaller(token), fmt.Sprintf("k%d", i), fleet.SessionSpec{Cwd: "/work", Labels: labels}); err != nil {
			t.Fatalf("relayed create %d: %v", i, err)
		}
	}
	filter := driver.ListFilter{Labels: map[string]string{"issue": "153"}}

	t.Run("reachable peer", func(t *testing.T) {
		svcA := homeService(t, "homebox", "peerbox", rd)
		got, err := svcA.ListSessions(ctx, fleetCaller(token), service.ScopeFleet, filter, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items()) != 1 || got.Items()[0].Labels["issue"] != "153" || got.Items()[0].Machine != "peerbox" {
			t.Fatalf("items = %+v, want exactly the one session labelled issue:153", got.Items())
		}
		if !got.Complete() {
			t.Errorf("complete = false with every source answering: %+v", got.Sources())
		}
	})

	t.Run("plus an unreachable peer", func(t *testing.T) {
		svcA := homeService(t, "homebox", "peerbox", rd)
		ghost := New("ghost", "http://127.0.0.1:1", WithDeadline(500*time.Millisecond))
		if err := svcA.RegisterPeerDriver("ghost", ghost); err != nil {
			t.Fatal(err)
		}
		got, _ := svcA.ListSessions(ctx, fleetCaller(token), service.ScopeFleet, filter, 0)
		if len(got.Items()) != 1 || got.Complete() {
			t.Fatalf("items=%d complete=%v, want the same one match and complete:false", len(got.Items()), got.Complete())
		}
	})

	t.Run("plus a peer that predates labels", func(t *testing.T) {
		svcA := homeService(t, "homebox", "peerbox", rd)
		if err := svcA.RegisterPeerDriver("oldbox", New("oldbox", oldPeer(t, "oldbox").URL)); err != nil {
			t.Fatal(err)
		}
		got, _ := svcA.ListSessions(ctx, fleetCaller(token), service.ScopeFleet, filter, 0)
		for _, it := range got.Items() {
			if it.Machine == "oldbox" {
				t.Fatalf("the old peer's unfiltered session was counted as a match: %+v", it)
			}
		}
		if len(got.Items()) != 1 || got.Complete() {
			t.Fatalf("items=%d complete=%v, want one match and complete:false", len(got.Items()), got.Complete())
		}
	})
}

// A label write relayed through one service lands on the peer's session, and
// the peer's own reads carry it afterwards.
func TestFederatedLabelWriteRelays(t *testing.T) {
	const token = "federation-token"
	base := peerService(t, "peerbox", "mem", &memDriver{machine: "peerbox"}, token, true)
	rd := New("peerbox", base, WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)
	front := httptest.NewServer(service.NewMux(svcA, service.Config{Token: token, AllowPeerRelay: true}))
	t.Cleanup(front.Close)

	sess, err := rd.Create(context.Background(), fleetCaller(token), "k1", fleet.SessionSpec{Cwd: "/work"})
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/machines/peerbox/sessions/"+sess.ID+"/labels",
		bytes.NewReader([]byte(`{"labels":{"issue":"153"}}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"labels":{"issue":"153"}`) {
		t.Fatalf("relayed write = %d %s", resp.StatusCode, raw)
	}

	got, _ := rd.List(context.Background(), fleetCaller(token), driver.ListFilter{Labels: map[string]string{"issue": "153"}})
	if len(got.Items()) != 1 || got.Items()[0].ID != sess.ID {
		t.Fatalf("peer's own filtered read = %+v", got.Items())
	}
}
