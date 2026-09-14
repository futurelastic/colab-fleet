package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet #165: the applied marker is published on the session record
// as a fact the driver recorded, never re-derived from the name.

const markerTestCreated = int64(1785700000)

// addLive makes a session the fake multiplexer will enumerate. The fake's
// own new-session records nothing, so a test that wants Create's session to
// appear in a List has to put it there.
func addLive(f *fakeMux, name, pane string, created int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, fakeSession{
		name: name, paneID: pane, cwd: "/work/new", pid: 300, created: created, title: "2_1_220",
	})
	f.captures[pane] = idleFixtureFor(name)
}

func TestAppliedMarkerIsOnTheSessionRecord(t *testing.T) {
	cases := []struct {
		label, name, marker, wantID, want string
	}{
		{"appended by this create", "fresh", "💬", "fresh💬", "💬"},
		{"already carried by the name", "gamma💬", "💬", "gamma💬", "💬"},
		{"name kept a different marker", "release📋", "💬", "release📋", ""},
		{"no marker asked for", "plain", "", "plain", ""},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ctx := context.Background()
			f := twoSessions()
			d := stateDriver(t, f, t.TempDir())
			spec := fleet.SessionSpec{Name: tc.name, Marker: tc.marker, Cwd: "/work/new"}

			created, err := d.Create(ctx, testCaller, "key-1", spec)
			if err != nil {
				t.Fatal(err)
			}
			if created.ID != tc.wantID {
				t.Fatalf("resolved id = %q, want %q", created.ID, tc.wantID)
			}
			if created.Marker != tc.want {
				t.Errorf("create response marker = %q, want %q", created.Marker, tc.want)
			}

			addLive(f, created.ID, "%9", markerTestCreated)
			col, err := d.List(ctx, testCaller, listAll())
			if err != nil {
				t.Fatal(err)
			}
			s, ok := sessionByID(col, created.ID)
			if !ok {
				t.Fatalf("created session missing from list: %v", idsOf(col))
			}
			if s.Marker != tc.want {
				t.Errorf("read marker = %q, want %q", s.Marker, tc.want)
			}
			// A name ending in a marker is not a record of one: this driver
			// never created "alpha💬", so it must not report a marker for it.
			if alpha, ok := sessionByID(col, "alpha💬"); !ok || alpha.Marker != "" {
				t.Errorf("foreign session alpha💬 reported marker %q; a suffix is not a record", alpha.Marker)
			}

			replay, err := d.Create(ctx, testCaller, "key-1", spec)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Marker != tc.want {
				t.Errorf("replayed create marker = %q, want %q", replay.Marker, tc.want)
			}
		})
	}
}

func TestAppliedMarkerSurvivesARename(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := stateDriver(t, f, t.TempDir())

	created, err := d.Create(ctx, testCaller, "key-1", fleet.SessionSpec{Name: "fresh", Marker: "💬", Cwd: "/work/new"})
	if err != nil {
		t.Fatal(err)
	}
	addLive(f, created.ID, "%9", markerTestCreated)
	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}

	req := testCaller
	started := time.Unix(markerTestCreated, 0)
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: created.ID}, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	col, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	s, ok := sessionByID(col, "renamed")
	if !ok {
		t.Fatalf("renamed session missing: %v", idsOf(col))
	}
	if s.Marker != "💬" {
		t.Errorf("marker after rename = %q, want it carried", s.Marker)
	}
	// The naming decision is still cleared: the caller dictated "renamed",
	// so the next create resolving that string must not believe this
	// driver appended a marker to it.
	if got := d.markerStateFor("renamed", "💬"); got != markerUnknown {
		t.Errorf("markerStateFor after rename = %v, want markerUnknown", got)
	}

	// A second actor puts the old name back: the run is the same, so the
	// marker still describes it.
	f.mu.Lock()
	for i := range f.sessions {
		if f.sessions[i].name == "renamed" {
			f.sessions[i].name = created.ID
		}
	}
	f.mu.Unlock()
	col, err = d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	reverted, ok := sessionByID(col, created.ID)
	if !ok {
		t.Fatalf("reverted session missing: %v", idsOf(col))
	}
	if reverted.Marker != "💬" {
		t.Errorf("marker after a revert = %q, want it carried", reverted.Marker)
	}
}

func TestARecycledIdDoesNotInheritTheMarker(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := stateDriver(t, f, t.TempDir())

	created, err := d.Create(ctx, testCaller, "key-1", fleet.SessionSpec{Name: "fresh", Marker: "💬", Cwd: "/work/new"})
	if err != nil {
		t.Fatal(err)
	}
	addLive(f, created.ID, "%9", markerTestCreated)
	if _, err := d.List(ctx, testCaller, listAll()); err != nil {
		t.Fatal(err)
	}

	// The run ends and a different one takes the same name.
	f.mu.Lock()
	for i := range f.sessions {
		if f.sessions[i].name == created.ID {
			f.sessions[i].paneID = "%10"
			f.sessions[i].created = markerTestCreated + 50
		}
	}
	f.captures["%10"] = idleFixtureFor(created.ID)
	f.mu.Unlock()

	col, err := d.List(ctx, testCaller, listAll())
	if err != nil {
		t.Fatal(err)
	}
	s, ok := sessionByID(col, created.ID)
	if !ok {
		t.Fatalf("session missing: %v", idsOf(col))
	}
	if s.Marker != "" {
		t.Errorf("a recycled id reported the old run's marker %q", s.Marker)
	}
}

func TestPublishedMarkerFromARecordThatPredatesTheField(t *testing.T) {
	if got := (sessionRecord{Marker: "💬", MarkerApplied: true}).publishedMarker(); got != "💬" {
		t.Errorf("applied marker on an older record = %q, want it published", got)
	}
	if got := (sessionRecord{Marker: "💬"}).publishedMarker(); got != "" {
		t.Errorf("an older record that did not apply its marker published %q", got)
	}
}
