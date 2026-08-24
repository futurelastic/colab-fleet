package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// idDriver is stub.Driver plus a controllable State() and a working
// Create() — the smallest fake that can play every existence-first
// scenario colab-fleet issue #60 needs proven: a driver that
// affirmatively HAS an id, one that affirmatively never had it (the
// ordinary zero value), and one whose read of a specific id fails for a
// reason that is NOT "never had it".
type idDriver struct {
	stub.Driver
	held    map[string]bool
	failErr map[string]error
	created int
}

func (d *idDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	if d.failErr != nil {
		if err, ok := d.failErr[ref.ID]; ok {
			return fleet.SessionState{}, err
		}
	}
	if d.held != nil && d.held[ref.ID] {
		return fleet.ObservedState(fleet.StatusIdle, "idDriver test fixture", nil), nil
	}
	// The zero value affirms absence — errors.go's ErrNoSuchSession is
	// exactly "a read whose id the machine has never had", which is what
	// resolveSessionDriver's existence probe is asking for.
	return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
}

func (d *idDriver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	d.created++
	id := fmt.Sprintf("created-%d", d.created)
	if d.held == nil {
		d.held = map[string]bool{}
	}
	d.held[id] = true
	return fleet.Session{SessionRef: fleet.SessionRef{Machine: spec.Machine, ID: id}}, nil
}

// --- guardrail 1: a default naming an unregistered runtime fails at
// startup, not per request -----------------------------------------------

func TestSetDefaultRuntime_RefusesUnregisteredRuntime(t *testing.T) {
	svc := New("self")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	err := svc.SetDefaultRuntime("does-not-exist")
	if err == nil {
		t.Fatal("SetDefaultRuntime accepted a runtime that was never registered")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not name the offending runtime", err.Error())
	}
	if got := svc.DefaultRuntime(); got != "" {
		t.Errorf("DefaultRuntime() = %q after a refused Set, want empty — a rejected value must not partially apply", got)
	}
}

func TestSetDefaultRuntime_AcceptsRegisteredRuntime(t *testing.T) {
	svc := New("self")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("rt-a"); err != nil {
		t.Fatalf("SetDefaultRuntime(registered) = %v, want nil", err)
	}
	if got := svc.DefaultRuntime(); got != "rt-a" {
		t.Errorf("DefaultRuntime() = %q, want %q", got, "rt-a")
	}
}

func TestSetDefaultRuntime_EmptyClearsAndIsAlwaysAccepted(t *testing.T) {
	svc := New("self")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("rt-a"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime(""); err != nil {
		t.Fatalf("SetDefaultRuntime(\"\") = %v, want nil — clearing must always succeed", err)
	}
	if got := svc.DefaultRuntime(); got != "" {
		t.Errorf("DefaultRuntime() = %q after clearing, want empty", got)
	}
}

// --- test harness: two local drivers, an optional default -----------------

func newDualDriverServer(t *testing.T, a, b *idDriver, defaultRuntime fleet.RuntimeId) (*Service, *httptest.Server) {
	t.Helper()
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", a); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-b", b); err != nil {
		t.Fatal(err)
	}
	if defaultRuntime != "" {
		if err := svc.SetDefaultRuntime(defaultRuntime); err != nil {
			t.Fatal(err)
		}
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return svc, srv
}

func getSession(t *testing.T, srv *httptest.Server, id string) *http.Response {
	t.Helper()
	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// --- guardrail 3: existence first, default as tiebreak ---------------------

// TestResolve_ExistenceMatchWinsOverDefault is the sharpest test guardrail 3
// asks for: a bare id belongs to the NON-default runtime and must still
// resolve there — not fall to the configured default and render a false
// `not_found` for a session that plainly exists.
func TestResolve_ExistenceMatchWinsOverDefault(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}} // configured default; does NOT hold the id
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: map[string]bool{"abc": true}}
	_, srv := newDualDriverServer(t, a, b, "rt-a")

	resp := getSession(t, srv, "abc")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 — the id belongs to rt-b and must resolve there regardless of "+
			"rt-a being the configured default; body: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "rt-b" {
		t.Errorf("Fleet-Runtime = %q, want %q", got, "rt-b")
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — an existence match is not a default guess", got)
	}
}

// TestResolve_GenuineMissFallsToDefault covers the case the ruling names
// explicitly: every local driver affirmatively confirms it never had the
// id, so there is nothing to route to except the configured default —
// mirroring create's own ambiguous case.
func TestResolve_GenuineMissFallsToDefault(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	_, srv := newDualDriverServer(t, a, b, "rt-a")

	resp := getSession(t, srv, "nowhere")
	defer resp.Body.Close()

	// Neither driver has ever had this id, so the eventual answer is an
	// honest 404 — but it must be attributed to the configured default,
	// and that attribution must be visible on the response.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "rt-a" {
		t.Errorf("Fleet-Runtime = %q, want %q", got, "rt-a")
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "default" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want %q — guardrail 2 requires this be visible in the answer", got, "default")
	}
}

// TestResolve_NoDefaultConfigured_GenuineMissStaysAmbiguous is
// config.go's own "nothing here has a default; an absent value means the
// older behaviour" applied to this setting: with nothing configured, a
// genuine miss across more than one local runtime is still refused rather
// than guessed at.
func TestResolve_NoDefaultConfigured_GenuineMissStaysAmbiguous(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	_, srv := newDualDriverServer(t, a, b, "")

	resp := getSession(t, srv, "nowhere")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — no default configured means the older ambiguous behaviour", resp.StatusCode)
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — nothing resolved", got)
	}
}

