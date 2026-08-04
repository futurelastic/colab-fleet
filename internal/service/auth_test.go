package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

func principalSrv(t *testing.T, ps []Principal) *httptest.Server {
	t.Helper()
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 1000}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: ps}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// §6 requirement 3, finally expressible as written: opt-in PER VERB, PER
// PRINCIPAL. A shared token can express neither.
func TestGrantsAreCheckedPerVerbPerPrincipal(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "watcher", Token: "tok-watch", Grants: []Grant{GrantRead}},
		{Name: "operator", Token: "tok-op", Grants: []Grant{GrantRead, GrantSend}},
		{Name: "destroyer", Token: "tok-kill", Grants: []Grant{GrantRead, GrantClose}},
	})
	denied := func(c int) bool { return c == 401 || c == 403 }

	// Reads for everyone who holds read.
	if denied(call(t, srv, http.MethodGet, "/v1/sessions", "tok-watch")) {
		t.Error("watcher holds read and was refused a read")
	}
	// The distinction a single mutate bit cannot make.
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-watch")) {
		t.Error("watcher has no send grant but was allowed to send")
	}
	if denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-op")) {
		t.Error("operator holds send and was refused")
	}
	if !denied(call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok-op")) {
		t.Error("operator has no close grant but was allowed to destroy")
	}
	if denied(call(t, srv, http.MethodDelete, "/v1/machines/testbox/sessions/s1", "tok-kill")) {
		t.Error("destroyer holds close and was refused")
	}
}

// An unrecognised credential is unauthorized, and says nothing about which
// part was wrong.
func TestUnknownCredentialIsRefused(t *testing.T) {
	srv := principalSrv(t, []Principal{{Name: "a", Token: "good", Grants: []Grant{GrantRead}}})
	if code := call(t, srv, http.MethodGet, "/v1/sessions", "not-a-real-token"); code != 401 && code != 403 {
		t.Errorf("status = %d, want unauthorized", code)
	}
}

// D6's host/client split survives as a grant: relaying to a peer is a separate
// permission from mutating locally.
func TestRelayIsASeparateGrantFromLocalMutation(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "client-only", Token: "tok-c", Grants: []Grant{GrantRead, GrantRelay}},
		{Name: "host-only", Token: "tok-h", Grants: []Grant{GrantRead, GrantSend}},
	})
	denied := func(c int) bool { return c == 401 || c == 403 }

	// Hardened host, full client.
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-c")) {
		t.Error("client-only mutated a local session")
	}
	if denied(call(t, srv, http.MethodPost, "/v1/machines/otherbox/sessions/s1/input", "tok-c")) {
		t.Error("client-only holds relay but could not reach a peer")
	}
	// And the mirror.
	if denied(call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-h")) {
		t.Error("host-only holds send and was refused locally")
	}
	if !denied(call(t, srv, http.MethodPost, "/v1/machines/otherbox/sessions/s1/input", "tok-h")) {
		t.Error("host-only has no relay grant but reached a peer")
	}
}

// §6 requirement 4 wants an actor. With a table, the actor is a name; a
// relayed request names who asked and who relayed, because an audit line
// saying only "the peer did it" cannot answer who asked the peer.
func TestCallerIsANameAndRelayNamesBothParties(t *testing.T) {
	p := Principal{Name: "peer-b", Token: "t", Grants: []Grant{GrantRead}}

	direct := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	if got := callerFor(p, direct).Principal; got != "peer-b" {
		t.Errorf("principal = %q, want the identity", got)
	}

	relayed := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	relayed.Header.Set(onBehalfOfHeader, "dashboard")
	got := callerFor(p, relayed).Principal
	if !strings.Contains(got, "dashboard") || !strings.Contains(got, "peer-b") {
		t.Errorf("principal = %q, want both the original asker and the relay", got)
	}
}

// Grants default to nothing. §6's default is denied, and an empty list must
// not read as "unrestricted" the way an empty filter does elsewhere.
func TestNoGrantsMeansNothingPermitted(t *testing.T) {
	p := Principal{Name: "mute", Token: "t"}
	for _, g := range []Grant{GrantRead, GrantSend, GrantCreate, GrantClose, GrantInterrupt, GrantRelay} {
		if p.Allows(g) {
			t.Errorf("a principal with no grants must not hold %q", g)
		}
	}
}

var _ = fleet.MachineId("")

// §6 requirement 4 wants the ACTOR, and for a relayed request the actor is
// whoever asked — not the machine that carried it. A live relayed mutation
// logged only the relay, because the audit path derived the name separately
// from the caller path.
func TestAuditActorMatchesTheCallerItLogs(t *testing.T) {
	p := Principal{Name: "relaybox", Token: "t", Grants: []Grant{GrantSend}}
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/x/sessions/s1/input", nil)
	r.Header.Set(onBehalfOfHeader, "operator")

	actor := actorOf(p, r)
	if actor != callerFor(p, r).Principal {
		t.Fatalf("audit actor %q differs from the caller %q; two derivations of "+
			"one fact will drift", actor, callerFor(p, r).Principal)
	}
	if !strings.Contains(actor, "operator") {
		t.Errorf("actor = %q; an audit line naming only the relay cannot answer "+
			"who asked the peer", actor)
	}
}
