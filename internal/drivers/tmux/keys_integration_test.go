package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// #188, against a REAL multiplexer: the unit tests above prove the driver asks
// for a key called BTab; only a real server can prove that name is one the
// multiplexer knows, that what reaches the pane is Shift+Tab, and that a real
// classified idle composer lets it through while still refusing an arrow.
//
// Gated like the other multiplexer tests (FLEET_TMUX_INTEGRATION=1), and safe to
// run on a machine with live sessions: everything happens on the compat
// harness's PRIVATE server, with a synthetic runtime (testdata/faketui.py,
// FAKE_MODES=1) — no real agent, no network, no tokens, and nothing here ever
// addresses a server it did not start.
func TestLiveKeysBTabCyclesTheModeFooterOnAPrivateServer(t *testing.T) {
	_, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, map[string]string{"FAKE_MODES": "1"}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := h.world.create(ctx, "a", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := h.world.d

	// The footer is the last non-empty line of the pane: what a person would read
	// as "which mode is this session in".
	footer := func() string {
		out, err := h.world.tmux(ctx, "capture-pane", "-p", "-t", ref.ID)
		if err != nil {
			t.Fatalf("capture-pane: %v: %s", err, out)
		}
		lines := strings.Split(strings.TrimRight(out, "\n \t"), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	cfcWaitFor(t, "the synthetic runtime's first paint", 20*time.Second, func() bool {
		return strings.Contains(footer(), "? for shortcuts")
	})

	read := func() string {
		t.Helper()
		st, err := d.State(ctx, compatRequest, ref)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if st.ScreenDigest == "" {
			t.Fatal("a session whose screen was read must publish a digest to quote back")
		}
		return st.ScreenDigest
	}

	// Control, so the pass below cannot be an accident of classification: on this
	// very screen an arrow is refused as "no dialog, empty composer". If the
	// screen were not read as an idle composer this would not refuse, and BTab
	// getting through would prove nothing about the exemption.
	arrow, err := d.Keys(ctx, compatRequest, ref, fleet.KeyLeft, read())
	if err != nil {
		t.Fatalf("Keys(Left): %v", err)
	}
	if arrow.Outcome != fleet.OutcomeRefused || !strings.Contains(arrow.Reason, "arrow") {
		t.Fatalf("Left = %s (%s); this screen must read as an idle composer for the test to mean anything",
			arrow.Outcome, arrow.Reason)
	}
	if got := footer(); !strings.Contains(got, "? for shortcuts") {
		t.Fatalf("a REFUSED key still changed the footer to %q", got)
	}

	// One full lap of the cycle, one press per request and a fresh digest before
	// each — the honest cost of a corroborated keypress — ending where it began.
	//
	// #194: after each press, what the DRIVER reports in state.permissionMode must
	// be the mode the footer now shows — the read side of the loop a mode control
	// runs, exercised through the real driver rather than the classifier alone.
	// (The last stop is the default mode again: its row is `manual mode on`, and it
	// reads as `default`.)
	mode := func() fleet.PermissionModeState {
		t.Helper()
		st, err := d.State(ctx, compatRequest, ref)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		return st.PermissionMode
	}
	if got := mode(); got != fleet.PermissionModeDefault {
		t.Fatalf("a session that has not been pressed reads as %q, want default", got)
	}
	want := []struct {
		footer string
		mode   fleet.PermissionModeState
	}{
		{"accept edits on", fleet.PermissionModeAcceptEdits},
		{"plan mode on", fleet.PermissionModePlan},
		{"auto mode on", fleet.PermissionModeAuto},
		{"manual mode on", fleet.PermissionModeDefault},
	}
	for i, w := range want {
		got, err := d.Keys(ctx, compatRequest, ref, fleet.KeyBTab, read())
		if err != nil {
			t.Fatalf("Keys(BTab) #%d: %v", i+1, err)
		}
		if got.Outcome != fleet.OutcomeSubmitted {
			t.Fatalf("Keys(BTab) #%d = %s (%s); the footer repaint is the confirmation", i+1, got.Outcome, got.Reason)
		}
		cfcWaitFor(t, "the footer to show "+w.footer, 5*time.Second, func() bool {
			return strings.Contains(footer(), w.footer)
		})
		if got := mode(); got != w.mode {
			t.Errorf("after press #%d the footer reads %q and state.permissionMode = %q, want %q", i+1, footer(), got, w.mode)
		}
	}
}

// #194: a session launched with bypass permissions starts in bypass and follows
// the measured ring from there, and state reads each stop. The synthetic runtime
// decides that from its argv, as the real one does.
func TestLiveBypassLaunchStartsInBypassAndStateFollowsTheRing(t *testing.T) {
	_, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, map[string]string{"FAKE_MODES": "1"}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := h.world.create(ctx, "b", "b", func(sp *fleet.SessionSpec) { sp.PermissionMode = fleet.PermissionModeBypass })
	if err != nil {
		t.Fatal(err)
	}
	d := h.world.d
	read := func() fleet.SessionState {
		t.Helper()
		st, err := d.State(ctx, compatRequest, ref)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		return st
	}
	cfcWaitFor(t, "a bypass session to read as bypass", 20*time.Second, func() bool {
		return read().PermissionMode == fleet.PermissionModeBypass
	})
	// bypass -> auto -> default -> acceptEdits -> plan -> bypass
	for i, want := range []fleet.PermissionModeState{
		fleet.PermissionModeAuto, fleet.PermissionModeDefault, fleet.PermissionModeAcceptEdits,
		fleet.PermissionModePlan, fleet.PermissionModeBypass,
	} {
		before := read()
		got, err := d.Keys(ctx, compatRequest, ref, fleet.KeyBTab, before.ScreenDigest)
		if err != nil {
			t.Fatalf("Keys(BTab) #%d: %v", i+1, err)
		}
		if got.Outcome != fleet.OutcomeSubmitted {
			t.Fatalf("Keys(BTab) #%d = %s (%s)", i+1, got.Outcome, got.Reason)
		}
		cfcWaitFor(t, "state to read as "+string(want), 5*time.Second, func() bool {
			return read().PermissionMode == want
		})
	}
}
