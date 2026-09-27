package tmux

import (
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet#230: the ENOSPC footer notice #229 taught spinner() to ignore
// is exactly the line this field exists to surface — the same shape test,
// aimed at the region #229 carved out rather than discarding it.
func TestWarningsOfFindsTheENOSPCFooterNotice(t *testing.T) {
	got := warningsOf(newScreen(fixtureIdleWithTranscriptWarning))
	if len(got) != 1 {
		t.Fatalf("warnings = %#v, want exactly one notice", got)
	}
	if got[0].Kind != fleet.WarningTranscriptUnreliable {
		t.Errorf("kind = %q, want %q", got[0].Kind, fleet.WarningTranscriptUnreliable)
	}
	want := "⚠ Transcript writes are failing (disk full — ENOSPC) · recent messages may …"
	if got[0].Text != want {
		t.Errorf("text = %q, want %q", got[0].Text, want)
	}
}

// The ordinary fixtures — no notice painted — must report no warnings at
// all, not a slice of one empty/zero-value entry: the whole field exists so
// absence and presence stay two different answers.
func TestWarningsOfEmptyWhenNoNoticeIsPainted(t *testing.T) {
	for _, name := range []string{"working", "idle", "unsent", "menu"} {
		var fixture string
		switch name {
		case "working":
			fixture = fixtureWorking
		case "idle":
			fixture = fixtureIdle
		case "unsent":
			fixture = fixtureUnsent
		case "menu":
			fixture = fixtureMenu
		}
		t.Run(name, func(t *testing.T) {
			if got := warningsOf(newScreen(fixture)); len(got) != 0 {
				t.Errorf("warnings = %#v, want none", got)
			}
		})
	}
}

// The known chrome rows below the composer — the model/plan row, the
// auto-mode hint, the permission-mode indicator — must never be misread as a
// notice: none of them share the shape statusLine() requires (a single
// glyph followed by a space, then a CAPITALISED word).
func TestWarningsOfDoesNotMisreadKnownChrome(t *testing.T) {
	cases := map[string]string{
		"model/plan row + auto-mode hint (fixtureWorking's own footer)": fixtureWorking,
		"manual mode indicator":                                         "  Done.\n✻ Brewed for 1s\n" + rule + "\n❯\n" + rule + "\n  ⏸ manual mode on · ? for shortcuts · ← for agents",
		"bypass mode indicator":                                         "  Done.\n✻ Brewed for 1s\n" + rule + "\n❯\n" + rule + "\n  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			if got := warningsOf(newScreen(fixture)); len(got) != 0 {
				t.Errorf("warnings = %#v, want none — this is known chrome, not a notice", got)
			}
		})
	}
}

// A notice whose wording this driver has not been taught still reaches the
// caller — Kind empty, Text carrying the runtime's own words — rather than
// being silently dropped for lack of a name (WarningKind's own doc).
func TestWarningsOfSurfacesAnUnrecognisedNoticeWithoutAName(t *testing.T) {
	fixture := "  Done.\n✻ Brewed for 1s\n" + rule + "\n❯\n" + rule +
		"\n  ▸ Opus 5 · agents\n  ⚠ Something the driver has never been told about…"
	got := warningsOf(newScreen(fixture))
	if len(got) != 1 {
		t.Fatalf("warnings = %#v, want exactly one unnamed notice", got)
	}
	if got[0].Kind != "" {
		t.Errorf("kind = %q, want empty — this wording is not in warningKnownText", got[0].Kind)
	}
	if got[0].Text != "⚠ Something the driver has never been told about…" {
		t.Errorf("text = %q, the runtime's own words must be preserved verbatim", got[0].Text)
	}
}

// End to end through classify(): the notice reaches SessionState.Warnings
// independent of Status, the same way it independently left Status untouched
// at `idle` in TestClassifyStatuses's own #229 case.
func TestClassifyStampsWarningsIndependentOfStatus(t *testing.T) {
	got := classify(fixtureIdleWithTranscriptWarning, true)
	if got.Status != fleet.StatusIdle {
		t.Fatalf("status = %q, want idle (unrelated to this test, but a precondition of it)", got.Status)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Kind != fleet.WarningTranscriptUnreliable {
		t.Errorf("Warnings = %#v, want the ENOSPC notice classified", got.Warnings)
	}

	clean := classify(fixtureIdle, true)
	if len(clean.Warnings) != 0 {
		t.Errorf("Warnings = %#v, want none on a screen with no footer notice", clean.Warnings)
	}
}
