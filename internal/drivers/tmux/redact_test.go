package tmux

import (
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// #73: a real capture (#70) showed the control-channel label sharing the
// model/plan row with the project name, glued on by alignment padding
// rather than the " · " separator every other footer element uses. The
// prior rule — replace everything after the last " · " — could not tell
// the label apart from the name it shares the row with and discarded both.
// This pins the fix: only the name is sensitive, the label survives.
func TestRedactModelLineKeepsAControlChannelLabelSharingItsRow(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "active renders as a bare label with no trailing word",
			raw:  "  ▸ Opus 5 · src                                                    /rc",
			want: "  ▸ Opus 5 · <project> /rc",
		},
		{
			name: "a state carrying a trailing word survives the same way",
			raw:  "  ▸ Opus 5 · src                                              /rc failed",
			want: "  ▸ Opus 5 · <project> /rc failed",
		},
		{
			name: "no control channel on this row redacts exactly as before",
			raw:  "  ▸ Opus 5 · src",
			want: "  ▸ Opus 5 · <project>",
		},
		{
			name: "a name that merely ends in the same three letters is not mistaken for the label",
			raw:  "  ▸ Opus 5 · marc",
			want: "  ▸ Opus 5 · <project>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCapture(tc.raw)
			if got != tc.want {
				t.Errorf("RedactCapture(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The same fixed-point property TestCorpusIsFullyRedacted holds every
// committed screen to: redacting an already-redacted line must reproduce
// it exactly, or a corpus case built from this row would fail that gate
// the moment it landed.
func TestRedactModelLineWithControlLabelIsAFixedPoint(t *testing.T) {
	cases := []string{
		"  ▸ Opus 5 · src                                                    /rc",
		"  ▸ Opus 5 · src                                              /rc failed",
	}
	for _, raw := range cases {
		once := RedactCapture(raw)
		twice := RedactCapture(once)
		if once != twice {
			t.Errorf("not a fixed point for %q: first pass %q, second pass %q", raw, once, twice)
		}
	}
}

// #74: a real capture (taken while working #65) showed the composer's own
// opening fence rendering the session name centred on it — exactly what
// isRule's own doc comment in classify.go says happens ("the opening rule
// is labelled with the session name"), and exactly the vocabulary this
// repo's local conventions forbid naming. The prior rule — keep any line
// isRule accepts verbatim, on the assumption it is pure box-drawing — kept
// that name too. This pins the fix: the rule survives, the label does not.
func TestRedactRuleLineDropsALabelButKeepsTheRule(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "a labelled opening fence loses the label, keeps the rule",
			raw:  "──────────────────────────────────────────────────────────────── my-session-42 ─",
			want: "──────────────────────────────────────────────────────────────── <session> ─",
		},
		{
			name: "a pure rule with no label is untouched",
			raw:  "────────────────────────────────────────────────────────────────────────────────",
			want: "────────────────────────────────────────────────────────────────────────────────",
		},
		{
			// A first version of this fix stripped every isRule-accepted
			// line the same way, and a real capture (still working #74)
			// showed that reaching too far: the welcome screen's own boxed
			// border also satisfies isRule (plenty of dashes) but is not a
			// labelled fence — it is fixed, non-sensitive product chrome
			// wrapped in box corners, and shredding its title into several
			// stray placeholders made an already-safe line unreadable for
			// no safety gained. A line carrying box corners or verticals
			// is left exactly as it always was.
			name: "a boxed welcome-screen border is not touched at all",
			raw:  "╭─── Claude Code v2.1.238 ─────────────────────────────────────────╮",
			want: "╭─── Claude Code v2.1.238 ─────────────────────────────────────────╮",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCapture(tc.raw)
			if got != tc.want {
				t.Errorf("RedactCapture(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// The redacted line must still be recognisable as a rule, or the
			// fence it marks would stop classifying as one on replay.
			if !isRule(got) {
				t.Errorf("redacted line %q no longer satisfies isRule", got)
			}
		})
	}
}

// Same fixed-point discipline as the model-line case above, for the same
// reason: a corpus case built from a labelled fence must survive
// TestCorpusIsFullyRedacted the moment it lands.
func TestRedactRuleLineWithLabelIsAFixedPoint(t *testing.T) {
	raw := "──────────────────────────────────────────────────────────────── my-session-42 ─"
	once := RedactCapture(raw)
	twice := RedactCapture(once)
	if once != twice {
		t.Errorf("not a fixed point: first pass %q, second pass %q", once, twice)
	}
}

// colab-fleet#176: a multi-select question's checkbox glyphs and its dialog
// tab bar are what corroborate the shape, so redaction keeps them — and only
// them: the labels and question headers are the agent's words.
func TestRedactKeepsMultiSelectChromeAndNothingElse(t *testing.T) {
	cases := map[string]string{
		"  1. [✔] A private label":             "  1. [✔] " + placeholderToken,
		"❯ 2. [ ] Another private label":       "❯ 2. [ ] " + placeholderToken,
		"  4. [ ] Type something":              "  4. [ ] Type something",
		"←  ☒ Secret  ☐ Other  ✔ Submit  →":    "←  ☒ " + placeholderToken + "  ☐ " + placeholderToken + "  ✔ Submit  →",
		"←  ☐ Header with spaces  ✔ Submit  →": "←  ☐ " + placeholderToken + "  ✔ Submit  →",
	}
	for in, want := range cases {
		got := redactLine(in)
		if got != want {
			t.Errorf("redactLine(%q) = %q, want %q", in, got, want)
		}
		if again := redactLine(got); again != got {
			t.Errorf("redactLine is not a fixed point on %q: second pass gave %q", got, again)
		}
	}
}

// #212: a compat pack exists so a tool outside this repository can replay its
// own classifiers over the states a run saw. The external-imports dialog's two
// options are what the classifier reads, so they must survive redaction — with
// the highlight and the indentation the unnumbered-menu reader keys on — while
// the question and the imported path, which are prose, do not.
func TestRedactKeepsTheExternalImportsOptionsSoARedactedDialogStillClassifies(t *testing.T) {
	redacted := RedactCapture(fixtureExternalImportsMenu)
	for _, want := range []string{"❯ No, disable external imports", "    Yes, allow external imports"} {
		if !strings.Contains(redacted, want) {
			t.Errorf("the redacted dialog lost %q:\n%s", want, redacted)
		}
	}
	if strings.Contains(redacted, "/shared/rules.md") {
		t.Errorf("the imported path reached the redacted dialog:\n%s", redacted)
	}
	p, unnumbered := parsePromptMenu(newScreen(redacted))
	if p == nil || !unnumbered {
		t.Fatalf("the redacted dialog no longer reads as an unnumbered menu: %+v, unnumbered=%v", p, unnumbered)
	}
	if got := classifyPromptKind(p); got != fleet.PromptExternalImports {
		t.Errorf("the redacted dialog classifies as %q, want %q", got, fleet.PromptExternalImports)
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want the decline (1)", p.Selected)
	}
	if again := RedactCapture(redacted); again != redacted {
		t.Errorf("redaction is not a fixed point on this dialog:\n%s\n--- then ---\n%s", redacted, again)
	}
}
