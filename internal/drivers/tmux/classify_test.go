package tmux

import (
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// The fixtures below are real pane captures taken from a machine running 22
// concurrent sessions, trimmed and with content redacted but with every
// structural feature preserved verbatim — the rule widths, the composer
// fencing, the spinner forms, the exact footer strings. Invented fixtures
// would test the classifier against the author's belief about the TUI
// rather than against the TUI.

const rule = "────────────────────────────────────────────────────────────────────────────────"

// working: spinner in its running form, composer empty.
const fixtureWorking = `  ⎿  ◻ Build first working tmux driver
     ◻ Probe tmux control mode for real push events
✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)
` + rule + `
❯
` + rule + `
  ▸ Opus 5 · colab-fleet                                                   /rc
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// idle: spinner in its finished form, composer empty.
const fixtureIdle = `  Done — the userscript hides both columns and centres the timeline.
✻ Brewed for 8m 21s
` + rule + `
❯
` + rule + `
  ▸ Opus 5 · agents
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// waiting_input via unsent composer text: turn finished, human has typed
// but not submitted. This is the §2.4 hazard case.
const fixtureUnsent = `  Want me to fold the composer-text hazard into the skill?
✻ Worked for 2m 7s
` + rule + `
❯ yes, update the skill
` + rule + `
  ▸ Opus 5 (1M context) · Claude
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

// waiting_input via a blocking menu.
const fixtureMenu = `  3. Third option, summarised
     A sentence of explanation under the option label.
  4. Type something.
` + rule + `
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// A pane whose transcript contains ❯-prefixed lines that are history, not
// live input — the TUI echoes previously-run commands this way. The live
// composer below them is empty. A classifier that matched "a line starting
// with ❯" instead of the fenced composer would read "/remote-control" as
// unsent input and refuse every send to this session forever.
const fixtureEchoedHistory = `❯ /remote-control
  ⎿  Remote Control disconnected.
❯ /remote-control
❯ /remote-control
` + rule + `
❯
` + rule + `
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents`

func TestClassifyDeadNeedsNoScreen(t *testing.T) {
	got := classify(fixtureWorking, false)
	if got.Status != fleet.StatusDead {
		t.Fatalf("a pane with no live process must be dead, got %q", got.Status)
	}
	if got.Confidence != fleet.ConfidenceInferred {
		t.Errorf("this driver never observes; got confidence %q", got.Confidence)
	}
}

func TestClassifyStatuses(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    fleet.Status
	}{
		{"running spinner", fixtureWorking, fleet.StatusWorking},
		{"finished spinner, empty composer", fixtureIdle, fleet.StatusIdle},
		{"finished spinner, unsent composer text", fixtureUnsent, fleet.StatusWaitingInput},
		{"blocking selection menu", fixtureMenu, fleet.StatusWaitingInput},
		{"echoed history above an empty composer", fixtureEchoedHistory, fleet.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.fixture, true)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (evidence: %s)", got.Status, tc.want, got.Evidence)
			}
			if got.Confidence != fleet.ConfidenceInferred {
				t.Errorf("confidence = %q, want inferred: this driver reads a screen, "+
					"it never observes a structured status (§5.6)", got.Confidence)
			}
			if strings.TrimSpace(got.Evidence) == "" {
				t.Error("every state must carry evidence (§2.3); this one is empty")
			}
		})
	}
}

