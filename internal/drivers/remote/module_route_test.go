package remote

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// #185: what crosses the peer boundary for an optional external delivery
// module. Two strings and one field: the `route` a caller forced, the session's
// `delivery` lane on a read, and the receipt's module route. A lane itself is
// decided — and held — on the machine that owns the session, never across the
// network, so nothing here carries module operations.

// A forced module route travels verbatim. The owning machine, which holds the
// lane, makes the real decision; a peer built before #185 answers an unknown
// route with its own 400, which is the right answer to a forced request.
func TestSendForwardsModuleRoute(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "hello",
		driver.SendOptions{Submit: true, Route: "relay-a"}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body = %q: %v", rec.body, err)
	}
	if body["route"] != "relay-a" {
		t.Fatalf("body = %q, want route:\"relay-a\" carried to the owning machine", rec.body)
	}
}

// #257: a human relay's auto crosses a peer as auto (no route field) together
// with the human-relay assertion, and the owner decides. LiveLaneOnly is the
// owner's own service's fact and is not forwarded.
func TestSendForwardsHumanRelayAutoAsAuto(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, &rec)
	d := New("peerbox", srv.URL)

	if _, err := d.Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "a human's message",
		driver.SendOptions{Submit: true, Route: fleet.RouteAuto, HumanRelay: true, LiveLaneOnly: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(rec.body), "livelane") {
		t.Fatalf("body = %q: the live-lane marker crossed the peer boundary", rec.body)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body["route"]; has {
		t.Fatalf("body = %q, want no route field (auto)", rec.body)
	}
	if body["submit"] != true {
		t.Fatalf("body = %q, want submit:true", rec.body)
	}
	for k := range body {
		switch k {
		case "text", "submit":
		default:
			t.Errorf("unexpected field %q in the relayed body %s", k, rec.body)
		}
	}
}

// A peer's list carries each session's `delivery` lane through unchanged, and a
// receipt's module route comes back readable.
func TestFederatedListCarriesDeliveryField(t *testing.T) {
	since := time.Date(2026, 9, 25, 1, 2, 3, 0, time.UTC)
	lane := &fleet.DeliveryLane{Lane: "relay-a", ClientConnected: true, Evidence: "attach live", Since: since}
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		[]fleet.Session{
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "with-lane"}, Delivery: lane,
				State: fleet.SessionState{Status: fleet.StatusIdle, Confidence: fleet.ConfidenceInferred}},
			{SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "no-lane"},
				State: fleet.SessionState{Status: fleet.StatusIdle, Confidence: fleet.ConfidenceInferred}},
		}), nil)
	d := New("peerbox", srv.URL)
	got, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]fleet.Session{}
	for _, s := range got.Items() {
		byID[s.ID] = s
	}
	if l := byID["with-lane"].Delivery; l == nil || l.Lane != "relay-a" || !l.ClientConnected || !l.Since.Equal(since) {
		t.Errorf("the lane was lost or altered in transit: %+v", l)
	}
	if byID["no-lane"].Delivery != nil {
		t.Errorf("a session with no lane grew one in transit: %+v", byID["no-lane"].Delivery)
	}

	var rec capture
	srv = peerServing(t, 200, fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}.WithModule("relay-a"), &rec)
	receipt, err := New("peerbox", srv.URL).Send(context.Background(), caller, fleet.SessionRef{ID: "s1"}, "x",
		driver.SendOptions{Submit: true, Route: "relay-a"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ModuleOf() != "relay-a" || receipt.RouteOf() != fleet.RouteModule {
		t.Errorf("the receipt's module route did not survive the hop: %+v", receipt.Delivery)
	}
}
