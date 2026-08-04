package tmux

import (
	"strings"
	"unicode"

	fleet "github.com/godx-jp/colab-fleet"
)

// This file holds everything that reads a terminal screen and guesses what
// the agent behind it is doing. It is deliberately the only such place in
// the driver: §5.1 says the interface expresses questions ("state") and
// never mechanisms ("readScreen"), so the mechanism is quarantined here
// rather than smeared across the operations.
//
// # Why almost nothing here is certain
//
// Every function below returns fleet.ConfidenceInferred, never
// ConfidenceObserved, and that is not modesty — it is measured. Claude
// Code's TUI signals that a turn is in progress with a spinner line whose
// verb is drawn at random from a large set ("Zigzagging", "Brewed",
// "Sautéed", "Worked"), and distinguishes running from finished by that
// verb's grammatical tense plus the shape of its suffix:
//
//	✻ Zigzagging… (5m 57s · ↓ 21.3k tokens)   <- running
//	✻ Worked for 2m 7s                         <- finished
//
// A driver keying on that is keying on the tense of a randomly chosen
// English word in a UI with no compatibility contract. It works today. It
// is one release note away from being wrong, and — this is the part that
// matters — wrong *silently*, because a missing spinner reads exactly like
// a finished turn.
//
// So the classifier is built to fail toward fleet.StatusUnknown rather than
// toward a plausible answer (§5.6, "degrade, never emulate"). A caller that
// sees unknown can go look; a caller that sees a confident "idle" that is
// actually "working" cannot. §2.3 makes unknown a first-class answer for
// exactly this situation, and this driver is the reason to believe that was
// the right call.
//
// # The signals that ARE structural
//
// Three do not depend on prose, and they carry the weight:
//
//   - the composer box — the "❯ " line fenced between two horizontal rules —
//     is a layout feature, not a message. Text in it is input a human typed
//     and has not submitted, which is the §2.4 refusal case.
//   - the selection footer ("Enter to select · Tab/Arrow keys to navigate")
//     is emitted by a menu widget and means the session is blocked on a
//     human choosing, which is unambiguously waiting_input.
//   - process liveness comes from the OS, not the screen.

// SGR 2 is "dim/faint". The TUI renders the composer's PLACEHOLDER hint dim
// and real typed input at normal intensity, which is the only reliable way to
// tell them apart.
//
// Matching the hint's words instead would repeat the spinner-verb mistake:
// prose in an interface with no compatibility contract. Intensity is a
// rendering attribute the TUI must use for the hint to look like a hint.
const (
	sgrDimOn  = "\x1b[2m"
	sgrReset  = "\x1b[0m"
	sgrEscape = '\x1b'
)

const (
	// composerRuneMarker begins the composer (input) line of the TUI.
	composerRuneMarker = "❯"
	// ruleRune is the box-drawing character the TUI fences the composer
	// with. Matching a run of them, rather than an exact width, keeps this
	// independent of terminal width.
	ruleRune = '─'
	// Menu footers. There is more than one, which cost a real incident: the
	// detector knew only "Enter to select", so a folder-trust prompt and a
	// session-resume prompt — both saying "Enter to confirm" — classified as
	// unknown instead of waiting_input. A supervisor cannot tell "blocked on
	// a question" from "I cannot read this screen", so it waits forever on
	// something that will never move by itself.
	selectFooter  = "Enter to select"
	confirmFooter = "Enter to confirm"
	// spinnerRune prefixes the status line in both its running and
	// finished forms.
	spinnerRune = "✻"
	// runningSuffixMarker distinguishes a running spinner ("… (5m 57s · ↓
	// 21.3k tokens)") from a finished one ("for 2m 7s"). The ellipsis is
	// a single rune in the TUI's output, not three periods.
	runningSuffixMarker = "…"
	// finishedInfix is the finished spinner's shape: "<Verb> for <duration>".
	finishedInfix = " for "
)

