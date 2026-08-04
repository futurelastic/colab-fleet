package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

const peerToken = "the-token-the-peer-accepts"
const callerTok = "the-original-callers-token"

// caller is what a service derives from an inbound request and hands down.
var caller = fleet.Request{Caller: fleet.Caller{Principal: "addr:198.51.100.7", Credential: callerTok}}

// noAuthority is a caller with nothing to present. Every operation must
// refuse it — this driver holds no credential of its own to fall back to.
var noAuthority = fleet.Request{Caller: fleet.Caller{Principal: "addr:198.51.100.7"}}

// capture records what the peer actually received, which is where most of
// this driver's contract lives: the rules it must obey are about what goes
// on the wire, not about what it returns.
type capture struct {
	method string
	path   string
	query  string
	auth   string
	idem   string
	dline  string
	body   string
}

func peerServing(t *testing.T, status int, payload any, rec *capture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			raw, _ := io.ReadAll(r.Body)
			*rec = capture{
				method: r.Method,
				path:   r.URL.Path,
				query:  r.URL.RawQuery,
				auth:   r.Header.Get("Authorization"),
				idem:   r.Header.Get("Idempotency-Key"),
				dline:  r.Header.Get("Fleet-Deadline-Ms"),
				body:   string(raw),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collectionJSON(sources []fleet.SourceStatus, items []fleet.Session) any {
	return map[string]any{"items": items, "sources": sources, "complete": true}
}

// §13.1: a proxied request asks for the peer's LOCAL view only. Without
// this, two mutually-configured peers query each other forever — or, worse,
// double-count and look fine.
func TestListAsksForThePeersLocalViewOnly(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)

	d := New("peerbox", srv.URL)
	if _, err := d.List(context.Background(), caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "scope=local") {
		t.Errorf("query = %q, must contain scope=local (§13.1)", rec.query)
	}
	if strings.Contains(rec.query, "scope=fleet") {
		t.Error("a proxied call must never ask a peer to fan out further")
	}
}

// §13.2: adopt the peer's SourceStatus; never re-synthesize it. A peer can
// answer promptly AND report itself degraded.
func TestListAdoptsADegradedPeersOwnSourceStatus(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{
			Machine: "peerbox", Status: fleet.SourceDegraded,
			Error: "one runtime is not answering", ObservedAt: time.Now(),
		}}, nil), nil)

	d := New("peerbox", srv.URL)
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources()) != 1 {
		t.Fatalf("want exactly one adopted source, got %d", len(got.Sources()))
	}
	if got.Sources()[0].Status != fleet.SourceDegraded {
		t.Errorf("status = %q; the call succeeding must not overwrite the peer's "+
			"own self-report with ok (§13.2)", got.Sources()[0].Status)
	}
	if got.Sources()[0].Error == "" {
		t.Error("the peer's explanation was dropped in transit")
	}
	if got.Complete() {
		t.Error("a degraded source must not produce a complete envelope (§9)")
	}
}

// §5.7 across a network: a peer that cannot be reached contributes a
// SourceStatus, never an absence.
func TestUnreachablePeerIsASourceNotAnEmptyList(t *testing.T) {
	// A closed server: the connection fails at the transport layer.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	d := New("peerbox", url)
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("an unreachable peer belongs in the envelope, not in err: %v", err)
	}
	if got.Complete() {
		t.Error("unreachable peer must not produce a complete envelope")
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Fatalf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Sources()[0].Machine != "peerbox" {
		t.Errorf("source machine = %q, want peerbox", got.Sources()[0].Machine)
	}
	if got.Sources()[0].Error == "" {
		t.Error("an unreachable source must say why (§9)")
	}
}

