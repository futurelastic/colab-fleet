package tmux

import (
	"context"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet #85: a session can be reachable on a surface the runtime
// operates, and nothing said so or said where. These tests exercise the fix
// through the real driver path — surface_test.go in the root package
// already covers the fleet.RuntimeSurfaceRef type's own round-trip and
// refusal-to-encode discipline.

// A default create (remote control requested, nothing corroborated yet)
// must report the surface as pending — never Target populated ahead of
// corroboration, never read as "no".
func TestCreate_RuntimeSurfacePendingWhenRequested(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "reachable", Cwd: "/work/x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.RuntimeSurface == nil {
		t.Fatal("RuntimeSurface is nil on a create that requested remote control (the default)")
	}
	if sess.RuntimeSurface.Known != nil {
		t.Errorf("Known = %v, want nil (pending, not yet corroborated)", *sess.RuntimeSurface.Known)
	}
	if sess.RuntimeSurface.Kind != "" || sess.RuntimeSurface.Target != "" {
		t.Errorf("a pending surface must not publish kind/target ahead of corroboration: %+v", sess.RuntimeSurface)
	}
	if sess.RuntimeSurface.Evidence == "" {
		t.Error("a pending surface must still explain itself")
	}
}

// remoteControl: false is a settled "no" from the moment of create — it
// never resolves later, so a caller polling for one should stop.
func TestCreate_RuntimeSurfaceSettledNoWhenOptedOut(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	no := false
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "local-only", Cwd: "/work/x", RemoteControl: &no})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.RuntimeSurface == nil {
		t.Fatal("RuntimeSurface is nil")
	}
	if sess.RuntimeSurface.Known == nil || *sess.RuntimeSurface.Known {
		t.Errorf("Known = %v, want false (settled)", sess.RuntimeSurface.Known)
	}
	if sess.RuntimeSurface.Target != "" {
		t.Errorf("Target = %q, want empty", sess.RuntimeSurface.Target)
	}
}

// Once the runtime's own footer corroborates the channel is active, a
// listing resolves the surface to the session's own resolved name.
func TestList_RuntimeSurfaceResolvesOnActiveChannel(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "alpha", Cwd: "/work/alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Model what a subsequent list-panes would show: the session's own pane,
	// now painting the runtime's active control-channel label — the same
	// technique TestCreateReportsQuotaBlockedFromASessionCreatedWhileRefusing
	// (tmux_test.go) uses for the identical reason (the fake's new-session is
	// a no-op against its own session table).
	f.mu.Lock()
	f.sessions = append(f.sessions, fakeSession{
		name: sess.ID, paneID: "%9", cwd: "/work/alpha", pid: 999, created: 1785760000,
	})
	f.captures["%9"] = idleFixtureFor("alpha") + "   /rc active"
	f.mu.Unlock()

	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *fleet.Session
	for i, s := range got.Items() {
		if s.ID == sess.ID {
			found = &got.Items()[i]
		}
	}
	if found == nil {
		t.Fatalf("session %s not in the listing", sess.ID)
	}
	if found.RuntimeSurface == nil || found.RuntimeSurface.Known == nil || !*found.RuntimeSurface.Known {
		t.Fatalf("RuntimeSurface = %+v, want Known: true", found.RuntimeSurface)
	}
	if found.RuntimeSurface.Kind != fleet.RuntimeSurfaceControlChannel {
		t.Errorf("Kind = %q, want %q", found.RuntimeSurface.Kind, fleet.RuntimeSurfaceControlChannel)
	}
	if found.RuntimeSurface.Target != sess.ID {
		t.Errorf("Target = %q, want the session's own resolved name %q", found.RuntimeSurface.Target, sess.ID)
	}
	if found.RuntimeSurface.Source != fleet.RuntimeSurfaceDerived {
		t.Errorf("Source = %q, want derived", found.RuntimeSurface.Source)
	}
}

// Identity, not liveness: once corroborated, the address must not be
// unresolved just because the channel later reads unhealthy. That is
// state.controlChannel's own job.
func TestList_RuntimeSurfaceStaysKnownAfterChannelFails(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "alpha", Cwd: "/work/alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.mu.Lock()
	f.sessions = append(f.sessions, fakeSession{
		name: sess.ID, paneID: "%9", cwd: "/work/alpha", pid: 999, created: 1785760000,
	})
	f.captures["%9"] = idleFixtureFor("alpha") + "   /rc active"
	f.mu.Unlock()

	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatalf("List (first, corroborating): %v", err)
	}

	// The channel goes bad on the next read.
	f.setCapture("%9", idleFixtureFor("alpha")+"   /rc failed")

	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List (second): %v", err)
	}
	var found *fleet.Session
	for i, s := range got.Items() {
		if s.ID == sess.ID {
			found = &got.Items()[i]
		}
	}
	if found == nil {
		t.Fatalf("session %s not in the listing", sess.ID)
	}
	if found.RuntimeSurface == nil || found.RuntimeSurface.Known == nil || !*found.RuntimeSurface.Known {
		t.Errorf("RuntimeSurface = %+v, want Known still true — identity does not flicker with health", found.RuntimeSurface)
	}
	if found.RuntimeSurface.Target != sess.ID {
		t.Errorf("Target = %q, want unchanged at %q", found.RuntimeSurface.Target, sess.ID)
	}
	if found.State.ControlChannel == nil || found.State.ControlChannel.State != fleet.ControlChannelFailed {
		t.Errorf("state.controlChannel = %+v, want failed — that is the field for the health signal", found.State.ControlChannel)
	}
}

func TestCapabilities_ReportsRuntimeSurfaceTrue(t *testing.T) {
	d := newTestDriver(twoSessions())
	if !d.Capabilities().ReportsRuntimeSurface {
		t.Error("ReportsRuntimeSurface = false, want true")
	}
}
