package tmux

import (
	"context"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet #236's own oracle: reapDeadRows must act only on a session
// this driver marked as its own at Create (managedSessionOption), never on
// every dead pane the enumeration happens to see. Create's own comment on
// remain-on-exit says this driver's multiplexer server may host sessions
// other tools started; before this fix, any one of those with
// remain-on-exit set — for its own reasons, unrelated to this service —
// was killed, screen-captured and reported the moment its process exited,
// exactly as if this driver had started it.
func TestReapActsOnlyOnSessionsThisDriverMarkedAsItsOwn(t *testing.T) {
	f := &fakeMux{
		sessions: []fakeSession{
			// Started outside this service (remain-on-exit set by an
			// operator or another tool), never carries the marker.
			{name: "outsider", paneID: "%1", cwd: "/work/outsider", pid: 100,
				created: 1785600000, dead: true, deadStatus: 9, managed: false},
			// Started by this driver's own Create, so it carries the marker.
			{name: "ours", paneID: "%2", cwd: "/work/ours", pid: 200,
				created: 1785600001, dead: true, deadStatus: 7, managed: true},
		},
		captures: map[string]string{
			"%1": "outsider's last screen\n",
			"%2": "ours's last screen\n",
		},
	}
	d := newTestDriver(f)

	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// The unmarked session is left exactly as it was before colab-fleet
	// #235 ever shipped: still reported by List (never silently dropped),
	// never killed.
	byID := map[string]bool{}
	for _, s := range got.Items() {
		byID[s.ID] = true
	}
	if !byID["outsider"] {
		t.Error("the unmanaged dead pane must still be reported by List, not dropped")
	}
	for _, call := range f.callsSnapshot() {
		if len(call) >= 2 && call[0] == "kill-session" && call[1] == "-t" && len(call) >= 3 && call[2] == "outsider" {
			t.Fatalf("the unmanaged session must never be killed, got %v", call)
		}
	}

	// The marked session is reaped exactly as #235 specified: killed, and
	// removed from what List returns.
	if byID["ours"] {
		t.Error("the managed dead pane should have been reaped and dropped from List")
	}
	killedOurs := false
	for _, call := range f.callsSnapshot() {
		if len(call) >= 3 && call[0] == "kill-session" && call[1] == "-t" && call[2] == "ours" {
			killedOurs = true
		}
	}
	if !killedOurs {
		t.Fatal("the managed dead pane should have been killed")
	}

	exits := d.DrainExits()
	if len(exits) != 1 || exits[0].ID != "ours" {
		t.Fatalf("DrainExits should carry exactly the managed session's exit, got %+v", exits)
	}
	if exits[0].Exit.Status != 7 {
		t.Errorf("captured status = %d, want 7", exits[0].Exit.Status)
	}
}