// screen is a pane's captured text, split into non-empty trailing lines
// with trailing whitespace removed. The classifier only ever looks at the
// tail: scrollback above the composer is transcript, and transcript is
// whatever the agent chose to print — including, potentially, text designed
// to look like the TUI's own chrome.
type screen struct {
	lines []string
	// raw keeps the escape sequences, because one signal cannot be read
	// from the text alone: the composer's placeholder is rendered dim.
	raw []string
}

func newScreen(raw string) screen {
	all := strings.Split(raw, "\n")
	out := make([]string, 0, len(all))
	raws := make([]string, 0, len(all))
	for _, l := range all {
		raws = append(raws, l)
		out = append(out, strings.TrimRight(stripSGR(l), " \t\r"))
	}
	// Drop trailing blank lines: capture-pane pads to the pane height.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
		raws = raws[:len(raws)-1]
	}
	return screen{lines: out, raw: raws}
}

// stripSGR removes ANSI select-graphic-rendition sequences so text matching
// works on what a human sees rather than on how it is painted.
func stripSGR(s string) string {
	if !strings.ContainsRune(s, sgrEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == sgrEscape && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' && s[j] != 'K' && s[j] != 'H' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// allDim reports whether every visible character of a raw fragment is
// rendered dim — the signature of a placeholder rather than typed input.
//
// The caller passes the text AFTER the prompt marker, not the whole line: the
// marker is painted at normal intensity even when the hint beside it is dim,
// so including it makes every placeholder look partly real. That detail cost a
// test, and would have cost the fix.
//
// Returns false for a fragment with no escape sequences: plain text is normal
// intensity, which is what real input looks like.
func allDim(raw string) bool {
	if !strings.Contains(raw, sgrDimOn) {
		return false
	}
	var visible, dim int
	depth := 0
	rs := []rune(raw)
	for i := 0; i < len(rs); {
		if rs[i] == sgrEscape && i+1 < len(rs) && rs[i+1] == '[' {
			j := i + 2
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				switch string(rs[i : j+1]) {
				case sgrDimOn:
					depth++
				case sgrReset:
					depth = 0
				}
				i = j + 1
				continue
			}
		}
		if !unicode.IsSpace(rs[i]) {
			visible++
			if depth > 0 {
				dim++
			}
		}
		i++
	}
	return visible > 0 && dim == visible
}

// afterMarker returns the raw fragment following the composer prompt glyph,
// escapes intact.
func afterMarker(raw string) string {
	if i := strings.Index(raw, composerRuneMarker); i >= 0 {
		return raw[i+len(composerRuneMarker):]
	}
	return raw
}

// isRule reports whether a line is one of the TUI's horizontal rules. The
// rule may carry a centred label (the session name), so this checks that
// the line is made predominantly of the box-drawing rune rather than
// requiring it to be uniform.
func isRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	rules := 0
	for _, r := range trimmed {
		if r == ruleRune {
			rules++
		}
	}
	// A small absolute count, and nothing proportional.
	//
	// Both stricter rules were tried and both failed on real screens. An
	// absolute width (eight) missed the ONE fence that carries content: the
	// opening rule is labelled with the session name, so a long name in a
	// narrow pane leaves few rule characters. Requiring rule characters to
	// outnumber the label failed for the same reason — the label is often
	// longer than the dashes around it.
	//
	// Failing to recognise a fence is the damaging direction. It means the
	// composer is not fenced, so unsent input goes undetected and gets
	// concatenated into a message a human was still typing — invisible when
	// it happens. Over-recognising merely refuses a send, which is visible
	// and recoverable.
	//
	// This character is box-drawing (U+2500); prose does not contain it, and
	// an em dash is a different rune entirely. Transcript tables do contain
	// it, which is harmless: the composer is anchored from the bottom of the
	// screen, and a table is always above it.
	return rules >= 3
}

