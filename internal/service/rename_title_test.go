package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// titleSyncingDriver adds driver.TitleSyncer to labelDriver (colab-fleet
// #222), so handleRename's own orchestration — Rename first, THEN SyncTitle,
// targeting the NEW id — can be exercised without a real substrate.
type titleSyncingDriver struct {
	*labelDriver
	calls []fleet.SessionRef
}

func (d *titleSyncingDriver) SyncTitle(ctx context.Context, req fleet.Request, ref fleet.SessionRef) fleet.TitleSync {
	d.calls = append(d.calls, ref)
	return fleet.TitleSyncSynced("test double: always synced", fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued})
}

func decodeRenameAck(t *testing.T, raw []byte) fleet.RenameAck {
	t.Helper()
	var ack fleet.RenameAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("decode RenameAck: %v (%s)", err, raw)
	}
	return ack
}

// TestHandleRename_LocalDriverWithTitleSyncerGetsATitle: a local driver
// claiming the capability is called AFTER the id half, addressed by the
// NEW id — there is no old id left to sync a title on by then.
func TestHandleRename_LocalDriverWithTitleSyncerGetsATitle(t *testing.T) {
	d := &titleSyncingDriver{labelDriver: newLabelDriver()}
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, tokenCfg()))
	t.Cleanup(srv.Close)
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	code, raw := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+sess.ID+"/rename", testToken,
		map[string]string{"name": "renamed"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("rename = %d %s", code, raw)
	}
	ack := decodeRenameAck(t, raw)
	if !ack.Accepted || ack.Title == nil || ack.Title.Status != fleet.TitleSynced {
		t.Fatalf("ack = %+v", ack)
	}
	if len(d.calls) != 1 || d.calls[0].ID != "renamed" {
		t.Fatalf("SyncTitle calls = %v, want exactly one call naming the NEW id", d.calls)
	}
}

// TestHandleRename_LocalDriverWithoutTitleSyncerReportsNotApplicable: the
// ordinary labelDriver (used by every other rename test in this package)
// implements no TitleSyncer — this is the ONLY place fleet.TitleNotApplicable
// is ever produced (see rename_title.go's own doc comment).
func TestHandleRename_LocalDriverWithoutTitleSyncerReportsNotApplicable(t *testing.T) {
	d := newLabelDriver()
	srv := labelServer(t, New("test-machine"), d, tokenCfg())
	_, raw := create(t, srv, testToken, nil)
	sess := decodeSession(t, raw)

	code, raw := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/"+sess.ID+"/rename", testToken,
		map[string]string{"name": "renamed"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("rename = %d %s", code, raw)
	}
	ack := decodeRenameAck(t, raw)
	if ack.Title == nil || ack.Title.Status != fleet.TitleNotApplicable {
		t.Fatalf("ack.Title = %+v, want not_applicable", ack.Title)
	}
}

// peerRenameDriver is registered as if it fronted a peer machine
// (RegisterPeerDriver) — a stand-in for what remote.Driver.Rename actually
// decodes off the wire, without needing a real second HTTP server: this
// service's own routing does not care whether "the peer" is a real remote
// call or a directly-registered driver.Driver, only that machine != self.
type peerRenameDriver struct {
	stub.Driver
	ack fleet.RenameAck
}

func (d *peerRenameDriver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	return d.ack, nil
}

// TestHandleRename_PeerTitleIsPassedThroughVerbatim: colab-fleet#222's own
// rule — a peer-routed rename never runs this machine's syncTitle a second
// time; whatever fleet.RenameAck.Title the peer already decided is forwarded
// as-is, absent included.
func TestHandleRename_PeerTitleIsPassedThroughVerbatim(t *testing.T) {
	for name, ack := range map[string]fleet.RenameAck{
		"peer with a synced title": {Accepted: true, Title: func() *fleet.TitleSync {
			ts := fleet.TitleSyncSynced("the peer's own confirmation", fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued})
			return &ts
		}()},
		"peer predating this field": {Accepted: true, Title: nil},
	} {
		t.Run(name, func(t *testing.T) {
			svc := New("owner")
			peer := &peerRenameDriver{ack: ack}
			if err := svc.RegisterPeerDriver("entrybox", peer); err != nil {
				t.Fatal(err)
			}
			cfg := tokenCfg()
			cfg.AllowPeerRelay = true
			srv := labelServer(t, svc, newLabelDriver(), cfg)

			code, raw := labelCall(t, http.MethodPost, srv.URL+"/v1/machines/entrybox/sessions/some-id/rename", testToken,
				map[string]string{"name": "renamed"}, nil)
			if code != http.StatusAccepted {
				t.Fatalf("rename = %d %s", code, raw)
			}
			got := decodeRenameAck(t, raw)
			if got.Accepted != ack.Accepted {
				t.Fatalf("accepted = %v, want %v", got.Accepted, ack.Accepted)
			}
			if (got.Title == nil) != (ack.Title == nil) {
				t.Fatalf("title presence = %v, want %v", got.Title, ack.Title)
			}
			if ack.Title != nil && got.Title.Status != ack.Title.Status {
				t.Fatalf("title = %+v, want %+v", got.Title, ack.Title)
			}
		})
	}
}
