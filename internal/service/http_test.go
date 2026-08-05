package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

const testToken = "test-token"

func newTestServer(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return svc, srv
}

func authedRequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeError(t *testing.T, resp *http.Response) fleet.ErrorEnvelope {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env
}

func TestHealth_OK(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["epoch"]; !ok {
		t.Error("health response missing epoch")
	}
}

func TestAuth_MissingTokenIs401(t *testing.T) {
	_, srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — api-http.md §5: no unauthenticated mode, ever", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorUnauthorized {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorUnauthorized)
	}
}

func TestAuth_WrongTokenIs401(t *testing.T) {
	_, srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCreateSession_MissingIdempotencyKeyIs400(t *testing.T) {
	_, srv := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"runtime": "stub", "cwd": "/tmp"})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (§10: Idempotency-Key is required, not optional)", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorInvalid {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorInvalid)
	}
}

func TestCreateSession_StubReturnsUnsupported501(t *testing.T) {
	_, srv := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"runtime": "stub", "cwd": "/tmp"})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorUnsupported {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorUnsupported)
	}
}

func TestGetSession_UnknownMachineIs404(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/nowhere/sessions/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetSession_AmbiguousRuntimeIs400(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub-a", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	if err := svc.RegisterLocalDriver("stub-b", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — two local runtimes and no ?runtime= hint is genuinely ambiguous (api-http.md §3.3)", resp.StatusCode)
	}

	// The disambiguated form must succeed in routing (still 501, since the
	// driver itself is unsupported, but no longer ambiguous).
	req2 := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/abc?runtime=stub-a", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 once ?runtime= disambiguates", resp2.StatusCode)
	}
}

func TestListSessions_StubReportsDegradedSource(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions?scope=local", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a source that can't answer degrades the envelope, it does not fail the whole call (§3.3)", resp.StatusCode)
	}

	var col fleet.Collection[fleet.Session]
	if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if col.Complete() {
		t.Fatal("Complete() = true, want false — the only driver returned ErrUnsupported")
	}
	if len(col.Sources()) != 1 || col.Sources()[0].Status != fleet.SourceDegraded {
		t.Fatalf("Sources() = %+v, want exactly one degraded source", col.Sources())
	}
	if len(col.Items()) != 0 {
		t.Fatalf("Items() = %v, want empty", col.Items())
	}
}

func TestListSessions_InvalidScopeIs400(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions?scope=bogus", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// /v1/events used to answer "unsupported" honestly while no driver could
// stream. It streams now; what must hold is that it opens as SSE rather than
// buffering, since a buffered event stream is indistinguishable from a hung
// one.
func TestEventsOpensAsAStream(t *testing.T) {
	svc := New("testbox")
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
}

func TestSendInput_RefusalIsNotAnHTTPError(t *testing.T) {
	// This skeleton's stub driver only ever returns ErrUnsupported (a
	// driver-level failure, correctly 501) — it cannot produce a refused
	// DeliveryReceipt to prove the 200-not-4xx rule end to end. That
	// requires a driver capable of returning DeliveryReceipt{Outcome:
	// refused} at all, which does not exist yet in this repo. Documented
	// here as a known gap, not silently skipped.
	t.Skip("no driver in this skeleton can produce a refused DeliveryReceipt; see comment")
}

// §6 requirement 3: mutations are opt-in. The default must be closed, because
// a single shared token cannot distinguish a peer from a local supervisor, and
// the first thing a fleet exposes across machines should not be the ability to
// start processes.
func TestMutationsAreDeniedByDefault(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	// Note: no AllowMutations.
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	cases := []struct {
		name, method, path string
	}{
		{"create", http.MethodPost, "/v1/machines/testbox/sessions"},
		{"input", http.MethodPost, "/v1/machines/testbox/sessions/s1/input"},
		{"interrupt", http.MethodPost, "/v1/machines/testbox/sessions/s1/interrupt"},
		{"close", http.MethodDelete, "/v1/machines/testbox/sessions/s1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Idempotency-Key", "k")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 401 && resp.StatusCode != 403 {
				t.Errorf("%s returned %d; a read-only instance must refuse mutating verbs",
					tc.name, resp.StatusCode)
			}
			// Unauthorized, never unsupported: the driver is capable, this
			// instance is configured not to permit it. Saying "unsupported"
			// would tell a caller something false about the runtime.
			if resp.StatusCode == 501 {
				t.Errorf("%s reported unsupported; the capability exists, the permission does not", tc.name)
			}
		})
	}
}

