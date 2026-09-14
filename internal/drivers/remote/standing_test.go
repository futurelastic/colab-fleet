package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	"github.com/godx-jp/colab-fleet/internal/service"
)

// colab-fleet #154, the prober's half: what this machine learns about its own
// registration on a peer, and above all what it must NOT conclude from a peer
// that cannot answer.

// whoamiPeer answers /v1/whoami with the given status and body, and nothing
// else. It records the whoami query.
func whoamiPeer(t *testing.T, status int, body string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		query = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return query }
}

func standingAfterProbe(t *testing.T, base string, opts ...Option) fleet.PeerStanding {
	t.Helper()
	d := New("peerbox", base, opts...)
	_ = d.RefreshCapabilities(context.Background(), caller) // runtimes 404s; standing is gathered first
	return d.PeerStanding()
}

func TestStandingIsObservedWhenThePeerAnswers(t *testing.T) {
	srv, query := whoamiPeer(t, 200, `{"principal":"system:homebox","machine":"peerbox","grants":["read","send"],"source":"observed","listsYou":true}`)
	got := standingAfterProbe(t, srv.URL, WithIdentity("tok"), WithSelf("homebox"))
	if got.Source != fleet.CapabilitiesObserved || got.ListsMeBack == nil || !*got.ListsMeBack ||
		strings.Join(got.GrantsToMe, ",") != "read,send" || got.ObservedAt == nil {
		t.Fatalf("standing = %+v", got)
	}
	if q := query(); q != "peer=homebox" {
		t.Errorf("whoami query = %q, want peer=homebox", q)
	}
}

func TestStandingNotListedIsARealFalse(t *testing.T) {
	srv, _ := whoamiPeer(t, 200, `{"grants":["read"],"source":"observed","listsYou":false}`)
	got := standingAfterProbe(t, srv.URL, WithIdentity("tok"), WithSelf("homebox"))
	if got.Source != fleet.CapabilitiesObserved || got.ListsMeBack == nil || *got.ListsMeBack {
		t.Fatalf("standing = %+v, want observed listsMeBack:false", got)
	}
}

// The mixed-version rule this issue was flagged for: a peer on the previous
// build answers whoami without listsYou. That is assumed — NEVER false.
func TestStandingFromAPeerThatPredatesTheFieldIsAssumedNeverFalse(t *testing.T) {
	srv, _ := whoamiPeer(t, 200, `{"principal":"system:homebox","machine":"peerbox","grants":["read"],"source":"observed"}`)
	got := standingAfterProbe(t, srv.URL, WithIdentity("tok"), WithSelf("homebox"))
	if got.ListsMeBack != nil {
		t.Fatalf("listsMeBack = %v, want null: an older build said nothing about the roster", *got.ListsMeBack)
	}
	if got.Source != fleet.CapabilitiesAssumed || got.GrantsToMe == nil || len(got.GrantsToMe) != 0 {
		t.Fatalf("standing = %+v, want the assumed floor", got)
	}
}

func TestStandingFromAPeerThatPredatesWhoamiIsAssumed(t *testing.T) {
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(bare.Close)
	if got := standingAfterProbe(t, bare.URL, WithIdentity("tok"), WithSelf("homebox")); got.Source != fleet.CapabilitiesAssumed || got.ListsMeBack != nil {
		t.Fatalf("standing = %+v, want assumed", got)
	}
}

// A 401 from whoami is an ANSWER: the credential matches no principal there.
func TestStandingWithAnUnrecognisedCredentialIsAnObservedEmptyGrantList(t *testing.T) {
	srv, _ := whoamiPeer(t, 401, `{"error":{"kind":"unauthorized","message":"unrecognised credential"}}`)
	got := standingAfterProbe(t, srv.URL, WithIdentity("tok"), WithSelf("homebox"))
	if got.Source != fleet.CapabilitiesObserved || got.GrantsToMe == nil || len(got.GrantsToMe) != 0 || got.ListsMeBack != nil {
		t.Fatalf("standing = %+v, want observed, grantsToMe [], listsMeBack null", got)
	}
}

func TestStandingIsAssumedWithoutAnIdentityOrAProbe(t *testing.T) {
	srv, _ := whoamiPeer(t, 200, `{"grants":["read"],"listsYou":true}`)
	if got := standingAfterProbe(t, srv.URL, WithSelf("homebox")); got.Source != fleet.CapabilitiesAssumed {
		t.Errorf("shared-token mode standing = %+v, want assumed: the credential is some caller's, not ours", got)
	}
	if got := New("peerbox", srv.URL, WithIdentity("tok"), WithSelf("homebox")).PeerStanding(); got.Source != fleet.CapabilitiesAssumed || got.GrantsToMe == nil {
		t.Errorf("never-probed standing = %+v, want assumed floor", got)
	}
}

