package tmux

import (
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// testEpoch is an arbitrary fixed instant: classifyPaneRemembering needs a clock
// only to judge staleness across observations, and these tests look once.
var testEpoch = time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

// paneWithIndicator builds a screen whose composer is empty and whose indicator
// area (everything under the closing fence) is exactly the given rows.
func paneWithIndicator(rows ...string) string {
	return "  transcript line\n" +
		"✻ Brewed for 1m 0s\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		strings.Join(rows, "\n")
}

// The five rows are what a real runtime build (2.1.282) painted after each press
// of Shift+Tab, taken from live captures. The default row is not the bare
// `? for shortcuts` the synthetic composer used to model it as.
func TestPermissionModeReadsEachMeasuredIndicatorRow(t *testing.T) {
	cases := []struct {
		row  string
		want fleet.PermissionModeState
	}{
		{"  ⏸ manual mode on · ? for shortcuts · ← for agents", fleet.PermissionModeDefault},
		{"  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents", fleet.PermissionModeAcceptEdits},
		{"  ⏸ plan mode on (shift+tab to cycle) · ← for agents", fleet.PermissionModePlan},
		{"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", fleet.PermissionModeAuto},
		{"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents", fleet.PermissionModeBypass},
	}
	for _, tc := range cases {
		if got := permissionModeOf(newScreen(paneWithIndicator(tc.row))); got != tc.want {
			t.Errorf("row %q -> %q, want %q", tc.row, got, tc.want)
		}
	}
}

// The wording names the mode; the glyph and the hints around it are decoration
// the runtime moves freely, so none of them may be required and none may be
// mistaken for a mode.
func TestPermissionModeIsReadFromTheWordingNotTheDecoration(t *testing.T) {
	cases := []struct {
		name string
		row  string
		want fleet.PermissionModeState
	}{
		{"no glyph", "  plan mode on", fleet.PermissionModePlan},
		{"a different glyph", "  ▶▶ auto mode on", fleet.PermissionModeAuto},
		{"the hint gone", "  ⏵⏵ accept edits on · ← 3 agents", fleet.PermissionModeAcceptEdits},
		{"upper case", "  ⏸ PLAN MODE ON", fleet.PermissionModePlan},
		{"an older qualified wording", "  ⏵⏵ auto-accept edits on (shift+tab to cycle)", fleet.PermissionModeAcceptEdits},
		{"label shares the row with the control-channel label", "  ⏵⏵ auto mode on                    /rc active", fleet.PermissionModeAuto},
	}
	for _, tc := range cases {
		if got := permissionModeOf(newScreen(paneWithIndicator(tc.row))); got != tc.want {
			t.Errorf("%s: %q -> %q, want %q", tc.name, tc.row, got, tc.want)
		}
	}
}

// THE safety property, the same one the control-channel reader is built around:
// a session whose TRANSCRIPT is full of every mode's wording — which is what a
// session that has been asked about, or has been grepping for, permission modes
// looks like — must read as what its indicator row says. Otherwise a client that
// stops cycling when it reads `plan` would stop on a lie.
func TestATranscriptFullOfModeWordingCannotForgeTheMode(t *testing.T) {
	contaminated := "⏺ the fleet report says: ⏸ plan mode on\n" +
		"⏺ another said ⏵⏵ bypass permissions on and ⏵⏵ auto mode on\n" +
		"⏺ accept edits on, manual mode on\n" +
		"✻ Brewed for 1m 0s\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		"  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents"
	if got := permissionModeOf(newScreen(contaminated)); got != fleet.PermissionModeAcceptEdits {
		t.Errorf("mode = %q; the transcript forged it. Only the chrome under the composer's "+
			"closing fence may decide this", got)
	}
}

// A draft is text somebody typed (or that a caller's `input` left there unsent).
// It sits between the fences, above the closing rule, and is never the indicator
// area — including a multi-line one, which is exactly where a looser "everything
// after the marker" reading would start including it.
func TestAMultiLineDraftCannotForgeTheMode(t *testing.T) {
	s := "  transcript\n" +
		rule + "\n" +
		"❯ first line of a draft\n" +
		"  ⏸ plan mode on (shift+tab to cycle)\n" +
		"  bypass permissions on\n" +
		rule + "\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents"
	if got := permissionModeOf(newScreen(s)); got != fleet.PermissionModeAuto {
		t.Errorf("mode = %q; a draft's own text was read as the indicator", got)
	}
}

// The hint that says auto is unavailable for the model is painted ABOVE the
// composer, not in the indicator area. It contains "auto mode" and is not a
// statement that the session is in it.
func TestAutoUnavailableHintAboveTheFenceIsNotTheMode(t *testing.T) {
	s := "  transcript\n" +
		"                              auto mode unavailable for this model\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		"  ⏸ manual mode on · ? for shortcuts · ← for agents"
	if got := permissionModeOf(newScreen(s)); got != fleet.PermissionModeDefault {
		t.Errorf("mode = %q, want default", got)
	}
	// And in the footer itself, "unavailable" is not "on".
	if got := permissionModeOf(newScreen(paneWithIndicator("  auto mode unavailable for this model"))); got != fleet.PermissionModeUnknown {
		t.Errorf("an unavailable notice in the indicator area = %q, want unknown", got)
	}
}

// Unknown is an answer, not a shrug: the indicator area is there and names no
// mode this build knows. Never the nearest neighbour.
func TestAnIndicatorAreaThatNamesNoKnownModeIsUnknown(t *testing.T) {
	cases := []struct{ name, row string }{
		{"a mode this list does not have", "  ⏵⏵ don't ask mode on (shift+tab to cycle)"},
		{"the default row reworded", "  ⏸ ask mode on · ? for shortcuts"},
		{"a hint painted in its place", "  ! for bash mode"},
		{"only the shortcut hint", "  ? for shortcuts"},
		{"a near miss", "  ⏸ plan mode of"},
	}
	for _, tc := range cases {
		if got := permissionModeOf(newScreen(paneWithIndicator(tc.row))); got != fleet.PermissionModeUnknown {
			t.Errorf("%s: %q -> %q, want unknown", tc.name, tc.row, got)
		}
	}
}

// Two different modes named at once is not an answer either. Picking one by list
// order would be a coin toss reported as a fact.
func TestTwoModesAtOnceIsUnknownNotWhicheverIsListedFirst(t *testing.T) {
	got := permissionModeOf(newScreen(paneWithIndicator(
		"  ⏵⏵ accept edits on · ⏸ plan mode on (shift+tab to cycle)")))
	if got != fleet.PermissionModeUnknown {
		t.Errorf("mode = %q, want unknown", got)
	}
	// The same mode twice is still one mode.
	got = permissionModeOf(newScreen(paneWithIndicator(
		"  ⏸ plan mode on (shift+tab to cycle)", "  plan mode on")))
	if got != fleet.PermissionModePlan {
		t.Errorf("mode = %q, want plan: the same mode named twice is one answer", got)
	}
}

// Nothing read is different from unknown. A screen with no fenced composer, or a
// composer with nothing painted under it yet, has no indicator area to be
// unrecognised about.
func TestNothingReadIsAbsentAndNotUnknown(t *testing.T) {
	cases := []struct{ name, screen string }{
		{"empty capture", ""},
		{"no composer at all", "just some output\nand more output"},
		{"a composer with nothing under it", "  transcript\n" + rule + "\n❯\n" + rule},
		{"a composer with only blank rows under it", "  transcript\n" + rule + "\n❯\n" + rule + "\n\n   \n"},
		// A menu's highlighted row is also ❯. It is not a composer, so whatever
		// follows it is not an indicator area, and must not read as "unknown".
		{"a selection menu", "  Do you want to proceed?\n" +
			"❯ 1. Yes\n" +
			"  2. No\n" +
			"\n" +
			rule + "\n" +
			"  Enter to select · Tab/Arrow keys to navigate"},
	}
	for _, tc := range cases {
		if got := permissionModeOf(newScreen(tc.screen)); got != "" {
			t.Errorf("%s -> %q, want nothing read", tc.name, got)
		}
	}
}

// The mode is orthogonal to what the session is doing, so it must survive every
// return path out of the classifier — a working session, an idle one, one blocked
// on unsent text — not just the one somebody thought of first.
func TestPermissionModeIsCarriedByEveryStatus(t *testing.T) {
	indicator := "  ⏸ plan mode on (shift+tab to cycle) · ← for agents"
	screens := map[string]string{
		"idle, empty composer": "  transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯\n" + rule + "\n" + indicator,
		"working":              "  transcript\n✻ Brewing… (12s · esc to interrupt)\n" + rule + "\n❯\n" + rule + "\n" + indicator,
		"unsent text":          "  transcript\n✻ Brewed for 1m 0s\n" + rule + "\n❯ half a thought\n" + rule + "\n" + indicator,
	}
	for name, raw := range screens {
		st, _ := classifyPaneRemembering(raw, true, true, false, paneMemory{}, testEpoch)
		if st.PermissionMode != fleet.PermissionModePlan {
			t.Errorf("%s: status %q carried permission mode %q, want plan", name, st.Status, st.PermissionMode)
		}
	}
}

// A pane whose process is gone has no runtime left to be describing itself.
func TestADeadPaneReportsNoPermissionMode(t *testing.T) {
	raw := paneWithIndicator("  ⏸ plan mode on (shift+tab to cycle) · ← for agents")
	st, _ := classifyPaneRemembering(raw, true, false, false, paneMemory{}, testEpoch)
	if st.PermissionMode != "" {
		t.Errorf("a dead pane reported permission mode %q", st.PermissionMode)
	}
}

// The redactor must keep every label the reader matches, or a committed real
// capture of that mode would be discarded as unrecognised and could not exercise
// the row it exists for.
func TestKnownFooterPhrasesCoverEveryModeLabel(t *testing.T) {
	for _, l := range permissionModeLabels {
		if !isKnownFooter("  ⏵⏵ " + l.label + " (shift+tab to cycle)") {
			t.Errorf("redaction would discard a footer row naming %q", l.label)
		}
	}
	if got := RedactCapture("  ⏸ manual mode on · ? for shortcuts · ← for agents"); got != "  ⏸ manual mode on · ? for shortcuts · ← for agents" {
		t.Errorf("the default-mode footer row did not survive redaction whole: %q", got)
	}
}

// Every mode label is the wording a candidate build is checked for, so the
// reader's table and the compat markers may not drift apart.
func TestCompatChecksTheWordingTheReaderMatches(t *testing.T) {
	static := map[string]bool{}
	for _, m := range compatStaticMarkers["F-MODE"] {
		static[m] = true
	}
	for _, l := range permissionModeLabels {
		// The default and bypass wordings are composed at runtime and are not
		// literal strings in the candidate; the live sessions the check boots
		// exercise those two instead (see F-MODE).
		if l.mode == fleet.PermissionModeDefault || l.mode == fleet.PermissionModeBypass {
			continue
		}
		if !static[l.label] {
			t.Errorf("F-MODE's static markers omit %q, which the reader matches", l.label)
		}
	}
}
