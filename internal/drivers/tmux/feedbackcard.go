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
// Read out of the runtime's own binary (one build) and confirmed on real panes
// (#217): the card owns no focus. Its three keys are read through the
// composer's input value and act only while that value is empty, so a person can
// type into the composer under it, and a message pasted into the composer is
// delivered as it always was. Only a lone digit, or Escape on an empty composer,
// reaches the card, and Escape dismisses the card and nothing else: the draft
// stays queued.
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
// own status text, exactly, at the bottom of a validated box, directly above the
// live composer's fence. A transcript that prints the same words is indented under
// its own output and is nowhere near the fence.
//
// # The box has five texts, and only the first is a prompt (colab-fleet #217)
//
// The status row is the one thing that changes as the card is answered, and each
// of its states was measured on a real pane: the key row above; the send
// confirmation ("Send without reviewing … ? 2 to send · Esc to back"); "Sending…";
// and the send error ("✘ Couldn't send feedback (…). … 1 to review & retry · Esc
// to dismiss"). The confirmation and the error wrap and are a row taller than the
// key row, so on a short pane they push even the composer's ❯ row off the bottom.
// Only the key row over an empty, visible composer is answerable through respond;
// the rest are a person's, read as unknown with the state named. Two more screens
// belong to the same feature and are recognised here: the /feedback panel that
// "1" opens, which replaces the composer, and the plain row asking whether to
// turn drafts off. See docs/adr/217-*.md for what each reports and why.

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
	// feedbackCardMaxStatus bounds the rows the card's status text may wrap
	// over. The longest measured is the send error, two rows at 80 columns;
	// a narrower pane wraps it further.
	feedbackCardMaxStatus = 4
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

// The card's other states, each measured on a real pane (colab-fleet #217).
// Every one is the runtime's own text end to end, so each is matched exactly;
// the draft's words, which are the agent's, are never part of one.
const (
	// feedbackConfirmText replaces the key row when 2 is pressed. It is the
	// runtime's own second guard on sending, and wraps at the box width.
	feedbackConfirmText = "Send without reviewing (full draft + env, no transcript)? 2 to send · Esc to back"
	// feedbackSendingText replaces the key row while the request is in flight.
	feedbackSendingText = "Sending…"
	// feedbackErrorHead and feedbackErrorTail bracket the send error: the head,
	// then a parenthesised reason the runtime or the service supplies, then a
	// fixed sentence, then the key hint as the tail of the same wrapped text.
	feedbackErrorHead = "✘ Couldn't send feedback"
	feedbackErrorTail = "1 to review & retry · Esc to dismiss"
	// feedbackQuestionRow is the question that can follow a dismissal. It is
	// not boxed: a plain row above the composer's fence.
	feedbackQuestionRow = "Turn off Claude-drafted feedback? 0 to turn off · Esc to keep"
	// feedbackPanelTitle is the title of the panel `1` opens (/feedback).
	feedbackPanelTitle = "Feedback drafts"
	// feedbackPanelHint is the list view's footer: runtime vocabulary, kept
	// by the redactor.
	feedbackPanelHint = "Enter to review · d to discard · Esc to close"
)

// feedbackErrorReasons are the parenthesised reasons measured in a build. The
// redactor keeps an error row only when its reason is one of these; any other
// reason came from a service or a runtime path nobody has read.
var feedbackErrorReasons = []string{"couldn't reach the service"}

// feedbackState is which of the card's states the box shows.
type feedbackState int

const (
	// feedbackKeys is the card as it first appears: the three-key row.
	feedbackKeys feedbackState = iota + 1
	// feedbackConfirm: `2` was pressed and the runtime asks once more.
	feedbackConfirm
	// feedbackSending: the request is in flight.
	feedbackSending
	// feedbackError: the send failed and the draft is still queued.
	feedbackError
)

func (f feedbackState) String() string {
	switch f {
	case feedbackKeys:
		return "card"
	case feedbackConfirm:
		return "confirm"
	case feedbackSending:
		return "sending"
	case feedbackError:
		return "error"
	}
	return "none"
}

// parseFeedbackFooter reads the inner text of a card's key row. It accepts the
// three items in order and nothing else, plus the runtime's optional trailing
// " · +N more queued" (measured: it is appended, after the same separator, when
// more drafts wait behind this one). queued is that suffix, or "".
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

