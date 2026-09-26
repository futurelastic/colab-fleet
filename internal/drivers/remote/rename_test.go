package remote

import (
	"context"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// TestRenameForwardsTheTitleVerbatim is colab-fleet#222's own remote
// contract: this driver never implements driver.TitleSyncer itself — the
// peer machine's own service already ran that step against its own driver —
// so whatever fleet.RenameAck it answers with, title included or absent,
// is decoded and returned as-is.
func TestRenameForwardsTheTitleVerbatim(t *testing.T) {
	for name, payload := range map[string]any{
		"a synced title": map[string]any{
			"accepted": true,
			"title":    map[string]any{"status": "synced", "evidence": "the peer's own confirmation"},
		},
		"no title at all (a peer predating #222)": map[string]any{
			"accepted": true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var rec capture
			srv := peerServing(t, 202, payload, &rec)
			d := New("peerbox", srv.URL)

			ack, err := d.Rename(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "old"}, "new")
			if err != nil {
				t.Fatalf("rename: %v", err)
			}
			if !ack.Accepted {
				t.Fatalf("accepted = false, want true")
			}
			wantTitle := payload.(map[string]any)["title"] != nil
			if (ack.Title != nil) != wantTitle {
				t.Fatalf("title presence = %v, want %v (payload: %v)", ack.Title != nil, wantTitle, payload)
			}
			if wantTitle && ack.Title.Status != fleet.TitleSynced {
				t.Fatalf("title = %+v, want synced", ack.Title)
			}
		})
	}
}

// TestRenameDecodesAnUnrecognisedTitleAsAbsent: a peer running a build that
// added a status this one does not know yet must not make the whole rename
// undecodable — the id half already happened on the peer.
func TestRenameDecodesAnUnrecognisedTitleAsAbsent(t *testing.T) {
	var rec capture
	srv := peerServing(t, 202, map[string]any{
		"accepted": true,
		"title":    map[string]any{"status": "some-future-status", "evidence": "x"},
	}, &rec)
	d := New("peerbox", srv.URL)

	ack, err := d.Rename(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "old"}, "new")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !ack.Accepted {
		t.Fatalf("accepted = false, want true — the id half must decode even when the title half cannot")
	}
	if ack.Title != nil {
		t.Fatalf("title = %+v, want nil (absent) for an unrecognised shape", ack.Title)
	}
}