// The composer is the input to the §2.4 refusal decision, so it gets its
// own tests: a false positive here refuses legitimate sends forever, and a
// false negative corrupts a human's half-typed message.
func TestComposerTextDistinguishesLiveInputFromTranscript(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		wantText string
		wantOK   bool
	}{
		{"empty composer", fixtureIdle, "", true},
		{"unsent text", fixtureUnsent, "yes, update the skill", true},
		{"echoed history is not live input", fixtureEchoedHistory, "", true},
		{"menu has no composer", fixtureMenu, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := composerText(newScreen(tc.fixture))
			if ok != tc.wantOK {
				t.Fatalf("found = %v, want %v", ok, tc.wantOK)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

// An agent can print anything to its own transcript, including a convincing
// forgery of the TUI's chrome. The classifier reads only the tail below the
// final rule, so transcript text cannot masquerade as the live composer.
func TestForgedChromeInTranscriptDoesNotBecomeTheComposer(t *testing.T) {
	forged := rule + "\n❯ rm -rf something the human never typed\n" + rule + "\n" + fixtureIdle
	text, ok := composerText(newScreen(forged))
	if !ok {
		t.Fatal("composer should still be found")
	}
	if text != "" {
		t.Errorf("transcript forgery leaked into composer text: %q", text)
	}
}

// The spinner's verb is randomised by the TUI. Any classifier that pattern
// matches on specific verbs is wrong; this test pins the property that only
// the shape of the suffix is load-bearing.
func TestSpinnerVerbIsNotLoadBearing(t *testing.T) {
	for _, verb := range []string{"Zigzagging", "Brewed", "Sautéed", "Percolating", "Noodling"} {
		running, found := spinner(newScreen("✻ " + verb + "… (1m 2s · ↓ 3.1k tokens)"))
		if !found || !running {
			t.Errorf("verb %q: running spinner not recognised (found=%v running=%v)", verb, found, running)
		}
		running, found = spinner(newScreen("✻ " + verb + " for 1m 2s"))
		if !found || running {
			t.Errorf("verb %q: finished spinner not recognised (found=%v running=%v)", verb, found, running)
		}
	}
}

// A spinner line in a shape this driver does not recognise must report "no
// usable spinner" rather than defaulting to finished — a wrong "idle" for a
// session that is actually working is the exact silent failure §5.6 exists
// to prevent.
func TestUnrecognisedSpinnerShapeIsNotTreatedAsFinished(t *testing.T) {
	_, found := spinner(newScreen("✻ something in a shape from a future release"))
	if found {
		t.Error("an unparseable spinner must not be reported as a usable reading")
	}
	got := classify("✻ something in a shape from a future release\n"+rule+"\n❯ \n"+rule, true)
	if got.Status != fleet.StatusUnknown {
		t.Errorf("status = %q, want unknown when the spinner shape is unrecognised", got.Status)
	}
}

// Rules carry a centred session-name label. Width varies with the terminal.
func TestIsRuleToleratesLabelsAndWidths(t *testing.T) {
	if !isRule("──────────────── continue-exploring📋 ──") {
		t.Error("a labelled rule must still be a rule")
	}
	if !isRule(strings.Repeat("─", 200)) {
		t.Error("a wide rule must be a rule")
	}
	if isRule("  the design — as discussed — is settled") {
		t.Error("prose containing a dash is not a rule")
	}
	if isRule("") {
		t.Error("empty line is not a rule")
	}
}

// --- extreme cases -------------------------------------------------------
//
// Every one of these was either observed on a live machine or is a direct
// neighbour of one that was. The failure mode they guard against is specific:
// a composer detector that is wrong in one direction refuses every send to a
// session forever (the session looks stuck for no visible reason), and wrong
// in the other direction concatenates into a message a human was still typing.

// The one that was live. A selection menu marks its highlighted option with
// the same glyph as the composer prompt, and sits above a rule — so it is a
// composer to anything that does not require the OPENING fence too.
const fixtureMenuSelected = `  Which name should it use?

❯ 1. the-first-option (Recommended)
     An explanatory line under the option.
  2. the-second-option
     Another explanation.
  4. Type something.
` + rule + `
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

func TestSelectedMenuItemIsNotTreatedAsUnsentInput(t *testing.T) {
	text, ok := composerText(newScreen(fixtureMenuSelected))
	if ok || text != "" {
		t.Fatalf("menu selection read as composer input (%q); every send to this "+
			"session would be refused forever, for text nobody typed", text)
	}
	// It is still blocked on a human, which is a different statement.
	if got := classify(fixtureMenuSelected, true); got.Status != fleet.StatusWaitingInput {
		t.Errorf("status = %q, want waiting_input", got.Status)
	}
}

// A long message wraps below the prompt and is still unsent. Reading only the
// first line under-reports it — the direction that corrupts a message.
func TestWrappedComposerCapturesEveryLine(t *testing.T) {
	f := rule + "\n❯ this is a long message that the human\n  wrapped onto a second line and a\n  third\n" + rule
	text, ok := composerText(newScreen(f))
	if !ok {
		t.Fatal("fenced composer not found")
	}
	for _, want := range []string{"long message", "second line", "third"} {
		if !strings.Contains(text, want) {
			t.Errorf("wrapped composer lost %q: got %q", want, text)
		}
	}
}

// A labelled opening fence in a narrow pane leaves few rule characters. If
// that is not recognised as a rule, the composer is not fenced and unsent
// input goes undetected.
func TestNarrowPaneWithLongSessionNameStillFences(t *testing.T) {
	narrow := "── a-very-long-session-name-here ──"
	if !isRule(narrow) {
		t.Fatal("labelled fence in a narrow pane not recognised; unsent input would go undetected")
	}
	f := narrow + "\n❯ half-typed thought\n" + narrow
	text, ok := composerText(newScreen(f))
	if !ok || text != "half-typed thought" {
		t.Errorf("composer = %q ok=%v, want the typed text", text, ok)
	}
}

// ...and prose must still not be mistaken for a fence.
func TestProseIsNotAFence(t *testing.T) {
	for _, line := range []string{
		"the design — as discussed — is settled",
		"a-b-c-d-e-f-g-h",
		"",
		"   ",
		"1. option — with an em dash",
	} {
		if isRule(line) {
			t.Errorf("prose treated as a rule: %q", line)
		}
	}
}

// Whitespace-only input is not input. Refusing on it would stick a session on
// a stray space.
func TestWhitespaceOnlyComposerIsEmpty(t *testing.T) {
	f := rule + "\n❯      \n" + rule
	text, ok := composerText(newScreen(f))
	if !ok {
		t.Fatal("composer not found")
	}
	if text != "" {
		t.Errorf("composer = %q, want empty; a stray space must not block a session", text)
	}
}

// A pane that is not the TUI at all yields no composer — and therefore no
// refusal on composer grounds, which is correct: we know nothing about it.
func TestNonTUIPaneYieldsNoComposer(t *testing.T) {
	for _, f := range []string{
		"$ ls -la\ntotal 8\ndrwxr-xr-x  2 user  staff",
		"",
		"\n\n\n",
		"❯ a bare prompt with no fences at all",
	} {
		if _, ok := composerText(newScreen(f)); ok {
			t.Errorf("claimed a composer in non-TUI output: %q", f)
		}
	}
}

// A fresh session that has not painted its chrome yet must not be read as
// holding input.
func TestUnpaintedSessionHoldsNothing(t *testing.T) {
	if _, ok := composerText(newScreen("\n\n")); ok {
		t.Error("unpainted pane reported a composer")
	}
	if got := classify("", true); got.Status != fleet.StatusUnknown {
		t.Errorf("status = %q, want unknown for an empty screen", got.Status)
	}
}