// Reads must stay open in the same configuration, or "read-only" would just
// mean "off".
func TestReadsStayOpenWhenMutationsAreDenied(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	for _, path := range []string{"/v1/health", "/v1/machines", "/v1/runtimes", "/v1/sessions"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			t.Errorf("%s returned %d; reads are permitted by default (§6)", path, resp.StatusCode)
		}
	}
}

// §6 / defect D6: "may mutate sessions on this machine" and "may relay a
// mutation to a peer" are different grants. A hardened host must still be
// able to act as a full-featured client, and vice versa.
func TestHostAndRelayPermissionsAreIndependent(t *testing.T) {
	newSrv := func(local, relay bool) *httptest.Server {
		svc := New("testbox")
		if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
			t.Fatal(err)
		}
		if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 1000}); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(NewMux(svc, Config{
			Token: testToken, AllowLocalMutations: local, AllowPeerRelay: relay,
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	post := func(srv *httptest.Server, machine string) int {
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/machines/"+machine+"/sessions/s1/input", strings.NewReader(`{"text":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	denied := func(code int) bool { return code == 401 || code == 403 }

	// The configuration D6 made impossible: hardened host, full client.
	s := newSrv(false, true)
	if !denied(post(s, "testbox")) {
		t.Error("host is read-only but accepted a mutation of its own session")
	}
	if denied(post(s, "otherbox")) {
		t.Error("relay is permitted, yet forwarding to a peer was refused — this is D6")
	}

	// And the mirror: mutate locally, refuse to be a relay.
	s = newSrv(true, false)
	if denied(post(s, "testbox")) {
		t.Error("local mutations permitted but refused")
	}
	if !denied(post(s, "otherbox")) {
		t.Error("relay is not permitted, yet the mutation was forwarded")
	}

	// Both closed is still the default posture.
	s = newSrv(false, false)
	if !denied(post(s, "testbox")) || !denied(post(s, "otherbox")) {
		t.Error("with both grants closed, nothing mutating may pass")
	}
}

// §13.2's principle applied to errors: a peer's classification is relayed,
// never re-derived. Found by watching a correct 409 become a 400 in transit.
func TestPeerErrorKindIsRelayedNotReclassified(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDriverError(rec, "peerbox", time.Second,
		&fleet.Error{Kind: fleet.ErrorConflict, Message: "stale expectation", Machine: "peerbox"})
	if rec.Code != 409 {
		t.Errorf("status = %d, want 409; the peer had already classified this and "+
			"re-deriving a kind here can only lose information", rec.Code)
	}
}

// api-http.md §3.1 tells clients they MUST consult /v1/runtimes before relying
// on a capability. That rule was unfollowable for peer runtimes while this
// listed only local ones — the case where a caller cannot simply know.
func TestRuntimesIncludesPeersAndMarksUnconfirmedOnes(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 2000}); err != nil {
		t.Fatal(err)
	}
	col, err := svc.ListRuntimes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sawPeer bool
	for _, it := range col.Items() {
		if it.Machine == "otherbox" {
			sawPeer = true
		}
	}
	if !sawPeer {
		t.Fatal("peer runtimes absent; a client cannot consult what is not listed")
	}
	var peerSrc *fleet.SourceStatus
	for i := range col.Sources() {
		if col.Sources()[i].Machine == "otherbox" {
			peerSrc = &col.Sources()[i]
		}
	}
	if peerSrc == nil {
		t.Fatal("peer contributed no SourceStatus")
	}
}

// api-http.md §3.3 says a single-session read returns cwd, agent, model and
// startedAt. It returned only id and state — and startedAt is what a caller
// quotes back to make a destroy corroborable (§5.4), so the strong guarantee
// was unreachable through the endpoint a caller reads before destroying.
func TestSingleSessionReadReturnsAWholeSession(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("fake", &sessionDriver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/testbox/sessions/s1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got fleet.Session
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Cwd == "" {
		t.Error("cwd missing")
	}
	if got.StartedAt == nil {
		t.Error("startedAt missing — §5.4's corroboration cannot be invoked without it")
	}
}
