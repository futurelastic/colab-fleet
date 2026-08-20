package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// oscBEL is the older of the two OSC terminators. Both are in use on this
	// substrate, so both are recognised — see stripEscapes.
	oscBEL = '\x07'
)

const (
	// composerRuneMarker begins the composer (input) line of the TUI.
	composerRuneMarker = "❯"
	// ruleRune is the box-drawing character the TUI fences the composer
	// with. Matching a run of them, rather than an exact width, keeps this
	// independent of terminal width.
	ruleRune = '─'
	// promptScanDepth is how far up from the bottom a prompt may reach. A
	// numbered list further up is transcript, not a question.
	promptScanDepth = 24

	// maxPromptOptions bounds how many options a prompt may enumerate. See
	// parsePrompt: the index comes from the screen, and the screen is
	// attacker-influenced in the general case.
	maxPromptOptions = 32

	// Menu footers. There is more than one, which cost a real incident: the
	// detector knew only "Enter to select", so a folder-trust prompt and a
	// session-resume prompt — both saying "Enter to confirm" — classified as
	// unknown instead of waiting_input. A supervisor cannot tell "blocked on
	// a question" from "I cannot read this screen", so it waits forever on
	// something that will never move by itself.
	selectFooter  = "Enter to select"
	confirmFooter = "Enter to confirm"
	// A tool-permission dialog uses neither of the above. Four footers on one
	// runtime is why detection is structural first and footer second.
	amendFooter = "Tab to amend"
	// spinnerRune is ONE of the glyphs that prefix the status line — see
	// hasSpinnerGlyph for why matching this exact character was a bug. Kept
	// as a fixture anchor for tests, not used for detection.
	spinnerRune = "✻"
	// runningSuffixMarker distinguishes a running spinner ("… (5m 57s · ↓
	// 21.3k tokens)") from a finished one ("for 2m 7s"). The ellipsis is
	// a single rune in the TUI's output, not three periods.
	runningSuffixMarker = "…"
	// finishedInfix is the finished spinner's shape: "<Verb> for <duration>".
	finishedInfix = " for "
	// responseBullet marks a line the AGENT produced. Chrome does not carry
	// it, which makes it the divider between "the runtime said this just now"
	// and "the agent has since carried on".
	responseBullet = "⏺"
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
		out = append(out, strings.TrimRight(stripEscapes(l), " \t\r"))
	}
	// Drop trailing blank lines: capture-pane pads to the pane height.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
		raws = raws[:len(raws)-1]
	}
	return screen{lines: out, raw: raws}
}

