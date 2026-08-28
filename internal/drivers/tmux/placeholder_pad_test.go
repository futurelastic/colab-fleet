package tmux

import "testing"

// The exact bytes a sibling project measured on a live pane, NBSP pad and all:
//
//	ESC[39m ❯   ESC[2m merge it ESC[0m
//
// Their fix keyed on the SEPARATOR — /❯[ \t]*\x1b\[2m/ — and the pad is U+00A0,
// which is neither space nor tab, so it matched nothing and 50 of 50 sessions
// stayed falsely flagged after the merge.
//
// This asks a different question: is every VISIBLE character after the marker
// dim? That is a property of the rendering, not a byte sequence, so whatever
// pads the gap is irrelevant.
func TestPlaceholderPadIsIrrelevant(t *testing.T) {
	const rule = "────────────────────"
	ghost := "transcript\n" + rule + "\n\x1b[39m❯ \x1b[2mmerge it\x1b[0m\n" + rule + "\n"
	real := "transcript\n" + rule + "\n\x1b[39m❯ Now update the DB\n" + rule + "\n"

	if got, scan := composerText(newScreen(ghost)); got != "" || scan != composerFound {
		t.Errorf("NBSP-padded placeholder read as unsent text %q (scan=%v) — this is the false veto", got, scan)
	}
	if got, scan := composerText(newScreen(real)); scan != composerFound || got == "" {
		t.Errorf("real typed text after an NBSP pad was missed: %q (scan=%v)", got, scan)
	}
}
