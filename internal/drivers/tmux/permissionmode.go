package tmux

import (
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// Reading the runtime's permission-mode indicator off the pane (#194).
//
// # What is being read, and where it came from
//
// The row below the composer names the mode the session is in. This is not
// taken from a settings file or a command line: it was measured on a real
// runtime build (2.1.282) by starting sessions in a private multiplexer server
// and pressing Shift+Tab through the whole cycle, reading the row after each
// press. Verbatim, the first footer row in each mode:
//
//	default      ⏸ manual mode on · ? for shortcuts · ← for agents
//	acceptEdits  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents
//	plan         ⏸ plan mode on (shift+tab to cycle) · ← for agents
//	auto         ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
//	bypass       ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
//
// and the cycle, in a session launched with bypass permissions on a model that
// offers auto, was bypass → auto → default → acceptEdits → plan → bypass; in a
// session launched without, on a model that does not offer auto, default →
// acceptEdits → plan → default. A mode a build does not offer is simply skipped
// by the cycle, so a client must read after every press and never count them.
//
// Two things in that table were not what the repository had assumed, and both
// are why it was measured rather than copied from a test fixture. The default
// mode is NOT a bare `? for shortcuts` (the synthetic composer used in tests
// modelled it that way): it says `manual mode on` too. And bypass, the mode
// most of an unattended fleet runs in, has a row of its own.
//
// # Only the wording, never the decoration
//
// Each label is matched as a lower-case phrase anywhere on a footer row, not at
// a column and not with its glyph. The leading `⏸`/`⏵⏵`, the `(shift+tab to
// cycle)` hint and the trailing `← for agents` are decoration the runtime moves
// freely — the hint even appears and disappears with the mode (default has none
// and shows `? for shortcuts` instead). The wording is what names the mode, and
// it is the one part `colab-fleetd compat` checks a candidate build still has.
//
// # Why only below the closing fence, and why that is the safety property
//
// Same rule, same reason as controlchannel.go: scrollback above the composer is
// transcript, transcript is whatever the agent chose to print, and a matcher
// that scanned the whole screen would be readable and forgeable by every agent
// this service watches — a session that printed `plan mode on` into its own
// transcript would classify ITSELF as being in plan mode, and a client that
// stops cycling when it reads `plan` would stop on a lie. The footer is chrome
// the runtime redraws; the agent cannot write into it.
//
// The anchor is composerSpan, not footerLines. footerLines walks to the last ❯
// on the screen and takes everything after it, which is right for a label that
// is only ever read when present but is too loose for a field that says
// `unknown` when it finds nothing: a selection menu's highlighted row is also
// ❯, and the menu's own lines would be read as an indicator area. composerSpan
// requires the composer to be FENCED, so a menu is not one, and it hands back
// the closing rule, so a multi-line draft — text somebody typed, above that
// rule — is never part of what is read.
//
// # The three answers
//
// permissionModeOf returns one of:
//
//   - "" — nothing was read. No fenced composer on screen (a dialog owns it, the
//     capture is clipped, the session is starting), or a composer with nothing
//     painted under it yet. Read again.
//   - a named mode — exactly one mode's wording is on the indicator area.
//   - PermissionModeUnknown — there IS an indicator area and it does not name
//     exactly one mode: wording this build does not know, a hint painted in its
//     place (shell mode shows its own), or two labels at once. Never resolved to
//     the nearest neighbour; this package's discipline is that a screen it
//     cannot read produces no claim, and a wrong mode read confidently is the
//     one failure that turns a client's loop into an escalation.

// permissionModeLabels maps the wording the runtime prints onto the model's
// closed set. Order is irrelevant: a row naming two modes is unknown, not
// whichever is listed first.
//
// `accept edits on` is deliberately not prefixed: a build that paints it with a
// leading qualifier (an older one said `auto-accept edits on`) still names the
// same mode.
var permissionModeLabels = []struct {
	label string
	mode  fleet.PermissionModeState
}{
	{"manual mode on", fleet.PermissionModeDefault},
	{"accept edits on", fleet.PermissionModeAcceptEdits},
	{"plan mode on", fleet.PermissionModePlan},
	{"auto mode on", fleet.PermissionModeAuto},
	{"bypass permissions on", fleet.PermissionModeBypass},
}

// permissionModeOf reads the permission-mode indicator off a screen. See the
// block comment above for the three things it can return.
func permissionModeOf(s screen) fleet.PermissionModeState {
	_, closing, found := composerSpan(s)
	if found != composerFound {
		return ""
	}
	seen := map[fleet.PermissionModeState]bool{}
	area := false
	for _, line := range s.lines[closing+1:] {
		text := strings.ToLower(strings.TrimSpace(line))
		if text == "" {
			continue
		}
		area = true
		for _, l := range permissionModeLabels {
			if strings.Contains(text, l.label) {
				seen[l.mode] = true
			}
		}
	}
	switch {
	case !area:
		return ""
	case len(seen) == 1:
		for m := range seen {
			return m
		}
	}
	return fleet.PermissionModeUnknown
}