// stripEscapes removes the ANSI sequences a pane carries so text matching works
// on what a human sees rather than on how it is painted.
//
// Two families, because both were found on one screen:
//
//   - CSI (`ESC [ … m/K/H`) — colour and cursor movement. Matching without
//     removing these was the original reason this function exists.
//
//   - OSC (`ESC ] … BEL` or `ESC ] … ESC \`) — chiefly the hyperlink the
//     runtime wraps around link text. This one was NOT removed, and unlike a
//     colour code it does not merely disturb matching: the surviving bytes are
//     PUBLISHED. Measured on a live fleet, a folder-trust prompt's question
//     reached a client as
//
//     …only proceed if you trust this configuration. \x1b]8;id=…;https://…\x1b\\Security guide\x1b]8;;\x1b\\
//
//     A client rendering that shows escape debris to a human at exactly the
//     moment it is asking them to make a security decision.
//
// Both are consumed only when TERMINATED. A capture can cut a sequence at the
// right edge of the pane, and a scanner that swallowed to end-of-line on an
// unterminated one would eat visible text — silently, and the text it would eat
// is the text being classified.
func stripEscapes(s string) string {
	if !strings.ContainsRune(s, sgrEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == sgrEscape && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				j := i + 2
				for j < len(s) && s[j] != 'm' && s[j] != 'K' && s[j] != 'H' {
					j++
				}
				if j < len(s) {
					i = j + 1
					continue
				}
			case ']':
				// OSC ends at BEL, or at ST (`ESC \`).
				if j, ok := oscEnd(s, i+2); ok {
					i = j
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// oscEnd finds the index just past an OSC terminator, or reports that the
// sequence is unterminated in this fragment.
func oscEnd(s string, from int) (int, bool) {
	for j := from; j < len(s); j++ {
		if s[j] == oscBEL {
			return j + 1, true
		}
		if s[j] == sgrEscape && j+1 < len(s) && s[j+1] == '\\' {
			return j + 2, true
		}
	}
	return 0, false
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

// parsePrompt extracts the full question a session is blocked on: every
// option in order, which one is highlighted, and a nonce over the whole thing.
//
// Options are recognised by their leading "N." rather than by any wording, so
// a new prompt from a future release is enumerated without a new matcher —
// which is the failure mode a sibling project named explicitly: "chasing them
// individually means a new matcher every time the CLI adds a screen".
func parsePrompt(s screen) *fleet.SessionPrompt {
	// Detection is STRUCTURAL, not footer-based.
	//
	// Four footers have been seen on one runtime — "Enter to select · Tab/Arrow
	// keys to navigate", "Enter to confirm · Esc to cancel", and a tool-permission
	// dialog reading "Esc to cancel · Tab to amend" — and matching them meant a
	// new matcher for every screen the runtime adds, which is how this class of
	// stall stays permanently one release behind.
	//
	// What every one of them has is the thing being asked: a run of numbered
	// options near the bottom of the screen with exactly one marked as
	// highlighted. That is the question, and it is what a caller has to answer.
	p := &fleet.SessionPrompt{}
	footer := false
	var question []string
	from := 0
	if len(s.lines) > promptScanDepth {
		from = len(s.lines) - promptScanDepth
	}
	for _, raw := range s.lines[from:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		selected := strings.HasPrefix(line, composerRuneMarker)
		body := strings.TrimSpace(strings.TrimPrefix(line, composerRuneMarker))
		if n, text, ok := numberedOption(body); ok {
			if n > maxPromptOptions {
				// Bounded on purpose. This parses a pane whose contents are
				// written by an agent that can print anything, and padding
				// a slice up to a parsed index is unbounded allocation
				// driven by untrusted input: one transcript line reading
				// "1000000. x" hung the service until it was killed.
				//
				// A menu with more options than this is not a menu.
				continue
			}
			for len(p.Options) < n-1 {
				// A gap means a line did not parse; keep indices honest
				// rather than silently renumbering what a caller will
				// submit by index.
				p.Options = append(p.Options, "")
			}
			if len(p.Options) == n-1 {
				p.Options = append(p.Options, text)
			} else if n-1 < len(p.Options) {
				p.Options[n-1] = text
			}
			if selected {
				p.Selected = n
			}
			continue
		}
		if strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) ||
			strings.Contains(line, amendFooter) {
			footer = true
			continue
		}
		if len(p.Options) == 0 && !isRule(line) {
			question = append(question, line)
		}
	}
	// Structure OR footer — neither alone is sufficient.
	//
	// Structure (two or more options with one highlighted) catches dialogs
	// whose footer nobody has seen before, which is the case that kept
	// costing a new matcher per release. It ALSO requires the run to have no
	// gap (see optionsAreContiguous) — a measured incident (#58) found the
	// gap-fill padding this loop uses for honesty (see the comment above)
	// doubling as a false-positive tell when nothing legitimate produced the
	// gap: ordinary transcript text happened to contain a highlighted-looking
	// line at index 2 with nothing at index 1, and the empty pad at index 1
	// was reported as a real option. No real menu ships an option with no
	// label, so a gap here means the "menu" is not one.
	//
	// Footer catches the case structure misses: a long menu whose highlighted
	// option has scrolled above the captured window. That happens on real
	// panes, and requiring the marker would classify a genuinely blocked
	// session as merely unreadable. It gaps in exactly the same shape and is
	// deliberately NOT held to the contiguity rule above — see
	// TestMenuWithScrolledOffMarkerStillCounts — because the footer is
	// itself the corroboration this branch has instead.
	structural := len(p.Options) >= 2 && p.Selected > 0 && optionsAreContiguous(p.Options)
	if !structural && !(footer && len(p.Options) >= 1) {
		return nil
	}
	// Keep the question short: the last couple of lines before the options
	// are the ask; everything above is transcript.
	if n := len(question); n > 3 {
		question = question[n-3:]
	}
	p.Question = strings.Join(question, " ")
	p.Nonce = promptNonce(p)
	return p
}

// numberedOption parses "1. Some option" into its index and text.
func numberedOption(line string) (int, string, bool) {
	i := 0
	// At most four digits: a longer run is not an option number, and
	// accumulating it would overflow before it was ever rejected.
	for i < len(line) && i < 4 && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != '.' {
		return 0, "", false
	}
	n := 0
	for _, c := range line[:i] {
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[i+1:]), true
}

// optionsAreContiguous reports whether a structural match's option run has
// no gap — see parsePrompt's own comment on why a gap is a stronger tell
// than any wording could be: the padding that keeps indices honest for a
// genuinely scrolled-off menu (the footer branch) is the same padding a
// coincidental structural false-positive produces, and a real menu never
// prints an option with no label.
func optionsAreContiguous(opts []string) bool {
	for _, o := range opts {
		if o == "" {
			return false
		}
	}
	return true
}

// promptFooterPresent reports whether one of the runtime's own menu footers
// is visible in the same window parsePrompt scans.
//
// A second, minimal scan rather than a second return value threaded through
// parsePrompt's dozen call sites: this is used in exactly one place — see
// classifyAgedDetail's companion-clause check below — to decide how much
// corroboration an accepted structural prompt already has, and that call
// site is the only one that needs the footer fact on its own, separate from
// whatever kind of prompt it turned out to be.
func promptFooterPresent(s screen) bool {
	from := 0
	if len(s.lines) > promptScanDepth {
		from = len(s.lines) - promptScanDepth
	}
	for _, raw := range s.lines[from:] {
		line := strings.TrimSpace(raw)
		if strings.Contains(line, selectFooter) || strings.Contains(line, confirmFooter) ||
			strings.Contains(line, amendFooter) {
			return true
		}
	}
	return false
}

// promptNonce is a digest of what is being asked. It changes whenever the
// question or the options change, which is what makes a stale answer
// detectable rather than silently applied to a different menu.
// usageLimit reports whether the LIVE BOTTOM of the screen says this session is
// blocked by a usage limit, and any reset time it states.
//
// # Why the live bottom, and not the screen
//
// After a resume the runtime re-renders the transcript, so an OLD limit notice
// scrolls past again. A matcher that searched the whole capture would report a
// session blocked by a limit it hit yesterday and has long since recovered
// from — an operational runbook for this fleet records exactly that trap
// ("judge by the live bottom of the pane"). So this looks only below the last
// composer fence, which is the region the runtime redraws.
//
// # Why prose is matched here at all
//
// Reluctantly, and bounded the same way classifyPromptKind is: there is no
// structural signal for "the account ran out of quota" — no spinner, no menu,
// no dialog chrome. The screen simply says so in words. Matched loosely on the
// two shapes seen in the wild, and failing to "no limit" rather than to a
// guess.
func usageLimit(s screen) (resetHint string, blocked bool) {
	// The live region is the last few transcript lines ABOVE the composer, not
	// below it: the composer and its fences are the bottom of the screen, and
	// the runtime prints its notices just before them.
	end := len(s.lines)
	for i := len(s.lines) - 1; i >= 0 && i >= len(s.lines)-promptScanDepth; i-- {
		if strings.Contains(s.lines[i], composerRuneMarker) {
			// walk up past the opening fence
			end = i
			for end > 0 && isRule(s.lines[end-1]) {
				end--
			}
			break
		}
	}
	// The notice must be the last thing the AGENT printed — not literally the
	// last line, which was too strict and missed the real shape.
	//
	// Measured on a blocked machine: the runtime prints the notice, then its
	// own continuation line, then settles its status line. So the notice is
	// never last, and a one-line rule saw nothing on a fleet that had been
	// refusing work for days. The identical shape was already recorded for a
	// failed turn — error first, status line after — and this function was
	// written before that lesson was applied here.
	//
	// What still separates a live block from a replayed one is what comes
	// AFTER: a session that carried on has agent output below the notice, and
	// agent output is marked by the runtime's own response bullet. Chrome —
	// the status line, continuation lines, blanks — is not. So scan a window,
	// and disqualify the notice if the agent said anything after it.
	const liveTail = 8
	var found bool
	var hint string
	seen := 0
	for i := end - 1; i >= 0 && seen < liveTail; i-- {
		line := strings.ToLower(strings.TrimSpace(s.lines[i]))
		if line == "" || isRule(s.lines[i]) {
			continue
		}
		seen++
		// The runtime's response bullet means the agent produced output after
		// whatever is above it — so anything found from here up is history.
		if strings.HasPrefix(line, responseBullet) {
			return "", false
		}
		hit := (strings.Contains(line, "limit") &&
			(strings.Contains(line, "hit your") || strings.Contains(line, "reached") ||
				strings.Contains(line, "usage"))) ||
			strings.Contains(line, "/usage-credits")
		if !hit {
			continue
		}
		// A reset time when the screen offers one — the number an operator
		// actually needs, and the difference between "wait" and "switch".
		//
		// Keep scanning after a hit that carries no time. The notice spans
		// more than one line ("…weekly limit · resets Aug 10" then
		// "/usage-credits to finish…"), and scanning upward meets the
		// continuation FIRST — so returning on the first hit found the block
		// and threw the reset time away, which is the one detail an operator
		// wants.
		found = true
		if hint != "" {
			continue
		}
		if h, ok := resetHintIn(line); ok {
			hint = h
		}
	}
	return hint, found
}

// resetHintIn looks for the runtime's own words about when a limit lifts, in
// one already-lowercased line of prose.
//
// Factored out so the same rule applies whether the prose comes from a live
// screen line (usageLimit) or from the runtime's own durable record
// (latestAPIError, #56) — both are the runtime's words, and a caller must not
// be able to tell which source produced a given hint from the rule that
// extracted it.
func resetHintIn(line string) (hint string, ok bool) {
	for _, marker := range []string{"resets ", "try again ", "available again "} {
		if k := strings.Index(line, marker); k >= 0 {
			rest := strings.TrimSpace(line[k+len(marker):])
			if len(rest) > 40 {
				rest = rest[:40]
			}
			return rest, true
		}
	}
	return "", false
}

// retryableWords reports whether already-lowercased prose says a failure is
// temporary, the same test lastTurnFailed and latestAPIError (#56) both make
// — from the runtime's own words, never from a status code decided here (see
// lastTurnFailed's own comment for why that distinction matters).
func retryableWords(lower string) bool {
	return strings.Contains(lower, "temporary") || strings.Contains(lower, "try again")
}

// trimToSentence keeps prose to roughly one sentence: the rest is usually a
// support URL or advice a human does not need repeated in every listing.
// Shared by lastTurnFailed (screen) and latestAPIError (record, #56) so a
// caller sees the same shape of Reason regardless of source.
func trimToSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if cut := strings.IndexAny(s, ".\n"); cut > 0 {
		s = s[:cut]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// lastTurnFailed reports whether the live region shows the most recent turn
// ending in an error, and what the runtime said about it.
//
// # Why a window here, and exactly one line for the usage limit
//
// usageLimit demands the notice be the LAST live line, because a limit notice
// with work beneath it is history. This one cannot: the runtime prints the
// error and THEN settles its status line, so the error is never last. The
// window is a few lines, and the same replay hazard is handled differently —
// a session that errored and then carried on has that later work in the
// window, pushing the error out of it.
//
// # What "retryable" means, and where it comes from
//
// From the screen, not from us. The runtime says "usually temporary — try
// again in a moment" when it believes the failure is transient, and that
// sentence is the difference between a supervisor poking the session and a
// human being called. Inferring it from a status code instead would mean
// deciding, here, which of somebody else's error codes are worth retrying.
func lastTurnFailed(s screen) (*fleet.TurnEnd, bool) {
	end := len(s.lines)
	for i := len(s.lines) - 1; i >= 0 && i >= len(s.lines)-promptScanDepth; i-- {
		if strings.Contains(s.lines[i], composerRuneMarker) {
			end = i
			for end > 0 && isRule(s.lines[end-1]) {
				end--
			}
			break
		}
	}
	const window = 6
	var seen []string
	for i := end - 1; i >= 0 && len(seen) < window; i-- {
		line := strings.TrimSpace(s.lines[i])
		if line == "" || isRule(s.lines[i]) {
			continue
		}
		seen = append([]string{line}, seen...)
	}
	joined := strings.Join(seen, " ")
	lower := strings.ToLower(joined)

	// Matched on the runtime's error banner rather than on any status code:
	// the banner is what the runtime prints when a turn dies, and the code is
	// an implementation detail that has already changed shape once.
	marker := "api error"
	k := strings.Index(lower, marker)
	if k < 0 {
		return nil, false
	}
	reason := trimToSentence(joined[k:], 120)
	return &fleet.TurnEnd{
		Outcome:   "failed",
		Reason:    reason,
		Retryable: retryableWords(lower),
	}, true
}

// classifyPromptKind recognises which question a prompt is asking (§2.7's
// Kind), or returns empty when it does not recognise it.
//
// # This is prose matching, and that is the point
//
// Everything else in this file avoids matching the runtime's words. This
// function does the opposite deliberately, for one reason: the alternative is
// not "no prose matching" — it is prose matching in every client that wants to
// answer a prompt safely, which is the same fragility multiplied. Quarantining
// it here means it fails to EMPTY, and one place needs fixing when the runtime
// rewords a screen.
//
// # It reads the OPTIONS only. Never the question.
//
// This rule was learned in production, twenty minutes after the first version
// shipped. A ship-decision prompt was labelled bypass-permissions because the
// AGENT had written "No auth bypass" in its question text — and a client
// filtering on that kind would have auto-answered somebody's merge decision.
//
// The question is written by the agent. The options of a RUNTIME dialog are
// fixed strings the runtime emits. Matching agent-authored text is not merely
// fragile, it is injectable: an agent that writes "resume from summary" in its
// own question could have its decision auto-answered by a client that trusts
// the kind. Only the runtime's own option text is eligible.
//
// # And a question the agent asked is never a runtime dialog
//
// When an agent asks its own question, the runtime appends affordances no
// runtime dialog has — an escape hatch to type freely, or to chat instead.
// Their presence is a structural tell that this prompt belongs to the agent,
// and it disqualifies classification outright rather than being weighed.
func classifyPromptKind(p *fleet.SessionPrompt) fleet.PromptKind {
	if p == nil || len(p.Options) == 0 {
		return ""
	}
	lower := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		lower = append(lower, strings.ToLower(o))
	}
	// Agent-authored question → not a runtime dialog, whatever it resembles.
	for _, o := range lower {
		if strings.HasPrefix(o, "type something") || strings.HasPrefix(o, "chat about") {
			return ""
		}
	}
	// All needles must appear in ONE option. Spreading them across the set
	// is how unrelated words combine into a false match.
	hasOption := func(needles ...string) bool {
		for _, o := range lower {
			all := true
			for _, n := range needles {
				if !strings.Contains(o, n) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}
	switch {
	case hasOption("resume", "summary"):
		return fleet.PromptResumeChooser
	case hasOption("trust", "folder"):
		return fleet.PromptFolderTrust
	case hasOption("trust", "settings"):
		return fleet.PromptSettingsTrust
	case hasOption("don't ask again"), hasOption("allow this"):
		return fleet.PromptToolPermission
	}
	// Nothing here matches the permission-mode ACCEPTANCE screen, and that is a
	// finding rather than an omission.
	//
	// A rule for it existed and could never fire. It required "bypass" and
	// "permissions" to appear in one option, and the runtime's binary — read
	// directly, rather than sampled from a capture — contains no such option.
	// The complete set of boot-screen options it ships is:
	//
	//	Yes, I trust this folder        No, continue without these permissions
	//	Yes, I trust these settings     No, exit Claude Code
	//	Yes, I accept                   No, exit
	//
	// (Read from one installed build; this repository's README pins the span
	// it is tested against, and that build sits outside it. A fact read from one
	// build is evidence about that build, not a guarantee across the span.)
	//
	// The words that identify that screen — "Bypass Permissions mode" — are in
	// its QUESTION. Its options are generic, and exactly one other screen's
	// negative opens with the identical words — "No, exit Claude Code" — so a
	// rule keyed on that phrase would not even isolate this screen alone.
	//
	// So it cannot be classified here without reading the question, and the
	// question is exactly what this function must not read: it is written by the
	// agent, and a ship decision was once labelled with this very kind because
	// the AGENT had typed "No auth bypass" into its own prompt. A rule that
	// matched a generic "accept" would be worse still — it would put a kind on
	// screens this driver has never seen, and a client filtering on kind before
	// auto-answering would then answer them.
	//
	// The consent path reaches that screen a different way, and the difference
	// is the whole point: the driver knows it started the session in that mode,
	// so it identifies the screen by what it DID rather than by what the screen
	// says. See consentableKinds and settleNewSession. Provenance is available
	// there and is not available here, which is why the answer differs by
	// caller rather than by wording.
	return ""
}

func promptNonce(p *fleet.SessionPrompt) string {
	h := sha256.New()
	h.Write([]byte(p.Question))
	for _, o := range p.Options {
		h.Write([]byte{0})
		h.Write([]byte(o))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
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
	if p := parsePrompt(s); p != nil {
		if p.Selected > 0 && p.Selected <= len(p.Options) {
			return strconv.Itoa(p.Selected) + ". " + p.Options[p.Selected-1], true
		}
		return "", true
	}
	return "", false
}

// awaitingSelection reports whether the TUI is showing a menu that blocks
// on a human keypress.

// spinner reports the most recent status line and whether it indicates a
// turn still in progress. Second return is false when no spinner line was
// found at all, which is a different fact from "found, and it was
// finished".
func spinner(s screen) (running bool, found bool) {
	// Scan upward for the LAST line that is a status line in one of its two
	// shapes, rather than stopping at the first line that merely begins with
	// a symbol.
	//
	// The earlier version did stop at the first, which was safe only because
	// it matched exactly one glyph. Widening the glyph test (see
	// hasSpinnerGlyph) immediately broke it: the composer's own `❯` is below
	// the status line and begins with a symbol too, so every screen "found" a
	// spinner line in neither shape and gave up. Chrome is full of symbols —
	// `❯`, `⏵⏵`, `▸`, `⎿` — and a matcher loose enough to survive an
	// animation frame must not treat the first symbol it meets as decisive.
	for i := len(s.lines) - 1; i >= 0; i-- {
		if running, ok := statusLine(strings.TrimSpace(s.lines[i])); ok {
			return running, true
		}
	}
	return false, false
}

// statusLine reports whether a line is the TUI's turn-status line, and whether
// that status is running.
//
// Three conditions, all structural, none of them a particular character:
//
//   - it begins with a single non-ASCII symbol followed by a space — the
//     status line's shape, and the part that varies (five animation frames
//     were live at once on one machine);
//   - the next word is capitalised, which is what the TUI's verb always is and
//     what tool-output lines below the transcript generally are not;
//   - it carries one of the two tense markers, `…` for running and " for " for
//     finished.
//
// The verb itself is deliberately not matched. It is drawn at random from a
// large set, and enumerating it would be the same mistake as enumerating
// footers (F37) or glyphs (F42).
func statusLine(line string) (running bool, ok bool) {
	if !hasSpinnerGlyph(line) {
		return false, false
	}
	_, size := utf8.DecodeRuneInString(line)
	rest := strings.TrimLeft(line[size:], " ")
	first, _ := utf8.DecodeRuneInString(rest)
	if !unicode.IsUpper(first) {
		return false, false
	}
	switch {
	case strings.Contains(rest, runningSuffixMarker):
		return true, true
	case strings.Contains(rest, finishedInfix):
		return false, true
	}
	return false, false
}

// hasSpinnerGlyph reports whether a line begins with the status line's leading
// symbol.
//
// # Why this is a shape and not a character
//
// It was one character — `✻` — and that was wrong in a way nothing reported.
// The glyph is an ANIMATION FRAME: a live fleet was using five of them
// (`✻ ✽ ✢ ✶ ✳`) at the same instant, so a session's status line was
// legible or invisible depending on which frame the capture happened to catch.
//
// The consequence was not a cosmetic miss. A session 21 minutes into a turn,
// with a perfectly good running spinner on screen, fell through to "no spinner
// line" and was reported `unknown` — 16% of one machine's sessions, entirely
// at random, refreshing every few hundred milliseconds.
//
// F37 said it about footers and it is true here: **match what the line is FOR,
// not how it is decorated.** What the line is for is announcing a turn, and
// the parts that carry that meaning are the ellipsis and the "for <duration>"
// — both already matched below. The leading glyph only has to be recognised as
// "a symbol, not text", which is a property of the character class rather than
// of any particular character.
func hasSpinnerGlyph(line string) bool {
	if line == "" {
		return false
	}
	r, size := utf8.DecodeRuneInString(line)
	if r == utf8.RuneError {
		return false
	}
	// Must be a symbol or punctuation glyph, not a letter or digit: this is
	// the part that keeps ordinary transcript prose from matching.
	if !unicode.IsSymbol(r) && !unicode.IsPunct(r) {
		return false
	}
	// ASCII punctuation is excluded deliberately. Transcript lines routinely
	// begin with `-`, `*`, `>`, `#` and `|`, and admitting those would make
	// every bulleted list a candidate status line.
	if r < utf8.RuneSelf {
		return false
	}
	// Box drawing and block elements are chrome, never a status glyph.
	//
	// Found on a live session: the runtime's welcome screen draws a panel
	// whose rows begin with `│`, and its "what's new" list is full of
	// truncated lines ending in `…`. That is a non-ASCII symbol, a space, a
	// capitalised word and an ellipsis — every condition below — so the splash
	// screen classified as a RUNNING SPINNER, and a freshly started session
	// reported `working` forever while it sat idle at its own welcome panel.
	//
	// This is the cost of loosening the glyph test (F42) arriving in a second
	// place. The first was the composer's own `❯`, caught by tests; this one
	// needed a live session, because no fixture contained a splash screen.
	if (r >= 0x2500 && r <= 0x259F) || (r >= 0x25A0 && r <= 0x25FF) {
		return false
	}
	// A single glyph followed by a space, which is the status line's shape.
	// Box-drawing rules and other chrome fail this because they run.
	rest := line[size:]
	return strings.HasPrefix(rest, " ")
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
func classifyPaneAged(raw string, captured, alive, young bool) fleet.SessionState {
	st, _ := classifyPaneRemembering(raw, captured, alive, young, paneMemory{}, time.Time{})
	return st
}

// classifyPaneRemembering classifies a pane using what the driver saw of it
// last time, and returns the digest to remember for next time.
//
// A first sighting (prior.known == false) behaves exactly as before — which is
// the honest floor, since one capture genuinely cannot settle the ambiguity
// resolveAmbiguity settles.
func classifyPaneRemembering(raw string, captured, alive, young bool, prior paneMemory, now time.Time) (fleet.SessionState, string) {
	if !captured {
		// No digest is returned for a failed capture: remembering the
		// fingerprint of a screen we did not read would make the next
		// comparison agree with nothing, or worse, agree with an earlier
		// failure and call two failures a stable screen.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"driver failed to capture this pane's screen; this is a driver "+
				"malfunction, not an observation about the session"), ""
	}
	st, amb := classifyAgedDetail(raw, alive, young)
	digest := screenDigest(raw)
	return resolveAmbiguity(st, amb, prior, digest, now), digest
}

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
	return classifyAged(raw, alive, false)
}

// ambiguity names a classification that a single screen cannot settle but a
// second look at the same screen can.
//
// # Why this is a return value rather than a comment
//
// Two branches below refuse to commit because "no spinner" has two readings: a
// session sitting at a fresh prompt, and a turn that began so recently it has
// not painted one yet. From one capture those are identical, and guessing
// either way is how a stalled session reads as busy or a busy one gets
// interrupted.
//
// From two captures they are not identical at all. A turn that had just begun
// paints within a second; a screen that is byte-for-byte unchanged seconds
// later was not mid-anything. That evidence exists in the driver's memory
// already — §8's `since` is stamped from it — so the resolution is passive,
// costs no extra capture, and never touches the session (the same reasoning
// as F34).
//
// The alternative was string-matching the evidence text at the call site,
// which makes a prose field load-bearing. Naming the ambiguity keeps the
// question in the type.
//
// # This driver's fold, named (#54)
//
// Every case below is this classifier folding together the same two named
// facts, and it is worth saying so once rather than leaving it to be
// noticed. `classifyPaneRemembering`'s five positional inputs are, in fold
// terms, two SOURCES:
//
//   - the screen — this read's own capture, timestamped `now`. Everything
//     `classifyAgedDetail` computes (spinner, composer, prompt, usage limit,
//     turn outcome) comes from here, and it comes from here ALONE: this
//     driver never observes anything else (classify.go's package comment).
//   - the screen's own history — `prior` (`paneMemory`: a digest and the
//     time it was taken), this same pane's previous read. Not a second
//     signal in the sense #52 means it (a second driver, a runtime's own
//     record) — it is TIME applied to the one signal this driver has. That
//     is why every resolution below can only narrow toward less activity,
//     never invent a new fact the screen itself never carried.
//
// `ambNoSpinnerEmpty` and `ambNoSpinnerPending` are the screen reporting
// itself unable to decide, resolved by asking the history source whether
// the screen has held still long enough that "a turn just began" stops being
// plausible. `ambUnrecognisedPrompt`, added for #58, is the same two sources
// used for a different question: not "can the screen decide", but "has the
// screen's own candidate answer earned the confidence it was built with" —
// see resolveUnrecognisedPrompt.
//
// # The screen is upgrade-only — stated once, made explicit
//
// #54's rule, and every branch of resolveAmbiguity obeys it: a resolution
// may only ever move a classification toward a MORE SPECIFIC state working
// FROM less activity, never invent activity from silence — `ambNoSpinnerPending`
// promotes toward `waiting_input`, never toward `working`; a screen that
// CHANGED between observations is left unresolved rather than called
// `working`, because content moves for reasons other than a turn (§5.6,
// "degrade, never emulate"). `ambUnrecognisedPrompt` does not violate this:
// upgrade-only says nothing about withholding a promotion the screen
// reports it cannot itself corroborate, which is a second and independent
// rule (#58's companion clause) — see resolveUnrecognisedPrompt's own
// comment for why "upgrade-only" alone would not have prevented that
// incident.
type ambiguity int

const (
	ambNone ambiguity = iota
	// ambNoSpinnerEmpty: no spinner, composer painted and empty.
	ambNoSpinnerEmpty
	// ambNoSpinnerPending: no spinner, composer holds unsent text.
	ambNoSpinnerPending
	// ambUnrecognisedPrompt: a structural prompt match with no footer to
	// corroborate it, and a kind classifyPromptKind does not recognise —
	// exactly the shape that produced #58. See resolveUnrecognisedPrompt.
	ambUnrecognisedPrompt
)

// spinnerPaintGrace is how long a turn is allowed to have started without
// having painted a spinner. Generous by an order of magnitude: the cost of
// waiting is a slightly later answer, while the cost of being wrong is
// reporting a working session as idle.
const spinnerPaintGrace = 2 * time.Second

// screenDigest fingerprints a captured screen so two observations can be
// compared without keeping the text.
//
// Keeping the text would be the obvious implementation and is the wrong one:
// pane content is somebody's actual work, and a driver that retains it has
// turned a state cache into a transcript store.
func screenDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// resolveAmbiguity settles a classification using what the same pane looked
// like last time, or leaves it unknown when there is nothing to settle it
// with.
//
// It only ever resolves toward *less* activity, and only on evidence of
// stability. A screen that CHANGED between observations is deliberately left
// unknown rather than called working: content moves for reasons other than a
// turn (a redraw, a resize, a notification), and §5.6 says degrade rather than
// emulate. One direction here has evidence behind it; the other would be a
// guess wearing a status.
func resolveAmbiguity(st fleet.SessionState, amb ambiguity, prior paneMemory, digest string, now time.Time) fleet.SessionState {
	// ambUnrecognisedPrompt inverts the shape every other case here uses: st
	// already IS the confident candidate — see classifyAgedDetail — and what
	// this decides is whether it has EARNED that confidence yet, not what it
	// should become. Handled first and separately because the early return
	// just below assumes the opposite default (an unresolved st, returned
	// as-is when there is nothing to promote it with), which is exactly
	// backwards for a candidate that must be held DOWN, not left AS IS,
	// absent corroboration.
	if amb == ambUnrecognisedPrompt {
		return resolveUnrecognisedPrompt(st, prior, digest, now)
	}
	if amb == ambNone || !prior.known || prior.digest != digest {
		return st
	}
	stable := now.Sub(prior.at)
	if stable < spinnerPaintGrace {
		return st
	}
	age := stable.Round(time.Second).String()
	switch amb {
	case ambNoSpinnerEmpty:
		return fleet.InferredState(fleet.StatusIdle,
			"no spinner, and the screen is unchanged after "+age+
				" — a turn that had just begun would have painted one by now", nil)
	case ambNoSpinnerPending:
		// Unsent text on a screen that has stopped moving is the same
		// situation as the finished-turn case above: blocked on a human
		// pressing enter. §8's `since` then carries the age, which is the
		// discriminator between an operator mid-thought and a pane nobody
		// is coming back to.
		blocked := fleet.InferredState(fleet.StatusWaitingInput,
			"composer holds unsent input; screen unchanged after "+age, nil)
		blocked.WaitingOn = fleet.WaitingUnsentInput
		// Carried from the unresolved state, which computed it from the
		// composer text. NOT the screen digest this function was handed —
		// they fingerprint different things and only one is what discard
		// compares against.
		blocked.ComposerDigest = st.ComposerDigest
		return blocked
	}
	return st
}

// resolveUnrecognisedPrompt implements #58's companion clause to #54's rule:
// a promotion made on evidence the source itself reports as unrecognised is
// not a promotion, it is unknown — until something corroborates it.
//
// # Why "the screen is upgrade-only" did not already prevent the incident
//
// The promotion #58 measured was working (or idle) -> waiting_input, which
// is precisely the direction resolveAmbiguity's upgrade-only rule permits.
// What was missing was not a direction check but a SECOND, independent
// question: did the source that produced this candidate recognise what it
// saw. `classifyPromptKind` returning empty is that source's own admission
// that it did not — see its package-level doc: "fails to empty, never to a
// guess" — and classifyAgedDetail was discarding that admission before the
// verdict formed, exactly as #58 named it.
//
// # The corroboration is the fold's EXISTING second source, reused
//
// st already carries the full candidate — Prompt, WaitingOn, evidence — built
// as if trusted. This does not rebuild it: it is either released unchanged
// (corroborated) or replaced with an honest `unknown` (not yet). The
// corroboration reuses screen-history, the same source ambNoSpinnerEmpty and
// ambNoSpinnerPending already fold in: the same screen, held past
// spinnerPaintGrace. A false read built from ordinary transcript text moving
// past does not sit still that long — the text was never the same "menu"
// twice, because it was never a menu. A real dialog this classifier has not
// been taught a kind for yet does sit still, because it is genuinely
// blocking on a human. That asymmetry is what makes the wait a real test and
// not merely a delay.
//
// First sighting (prior.known == false) takes the same honest floor every
// other ambiguity here takes: one capture cannot corroborate itself.
func resolveUnrecognisedPrompt(st fleet.SessionState, prior paneMemory, digest string, now time.Time) fleet.SessionState {
	if prior.known && prior.digest == digest && now.Sub(prior.at) >= spinnerPaintGrace {
		return st
	}
	return fleet.UnknownState(fleet.ConfidenceInferred,
		"screen shows what looks like a selection prompt, but its kind was not "+
			"recognised and no runtime footer corroborates it; treated as "+
			"unrecognised evidence (#58) rather than a confident prompt until it "+
			"is seen unchanged")
}

// paneMemory is the screen-history fold source (#54): what the driver
// remembers about one pane from its previous observation — a digest and
// when it was taken, never the text itself (see screenDigest). Empty
// (known == false) is the first sighting, and every resolution above treats
// that the same way: nothing to corroborate against yet.
type paneMemory struct {
	known  bool
	digest string
	at     time.Time
}

// classifyAged adds whether the session is young enough to plausibly still be
// starting — see the default branch for why that distinction matters.
func classifyAged(raw string, alive, young bool) fleet.SessionState {
	st, _ := classifyAgedDetail(raw, alive, young)
	return st
}

func classifyAgedDetail(raw string, alive, young bool) (fleet.SessionState, ambiguity) {
	if !alive {
		// §8: dead is terminal. This is the one status this driver can
		// state without reading a screen, and still it is inferred: the
		// process being gone is observed, but "this session is dead"
		// infers that the process was the session.
		return fleet.InferredState(fleet.StatusDead, "pane process not present in process table", nil), ambNone
	}

	s := newScreen(raw)
	if len(s.lines) == 0 {
		return fleet.UnknownState(fleet.ConfidenceInferred, "pane captured empty"), ambNone
	}

	if option, blocked := selectionPrompt(s); blocked {
		evidence := "blocked on a prompt awaiting a keypress"
		if option != "" {
			// Name what it is asking. A supervisor deciding whether to
			// accept the default needs to know what the default IS.
			evidence += "; highlighted option: " + option
		}
		st := fleet.InferredState(fleet.StatusWaitingInput, evidence, nil)
		st.WaitingOn = fleet.WaitingPrompt
		st.Prompt = parsePrompt(s)
		if st.Prompt != nil {
			st.Prompt.Kind = classifyPromptKind(st.Prompt)
		}

		// Companion clause, #58: a structural-only match (no footer) whose
		// kind classifyPromptKind also would not name is exactly the shape
		// that wedged a healthy session — the classifier's own admission
		// that it could not classify what it saw, reported as a confident
		// answer anyway. That admission must survive into the verdict
		// rather than being discarded here, so this is not reported at full
		// confidence on a single read; see resolveUnrecognisedPrompt.
		//
		// A footer-corroborated match, or one classifyPromptKind DID name,
		// is unaffected — the footer, or the recognised kind, is already
		// independent corroboration and this driver has trusted either one
		// immediately since before #58.
		if st.Prompt != nil && st.Prompt.Kind == "" && !promptFooterPresent(s) {
			return st, ambUnrecognisedPrompt
		}
		return st, ambNone
	}

	// Checked before anything that could resolve to idle. A session blocked by
	// a usage limit paints exactly like a healthy one waiting for work — empty
	// composer, no spinner — and `idle` is the single status that means "send
	// it work", so this is the one misreading that actively causes harm.
	if hint, blocked := usageLimit(s); blocked {
		evidence := "blocked by a usage limit"
		if hint != "" {
			evidence += "; resets " + hint
		}
		// waiting_input, deliberately: the session is blocked and only a human
		// unblocks it (wait it out, or switch accounts). Prompt stays nil —
		// there is nothing to answer, and §2.7 is optional for exactly this
		// reason.
		// quota_blocked, which §2.3 has defined since the first commit as
		// "alive but refused by its provider" and §8 gives transitions for.
		// It was reported as waiting_input for several hours because nobody
		// checked the enum — see F52. waiting_input was always slightly false
		// here: the session is not blocked on a HUMAN, it is blocked on a
		// clock or an account, and no answer from a caller unblocks it.
		st := fleet.InferredState(fleet.StatusQuotaBlocked, evidence, nil)
		st.Quota = &fleet.QuotaBlock{ResetHint: hint}
		return st, ambNone
	}

	running, foundSpinner := spinner(s)

	// Unsent composer text is checked after the spinner, because a running
	// turn with queued input is still working — the queued text is a send
	// hazard (§2.4), not a state.
	pending, hasComposer := composerText(s)

	switch {
	case foundSpinner && running:
		return fleet.InferredState(fleet.StatusWorking, "spinner line in running form", nil), ambNone

	case foundSpinner && !running && hasComposer && pending != "":
		// Turn finished, and a human has typed something they have not
		// sent. The session is not working and not merely idle: it is
		// holding input. waiting_input is the honest §2.3 member — the
		// session is blocked on a human (to press enter).
		held := fleet.InferredState(fleet.StatusWaitingInput,
			"turn finished; composer holds unsent input", nil)
		held.WaitingOn = fleet.WaitingUnsentInput
		held.ComposerDigest = screenDigest(pending)
		return held, ambNone

	case foundSpinner && !running:
		// Idle is the honest status: the session is up and will take input.
		// But a turn that DIED here looks identical to one that finished, and
		// collapsing those two is how abandoned work goes unnoticed — so the
		// state carries a footnote when the screen says the last turn failed.
		st := fleet.InferredState(fleet.StatusIdle, "spinner line in finished form; composer empty", nil)
		if turn, failed := lastTurnFailed(s); failed {
			st.LastTurn = turn
			st.Evidence = "turn ended in an error; session is up and will accept input"
		}
		return st, ambNone

	case !foundSpinner && hasComposer && pending == "" && young:
		// A young session with a painted composer, no spinner and nothing
		// typed has not had a turn yet — so the "a turn may have just
		// begun" ambiguity below cannot apply to it. It is up and waiting
		// for work.
		//
		// This matters beyond tidiness: a caller asking "did this spawn
		// actually produce a running agent" needs an answer, and `unknown`
		// for a session whose interface is visibly painted is the reading
		// that let a dead spawn look the same as a healthy one.
		return fleet.InferredState(fleet.StatusIdle,
			"interface painted, composer empty, no turn yet", nil), ambNone

	case !foundSpinner && hasComposer && pending == "":
		// No spinner at all and an empty composer: most likely a session
		// sitting at a fresh prompt. "Most likely" is not good enough to
		// claim idle over working — a turn that has just begun may not
		// have painted a spinner yet.
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer present and empty"), ambNoSpinnerEmpty

	case !foundSpinner && hasComposer && pending != "":
		pendingState := fleet.UnknownState(fleet.ConfidenceInferred,
			"no spinner line; composer holds unsent input")
		// Computed HERE, where the composer text exists. The resolution path
		// below has only the screen digest, and publishing that instead made
		// the field useless: a caller quoted back a fingerprint of the whole
		// screen while discard compared the text, so a correct call could
		// never match.
		pendingState.ComposerDigest = screenDigest(pending)
		return pendingState, ambNoSpinnerPending

	default:
		// No composer found. Two very different situations share this
		// shape, and a caller needs them apart: a runtime still painting
		// its interface, and a pane that is not running the runtime at all.
		//
		// §8 has `starting` for the first and §2.3 has `unknown` for the
		// second, and returning `unknown` for both was why a spawn that
		// never got past a boot screen "read as healthy" — nothing could
		// distinguish "not up yet" from "cannot tell".
		//
		// Age is the discriminator, and it is honest: young means plausibly
		// still booting, old means something else is going on. The caller
		// gets the distinction; the confidence stays inferred.
		if young {
			return fleet.InferredState(fleet.StatusStarting,
				"no TUI composer yet; session is young enough to still be starting", nil), ambNone
		}
		return fleet.UnknownState(fleet.ConfidenceInferred,
			"no TUI composer found in pane; pane may not be running the expected runtime"), ambNone
	}
}
