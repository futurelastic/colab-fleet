package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	fleet "github.com/godx-jp/colab-fleet"
)

// The runtime's feedback-draft card (colab-fleet #215).
//
// When the agent drafts product feedback, the runtime paints a bordered card
// directly above the composer:
//
//	╭──────────────────────────────────────────────────────────────────────────────╮
//	│ ✻ Bug report drafted: <title>                                                │
//	│ │ <a few rows of the draft's details>                                        │
//	│ 1 to review · 2 to send · 0 to dismiss                                       │
//	╰──────────────────────────────────────────────────────────────────────────────╯
//
//	                                        new task? /clear to save 203.9k tokens
//	───────────────────────────────────────────────────── <session name> ─
//	❯
//
// # It is a NOTICE, not a dialog — and that decides everything below
//
// Read out of the runtime's own binary (one build): the card owns no focus. Its
// three keys are read through the composer's input value and act only while
// that value is empty, so a person can type into the composer under it, and a
// message pasted into the composer is delivered as it always was. Only a lone
// digit, or Escape on an empty composer, reaches the card.
//
// So it is not a wait in general. What it does is take rows. A fleet-created
// session is a 24-row pane (the multiplexer's detached default), and the card is
// about seven of them: the composer's closing rule and its mode row are pushed
// below the bottom of the pane, and composerSpan — which needs both fences —
// reports the composer ABSENT. The state read then said "idle, composer empty"
// from the finished-spinner branch while every send was refused with "no
// composer has been painted", a wording that blamed startup. Nothing named the
// card, and nothing could answer it.
//
// # So it is reported as a prompt only while it is what stands in the way
//
// When the composer IS readable (a taller pane) the session is genuinely idle
// and takes work, and reporting a prompt there would stop supervisors from
// sending to a session that would receive them. So liveFeedbackCard finds the
// card in either geometry, but only a card over a composer this driver cannot
// read as a whole is a prompt (feedbackCard.answerable) — the case the card
// makes unreachable, and the only one where "waiting on a person" is true from
// the caller's side.
//
// # Structure only
//
// The card's rows are written by the agent (the title and the details of its own
// draft), so nothing here matches their words. What is matched is the runtime's
// own key row, exactly, inside a validated box, directly above the live
// composer's fence. A transcript that prints the same words is indented under
// its own output and is nowhere near the fence.

// feedbackCardItem is one of the key row's three items: the key the runtime
// listens for, and the verb it prints beside it. The verbs are the prompt's
// options; the keys are how this substrate delivers an answer, so they live in
// menuShape.shortcuts and not in the options.
type feedbackCardItem struct{ key, verb string }

var feedbackCardItems = [...]feedbackCardItem{
	{"1", "review"},
	{"2", "send"},
	{"0", "dismiss"},
}

const (
	// feedbackCardMaxBody bounds the rows between the card's borders that
	// carry the draft: its title and a short preview. Bounded so a tall table
	// in the transcript cannot be walked into.
	feedbackCardMaxBody = 8
	// feedbackCardMaxGap is how many rows may sit between the card's bottom
	// border and the composer's fence: the blank margin and the runtime's own
	// hint row.
	feedbackCardMaxGap = 4
	// feedbackCardQuestionMax caps the title published as the prompt's
	// question. The runtime already truncates it with an ellipsis; this only
	// bounds a screen that does not.
	feedbackCardQuestionMax = 200
	// feedbackCardBottomRows is how far up from the bottom of the screen the
	// composer's prompt row may sit when its closing rule is missing.
	feedbackCardBottomRows = 4
)

// feedbackFooter is the key row's exact text with no queued suffix.
const feedbackFooter = "1 to review · 2 to send · 0 to dismiss"

// parseFeedbackFooter reads the inner text of a card's key row. It accepts the
// three items in order and nothing else, plus the runtime's optional trailing
// "+N more queued" (it is appended when more drafts wait behind this one; which
// separator precedes it is not measured, so both are accepted). queued is that
// suffix, or "".
func parseFeedbackFooter(inner string) (queued string, ok bool) {
	inner = strings.TrimSpace(inner)
	if !strings.HasPrefix(inner, feedbackFooter) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(inner, feedbackFooter))
	if rest == "" {
		return "", true
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "·"))
	// "+N more queued", N at least one digit.
	if !strings.HasPrefix(rest, "+") || !strings.HasSuffix(rest, " more queued") {
		return "", false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(rest, "+"), " more queued")
	if digits == "" {
		return "", false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return rest, true
}

// feedbackCardBox is the card's own box, located without reference to the
// composer.
type feedbackCardBox struct {
	top, key, bottom int
	// col is the box's left column (rune index).
	col int
	// body is the inner text of every row between the borders above the key
	// row, outer borders stripped, top to bottom.
	body []string
	// queued is the key row's "+N more queued" suffix, or "".
	queued string
}

// leadingBlanks counts the leading blanks of a line in runes.
func leadingBlanks(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' && r != '\t' {
			break
		}
		n++
	}
	return n
}