// composerText returns the text a human has typed into the input box but
// not submitted, and whether a composer was found at all.
//
// The composer is identified structurally: a line beginning with the prompt
// marker that sits between two horizontal rules. Finding it by structure
// rather than by "the line starts with ❯" matters, because the transcript
// above also contains ❯-prefixed lines — they are how the TUI echoes
// commands the human already ran (see the /remote-control fixture). Those
// are history. Only the fenced one is live input.
func composerText(s screen) (string, bool) {
	// Walk back to the closing rule.
	last := -1
	for i := len(s.lines) - 1; i >= 0; i-- {
		if isRule(s.lines[i]) {
			last = i
			break
		}
	}
	if last <= 0 {
		return "", false
	}

	// Find a prompt-marked line above it, stopping at another rule.
	prompt := -1
	for i := last - 1; i >= 0; i-- {
		if isRule(s.lines[i]) {
			return "", false // opening rule reached with no composer between
		}
		if strings.HasPrefix(strings.TrimSpace(s.lines[i]), composerRuneMarker) {
			prompt = i
			break
		}
	}
	if prompt < 0 {
		return "", false
	}

	// THE COMPOSER MUST BE FENCED, and this check is the whole reason this
	// function is not two lines long.
	//
	// The prompt glyph is not unique to the composer: the TUI marks the
	// SELECTED ITEM of a menu with the same character. A menu therefore
	// presents a ❯-prefixed line above a rule, which is indistinguishable
	// from a composer unless the opening fence is also required.
	//
	// Observed on a live session: a selection menu whose highlighted option
	// was read as pending input. Every send to that session would have been
	// refused, forever, with a reason naming input a human never typed —
	// a session stopped by text that was never there.
	//
	// So: the first non-blank line above the prompt must be a rule. A menu's
	// preceding line is menu text and fails this; a real composer's is its
	// opening fence.
	fenced := false
	for i := prompt - 1; i >= 0; i-- {
		if strings.TrimSpace(s.lines[i]) == "" {
			continue
		}
		fenced = isRule(s.lines[i])
		break
	}
	if !fenced {
		return "", false
	}

	// A wholly dim composer is the TUI's placeholder hint, not something a
	// human typed. Treating it as pending input refuses every send to a
	// FRESH session, forever — a session the supervisor can never speak to,
	// stuck for text nobody wrote. Observed live on a newly created session.
	if prompt < len(s.raw) && allDim(afterMarker(s.raw[prompt])) {
		return "", true
	}

	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.lines[prompt]), composerRuneMarker))
	// Continuation lines: a long message wraps below the prompt and is still
	// unsent input. Reading only the first line would under-report it, and
	// under-reporting is the direction that corrupts a human's message.
	for i := prompt + 1; i < last; i++ {
		if i < len(s.raw) && allDim(s.raw[i]) {
			continue
		}
		if seg := strings.TrimSpace(s.lines[i]); seg != "" {
			text += " " + seg
		}
	}
	return strings.TrimSpace(text), true
}

// awaitingSelection reports whether the TUI is showing a menu that blocks
// on a human keypress.
func awaitingSelection(s screen) bool {
	_, blocked := selectionPrompt(s)
	return blocked
}