func TestStandingDegradesWhenStaleAndSurvivesATransportBlip(t *testing.T) {
	srv, _ := whoamiPeer(t, 200, `{"grants":["read"],"listsYou":true}`)
	clock := time.Now()
	d := New("peerbox", srv.URL, WithIdentity("tok"), WithSelf("homebox"), withClock(func() time.Time { return clock }))
	_ = d.RefreshCapabilities(context.Background(), caller)
	if d.PeerStanding().Source != fleet.CapabilitiesObserved {
		t.Fatal("fresh observation not observed")
	}

	srv.Close() // unreachable now: the last observation stands
	_ = d.RefreshCapabilities(context.Background(), caller)
	if d.PeerStanding().Source != fleet.CapabilitiesObserved {
		t.Error("a transport failure discarded a still-fresh observation")
	}

	clock = clock.Add(capabilityStaleness + time.Second)
	if got := d.PeerStanding(); got.Source != fleet.CapabilitiesAssumed || got.ListsMeBack != nil {
		t.Errorf("stale standing = %+v, want assumed", got)
	}
}

// The acceptance scenario, through two real services: A lists B, B does not
// list A. A reads listsMeBack:false, and A is absent from B's machines — both
// sides show the same fact. Then B's principal table is varied.
func TestFederatedAsymmetricRegistrationReadsTheSameFromBothSides(t *testing.T) {
	const tokA = "credential-A-holds-on-B"

	peerB := func(t *testing.T, principals []service.Principal, listsA bool) (*service.Service, string) {
		t.Helper()
		svcB := service.New("peerbox")
		if err := svcB.RegisterLocalDriver("stub", &stub.Driver{}); err != nil {
			t.Fatal(err)
		}
		if listsA {
			if err := svcB.RegisterPeerDriver("homebox", &stub.Driver{DeadlineMs: 500}); err != nil {
				t.Fatal(err)
			}
		}
		srv := httptest.NewServer(service.NewMux(svcB, service.Config{Principals: principals}))
		t.Cleanup(srv.Close)
		return svcB, srv.URL
	}
	standingOnB := func(t *testing.T, base string) fleet.PeerStanding {
		t.Helper()
		rd := New("peerbox", base, WithIdentity(tokA), WithSelf("homebox"), WithDeadline(2*time.Second))
		_ = rd.RefreshCapabilities(context.Background(), fleet.Request{})
		svcA := homeService(t, "homebox", "peerbox", rd)
		col, err := svcA.ListMachines(context.Background(), fleet.Request{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range col.Items() {
			if it.Machine == "peerbox" {
				raw, _ := json.Marshal(it.Peer)
				t.Logf("A's view of its standing on B: %s", raw)
				return it.Peer
			}
		}
		t.Fatal("no peerbox item on A")
		return fleet.PeerStanding{}
	}

	t.Run("B does not list A", func(t *testing.T) {
		svcB, base := peerB(t, []service.Principal{{Name: "system:homebox", Token: tokA, Grants: []service.Grant{service.GrantRead}}}, false)
		got := standingOnB(t, base)
		if got.Source != fleet.CapabilitiesObserved || got.ListsMeBack == nil || *got.ListsMeBack {
			t.Fatalf("A reads %+v, want observed listsMeBack:false", got)
		}
		if strings.Join(got.GrantsToMe, ",") != "read" {
			t.Errorf("grantsToMe = %v, want [read]", got.GrantsToMe)
		}
		col, _ := svcB.ListMachines(context.Background(), fleet.Request{}, 0)
		for _, it := range col.Items() {
			if it.Machine == "homebox" {
				t.Fatalf("B lists homebox after all: %+v", it)
			}
		}
	})

	t.Run("B lists A", func(t *testing.T) {
		_, base := peerB(t, []service.Principal{{Name: "system:homebox", Token: tokA, Grants: []service.Grant{service.GrantRead, service.GrantSend}}}, true)
		got := standingOnB(t, base)
		if got.ListsMeBack == nil || !*got.ListsMeBack || strings.Join(got.GrantsToMe, ",") != "read,send" {
			t.Fatalf("A reads %+v, want listed with read,send", got)
		}
	})

	t.Run("B has no principal for A's credential", func(t *testing.T) {
		_, base := peerB(t, []service.Principal{{Name: "someone-else", Token: "other", Grants: []service.Grant{service.GrantRead}}}, true)
		got := standingOnB(t, base)
		if got.Source != fleet.CapabilitiesObserved || got.GrantsToMe == nil || len(got.GrantsToMe) != 0 {
			t.Fatalf("A reads %+v, want an OBSERVED empty grant list — a real negative, distinct from assumed", got)
		}
	})

	t.Run("B grants A's credential no read", func(t *testing.T) {
		_, base := peerB(t, []service.Principal{{Name: "system:homebox", Token: tokA, Grants: []service.Grant{service.GrantCreate}}}, true)
		got := standingOnB(t, base)
		if got.Source != fleet.CapabilitiesObserved || got.ListsMeBack != nil || strings.Join(got.GrantsToMe, ",") != "create" {
			t.Fatalf("A reads %+v, want observed [create] with listsMeBack null (roster needs read)", got)
		}
	})
}
