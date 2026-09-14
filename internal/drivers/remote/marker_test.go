package remote

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet #165: a peer's session keeps its marker through the relay.
func TestPeerSessionCarriesItsMarkerThroughTheRelay(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "peerbox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		[]fleet.Session{{
			SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1💬", Name: "s1💬"},
			Runtime:    "tmux", Cwd: "/work",
			Marker: "💬",
			State:  fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
		}}), &rec)
	d := New("peerbox", srv.URL)

	col, err := d.List(context.Background(), caller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	items := col.Items()
	if len(items) != 1 {
		t.Fatalf("got %d sessions, want 1", len(items))
	}
	if items[0].Marker != "💬" {
		t.Errorf("relayed marker = %q, want %q", items[0].Marker, "💬")
	}
}