// selectionPrompt reports whether a blocking prompt is on screen and, if so,
// what it is asking — the highlighted option, so a supervisor reading the
// state knows what it would be agreeing to.
//
// Both footers are matched. Knowing only one was a real incident: a
// folder-trust prompt and a session-resume prompt both say "Enter to confirm",
// and both classified as unknown, which reads as "cannot determine" rather
// than "blocked on a human".
func selectionPrompt(s screen) (string, bool) {
	found := false
	for i := len(s.lines) - 1; i >= 0 && i > len(s.lines)-6; i-- {
		if strings.Contains(s.lines[i], selectFooter) || strings.Contains(s.lines[i], confirmFooter) {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	// The highlighted option carries the prompt marker.
	for i := len(s.lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(s.lines[i])
		if strings.HasPrefix(line, composerRuneMarker) {
			return strings.TrimSpace(strings.TrimPrefix(line, composerRuneMarker)), true
		}
	}
	return "", true
}

// spinner reports the most recent status line and whether it indicates a
// turn still in progress. Second return is false when no spinner line was
// found at all, which is a different fact from "found, and it was
// finished".
func spinner(s screen) (running bool, found bool) {
	for i := len(s.lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(s.lines[i])
		if !strings.HasPrefix(line, spinnerRune) {
			continue
		}
		if strings.Contains(line, runningSuffixMarker) {
			return true, true
		}
		if strings.Contains(line, finishedInfix) {
			return false, true
		}
		// A spinner line in neither shape: the TUI changed, or this is
		// transcript text that happens to start with the rune. Report
		// "found nothing usable" rather than guessing a tense.
		return false, false
	}
	return false, false
}

// classifyPane is what callers use. It separates "the driver failed to read
// this pane" from "the driver read this pane and it was empty" — two facts
// that a single classify(text, alive) signature collapses into one
// indistinguishable `unknown`.
//
// That collapse is not hypothetical. A marker-corruption bug in the batched
// enumeration once caused every capture to be misfiled, so every session
// received an empty string, and every session classified as `unknown` with
// the evidence "pane captured empty". The driver reported a plausible fleet
// view, returned no error, and passed every unit test — because a driver
// that cannot read any screen at all is, at this signature, shaped exactly
// like a fleet of sessions that happen to be unreadable.
//
// This is §5.7 ("absence and failure are different answers") applied one
// level below where the spec states it. The spec makes the distinction for
// plural responses across machines; the same confusion is available inside
// a single driver, between a pane it failed to read and a pane with nothing
// in it, and it is just as capable of producing a confident wrong answer.
func classifyPane(raw string, captured, alive bool) fleet.SessionState {
	if !captured {
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"driver failed to capture this pane's screen; this is a driver "+
				"malfunction, not an observation about the session")
	}
	return classify(raw, alive)
}

// classify infers a session's state from its pane text and process
// liveness.
//
// alive is the only input here that is not a guess; it comes from the
// process table. Everything else is screen-reading, and the returned
// SessionState says so.
func classify(raw string, alive bool) fleet.SessionState {
	if !alive {
		// §8: dead is terminal. This is the one status this driver can
		// state without reading a screen, and still it is inferred: the
		// process being gone is observed, but "this session is dead"
		// infers that the process was the session.
		return fleet.InferredState(fleet.StatusDead, "pane process not present in process table", nil)
	}

	s := newScreen(raw)
	if len(s.lines) == 0 {
		return fleet.UnknownState(fleet.ConfidenceInferred, "pane captured empty")
	}

	if option, blocked := selectionPrompt(s); blocked {
		evidence := "blocked on a prompt awaiting a keypress"
		if option != "" {
			// Name what it is asking. A supervisor deciding whether to
			// accept the default needs to know what the default IS.
			evidence += "; highlighted option: " + option
		}
		return fleet.InferredState(fleet.StatusWaitingInput, evidence, nil)
	}

	running, foundSpinner := spinner(s)

	// Unsent composer text is checked after the spinner, because a running
	// turn with queued input is still working — the queued text is a send
	// hazard (§2.4), not a state.
	pending, hasComposer := composerText(s)

	switch {
	case foundSpinner && running:
		return fleet.InferredState(fleet.StatusWorking, "spinner line in running form", nil)

	case foundSpinner && !running && hasComposer && pending != "":
		// Turn finished, and a human has typed something they have not
		// sent. The session is not working and not merely idle: it is
		// holding input. waiting_input is the honest §2.3 member — the
		// session is blocked on a human (to press enter).
		return fleet.InferredState(fleet.StatusWaitingInput,
			"turn finished; composer holds unsent input", nil)

	case foundSpinner && !running:
		return fleet.InferredState(fleet.StatusIdle, "spinner line in finished form; composer empty", nil)

	case !foundSpinner && hasComposer && pending == "":
		// No spinner at all and an empty composer: most likely a session
		// sitting at a fresh prompt. "Most likely" is not good enough to
		// claim idle over working — a turn that has just begun may not
		// have painted a spinner yet.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer present and empty")

	case !foundSpinner && hasComposer && pending != "":
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer holds unsent input")

	default:
		// No composer found: this pane is probably not running the TUI at
		// all — a plain shell, a pager, an editor. The driver has no
		// business guessing what that means.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no TUI composer found in pane; pane may not be running the expected runtime")
	}
}
