package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// colab-fleet #154, the service's half: whoami answers "do you list me" for a
// peer that names itself, and GET /v1/machines carries each machine's standing.

func standingGet(t *testing.T, u, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestWhoAmIReportsWhetherThisMachineListsTheAskingPeer(t *testing.T) {
	svc := New("m1")
	if err := svc.RegisterPeerDriver("m2", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: []Principal{
		{Name: "reader", Token: "tok-read", Grants: []Grant{GrantRead}},
		{Name: "blind", Token: "tok-blind", Grants: []Grant{GrantCreate}},
	}}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name, query, token string
		want               *bool
	}{
		{"listed", "?peer=m2", "tok-read", ptrBool(true)},
		{"not listed", "?peer=m9", "tok-read", ptrBool(false)},
		{"no read, no answer", "?peer=m2", "tok-blind", nil},
		{"not asked", "", "tok-read", nil},
		{"about another machine", "?machine=m2&peer=m2", "tok-read", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, raw := standingGet(t, srv.URL+"/v1/whoami"+tc.query, tc.token)
			if code != http.StatusOK {
				t.Fatalf("status %d %s", code, raw)
			}
			// The key is always present on this build: absence is reserved
			// for a build that predates it.
			var keys map[string]json.RawMessage
			_ = json.Unmarshal(raw, &keys)
			if _, ok := keys["listsYou"]; !ok {
				t.Fatalf("body %s has no listsYou key", raw)
			}
			var report fleet.GrantReport
			_ = json.Unmarshal(raw, &report)
			switch {
			case tc.want == nil && report.ListsYou != nil:
				t.Errorf("listsYou = %v, want null", *report.ListsYou)
			case tc.want != nil && (report.ListsYou == nil || *report.ListsYou != *tc.want):
				t.Errorf("listsYou = %v, want %v", report.ListsYou, *tc.want)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

type standingPeer struct {
	stub.Driver
	refreshes atomic.Int32
	standing  fleet.PeerStanding
}

func (p *standingPeer) PeerStanding() fleet.PeerStanding { return p.standing }
func (p *standingPeer) RefreshCapabilities(ctx context.Context, req fleet.Request) error {
	p.refreshes.Add(1)
	return nil
}

func machineItems(t *testing.T, raw []byte) map[fleet.MachineId]map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding machines %s: %v", raw, err)
	}
	out := map[fleet.MachineId]map[string]json.RawMessage{}
	for _, it := range body.Items {
		var m fleet.MachineId
		_ = json.Unmarshal(it["machine"], &m)
		out[m] = it
	}
	return out
}

func TestMachinesCarryEachMachinesStanding(t *testing.T) {
	now := time.Now()
	yes := true
	observed := &standingPeer{Driver: stub.Driver{DeadlineMs: 500}, standing: fleet.PeerStanding{
		ListsMeBack: &yes, GrantsToMe: []string{"read"}, Source: fleet.CapabilitiesObserved, ObservedAt: &now,
	}}
	svc := New("m1")
	svc.SetPeerCredential("tok-self")
	if err := svc.RegisterPeerDriver("m2", observed); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("m3", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Principals: []Principal{
		{Name: "system:m1", Token: "tok-self", Grants: []Grant{GrantRead, GrantKeys}},
		{Name: "reader", Token: "tok-read", Grants: []Grant{GrantRead}},
	}}))
	t.Cleanup(srv.Close)

	code, raw := standingGet(t, srv.URL+"/v1/machines", "tok-read")
	if code != http.StatusOK {
		t.Fatalf("machines = %d %s", code, raw)
	}
	items := machineItems(t, raw)

	var self fleet.PeerStanding
	_ = json.Unmarshal(items["m1"]["peer"], &self)
	if self.ListsMeBack == nil || !*self.ListsMeBack || self.Source != fleet.CapabilitiesObserved ||
		len(self.GrantsToMe) != 2 || self.GrantsToMe[0] != "read" || self.GrantsToMe[1] != "keys" {
		t.Errorf("self standing = %+v, want listed, observed, the table's grants for the peer credential", self)
	}

	var reported fleet.PeerStanding
	_ = json.Unmarshal(items["m2"]["peer"], &reported)
	if reported.Source != fleet.CapabilitiesObserved || reported.ListsMeBack == nil || !*reported.ListsMeBack {
		t.Errorf("reporting peer standing = %+v", reported)
	}

	// A driver with nothing to report is the floor, in exactly this shape.
	if got := string(items["m3"]["peer"]); !bytes.Contains([]byte(got), []byte(`"listsMeBack":null`)) ||
		!bytes.Contains([]byte(got), []byte(`"grantsToMe":[]`)) || !bytes.Contains([]byte(got), []byte(`"source":"assumed"`)) {
		t.Errorf("non-reporting peer standing = %s, want the assumed floor", got)
	}

	// ?verify=1 re-probes, once per peer; a plain read does not.
	if observed.refreshes.Load() != 0 {
		t.Fatalf("a plain read probed the peer")
	}
	if code, raw := standingGet(t, srv.URL+"/v1/machines?verify=1", "tok-read"); code != http.StatusOK {
		t.Fatalf("verify = %d %s", code, raw)
	}
	if got := observed.refreshes.Load(); got != 1 {
		t.Errorf("verify refreshed %d time(s), want 1", got)
	}
}

// A service with a credential its own table does not know reports a real
// negative for self, not an assumption.
func TestMachinesSelfStandingWithAnUnknownPeerCredential(t *testing.T) {
	svc := New("m1")
	svc.SetPeerCredential("tok-nobody")
	srv := httptest.NewServer(NewMux(svc, Config{Principals: []Principal{
		{Name: "reader", Token: "tok-read", Grants: []Grant{GrantRead}},
	}}))
	t.Cleanup(srv.Close)
	_, raw := standingGet(t, srv.URL+"/v1/machines", "tok-read")
	var self fleet.PeerStanding
	_ = json.Unmarshal(machineItems(t, raw)["m1"]["peer"], &self)
	if self.Source != fleet.CapabilitiesObserved || len(self.GrantsToMe) != 0 || self.GrantsToMe == nil {
		t.Errorf("self standing = %+v, want observed with grantsToMe []", self)
	}
}