// borderRow reports whether a line is a box border of the given corners at
// column col, with an interior made only of the rule rune.
func borderRow(line string, col int, left, right rune) bool {
	r := []rune(strings.TrimRight(line, " \t\r"))
	if len(r) < col+5 || r[col] != left || r[len(r)-1] != right {
		return false
	}
	for _, c := range r[col+1 : len(r)-1] {
		if c != ruleRune {
			return false
		}
	}
	return true
}

// boxRowInner returns the text between a `│ … │` row's borders when the row is
// one at column col.
func boxRowInner(line string, col int) (string, bool) {
	r := []rune(strings.TrimRight(line, " \t\r"))
	if len(r) < col+2 || r[col] != '│' || r[len(r)-1] != '│' {
		return "", false
	}
	return strings.TrimSpace(string(r[col+1 : len(r)-1])), true
}

// feedbackCardBoxAt validates the card whose bottom border is row bottom.
func feedbackCardBoxAt(lines []string, bottom int) (feedbackCardBox, bool) {
	if bottom < 2 || bottom >= len(lines) {
		return feedbackCardBox{}, false
	}
	col := leadingBlanks(lines[bottom])
	if !borderRow(lines[bottom], col, '╰', '╯') {
		return feedbackCardBox{}, false
	}
	keyInner, ok := boxRowInner(lines[bottom-1], col)
	if !ok {
		return feedbackCardBox{}, false
	}
	queued, ok := parseFeedbackFooter(keyInner)
	if !ok {
		return feedbackCardBox{}, false
	}
	box := feedbackCardBox{key: bottom - 1, bottom: bottom, col: col, queued: queued}
	var rev []string
	for i := bottom - 2; i >= 0; i-- {
		if borderRow(lines[i], col, '╭', '╮') {
			if len(rev) == 0 {
				return feedbackCardBox{}, false
			}
			box.top = i
			for j := len(rev) - 1; j >= 0; j-- {
				box.body = append(box.body, rev[j])
			}
			return box, true
		}
		inner, isRow := boxRowInner(lines[i], col)
		if !isRow || len(rev) >= feedbackCardMaxBody {
			return feedbackCardBox{}, false
		}
		rev = append(rev, inner)
	}
	return feedbackCardBox{}, false
}

// findFeedbackCardBox finds a card's box anywhere in the lines, without the
// composer anchor liveFeedbackCard demands. It is for the redactor, which must
// keep the runtime's key row and drop the agent's draft around it, and has no
// use for the anchor (a corpus screen is a fixed point of redaction, not a
// live read).
func findFeedbackCardBox(lines []string) (feedbackCardBox, bool) {
	for i := len(lines) - 1; i >= 2; i-- {
		if box, ok := feedbackCardBoxAt(lines, i); ok {
			return box, true
		}
	}
	return feedbackCardBox{}, false
}

// feedbackCard is a card found directly above the live composer.
type feedbackCard struct {
	feedbackCardBox
	// fence and prompt are the composer's opening rule and its ❯ row.
	fence, prompt int
	// composerFound is composerSpan's verdict: true means the composer is
	// readable as a whole, so the card is not in the way.
	composerFound bool
	// rowEmpty: the ❯ row holds nothing, or only the dim placeholder hint.
	rowEmpty bool
	// rowText is what the ❯ row holds otherwise.
	rowText string
	// promptIsLast: the ❯ row is the last row of the screen — the shape a
	// card leaves a short pane in.
	promptIsLast bool
}

// answerable reports whether the card is the one thing between a caller and the
// composer, so that it is reported as a prompt: the composer could not be read
// as a whole, its ❯ row is on screen holding nothing, and it is the last row.
// Anything else — a composer that reads fine, or a row holding text a digit
// would be appended to — is not something a keypress can answer.
func (c feedbackCard) answerable() bool {
	return !c.composerFound && c.rowEmpty && c.promptIsLast
}