// api-http.md §2's "single most important line": not_found and unreachable
// must never be conflated. A client that treats 504 as 404 reports work as
// gone while it is running fine on an unreachable host.
func TestNotFoundAndUnreachableStaySeparate(t *testing.T) {
	cases := []struct {
		name   string
		status int
		kind   fleet.ErrorKind
		want   fleet.ErrorKind
	}{
		{"peer says no such session", 404, fleet.ErrorNotFound, fleet.ErrorNotFound},
		{"peer did not answer in time", 504, fleet.ErrorUnreachable, fleet.ErrorUnreachable},
		{"peer refused us", 401, fleet.ErrorUnauthorized, fleet.ErrorUnauthorized},
		{"driver lacks capability", 501, fleet.ErrorUnsupported, fleet.ErrorUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := peerServing(t, tc.status, fleet.ErrorEnvelope{
				Error: fleet.Error{Kind: tc.kind, Message: "from the peer", Machine: "peerbox"},
			}, nil)
			d := New("peerbox", srv.URL)
			_, err := d.State(context.Background(), caller, fleet.SessionRef{ID: "s1"})
			if err == nil {
				t.Fatal("expected an error")
			}
			var fe *fleet.Error
			if !errors.As(err, &fe) {
				t.Fatalf("error lost its kind in transit: %v", err)
			}
			if fe.Kind != tc.want {
				t.Errorf("kind = %q, want %q", fe.Kind, tc.want)
			}
		})
	}
}

// §13 / api-http.md §5: a proxy presents the ORIGINAL caller's authority.
//
// This used to be the design's most serious defect: authority travelled in a
// context value a service could forget, and a remote driver missing it fell
// back to its own token — succeeding, passing tests, and silently widening
// authorization. Authority is now a parameter, and this driver holds no
// credential at all, so the fallback has nothing to fall back to.
func TestEveryOperationPresentsTheCallersCredential(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello", driver.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.auth != "Bearer "+callerTok {
		t.Errorf("Authorization = %q, want the caller's own credential (§13)", rec.auth)
	}
}

// Reads included. "Which sessions exist, in which directories, on which
// machine" is exactly the reconnaissance an unauthorized caller wants, and §6
// grants read permission broadly rather than universally.
func TestNoOperationProceedsWithoutCallerAuthority(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)
	d := New("peerbox", srv.URL)
	ctx := context.Background()

	// A read returns its refusal inside the envelope (§5.7), not as an error.
	col, err := d.List(ctx, noAuthority, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if col.Complete() {
		t.Error("List without authority must not report a complete envelope")
	}

	if _, err := d.State(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("State: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Send(ctx, noAuthority, fleet.SessionRef{ID: "s1"}, "x", driver.SendOptions{}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Send: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Create(ctx, noAuthority, "k", fleet.SessionSpec{Cwd: "/w"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Create: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Close(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Close: want ErrNoCallerAuthority, got %v", err)
	}
	if _, err := d.Interrupt(ctx, noAuthority, fleet.SessionRef{ID: "s1"}); !errors.Is(err, ErrNoCallerAuthority) {
		t.Errorf("Interrupt: want ErrNoCallerAuthority, got %v", err)
	}

	// The strongest form of the assertion: not one request was attempted.
	// A driver that reached the peer at all would have had to present
	// something, and there is nothing it could honestly present.
	if rec.auth != "" {
		t.Errorf("a request was made without caller authority, presenting %q", rec.auth)
	}
}

// §10: the caller's idempotency key must be forwarded unchanged. A proxy
// minting its own key per attempt defeats the mechanism precisely when it
// matters — a retried federated create would arrive with a different key and
// read as a different request, producing two agents in one directory.
func TestCreateForwardsTheCallersIdempotencyKeyUnchanged(t *testing.T) {
	var rec capture
	// A zero SessionState will not marshal: Status is a closed set and the
	// empty string is not a member (state.go). The peer must return a real
	// one, which is the enforcement working rather than a fixture nicety.
	srv := peerServing(t, 201, fleet.Session{
		SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "new-1"},
		Runtime:    "claude-code-tmux",
		State:      fleet.InferredState(fleet.StatusStarting, "just created", nil),
	}, &rec)
	d := New("peerbox", srv.URL)

	ref, err := d.Create(context.Background(), caller, "caller-key-42", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.idem != "caller-key-42" {
		t.Errorf("Idempotency-Key = %q, want the caller's key verbatim (§10)", rec.idem)
	}
	if ref.ID != "new-1" {
		t.Errorf("ref = %+v", ref)
	}
}