// parseFeedbackStatus reads the text the card shows in place of a draft row:
// the key row, the send confirmation, the in-flight line or the send error. It
// matches the runtime's exact wording and nothing else, so a status that is
// anything other than one of the four is not a card.
func parseFeedbackStatus(joined string) (state feedbackState, queued string, ok bool) {
	joined = strings.Join(strings.Fields(joined), " ")
	switch {
	case joined == feedbackConfirmText:
		return feedbackConfirm, "", true
	case joined == feedbackSendingText:
		return feedbackSending, "", true
	case strings.HasPrefix(joined, feedbackErrorHead) && strings.HasSuffix(joined, " "+feedbackErrorTail) &&
		len(joined) > len(feedbackErrorHead)+len(feedbackErrorTail)+2:
		return feedbackError, "", true
	}
	if q, ok := parseFeedbackFooter(joined); ok {
		return feedbackKeys, q, true
	}
	return 0, "", false
}

// feedbackErrorReason is the parenthesised reason in a send error's text, or
// "". The text is "✘ Couldn't send feedback (<reason>). <fixed sentence> <hint>".
func feedbackErrorReason(status string) string {
	rest := strings.TrimPrefix(strings.Join(strings.Fields(status), " "), feedbackErrorHead)
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return ""
	}
	i := strings.Index(rest, ")")
	if i < 0 {
		return ""
	}
	return rest[1:i]
}

// feedbackReasonMask is what a send error's unmeasured reason is overwritten
// with, rune for rune, so the wrapped rows keep their shape.
const feedbackReasonMask = '#'

// feedbackErrorReasonSafe reports whether a send error's reason may stay in a
// published corpus screen: one that was measured, or one already masked.
func feedbackErrorReasonSafe(reason string) bool {
	if reason == "" {
		return false
	}
	for _, known := range feedbackErrorReasons {
		if reason == known {
			return true
		}
	}
	for _, r := range reason {
		if r != feedbackReasonMask && r != ' ' {
			return false
		}
	}
	return true
}

// maskFeedbackErrorReason overwrites, in a send error's wrapped rows, every
// character between the first "(" and the next ")" of the joined text with the
// mask, keeping the spaces so the rows wrap as they did. It returns the rows
// unchanged, and false, when the text has no parenthesised reason to mask.
func maskFeedbackErrorReason(rows []string, col int) ([]string, bool) {
	out := make([]string, len(rows))
	open, closed := false, false
	for i, row := range rows {
		r := []rune(row)
		for j := col + 2; j < len(r)-1; j++ {
			switch {
			case closed:
			case !open:
				open = r[j] == '('
			case r[j] == ')':
				closed = true
			case r[j] != ' ':
				r[j] = feedbackReasonMask
			}
		}
		out[i] = string(r)
	}
	return out, open && closed
}