// liveFeedbackCard finds the card that sits directly above the live composer.
func liveFeedbackCard(s screen) (feedbackCard, bool) {
	n := len(s.lines)
	if n < 4 {
		return feedbackCard{}, false
	}
	var c feedbackCard
	if prompt, _, scan := composerSpan(s); scan == composerFound {
		c.composerFound = true
		c.prompt = prompt
		c.fence = firstNonBlankAbove(s.lines, prompt)
	} else {
		// The composer's closing rule is missing: look for an unclosed ❯ row at
		// the bottom of the screen, opened by a rule.
		c.prompt = -1
		for i := n - 1; i >= 0 && i >= n-feedbackCardBottomRows; i-- {
			if isRule(s.lines[i]) {
				return feedbackCard{}, false
			}
			if strings.HasPrefix(strings.TrimSpace(s.lines[i]), composerRuneMarker) {
				c.prompt = i
				break
			}
		}
		if c.prompt < 0 {
			return feedbackCard{}, false
		}
		c.fence = firstNonBlankAbove(s.lines, c.prompt)
		if c.fence < 0 || !isRule(s.lines[c.fence]) {
			return feedbackCard{}, false
		}
	}
	if c.fence < 0 || c.fence < s.visibleTop || c.prompt < s.visibleTop {
		return feedbackCard{}, false
	}

	// The gap between the card's bottom border and the fence: blank rows and
	// the runtime's hint row. A rule, another box row or the agent's own output
	// in it means whatever is above is not this composer's card.
	bottom := -1
	for j := c.fence - 1; j >= 0 && c.fence-j <= feedbackCardMaxGap+1; j-- {
		line := strings.TrimSpace(s.lines[j])
		if strings.HasPrefix(line, "╰") {
			bottom = j
			break
		}
		if isRule(line) || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "╭") ||
			strings.HasPrefix(line, responseBullet) {
			return feedbackCard{}, false
		}
	}
	if bottom < 0 || bottom < s.visibleTop {
		return feedbackCard{}, false
	}
	box, ok := feedbackCardBoxAt(s.lines, bottom)
	if !ok || box.col != leadingBlanks(s.lines[c.fence]) || box.top < s.visibleTop {
		return feedbackCard{}, false
	}
	c.feedbackCardBox = box

	c.promptIsLast = c.prompt == n-1
	if c.prompt < len(s.raw) && allDim(afterMarker(s.raw[c.prompt])) {
		c.rowEmpty = true
	} else {
		c.rowText = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.lines[c.prompt]), composerRuneMarker))
		c.rowEmpty = c.rowText == ""
	}
	return c, true
}

// firstNonBlankAbove is the index of the nearest non-blank row above i, or -1.
func firstNonBlankAbove(lines []string, i int) int {
	for j := i - 1; j >= 0; j-- {
		if strings.TrimSpace(lines[j]) != "" {
			return j
		}
	}
	return -1
}

// question is the draft's title, published as the prompt's question: the first
// body row, its leading glyph (which can be an animation frame) and border
// dropped. It is written by the agent and shown to a person deciding what to do
// with the draft; nothing reads it.
func (c feedbackCard) question() string {
	if len(c.body) == 0 {
		return ""
	}
	title := strings.TrimLeftFunc(c.body[0], func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if utf8.RuneCountInString(title) > feedbackCardQuestionMax {
		title = string([]rune(title)[:feedbackCardQuestionMax])
	}
	return title
}

// nonce identifies THIS draft. The options are identical on every draft, so the
// nonce is what stops an answer meant for one draft landing on the next: it
// digests the whole body (glyph excluded, since the title's glyph animates) and
// the queued suffix, so a draft arriving behind this one is a changed prompt and
// a stale answer is refused rather than applied to it.
func (c feedbackCard) nonce() string {
	h := sha256.New()
	h.Write([]byte(fleet.PromptFeedbackReview))
	for i, row := range c.body {
		h.Write([]byte{0})
		if i == 0 {
			row = c.question()
		}
		h.Write([]byte(row))
	}
	h.Write([]byte{0, 2})
	h.Write([]byte(c.queued))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sessionPrompt is the card as the prompt a caller reads. Selected stays zero:
// the card highlights nothing, so there is no default to accept.
func (c feedbackCard) sessionPrompt() *fleet.SessionPrompt {
	opts := make([]string, len(feedbackCardItems))
	for i, it := range feedbackCardItems {
		opts[i] = it.verb
	}
	return &fleet.SessionPrompt{
		Question: c.question(),
		Options:  opts,
		Kind:     fleet.PromptFeedbackReview,
		Nonce:    c.nonce(),
	}
}

// feedbackCardShortcuts are the keys that answer the card's options, in option
// order. Choice 3 is delivered as 0, not 3.
func feedbackCardShortcuts() []string {
	keys := make([]string, len(feedbackCardItems))
	for i, it := range feedbackCardItems {
		keys[i] = it.key
	}
	return keys
}

// feedbackCardRefusal names the card as the reason a delivery cannot proceed,
// when a card is on screen over a composer this driver cannot read as a whole.
// ok is false for every other screen, including a card above a readable
// composer (which blocks nothing).
func feedbackCardRefusal(s screen) (reason string, ok bool) {
	c, found := liveFeedbackCard(s)
	if !found || c.composerFound {
		return "", false
	}
	if c.answerable() {
		return "the session is showing the runtime's feedback-draft card over its composer: the card " +
			"pushes the composer's closing rule off the bottom of this pane, so input cannot be " +
			"delivered or confirmed until a person answers the card (respond) or the pane is made " +
			"taller. Nothing was written", true
	}
	return "the session is showing the runtime's feedback-draft card over its composer, and the " +
		"composer row holds text this driver cannot read in full; a key delivered now would be " +
		"appended to it. Nothing was written", true
}

// loneDigit reports whether text is a single digit the card reads as its own
// shortcut. A message that is only "1" pasted into an empty composer under the
// card would open the review, "0" would dismiss the draft.
func loneDigit(text string) bool {
	t := strings.TrimSpace(text)
	if len(t) != 1 {
		return false
	}
	for _, it := range feedbackCardItems {
		if t == it.key {
			return true
		}
	}
	return false
}
