package tmux

import (
	"strings"
	"testing"
	"time"

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

// --- the two ways a session stops being reachable -------------------------
//
// Both were live, and both are how a fleet loses a session to a dialog nobody
// can reach.

// A brand-new session shows a dim placeholder hint in its composer. Read as
// typed input, it refuses every send forever — to a session that has never
// been spoken to at all.
func TestDimPlaceholderIsNotTypedInput(t *testing.T) {
	// Exactly as captured from a live newly-created session.
	raw := rule + "\n\x1b[39m❯ \x1b[2mTry\x1b[0m \x1b[2m\"how\x1b[0m \x1b[2mdo\x1b[0m \x1b[2mI\x1b[0m \x1b[2mlog\x1b[0m \x1b[2man\x1b[0m \x1b[2merror?\"\x1b[0m\n" + rule
	text, ok := composerText(newScreen(raw))
	if !ok {
		t.Fatal("composer not found")
	}
	if text != "" {
		t.Errorf("placeholder read as pending input (%q); every send to a fresh "+
			"session would be refused for text nobody typed", text)
	}
}

// ...while real typed input is normal intensity and must still be protected.
func TestNormalIntensityInputIsStillProtected(t *testing.T) {
	raw := rule + "\n\x1b[39m❯ a half-typed thought\n" + rule
	text, ok := composerText(newScreen(raw))
	if !ok || text != "a half-typed thought" {
		t.Errorf("composer = %q ok=%v; real input must not be mistaken for a placeholder", text, ok)
	}
}

// Both menu footers block. Knowing only one classified a folder-trust prompt
// and a resume prompt as unknown, which reads as "cannot determine" rather
// than "blocked on a human" — so a supervisor waits forever.
func TestBothMenuFootersAreRecognised(t *testing.T) {
	for _, footer := range []string{
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
		"Enter to confirm · Esc to cancel",
	} {
		f := "  Some question?\n❯ 1. Yes, the recommended one\n  2. No\n" + footer
		option, blocked := selectionPrompt(newScreen(f))
		if !blocked {
			t.Errorf("footer %q not recognised as blocking", footer)
			continue
		}
		if option != "1. Yes, the recommended one" {
			t.Errorf("highlighted option = %q; a supervisor deciding whether to accept "+
				"the default needs to know what the default is", option)
		}
		if got := classify(f, true); got.Status != fleet.StatusWaitingInput {
			t.Errorf("status = %q, want waiting_input", got.Status)
		}
	}
}

// The evidence must name what is being asked, not merely that something is.
func TestBlockedStateNamesTheHighlightedOption(t *testing.T) {
	f := "  Resume?\n❯ 1. Resume from summary (recommended)\n  2. Full\nEnter to confirm · Esc to cancel"
	got := classify(f, true)
	if !strings.Contains(got.Evidence, "Resume from summary") {
		t.Errorf("evidence = %q; it should name the option a caller would be accepting", got.Evidence)
	}
}

// --- #424's requirements: enumerate, version, verify -----------------------

// The two boot prompts observed on one fleet put the SAFE option at different
// indices. A caller that accepted the highlighted default would proceed in one
// case and kill the session in the other — which is why the options must be
// enumerated rather than described.
const fixtureTrustPrompt = `  Quick safety check: Is this a project you created or one you trust?
❯ 1. Yes, I trust this folder
  2. No, continue without these permissions
Enter to confirm · Esc to cancel`

const fixtureBypassPrompt = `  By proceeding, you accept all responsibility for actions taken.
❯ 1. No, exit
  2. Yes, I accept
Enter to confirm · Esc to cancel`

func TestPromptOptionsAreEnumeratedInOrder(t *testing.T) {
	p := parsePrompt(newScreen(fixtureTrustPrompt))
	if p == nil {
		t.Fatal("no prompt parsed")
	}
	if len(p.Options) != 2 {
		t.Fatalf("options = %v, want two", p.Options)
	}
	if p.Options[0] != "Yes, I trust this folder" {
		t.Errorf("option 1 = %q", p.Options[0])
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1", p.Selected)
	}
	if p.Question == "" {
		t.Error("question is empty; a caller has to show the human what is being asked")
	}
}

// The whole reason enumeration matters.
func TestSafeOptionIsAtADifferentIndexBetweenPrompts(t *testing.T) {
	trust := parsePrompt(newScreen(fixtureTrustPrompt))
	bypass := parsePrompt(newScreen(fixtureBypassPrompt))
	if trust == nil || bypass == nil {
		t.Fatal("both prompts must parse")
	}
	if trust.Selected != 1 || bypass.Selected != 1 {
		t.Fatal("fixtures both highlight option 1")
	}
	// Same highlighted index, opposite meanings.
	if trust.Options[0] == bypass.Options[0] {
		t.Fatal("fixtures drifted")
	}
	if !strings.HasPrefix(bypass.Options[0], "No, exit") {
		t.Errorf("bypass option 1 = %q; accepting the default here EXITS the session",
			bypass.Options[0])
	}
}

// A nonce must change when the question changes, and be stable when it does not.
func TestNonceTracksTheQuestion(t *testing.T) {
	a := parsePrompt(newScreen(fixtureTrustPrompt))
	again := parsePrompt(newScreen(fixtureTrustPrompt))
	b := parsePrompt(newScreen(fixtureBypassPrompt))
	if a.Nonce != again.Nonce {
		t.Error("nonce changed for an unchanged prompt; every answer would be refused")
	}
	if a.Nonce == b.Nonce {
		t.Error("two different prompts share a nonce; a stale answer would be applied " +
			"to a question the caller never saw")
	}
}

// Options are found by their numbering, not their wording, so a prompt from a
// future release enumerates without a new matcher.
func TestUnknownPromptStillEnumerates(t *testing.T) {
	f := `  Some question nobody has written a matcher for
❯ 1. First
  2. Second
  3. Third
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`
	p := parsePrompt(newScreen(f))
	if p == nil || len(p.Options) != 3 {
		t.Fatalf("parsed = %+v; enumeration must not depend on recognising the wording", p)
	}
}

// §8's `starting`: a booting session is not an unreadable one. Conflating them
// is why a spawn that never got past a boot screen "read as healthy".
func TestYoungPaneWithNoInterfaceIsStartingNotUnknown(t *testing.T) {
	booting := "  loading...\n"
	if got := classifyAged(booting, true, true); got.Status != fleet.StatusStarting {
		t.Errorf("young pane = %q, want starting", got.Status)
	}
	if got := classifyAged(booting, true, false); got.Status != fleet.StatusUnknown {
		t.Errorf("old pane = %q, want unknown — age is the discriminator", got.Status)
	}
}

// The pane is written by an agent that can print anything, so anything parsed
// out of it is untrusted input. Padding a slice up to a parsed index is
// unbounded allocation: one transcript line hung the live service.
func TestPromptParsingIsBoundedByHostileInput(t *testing.T) {
	hostile := `  A question
❯ 1. Real option
  2. Another
  1000000. absurd index from the transcript
  99999999999999999999. and an overflowing one
Enter to confirm · Esc to cancel`
	done := make(chan *fleet.SessionPrompt, 1)
	go func() { done <- parsePrompt(newScreen(hostile)) }()
	select {
	case p := <-done:
		if p == nil {
			t.Fatal("prompt should still parse")
		}
		if len(p.Options) > 32 {
			t.Errorf("options = %d; a menu with more options than this is not a menu", len(p.Options))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parsing did not terminate — unbounded work driven by screen content")
	}
}

// A fourth footer, from a tool-permission dialog: neither "Enter to select"
// nor "Enter to confirm". Footer-matching was always going to lose this race —
// detection is structural first.
const fixturePermissionDialog = `⏺ Write(REMOTE_TEST.md)
 Create file
 REMOTE_TEST.md
  1 written by Alex on the other machine
 Do you want to create REMOTE_TEST.md?
❯ 1. Yes
  2. Yes, allow all edits in this directory during this session
  3. No
 Esc to cancel · Tab to amend`

func TestPermissionDialogIsDetectedWithoutAKnownFooter(t *testing.T) {
	p := parsePrompt(newScreen(fixturePermissionDialog))
	if p == nil {
		t.Fatal("tool-permission dialog not detected; a session blocked on it would " +
			"read as unreadable and nobody would answer it")
	}
	if len(p.Options) != 3 || p.Selected != 1 {
		t.Errorf("parsed %d options, selected %d; want 3 and 1", len(p.Options), p.Selected)
	}
	if got := classify(fixturePermissionDialog, true); got.Status != fleet.StatusWaitingInput {
		t.Errorf("status = %q, want waiting_input", got.Status)
	}
}

// ...and the case structure alone would miss: a menu whose highlighted option
// has scrolled above the captured window. The footer still carries it.
func TestMenuWithScrolledOffMarkerStillCounts(t *testing.T) {
	f := "  4. Type something.\n  5. Chat about this\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel"
	if _, blocked := selectionPrompt(newScreen(f)); !blocked {
		t.Error("a menu whose marker scrolled out of view must still read as blocked")
	}
}

// A numbered list in a transcript is not a question.
func TestTranscriptListIsNotAPrompt(t *testing.T) {
	f := `  Here is what I found:
  1. the first thing
  2. the second thing
  3. the third thing

` + rule + `
❯
` + rule
	if p := parsePrompt(newScreen(f)); p != nil {
		t.Errorf("transcript list read as a prompt: %+v", p.Options)
	}
}

// The largest source of `unknown` in a real fleet was one screen shape: no
// spinner, composer painted and empty. It is genuinely ambiguous from a single
// capture — a fresh prompt and a turn that has not painted yet look identical
// — and it accounted for 10 of 91 sessions across two machines.
//
// A second look settles it, and these tests pin which way each case resolves.
func TestResolveAmbiguityUsesTheSecondLook(t *testing.T) {
	const idleScreen = "some transcript\n" +
		"────────────────────\n" +
		"❯ \x1b[2mTry \"fix the tests\"\x1b[0m\n" +
		"────────────────────\n"

	t0 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)

	// First sighting: nothing to compare against, so the honest answer is
	// still unknown. This is the floor the change must not lower.
	first, digest := classifyPaneRemembering(idleScreen, true, true, false, paneMemory{}, t0)
	if first.Status != fleet.StatusUnknown {
		t.Fatalf("first sighting = %s, want unknown — one capture cannot settle this", first.Status)
	}
	if digest == "" {
		t.Fatal("a successful capture must yield a digest to compare against next time")
	}

	// Same screen, later: a turn that had just begun would have painted a
	// spinner by now.
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(idleScreen, true, true, false, prior, t0.Add(30*time.Second))
	if second.Status != fleet.StatusIdle {
		t.Errorf("unchanged screen after 30s = %s, want idle (%s)", second.Status, second.Evidence)
	}
	if second.Confidence != fleet.ConfidenceInferred {
		t.Error("resolution is still an inference, not an observation")
	}

	// Too soon: within the paint grace, the ambiguity is real.
	tooSoon, _ := classifyPaneRemembering(idleScreen, true, true, false, prior, t0.Add(time.Second))
	if tooSoon.Status != fleet.StatusUnknown {
		t.Errorf("within the paint grace = %s, want unknown", tooSoon.Status)
	}

	// Changed screen: deliberately NOT resolved to working. Content moves for
	// reasons other than a turn, and guessing the busy direction is how a
	// caller interrupts a session that was doing nothing.
	changed, _ := classifyPaneRemembering(idleScreen+"more output\n", true, true, false, prior, t0.Add(30*time.Second))
	if changed.Status != fleet.StatusUnknown {
		t.Errorf("changed screen = %s, want unknown — resolution only ever goes toward less activity", changed.Status)
	}
}

// Unsent text on a screen that has stopped moving is the case a sibling
// project measured at 37 of 39 panes: blocked on a human pressing enter, with
// the age as the discriminator between mid-thought and abandoned.
func TestResolveAmbiguityForStrandedInput(t *testing.T) {
	const pendingScreen = "transcript\n" +
		"────────────────────\n" +
		"❯ please refactor the parser\n" +
		"────────────────────\n"

	t0 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	first, digest := classifyPaneRemembering(pendingScreen, true, true, false, paneMemory{}, t0)
	if first.Status != fleet.StatusUnknown {
		t.Fatalf("first sighting = %s, want unknown", first.Status)
	}
	prior := paneMemory{known: true, digest: digest, at: t0}
	second, _ := classifyPaneRemembering(pendingScreen, true, true, false, prior, t0.Add(time.Minute))
	if second.Status != fleet.StatusWaitingInput {
		t.Errorf("stable screen with unsent input = %s, want waiting_input (%s)", second.Status, second.Evidence)
	}
}

// A failed capture must not produce a digest: two failures in a row would
// otherwise compare equal and be read as a stable screen — a driver
// malfunction laundered into an observation about the session (§5.7, F5).
func TestFailedCaptureYieldsNoDigest(t *testing.T) {
	st, digest := classifyPaneRemembering("", false, true, false, paneMemory{}, time.Now())
	if st.Status != fleet.StatusUnknown {
		t.Errorf("failed capture = %s, want unknown", st.Status)
	}
	if digest != "" {
		t.Error("a failed capture must not be remembered as a screen")
	}
}

// Five animation frames were live on ONE machine at one instant. Matching a
// single glyph meant a session's status line was legible or invisible
// depending on which frame the capture caught — 16% of that machine's sessions
// read `unknown` while showing a perfectly good running spinner.
func TestStatusLineAcceptsEveryObservedGlyph(t *testing.T) {
	for _, glyph := range []string{"✻", "✽", "✢", "✶", "✳"} {
		t.Run(glyph, func(t *testing.T) {
			running, ok := statusLine(glyph + " Metamorphosing… (21m 39s · ↓ 91.4k tokens)")
			if !ok {
				t.Fatalf("%s not recognised as a status line", glyph)
			}
			if !running {
				t.Error("ellipsis form means the turn is still running")
			}
			finished, ok := statusLine(glyph + " Worked for 2m 7s")
			if !ok || finished {
				t.Errorf("%s finished form: ok=%v running=%v", glyph, ok, finished)
			}
		})
	}
}

// The glyph test had to be widened, and a wide test meets far more than the
// status line: the TUI's chrome is made of symbols. These are lines from real
// captures that must never be read as a turn status.
func TestStatusLineRejectsChrome(t *testing.T) {
	for _, line := range []string{
		"❯ ",                          // the composer, which sits BELOW the status line
		"❯ please refactor for me",    // ... and can contain the finished infix
		"⏵⏵ auto mode on (shift+tab)", // mode footer
		"▸ Opus 5 · coding-dashboard", // model footer
		"⎿  $ cd /some/path",          // tool output
		"────────────────────", // composer fencing
		"- a bulleted line", // ASCII punctuation: transcript prose
		"* another for good measure",
		"",
	} {
		if _, ok := statusLine(line); ok {
			t.Errorf("%q was read as a status line", line)
		}
	}
}

// The kind exists so a caller can auto-answer a question it recognises without
// matching prose itself. Its most important property is the last case: an
// unfamiliar prompt gets NO kind, so a filter on a known kind can never select
// it. Empty is not permission.
func TestClassifyPromptKind(t *testing.T) {
	cases := []struct {
		name string
		opts []string
		want fleet.PromptKind
	}{
		{"resume chooser", []string{"Resume from summary (recommended)", "Resume full session as-is", "Don't ask me again"}, fleet.PromptResumeChooser},
		{"folder trust", []string{"Yes, I trust this folder", "No, continue without these"}, fleet.PromptFolderTrust},
		{"bypass", []string{"Yes, I accept the bypass permissions mode", "No, exit"}, fleet.PromptBypassAcceptance},
		{"tool permission", []string{"Yes, allow this command", "Yes, and don't ask again", "No, tell Claude what to do"}, fleet.PromptToolPermission},
		{"something never seen before", []string{"Deploy to production", "Cancel"}, ""},
		{"no options at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPromptKind(&fleet.SessionPrompt{Options: tc.opts})
			if got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// The prompt whose highlighted default is "No, exit" is the reason policy
// never moves into this service: a caller that answered the default here would
// kill the session it meant to rescue. The kind must not imply it is safe.
func TestUnrecognisedPromptIsNeverGivenAKind(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{"No, exit", "Yes, proceed"}, Selected: 1}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("a prompt we do not recognise was labelled %q — a filter would then select it", k)
	}
}
