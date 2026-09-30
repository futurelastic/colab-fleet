package service

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// Tests for colab-fleet #237's service half: GET /v1/machines reads a peer's
// remembered down state instead of dialling it, and reports an unreachable
// peer as unreachable even when its driver folded the failure into an
// envelope rather than returning an error.

// downPeer is a peer driver whose down state is scripted and whose List is
// counted, so a test can prove whether it was asked.
type downPeer struct {
	stub.Driver
	down  bool
	since time.Time
	lists int
	// listSources is what List answers; nil means a plain ok envelope.
	listSources []fleet.SourceStatus
}

func (p *downPeer) PeerDown() (bool, time.Time, int) { return p.down, p.since, 3 }

func (p *downPeer) List(_ context.Context, _ fleet.Request, _ driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	p.lists++
	srcs := p.listSources
	if srcs == nil {
		srcs = []fleet.SourceStatus{{Machine: "otherbox", Status: fleet.SourceOK, ObservedAt: time.Now()}}
	}
	return fleet.NewCollection([]fleet.Session{}, srcs)
}

var _ driver.PeerDownReporter = (*downPeer)(nil)

func machineRow(t *testing.T, col fleet.Collection[fleet.MachineInfo], name fleet.MachineId) (fleet.MachineInfo, fleet.SourceStatus) {
	t.Helper()
	var row fleet.MachineInfo
	var src fleet.SourceStatus
	for _, it := range col.Items() {
		if it.Machine == name {
			row = it
		}
	}
	for _, s := range col.Sources() {
		if s.Machine == name {
			src = s
		}
	}
	if row.Machine == "" {
		t.Fatalf("no %s row in /v1/machines", name)
	}
	return row, src
}

func TestMachinesReportsACachedDownPeerWithoutDialling(t *testing.T) {
	svc := New("testbox")
	since := time.Date(2026, 9, 28, 22, 0, 0, 0, time.UTC)
	p := &downPeer{Driver: stub.Driver{DeadlineMs: 500}, down: true, since: since}
	if err := svc.RegisterPeerDriver("otherbox", p); err != nil {
		t.Fatal(err)
	}

	col, err := svc.ListMachines(context.Background(), fleet.Request{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, src := machineRow(t, col, "otherbox")
	if row.Status != fleet.SourceUnreachable || src.Status != fleet.SourceUnreachable {
		t.Errorf("row=%q source=%q, want both unreachable", row.Status, src.Status)
	}
	if p.lists != 0 {
		t.Errorf("a marked-down peer was asked %d time(s); the cached state must answer", p.lists)
	}
	if want := since.Format(time.RFC3339); !strings.Contains(src.Error, want) {
		t.Errorf("error %q must name when the peer went down (%s)", src.Error, want)
	}
	if col.Complete() {
		t.Error("an unreachable peer means the read is not complete")
	}
}

// A remote driver folds "the peer did not answer" into the envelope's own
// sources and returns a nil error. ListMachines used to read that nil as
// contact and list the peer as ok.
func TestMachinesDoesNotReadAFoldedFailureAsContact(t *testing.T) {
	svc := New("testbox")
	p := &downPeer{Driver: stub.Driver{DeadlineMs: 500}, listSources: []fleet.SourceStatus{{
		Machine: "otherbox", Status: fleet.SourceUnreachable, Error: "no answer from otherbox", ObservedAt: time.Now(),
	}}}
	if err := svc.RegisterPeerDriver("otherbox", p); err != nil {
		t.Fatal(err)
	}
	col, err := svc.ListMachines(context.Background(), fleet.Request{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := machineRow(t, col, "otherbox")
	if row.Status != fleet.SourceUnreachable {
		t.Errorf("status = %q, want unreachable", row.Status)
	}
}

// Regression for #237's third bullet: a scope=local read never touches a
// peer, whatever state it is in.
func TestLocalScopeNeverAsksAPeer(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	p := &downPeer{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterPeerDriver("otherbox", p); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListSessions(context.Background(), fleet.Request{}, ScopeLocal, driver.ListFilter{}, 0); err != nil {
		t.Fatal(err)
	}
	if p.lists != 0 {
		t.Errorf("scope=local asked the peer %d time(s)", p.lists)
	}
	if _, err := svc.ListSessions(context.Background(), fleet.Request{}, ScopeFleet, driver.ListFilter{}, 0); err != nil {
		t.Fatal(err)
	}
	if p.lists != 1 {
		t.Errorf("scope=fleet asked the peer %d time(s), want 1", p.lists)
	}
}
