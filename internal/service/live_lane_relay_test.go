package service

import (
	"net/http/httptest"
	"testing"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/remote"
	"github.com/futurelastic/muster/internal/drivers/stub"
)

// #257: a human relay's `auto` that enters on one machine for a session on
// another crosses the peer hop as auto, together with the human-relay
// assertion, so the owner (the only machine that can see the session's lane)
// decides. Before, the entering machine rewrote it to `terminal` and it could
// never reach a lane on the owner.
func TestHumanRelayAutoCrossesAPeerAsAuto(t *testing.T) {
	ownerSvc := New("owner")
	od := &routeDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := ownerSvc.RegisterLocalDriver("stub", od); err != nil {
		t.Fatal(err)
	}
	if err := ownerSvc.RegisterPeerDriver("entrybox", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	ownerSrv := httptest.NewServer(NewMux(ownerSvc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantSend}},
	}}))
	t.Cleanup(ownerSrv.Close)

	entrySvc := New("entrybox")
	if err := entrySvc.RegisterLocalDriver("stub", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	if err := entrySvc.RegisterPeerDriver("owner",
		remote.New("owner", ownerSrv.URL, remote.WithIdentity("peer-token"), remote.WithSelf("entrybox"))); err != nil {
		t.Fatal(err)
	}
	entrySrv := httptest.NewServer(NewMux(entrySvc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "human-relay", Token: "human-token", Grants: []Grant{GrantSend, GrantHumanRelay, GrantRelay}},
	}}))
	t.Cleanup(entrySrv.Close)

	mustOK(t, postInputTo(t, entrySrv, "owner", "human-token", nil,
		map[string]any{"text": "a person's message"}))
	got := od.last(t)
	if got.Route != fleet.RouteAuto || !got.HumanRelay || !got.Submit || got.From != nil || !got.LiveLaneOnly {
		t.Fatalf("the owner's driver got %+v, want an unlabelled human-relay auto send with submit true, set live-lane-only by the owner", got)
	}
}