// feedbackCardBox is the card's own box, located without reference to the
// composer.
type feedbackCardBox struct {
	// top and bottom are the box's border rows. first is the row the status
	// text starts on; it ends on the row above bottom, so for the ordinary
	// card's key row first is bottom-1.
	top, first, bottom int
	// col is the box's left column (rune index).
	col int
	// body is the inner text of every row between the top border and the
	// status text, outer borders stripped, top to bottom: the draft's title
	// and its preview. The agent's words.
	body []string
	// state is which of the card's states the status text is.
	state feedbackState
	// status is the status text, its wrapped rows joined.
	status string
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
//
// The status text is found from the bottom: the shortest run of trailing rows
// that, joined, is exactly one of the runtime's four status texts. Nothing
// above it is read, so the agent's title and preview can say anything, and a
// status that wraps over two rows (the confirmation and the error do, at 80
// columns) is one text. A box whose tail is not one of the four is not a card.
func feedbackCardBoxAt(lines []string, bottom int) (feedbackCardBox, bool) {
	if bottom < 2 || bottom >= len(lines) {
		return feedbackCardBox{}, false
	}
	col := leadingBlanks(lines[bottom])
	if !borderRow(lines[bottom], col, '╰', '╯') {
		return feedbackCardBox{}, false
	}
	var rev []string
	top := -1
	for i := bottom - 1; i >= 0; i-- {
		if borderRow(lines[i], col, '╭', '╮') {
			top = i
			break
		}
		inner, isRow := boxRowInner(lines[i], col)
		if !isRow || len(rev) >= feedbackCardMaxBody+feedbackCardMaxStatus {
			return feedbackCardBox{}, false
		}
		rev = append(rev, inner)
	}
	if top < 0 || len(rev) < 2 {
		return feedbackCardBox{}, false
	}
	rows := make([]string, len(rev))
	for i, r := range rev {
		rows[len(rev)-1-i] = r
	}
	for k := 1; k <= feedbackCardMaxStatus && k < len(rows); k++ {
		status := strings.Join(rows[len(rows)-k:], " ")
		state, queued, ok := parseFeedbackStatus(status)
		if !ok {
			continue
		}
		body := rows[:len(rows)-k]
		if len(body) > feedbackCardMaxBody {
			return feedbackCardBox{}, false
		}
		return feedbackCardBox{
			top: top, first: bottom - k, bottom: bottom, col: col,
			body: body, state: state,
			status: strings.Join(strings.Fields(status), " "), queued: queued,
		}, true
	}
	return feedbackCardBox{}, false
}

// findFeedbackCardBox finds a card's box anywhere in the lines, without the
// composer anchor liveFeedbackCard demands. It is for the redactor, which must
// keep the runtime's status text and drop the agent's draft around it, and has no
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
	// fence and prompt are the composer's opening rule and its ❯ row. prompt is
	// -1 when the ❯ row is below the bottom of the pane too (promptHidden).
	fence, prompt int
	// composerFound is composerSpan's verdict: true means the composer is
	// readable as a whole, so the card is not in the way.
	composerFound bool
	// promptHidden: the screen ends on the composer's opening rule, so even
	// its ❯ row is off the bottom. Measured with the send confirmation and the
	// send error, whose text wraps and takes a row more than the key row.
	promptHidden bool
	// rowEmpty: the ❯ row holds nothing, or only the dim placeholder hint.
	rowEmpty bool
	// rowText is what the ❯ row holds otherwise.
	rowText string
	// promptIsLast: the ❯ row is the last row of the screen — the shape a
	// card leaves a short pane in.
	promptIsLast bool
}

// answerable reports whether the card is the one thing between a caller and the
// composer, so that it is reported as a prompt: it is the key row (the other
// states are answered by a person at the terminal, see ADR 217), the composer
// could not be read as a whole, its ❯ row is on screen holding nothing, and it
// is the last row. Anything else — a composer that reads fine, or a row holding
// text a digit would be appended to, or a row that cannot be seen at all — is
// not something a keypress can answer.
func (c feedbackCard) answerable() bool {
	return c.state == feedbackKeys && !c.composerFound && c.rowEmpty && c.promptIsLast
}

// feedbackAnchor finds the composer's opening rule and its ❯ row, the two rows
// every state of the card is read against. When the composer's closing rule is
// below the bottom of the pane it looks for an unclosed ❯ row at the bottom of
// the screen, opened by a rule; and when even that row is below the bottom, for
// an opening rule that is itself the last row (prompt == -1).
func feedbackAnchor(s screen) (fence, prompt int, found, ok bool) {
	n := len(s.lines)
	if n < 4 {
		return 0, 0, false, false
	}
	if p, _, scan := composerSpan(s); scan == composerFound {
		fence, prompt, found = firstNonBlankAbove(s.lines, p), p, true
	} else {
		// The composer's closing rule is missing: look for an unclosed ❯ row at
		// the bottom of the screen, opened by a rule.
		prompt = -2
		for i := n - 1; i >= 0 && i >= n-feedbackCardBottomRows; i-- {
			if isRule(s.lines[i]) {
				if i == n-1 {
					// The opening rule is the last row: the ❯ row is below it.
					fence, prompt = i, -1
				}
				break
			}
			if strings.HasPrefix(strings.TrimSpace(s.lines[i]), composerRuneMarker) {
				prompt = i
				break
			}
		}
		if prompt == -2 {
			return 0, 0, false, false
		}
		if prompt >= 0 {
			fence = firstNonBlankAbove(s.lines, prompt)
		}
	}
	if fence < 0 || !isRule(s.lines[fence]) || fence < s.visibleTop || (prompt >= 0 && prompt < s.visibleTop) {
		return 0, 0, false, false
	}
	return fence, prompt, found, true
}

