package tmux

import (
	"strings"

	fleet "github.com/futurelastic/muster"
)

// Reading the runtime's own footer notices off the pane (muster#230).
//
// # The detector IS #229's exclusion, run the other way round
//
// #229 found that a footer notice below the composer's closing rule can
// satisfy statusLine()'s shape test for a real turn-status line — a single
// non-ASCII symbol, a space, a capitalised word, a tense marker — and fixed
// the misread by bounding spinner()'s backward scan at the composer so that
// region is never read as a turn status. That fix throws the line away once
// it is ruled out as a spinner.
//
// This reuses the identical shape test on the identical region for the
// opposite purpose: a line below the composer's closing rule that is shaped
// like a status line but ISN'T one — because nothing there is EVER the real
// spinner, per #229's own boundary — is exactly what a footer notice is. No
// second, independent detector was built, because the shape that caused the
// incident is the same shape that names the thing worth surfacing from it.
//
// # Why known chrome does not collide with this test
//
// The permission-mode indicator's five labels (permissionmode.go) all begin
// with a LOWER-case word — "manual", "accept", "plan", "auto", "bypass" —
// so none of them can satisfy statusLine's capitalised-first-word
// requirement. The model/plan row and the control-channel label share one
// composite `▸ …` row whose leading glyph runs straight into a second
// character rather than a single space (hasSpinnerGlyph requires exactly
// "glyph, then space"), and the auto-mode hint's own leading `⏵⏵` fails the
// same test for the same reason — two glyphs, not one followed by a space.
// Verified against every real footer fixture in this package's corpus
// (classify_test.go, corpus_test.go) rather than assumed: none of the known
// chrome rows are misread as a warning by this function.
//
// # Kind absent is not "no notice"
//
// A notice this driver has not been taught the wording for still reaches the
// caller with Text set and Kind empty, rather than being silently dropped —
// see fleet.Warning and fleet.WarningKind for the discipline this preserves.
func warningsOf(s screen) []fleet.Warning {
	var out []fleet.Warning
	for _, raw := range footerLines(s) {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if _, ok := statusLine(line); !ok {
			continue
		}
		out = append(out, fleet.Warning{Kind: warningKindOf(line), Text: line})
	}
	return out
}

// warningKnownText maps a substring of a footer notice's own wording onto
// this driver's WarningKind vocabulary. Matched by substring rather than the
// whole line, because the runtime is free to move the decoration around it
// (the truncation point, the joining " · ", the leading glyph) without
// changing what the notice is about — the same tolerance controlStateIn
// already takes on its own anchor, for the identical reason: match what the
// notice is FOR, not how it is decorated today.
var warningKnownText = []struct {
	substr string
	kind   fleet.WarningKind
}{
	{"Transcript writes are failing", fleet.WarningTranscriptUnreliable},
}

// warningKindOf names a notice-shaped footer line from its own wording, or
// returns "" when this driver does not recognise it — never a guess.
func warningKindOf(line string) fleet.WarningKind {
	for _, known := range warningKnownText {
		if strings.Contains(line, known.substr) {
			return known.kind
		}
	}
	return ""
}