// §2.4 / api-http.md §3.3: a refusal is a 200 carrying an outcome, not an
// HTTP error, and this driver must not convert it into one — that would
// train callers to retry the thing the refusal exists to prevent.
func TestRefusalSurvivesAsAValueNotAnError(t *testing.T) {
	srv := peerServing(t, 200, fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeRefused,
		Reason:  "composer holds unsent input",
	}, nil)
	d := New("peerbox", srv.URL)

	got, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hi", driver.SendOptions{})
	if err != nil {
		t.Fatalf("a refusal must not surface as an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused || got.Reason == "" {
		t.Errorf("receipt = %+v, want a refusal carrying its reason", got)
	}
}

// §4.4: a caller may shorten a deadline, never lengthen it, and the peer is
// told the bound so it can fail fast rather than working on a request the
// caller has already abandoned.
func TestDeadlineIsAnnouncedAndCallersMayOnlyShortenIt(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)

	d := New("peerbox", srv.URL, WithDeadline(5*time.Second))

	// A shorter caller deadline wins.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := d.List(ctx, caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if rec.dline == "" {
		t.Fatal("no Fleet-Deadline-Ms header sent (§3.3)")
	}
	if ms := parseMs(t, rec.dline); ms > 1000 {
		t.Errorf("announced deadline %dms; the caller asked for 200ms and a "+
			"driver may never lengthen it (§4.4)", ms)
	}

	// A longer caller deadline does NOT win.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if _, err := d.List(ctx2, caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if ms := parseMs(t, rec.dline); ms > 6000 {
		t.Errorf("announced deadline %dms; a caller must not be able to extend "+
			"the driver's declared 5s bound (§4.4)", ms)
	}
}

func parseMs(t *testing.T, s string) int64 {
	t.Helper()
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad deadline header %q: %v", s, err)
	}
	return ms
}

// A hung peer must produce unreachable in bounded time — §4.4's whole
// reason for existing, measured originally against a SIGSTOPped host that
// blocked a caller for seven seconds and would have waited forever.
func TestAHungPeerBecomesUnreachableWithinTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer
	}))
	defer srv.Close()

	d := New("peerbox", srv.URL, WithDeadline(300*time.Millisecond))
	started := time.Now()
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("a hung peer degrades an envelope; it does not fail the call: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %s to give up on a 300ms deadline", elapsed)
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Errorf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Complete() {
		t.Error("must not report complete")
	}
}

// FINDING 1/2: capabilities of an unreached peer are indistinguishable from
// a peer that supports nothing — except through this concrete type.
func TestCapabilitiesAreConservativeUntilThePeerAnswers(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), nil)
	_ = srv

	d := New("peerbox", "http://127.0.0.1:1", WithDeadline(200*time.Millisecond))

	caps := d.Capabilities()
	if d.CapabilitiesKnown() {
		t.Error("peer has never answered; capabilities must not read as known")
	}
	if caps.ObservesState || caps.ConfirmsDelivery || caps.SupportsResume {
		t.Error("an unreached peer must not be credited with capabilities (§5.6)")
	}
	// §4.4 still has to hold, or the driver could never be registered.
	if err := caps.Validate(); err != nil {
		t.Errorf("a remote driver must be registrable before its peer answers: %v", err)
	}
}

func TestRefreshCapabilitiesAdoptsWhatThePeerReports(t *testing.T) {
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), nil)
	_ = srv

	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Runtime: "claude-code-tmux",
			Capabilities: fleet.DriverCapabilities{
				ObservesState: false, ConfirmsDelivery: false,
				SupportsResume: true, DeadlineMs: 5000,
				Source: fleet.CapabilitiesObserved,
			},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv2 := peerServing(t, 200, runtimes, nil)

	d := New("peerbox", srv2.URL)
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if !d.CapabilitiesKnown() {
		t.Error("capabilities should read as known after the peer answered")
	}
	caps := d.Capabilities()
	if !caps.SupportsResume {
		t.Error("did not adopt the peer's declared capability")
	}
	if caps.ObservesState {
		t.Error("invented a capability the peer did not declare")
	}
}

// The expectation must survive the wire, or every cross-machine destroy
// silently downgrades to the weak check.
func TestCloseForwardsTheCallersExpectation(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, nil, &rec)
	d := New("peerbox", srv.URL)

	ts := time.Unix(1785600000, 0)
	req := fleet.Request{
		Caller: fleet.Caller{Principal: "addr:test", Credential: callerTok},
		Expect: fleet.Expectation{StartedAt: &ts},
	}
	if _, err := d.Close(context.Background(), req, fleet.SessionRef{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.query, "startedAt=") {
		t.Fatalf("query = %q, must carry the caller's expected start time (§5.4)", rec.query)
	}
	if rec.method != "DELETE" {
		t.Errorf("method = %q", rec.method)
	}
}