// liveFeedbackCard finds the card that sits directly above the live composer.
func liveFeedbackCard(s screen) (feedbackCard, bool) {
	fence, prompt, found, ok := feedbackAnchor(s)
	if !ok {
		return feedbackCard{}, false
	}
	c := feedbackCard{fence: fence, prompt: prompt, composerFound: found, promptHidden: prompt < 0}

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

	if c.promptHidden {
		return c, true
	}
	c.promptIsLast = c.prompt == len(s.lines)-1
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
	// The state is part of the identity: the same draft in the key-row state
	// and in the confirmation is two different questions, and an answer read
	// against one must not be applied to the other. The key-row state adds
	// nothing, so a nonce read before this field existed still matches.
	if c.state != feedbackKeys {
		h.Write([]byte{0, 3})
		h.Write([]byte(c.state.String()))
	}
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

// waitsOn is what the card, in this state and hiding the composer, is waiting
// for, in the words the state read and every refusal share.
func (c feedbackCard) waitsOn() string {
	switch c.state {
	case feedbackConfirm:
		return "asking to confirm sending the draft (2 to send · Esc to back), which is the " +
			"runtime's own second guard and a person's to give"
	case feedbackSending:
		return "sending the draft (\"Sending…\"): the request is in flight, and becomes the send " +
			"error if it fails"
	case feedbackError:
		return "showing that sending the draft failed (the draft stays queued) and offering to " +
			"review and retry or to dismiss, which is a person's to choose"
	}
	return "offering the draft it wrote (review, send or dismiss)"
}

// hides is why the composer under this card cannot be used, or "" when it can.
func (c feedbackCard) hides() string {
	switch {
	case c.composerFound:
		return ""
	case c.promptHidden:
		return "the card pushes the composer, its ❯ row included, off the bottom of this pane"
	case !c.rowEmpty:
		return "the composer row holds text this driver cannot read in full, and a key delivered " +
			"now would be appended to it"
	}
	return "the card pushes the composer's closing rule off the bottom of this pane"
}

// evidence is the state read's account of a card that hides the composer and is
// not a prompt.
func (c feedbackCard) evidence() string {
	return "the runtime's feedback-draft card is " + c.waitsOn() + "; " + c.hides() +
		", so the composer beneath it cannot be read as a whole"
}

// refusal is what a delivery that cannot proceed says, when a card is on screen
// over a composer this driver cannot read as a whole.
func (c feedbackCard) refusal() string {
	if c.answerable() {
		return "the session is showing the runtime's feedback-draft card over its composer: the card " +
			"pushes the composer's closing rule off the bottom of this pane, so input cannot be " +
			"delivered or confirmed until a person answers the card (respond) or the pane is made " +
			"taller. Nothing was written"
	}
	hides := c.hides()
	return "the session is showing the runtime's feedback-draft card over its composer; the card is " +
		c.waitsOn() + ". " + strings.ToUpper(hides[:1]) + hides[1:] + ". Nothing was written"
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
	return c.refusal(), true
}

// feedbackNoticeRefusal is feedbackCardRefusal or feedbackPanelRefusal: the
// reason a delivery cannot proceed when either of the runtime's feedback
// screens is over the composer. It is what send, discard and respond ask;
// keys asks the two separately, because Escape is welcome on the panel.
func feedbackNoticeRefusal(s screen) (reason string, ok bool) {
	if reason, ok = feedbackCardRefusal(s); ok {
		return reason, true
	}
	return feedbackPanelRefusal(s)
}

// feedbackPanel is the runtime's /feedback panel, which `1` opens: it replaces
// the composer, and everything on it is the person's to decide.
//
// Measured: a heavy rule (▔) across the pane at column 0, then the title
// "Feedback drafts" three columns in, then either the list of drafts ("Enter to
// review · d to discard · Esc to close") or one draft's editor, whose highlighted
// "Send feedback" row sends it, transcript included. There is no composer on
// screen. Keys that are harmless in the composer are not harmless here: Enter
// opens a draft or sends it, and `d` discards one.
type feedbackPanel struct{ rule, title int }

// heavyRule reports whether a line is the panel's top edge: one unbroken run of
// the heavy-rule rune from column 0.
func heavyRule(line string) bool {
	r := []rune(strings.TrimRight(line, " \t\r"))
	if len(r) < 20 {
		return false
	}
	for _, c := range r {
		if c != '▔' {
			return false
		}
	}
	return true
}

// liveFeedbackPanel finds the panel at the bottom of the screen.
func liveFeedbackPanel(s screen) (feedbackPanel, bool) {
	rule := -1
	for i := len(s.lines) - 1; i >= 0 && i >= s.visibleTop; i-- {
		if heavyRule(s.lines[i]) {
			rule = i
			break
		}
	}
	if rule < 0 {
		return feedbackPanel{}, false
	}
	title := -1
	for j := rule + 1; j < len(s.lines) && j <= rule+3; j++ {
		if strings.TrimSpace(s.lines[j]) == "" {
			continue
		}
		title = j
		break
	}
	if title < 0 || strings.TrimRight(s.lines[title], " \t\r") != strings.Repeat(" ", 3)+feedbackPanelTitle {
		return feedbackPanel{}, false
	}
	// A panel replaces the composer. A composer's fence below it means this is
	// the agent's own output, or an old panel in the transcript, not the live one.
	for j := title + 1; j < len(s.lines); j++ {
		if isRule(s.lines[j]) {
			return feedbackPanel{}, false
		}
	}
	return feedbackPanel{rule: rule, title: title}, true
}

// feedbackPanelRefusal names the panel as the reason a delivery cannot proceed.
func feedbackPanelRefusal(s screen) (reason string, ok bool) {
	if _, found := liveFeedbackPanel(s); !found {
		return "", false
	}
	return "the session has the runtime's feedback panel open (its list of queued drafts or a " +
		"draft's editor), which replaces the composer: a person is deciding what to do with a " +
		"draft, and a key delivered now could open, edit, discard or send one. Nothing was written. " +
		"Escape closes the panel", true
}

// feedbackPanelEvidence is the state read's account of the panel.
const feedbackPanelEvidence = "the runtime's feedback panel is open over the composer (its list of " +
	"queued drafts or a draft's editor): a person is deciding what to do with a draft, and there is " +
	"no composer to deliver into until it is closed"

// liveFeedbackQuestion reports whether the runtime is asking whether to turn
// drafts off: a plain row, not a box, directly above the composer's fence.
// It never hides the composer at the geometries a fleet creates, so it is not a
// prompt; it is recognised because a lone `0` would answer it.
func liveFeedbackQuestion(s screen) bool {
	fence, _, _, ok := feedbackAnchor(s)
	if !ok {
		return false
	}
	for j := fence - 1; j >= 0 && fence-j <= feedbackCardMaxGap+1; j-- {
		line := strings.TrimSpace(s.lines[j])
		if line == "" {
			continue
		}
		return line == feedbackQuestionRow
	}
	return false
}

// feedbackNoticeOnScreen reports whether any of the runtime's feedback notices
// that read a lone digit as a key is on screen: the card in any state, or the
// question about turning drafts off.
func feedbackNoticeOnScreen(s screen) bool {
	if _, ok := liveFeedbackCard(s); ok {
		return true
	}
	return liveFeedbackQuestion(s)
}

// loneDigit reports whether text is a single digit the card reads as its own
// shortcut. A message that is only "1" pasted into an empty composer under the
// card would open the review, "2" would ask to send (or, on the confirmation,
// send), "0" would dismiss the draft or turn drafts off.
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

// feedbackEscapeNote says what Escape does on a screen showing one of the
// runtime's feedback notices, for the receipt of a key that was sent: "" when
// none is on screen. Measured on a real pane (colab-fleet #217): with the
// composer empty Escape dismisses the card and only the card — the draft stays
// queued, its file and the footer's count unchanged, and /feedback still lists
// it — and is sometimes followed by the question about turning drafts off.
// With text in the composer it does nothing to the card.
func feedbackEscapeNote(s screen) string {
	if _, ok := liveFeedbackPanel(s); ok {
		return " (on the runtime's feedback panel Escape closes the panel, or steps back from a draft to the list)"
	}
	if c, ok := liveFeedbackCard(s); ok {
		if c.rowText != "" {
			return ""
		}
		switch c.state {
		case feedbackConfirm:
			return " (on the runtime's send confirmation Escape goes back to the card; nothing was sent)"
		case feedbackSending:
			return ""
		}
		return " (on the runtime's feedback-draft card Escape dismisses the notice; the draft stays " +
			"queued, and /feedback still lists it)"
	}
	if liveFeedbackQuestion(s) {
		return " (on the runtime's question about turning drafts off Escape keeps them on)"
	}
	return ""
}