// TestResolve_CollisionRefusesEvenWithDefault is §5.4 applied to routing: two
// runtimes both claiming the same id is exactly the case that rule exists
// for, and it must be surfaced explicitly — the configured default must not
// quietly pick a winner.
func TestResolve_CollisionRefusesEvenWithDefault(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: map[string]bool{"dup": true}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: map[string]bool{"dup": true}}
	_, srv := newDualDriverServer(t, a, b, "rt-a")

	resp := getSession(t, srv, "dup")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a genuine collision must be told to the caller, never silently resolved by the default", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "rt-a") || !strings.Contains(body, "rt-b") {
		t.Errorf("error body %q does not name both colliding runtimes", body)
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — nothing resolved", got)
	}
}

// TestResolve_InconclusiveProbeRefusesEvenWithDefault is guardrail 3's other
// edge: one driver's existence check could not be completed (it failed for
// a reason other than "never had this id"), and nothing else affirms the
// id. Defaulting past that risks the exact false absence guardrail 3
// forbids — the inconclusive driver might be the one that actually holds
// it — so this must refuse, not guess, even with a default configured.
func TestResolve_InconclusiveProbeRefusesEvenWithDefault(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200},
		failErr: map[string]error{"flaky": fmt.Errorf("driver: transient failure")}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}} // affirmatively confirms absence
	_, srv := newDualDriverServer(t, a, b, "rt-b")

	resp := getSession(t, srv, "flaky")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — rt-a's probe was inconclusive, so a genuine miss was never confirmed", resp.StatusCode)
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — nothing resolved", got)
	}
}

// TestResolve_SoleLocalDriverIsUnaffectedByDefault is the case that held for
// this repository's entire life before a second local driver existed: one
// registered runtime, no ambiguity, and no default consulted at all.
func TestResolve_SoleLocalDriverIsUnaffectedByDefault(t *testing.T) {
	svc := New("test-machine")
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: map[string]bool{"abc": true}}
	if err := svc.RegisterLocalDriver("rt-a", a); err != nil {
		t.Fatal(err)
	}
	// No second driver, so nothing is ambiguous and there is nothing for a
	// default to break a tie between — but set one anyway to prove it plays
	// no part in the sole-driver case.
	if err := svc.SetDefaultRuntime("rt-a"); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := getSession(t, srv, "abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "rt-a" {
		t.Errorf("Fleet-Runtime = %q, want %q", got, "rt-a")
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — the sole driver is not the default's doing", got)
	}
}

// --- create: the genuine-miss path that has no id to check existence for --

func TestCreate_AmbiguousFallsToDefault(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	_, srv := newDualDriverServer(t, a, b, "rt-a")

	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions",
		[]byte(`{"cwd":"/tmp"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "rt-a" {
		t.Errorf("Fleet-Runtime = %q, want %q", got, "rt-a")
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "default" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want %q", got, "default")
	}
	if a.created != 1 {
		t.Errorf("rt-a.created = %d, want 1 — the default should have served this create", a.created)
	}
	if b.created != 0 {
		t.Errorf("rt-b.created = %d, want 0 — only the default should have served this create", b.created)
	}

	var body struct {
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Runtime != "rt-a" {
		t.Errorf("response runtime = %q, want %q — it must name the driver that actually served the create, "+
			"not echo the (absent) caller hint", body.Runtime, "rt-a")
	}
}

func TestCreate_AmbiguousWithNoDefaultIsRefused(t *testing.T) {
	a := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	b := &idDriver{Driver: stub.Driver{DeadlineMs: 200}}
	_, srv := newDualDriverServer(t, a, b, "")

	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions",
		[]byte(`{"cwd":"/tmp"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — no default configured means the older ambiguous behaviour", resp.StatusCode)
	}
	if a.created != 0 || b.created != 0 {
		t.Errorf("a driver ran a create despite the ambiguity being refused: a=%d b=%d", a.created, b.created)
	}
}

// --- federation: the default never crosses to a peer ------------------------

// TestResolveSessionDriver_PeerNeverSeesLocalDefault is the FEDERATION
// requirement from colab-fleet issue #60: a proxied request never reaches
// this machine's default at all, so the same bare id addressed on a peer
// cannot mean something different depending on which machine answered it.
func TestResolveSessionDriver_PeerNeverSeesLocalDefault(t *testing.T) {
	svc := New("self")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-b", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("rt-a"); err != nil {
		t.Fatal(err)
	}
	peer := &stub.Driver{DeadlineMs: 200}
	if err := svc.RegisterPeerDriver("otherbox", peer); err != nil {
		t.Fatal(err)
	}

	d, rt, via, ferr := svc.resolveSessionDriver(context.Background(), fleet.Request{}, "otherbox", "abc", "", 0)
	if ferr != nil {
		t.Fatalf("resolveSessionDriver: %v", ferr)
	}
	if d != peer {
		t.Errorf("resolved driver is not the registered peer driver")
	}
	if rt != "" {
		t.Errorf("runtime = %q, want empty — a peer resolution names no local runtime", rt)
	}
	if via != resolvedPeer {
		t.Errorf("via = %q, want %q", via, resolvedPeer)
	}
}

// TestGetSession_PeerRelayCarriesNoRuntimeHeaders is the same guarantee at
// the wire: a call proxied to a peer must not carry Fleet-Runtime or
// Fleet-Runtime-Resolution, because neither fact is this machine's to
// report — reporting either would misattribute a peer's own resolution to
// this machine's local drivers and its configured default.
func TestGetSession_PeerRelayCarriesNoRuntimeHeaders(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("rt-a", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("rt-b", &idDriver{Driver: stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("rt-a"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/otherbox/sessions/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Fleet-Runtime"); got != "" {
		t.Errorf("Fleet-Runtime = %q, want absent on a peer-relayed call", got)
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want absent — this machine's default must never apply to a peer", got)
	}
}

func readAll(resp *http.Response) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}