// And a caller that supplies none must not have one invented for it.
func TestCloseWithoutExpectationSendsNone(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, nil, &rec)
	d := New("peerbox", srv.URL)
	if _, err := d.Close(context.Background(), caller, fleet.SessionRef{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.query, "startedAt=") {
		t.Errorf("query = %q; a proxy must not manufacture an expectation the caller did not have", rec.query)
	}
}

// §14 D7: a proxy that waits less time than its peer declared abandons calls
// the peer would have completed, and reports a healthy machine as unreachable.
func TestProxyWaitsAtLeastAsLongAsThePeerDeclared(t *testing.T) {
	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Runtime: "claude-code-tmux",
			Capabilities: fleet.DriverCapabilities{
				SupportsResume: true, DeadlineMs: 5000, Source: fleet.CapabilitiesObserved},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, runtimes, nil)
	d := New("peerbox", srv.URL, WithDeadline(3*time.Second), WithTransitMargin(2*time.Second))

	if got := d.Capabilities().DeadlineMs; got != 3000 {
		t.Fatalf("before the peer answers, the floor applies: got %dms", got)
	}
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	// 5s declared by the peer + 2s transit; never the 3s floor.
	if got := d.Capabilities().DeadlineMs; got != 7000 {
		t.Errorf("deadline = %dms, want 7000 (peer's 5000 + 2000 transit). A proxy "+
			"waiting less than its peer declared turns a healthy machine into an "+
			"unreachable one", got)
	}
}

// The floor still wins when it is the larger of the two — a caller that
// configured a generous proxy deadline does not get it silently reduced.
func TestPeerDeadlineNeverShortensTheConfiguredFloor(t *testing.T) {
	runtimes := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox", Capabilities: fleet.DriverCapabilities{
				DeadlineMs: 500, Source: fleet.CapabilitiesObserved},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, runtimes, nil)
	d := New("peerbox", srv.URL, WithDeadline(9*time.Second))
	if err := d.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().DeadlineMs; got != 9000 {
		t.Errorf("deadline = %dms, want the 9000 floor", got)
	}
}

// D3, stated as the distinction that did not previously exist: a peer that
// genuinely supports nothing and a peer nobody has reached produce identical
// flags. Only provenance separates them.
func TestMinimalPeerIsDistinguishableFromUnreachedPeer(t *testing.T) {
	minimal := map[string]any{
		"items": []fleet.RuntimeInfo{{
			Machine: "peerbox",
			Capabilities: fleet.DriverCapabilities{
				DeadlineMs: 1000, Source: fleet.CapabilitiesObserved,
			},
		}},
		"sources":  []fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		"complete": true,
	}
	srv := peerServing(t, 200, minimal, nil)

	reached := New("peerbox", srv.URL)
	if err := reached.RefreshCapabilities(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	unreached := New("peerbox", "http://127.0.0.1:1", WithDeadline(200*time.Millisecond))

	a, b := reached.Capabilities(), unreached.Capabilities()

	// The flags really are identical — that is the point.
	if a.ObservesState != b.ObservesState || a.SupportsResume != b.SupportsResume {
		t.Fatal("fixture drifted; both should report nothing supported")
	}
	if a.Source != fleet.CapabilitiesObserved {
		t.Errorf("a peer that answered should read observed, got %q", a.Source)
	}
	if b.Source != fleet.CapabilitiesAssumed {
		t.Errorf("a peer that never answered should read assumed, got %q", b.Source)
	}
	if a.ObservedAt == nil {
		t.Error("an observed declaration should say when")
	}
}

// A declaration with no provenance must not marshal. An absent source is not
// the same fact as "assumed", and silently defaulting would rebuild exactly
// the ambiguity this field removes.
func TestCapabilitiesWithoutProvenanceDoNotMarshal(t *testing.T) {
	if _, err := json.Marshal(fleet.DriverCapabilities{DeadlineMs: 1000}); err == nil {
		t.Error("capabilities with no source should not encode")
	}
}
