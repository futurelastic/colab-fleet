package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// ftQuestion is one question of a fakeFreeText dialog.
type ftQuestion struct {
	header   string
	question string
	options  []string
}

// fakeFreeText models the runtime's numbered question dialog with its
// free-text row, as measured live for colab-fleet#206. Every behaviour below
// was seen on a real screen; the two that a naive model would get wrong are
// the ones the driver's design rests on:
//
//   - a digit naming the free-text row MOVES THE HIGHLIGHT THERE and commits
//     nothing — a digit on any other row commits (#168) — and once the field
//     has focus a digit is TYPED into it;
//   - Enter on the free-text row while it is EMPTY declines the WHOLE dialog,
//     every question in it. The model does that, so a driver that confirms an
//     empty field fails visibly rather than passing quietly.
//
// Also modelled: a bracketed paste with the highlight on the row lands as the
// row's label (a long line wraps, an embedded newline continues on rows of its
// own); Enter with text commits and advances exactly as a digit does; Up/Down
// walk the options, the free-text row and the numbered chat row.
type fakeFreeText struct {
	qs        []ftQuestion
	cur       int      // question on screen; len(qs) = the review screen
	sel       int      // 1-based highlighted row: n options, n+1 free text, n+2 chat
	typed     []string // what the free-text field holds, per question
	answers   []string // "" unanswered; "opt:N" or "text:<what was typed>"
	declined  bool
	submitted bool

	swallow      map[string]int
	noPaste      bool
	mangle       func(string) string // applied to a paste before it lands
	wrapAt       int                 // runes the row shows before it wraps; 0 = 60
	lostConfirms int                 // Enter presses the runtime drops
}

func newFakeFreeText(qs ...ftQuestion) *fakeFreeText {
	return &fakeFreeText{
		qs: qs, sel: 1,
		typed: make([]string, len(qs)), answers: make([]string, len(qs)),
		swallow: map[string]int{},
	}
}

func oneQuestion(opts ...string) *fakeFreeText {
	return newFakeFreeText(ftQuestion{question: "Which do you want?", options: opts})
}

func (g *fakeFreeText) commit(answer string) {
	g.answers[g.cur] = answer
	g.cur++
	g.sel = 1
	if len(g.qs) == 1 {
		g.submitted = true // a single-question dialog has no review screen
	}
}

func (g *fakeFreeText) paste(text string) {
	if g.noPaste || g.submitted || g.declined || g.cur >= len(g.qs) {
		return
	}
	if g.sel != len(g.qs[g.cur].options)+1 {
		return // no field has focus: the paste has nowhere to land
	}
	if g.mangle != nil {
		text = g.mangle(text)
	}
	g.typed[g.cur] += text
}

func (g *fakeFreeText) press(key string) {
	if g.submitted || g.declined {
		return
	}
	if g.swallow[key] > 0 {
		g.swallow[key]--
		return
	}
	if g.cur >= len(g.qs) { // the review confirm widget
		switch key {
		case "1":
			g.submitted = true
		case "2", "Escape":
			g.declined = true
		}
		return
	}
	q := g.qs[g.cur]
	n := len(q.options)
	free, chat := n+1, n+2
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		d := int(key[0] - '0')
		switch {
		case g.sel == free:
			g.typed[g.cur] += key // the field has focus: a digit is text
		case d == free:
			g.sel = free // moves there; commits nothing
		case d <= n:
			g.commit(fmt.Sprintf("opt:%d", d))
		}
		return
	}
	switch key {
	case "Up":
		if g.sel > 1 {
			g.sel--
		}
	case "Down":
		if g.sel < chat {
			g.sel++
		}
	case "Escape":
		g.declined = true
	case "C-m":
		if g.lostConfirms > 0 {
			g.lostConfirms--
			return
		}
		switch {
		case g.sel <= n:
			g.commit(fmt.Sprintf("opt:%d", g.sel))
		case g.sel == free && g.typed[g.cur] == "":
			g.declined = true // measured: an empty answer declines the whole dialog
		case g.sel == free:
			g.commit("text:" + g.typed[g.cur])
		}
	}
}

func (g *fakeFreeText) screen() string {
	if g.submitted || g.declined {
		return idleFixtureFor("answered")
	}
	var b strings.Builder
	b.WriteString("  transcript line\n" + rule + "\n")
	if len(g.qs) > 1 {
		b.WriteString("←")
		for i, q := range g.qs {
			box := "☐"
			if g.answers[i] != "" {
				box = "☒"
			}
			fmt.Fprintf(&b, "  %s %s", box, q.header)
		}
		b.WriteString("  ✔ Submit  →\n")
	}
	if g.cur >= len(g.qs) {
		b.WriteString("Review your answers\n\n" + reviewQuestion + "\n\n")
		fmt.Fprintf(&b, "❯ 1. %s\n  2. %s\n", reviewOptions[0], reviewOptions[1])
		return b.String()
	}
	q := g.qs[g.cur]
	n := len(q.options)
	mark := func(row int) string {
		if row == g.sel {
			return "❯ "
		}
		return "  "
	}
	b.WriteString(q.question + "\n")
	for i, o := range q.options {
		fmt.Fprintf(&b, "%s%d. %s\n", mark(i+1), i+1, o)
	}
	if t := g.typed[g.cur]; t != "" {
		first, rest, _ := strings.Cut(t, "\n")
		wrap := g.wrapAt
		if wrap == 0 {
			wrap = 60
		}
		if r := []rune(first); len(r) > wrap {
			first = strings.TrimRight(string(r[:wrap]), " ")
			rest = strings.TrimLeft(string(r[wrap:]), " ") + "\n" + rest
		}
		fmt.Fprintf(&b, "%s%d. %s\n", mark(n+1), n+1, first)
		for _, l := range strings.Split(strings.TrimRight(rest, "\n"), "\n") {
			if l != "" {
				fmt.Fprintf(&b, "     %s\n", l)
			}
		}
	} else {
		fmt.Fprintf(&b, "%s%d. Type something.\n", mark(n+1), n+1)
	}
	fmt.Fprintf(&b, "%s\n%s%d. Chat about this\n\n", rule, mark(n+2), n+2)
	footer := "Enter to select · Tab/Arrow keys to navigate"
	if g.sel == n+1 {
		footer += " · ctrl+g to edit in Vim"
	}
	b.WriteString(footer + " · Esc to cancel")
	return b.String()
}

func textPtr(s string) *string { return &s }

func respondText(t *testing.T, d *Driver, nonce string, text string, choices ...int) fleet.DeliveryReceipt {
	t.Helper()
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.Response{Text: &text, Choices: choices, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func armFreeText(t *testing.T, g keyedScreen) (*fakeMux, *Driver) {
	t.Helper()
	f := twoSessions()
	armDialog(f, "%1", g)
	return f, newTestDriver(f)
}

func pastesTo(f *fakeMux) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pasteLog...)
}

func hasKey(f *fakeMux, key string) bool {
	for _, c := range sendCalls(f) {
		for _, k := range strings.Fields(c) {
			if k == key {
				return true
			}
		}
	}
	return false
}

// The whole sequence, on a single-select question: the digit moves the
// highlight onto the row, the text is pasted, and Enter — a key of its own —
// confirms it. Nothing else is sent.
func TestTextAnswersASingleQuestionThroughTheFreeTextRow(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile, please")
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
	}
	if !g.submitted || g.answers[0] != "text:chamomile, please" {
		t.Errorf("the dialog holds answers %v submitted=%v", g.answers, g.submitted)
	}
	if fmt.Sprint(sendCalls(f)) != "[3 C-m]" {
		t.Errorf("keys = %v, want the digit of the free-text row, then Enter, each in its own call", sendCalls(f))
	}
	if p := pastesTo(f); len(p) != 1 || p[0] != "chamomile, please" {
		t.Errorf("pastes = %q", p)
	}
	if strings.Contains(got.Reason, "chamomile") {
		t.Errorf("the receipt copies the caller's answer back: %q", got.Reason)
	}
}

// A digit sent while the field already has focus is TYPED into it. So when the
// highlight is already on the row, no digit is sent at all.
func TestTextDoesNotSendADigitWhenTheHighlightIsAlreadyOnTheRow(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.sel = 3
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "oolong")
	if got.Outcome != fleet.OutcomeSubmitted || g.answers[0] != "text:oolong" {
		t.Fatalf("respond = %q (%s), answers %v", got.Outcome, got.Reason, g.answers)
	}
	if fmt.Sprint(sendCalls(f)) != "[C-m]" {
		t.Errorf("keys = %v, want only Enter", sendCalls(f))
	}
}

// One question of a multi-question dialog: the text is typed, Enter advances to
// the next question and the receipt says so; the last question lands on the
// review screen, which is NOT confirmed.
func TestTextMovesAMultiQuestionDialogOnAndStopsAtTheReviewScreen(t *testing.T) {
	g := newFakeFreeText(
		ftQuestion{header: "Colour", question: "Which colour?", options: []string{"Red", "Green"}},
		ftQuestion{header: "Animal", question: "Which animal?", options: []string{"Cat", "Dog"}},
	)
	f, d := armFreeText(t, g)

	first := respondText(t, d, currentNonce(t, f), "teal")
	if first.Outcome != fleet.OutcomeSubmitted || !strings.Contains(first.Reason, "next question") {
		t.Fatalf("first = %q (%s)", first.Outcome, first.Reason)
	}
	if g.cur != 1 || g.answers[0] != "text:teal" {
		t.Fatalf("after the first: cur %d answers %v", g.cur, g.answers)
	}

	second := respondText(t, d, currentNonce(t, f), "a wombat")
	if second.Outcome != fleet.OutcomeSubmitted || !strings.Contains(second.Reason, "review screen") ||
		!strings.Contains(second.Reason, "NOT handed over") {
		t.Fatalf("second = %q (%s), want submitted, and a note that the review screen is unanswered", second.Outcome, second.Reason)
	}
	if g.submitted || g.cur != 2 {
		t.Errorf("the review screen was confirmed for the caller: submitted=%v cur=%d", g.submitted, g.cur)
	}
	if hasKey(f, "1") && g.answers[1] != "text:a wombat" {
		t.Errorf("answers = %v", g.answers)
	}
}

// Enter on an EMPTY free-text field declines the whole dialog (measured). The
// model does that, so what this pins is that nothing ever sends it: an empty
// answer is refused before a key is pressed, and a paste that did not land is
// never confirmed.
func TestTextNeverConfirmsAnEmptyField(t *testing.T) {
	for name, text := range map[string]string{
		"empty":             "",
		"blank":             "   \t ",
		"control bytes":     "\x07\x1b\x00",
		"newlines":          "\n\n",
		"zero after strips": "\u009b\u009d",
	} {
		t.Run(name, func(t *testing.T) {
			g := oneQuestion("Tea", "Coffee")
			f, d := armFreeText(t, g)
			got := respondText(t, d, currentNonce(t, f), text)
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("respond = %q (%s), want refused", got.Outcome, got.Reason)
			}
			if len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
				t.Errorf("keys %v pastes %v; a refused answer must touch nothing", sendCalls(f), pastesTo(f))
			}
			if g.declined {
				t.Error("the dialog was declined")
			}
		})
	}
}

func TestTextIsNotConfirmedWhenThePasteDidNotLand(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.noPaste = true
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if hasKey(f, "C-m") {
		t.Fatal("Enter was sent with an empty field: that declines the whole dialog")
	}
	if g.declined || g.submitted {
		t.Errorf("declined=%v submitted=%v; nothing should have been confirmed", g.declined, g.submitted)
	}
	if !strings.Contains(got.Reason, "did not read back") || !strings.Contains(got.Reason, "nothing was confirmed") {
		t.Errorf("receipt = %q", got.Reason)
	}
}

// A paste that lands but not as sent — here, its first character lost — is not
// confirmed either: the row is read back against what was sent, not just for
// "something is there".
func TestTextIsNotConfirmedWhenTheRowShowsSomethingElse(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.mangle = func(s string) string { return s[1:] }
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeUnknown || hasKey(f, "C-m") || g.submitted {
		t.Fatalf("respond = %q (%s), keys %v", got.Outcome, got.Reason, sendCalls(f))
	}
}

func TestTextStopsWhenTheDigitDidNotMoveTheHighlight(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.swallow["3"] = 1
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if len(pastesTo(f)) != 0 || hasKey(f, "C-m") {
		t.Errorf("pastes %v keys %v; nothing further may be sent once the highlight did not arrive", pastesTo(f), sendCalls(f))
	}
}

// Enter lost: the text is in the row, the question is still up. The receipt
// says unknown and says the text is there — never submitted.
func TestTextReportsUnknownWhenTheConfirmDidNotLand(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.lostConfirms = 1
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeUnknown || g.submitted {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "still on screen") {
		t.Errorf("receipt = %q", got.Reason)
	}
}

func TestTextWithAStaleNonceTouchesNothing(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	f, d := armFreeText(t, g)
	nonce := currentNonce(t, f)
	g.sel = 2
	g.qs[0].options = []string{"Tea", "Milk"} // the session moved on to a different question
	f.captures["%1"] = g.screen()

	got := respondText(t, d, nonce, "chamomile")
	if got.Outcome != fleet.OutcomeRefused || len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
		t.Fatalf("respond = %q (%s), keys %v", got.Outcome, got.Reason, sendCalls(f))
	}
}

// A prompt that does not carry freeText is not answered with text — an older
// build never reports it, and the caller is meant to have checked.
func TestTextIsRefusedOnAPromptWithNoFreeTextRow(t *testing.T) {
	g := newFakeDialog([]string{"Yes", "No"}, []string{"Red", "Blue"})
	f, d := armFreeText(t, g)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "freeText") {
		t.Fatalf("respond = %q (%s), want a refusal naming freeText", got.Outcome, got.Reason)
	}
	if len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
		t.Errorf("keys %v pastes %v", sendCalls(f), pastesTo(f))
	}
}

func TestTextIsRefusedOnAnUnnumberedMenu(t *testing.T) {
	f := twoSessions()
	armDialog(f, "%1", newTrustMenu())
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeRefused || len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
		t.Fatalf("respond = %q (%s), keys %v", got.Outcome, got.Reason, sendCalls(f))
	}
}

// Once text is in the row the placeholder is gone, so the row cannot be found
// again: prompt.freeText goes false, and typing over a person's text — or an
// earlier attempt's — is refused rather than guessed at.
func TestTextIsRefusedWhenTheRowAlreadyHoldsText(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.typed[0] = "someone was here first"
	f, d := armFreeText(t, g)

	if p := parsePrompt(newScreen(f.captures["%1"])); p == nil || p.FreeText {
		t.Fatalf("prompt = %+v, want a prompt that no longer reports freeText", p)
	}
	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "already holds text") {
		t.Fatalf("respond = %q (%s)", got.Outcome, got.Reason)
	}
	if len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
		t.Errorf("keys %v pastes %v", sendCalls(f), pastesTo(f))
	}
}

// The answer is put through send()'s sanitiser (control bytes and paste-bracket
// escapes dropped) and trimmed. A leading "!" or "/" is NOT refused: measured,
// the field takes both as plain text, and "/var/log" is an ordinary answer.
func TestTextIsSanitisedButNotRefusedForBeginningWithASlashOrABang(t *testing.T) {
	for _, tc := range []struct{ send, typed string }{
		{"/var/log/app", "/var/log/app"},
		{"!important", "!important"},
		{"  padded  \n", "padded"},
		{"a\x1b[201~b\x07", "a[201~b"},
	} {
		t.Run(tc.send, func(t *testing.T) {
			g := oneQuestion("Tea", "Coffee")
			f, d := armFreeText(t, g)
			got := respondText(t, d, currentNonce(t, f), tc.send)
			if got.Outcome != fleet.OutcomeSubmitted || g.answers[0] != "text:"+tc.typed {
				t.Fatalf("respond = %q (%s), answers %v, want %q", got.Outcome, got.Reason, g.answers, tc.typed)
			}
		})
	}
}

// A first line longer than a row wraps; an embedded newline continues on rows
// of its own. The row's label is then a PREFIX of what was sent, which is what
// the read-back accepts for a line too long to have fitted.
func TestTextReadBackToleratesWrappingAndNewlines(t *testing.T) {
	long := strings.Repeat("word ", 30) + "END"
	for name, text := range map[string]string{
		"wrapped line": long,
		"two lines":    "first line\nsecond line",
	} {
		t.Run(name, func(t *testing.T) {
			g := oneQuestion("Tea", "Coffee")
			f, d := armFreeText(t, g)
			got := respondText(t, d, currentNonce(t, f), text)
			if got.Outcome != fleet.OutcomeSubmitted || g.answers[0] != "text:"+text {
				t.Fatalf("respond = %q (%s), answers %q", got.Outcome, got.Reason, g.answers)
			}
		})
	}
}

// A pane whose bracketed-paste mode cannot be confirmed refuses, as send does:
// nothing is typed, and Enter is never pressed on an empty field.
func TestTextIsRefusedWhenBracketedPasteIsOff(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	f, d := armFreeText(t, g)
	f.setBracketPasteOff("%1", true)

	got := respondText(t, d, currentNonce(t, f), "chamomile")
	if got.Outcome != fleet.OutcomeRefused || hasKey(f, "C-m") || g.declined || g.submitted {
		t.Fatalf("respond = %q (%s), keys %v", got.Outcome, got.Reason, sendCalls(f))
	}
}

// Without a nonce the driver answers unchecked and says so — the same rule as
// every other respond form.
func TestTextWithoutANonceSaysNothingVerifiedThePrompt(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	_, d := armFreeText(t, g)
	got := respondText(t, d, "", "chamomile")
	if got.Outcome != fleet.OutcomeSubmitted || !strings.Contains(got.Reason, "without a nonce") {
		t.Fatalf("respond = %q (%s)", got.Outcome, got.Reason)
	}
}

// ----- multi-select ---------------------------------------------------------

// The boxes are flipped to the set first, then the highlight walks Down onto
// the free-text row one key at a time (a digit there only toggles its tick),
// the text is pasted and read back, the highlight walks Up onto a checkbox
// (Right is swallowed by the field) and Right moves the dialog on. Enter is
// never pressed, and no digit is ever typed while the field has focus.
func TestTextWithChoicesTicksTheBoxesTypesTheTextAndStopsOnTheReviewScreen(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	g.ticks[1] = true // Banana is ticked; the caller wants Apple and Cherry
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "durian", 1, 3)
	if got.Outcome != fleet.OutcomeSubmitted || g.stage != 2 {
		t.Fatalf("respond = %q (%s), stage %d, keys %v", got.Outcome, got.Reason, g.stage, sendCalls(f))
	}
	if fmt.Sprint(g.ticks) != "[true false true]" || g.typed != "durian" {
		t.Errorf("ticks %v typed %q, want Apple and Cherry ticked and the text in the row", g.ticks, g.typed)
	}
	if want := "[1 2 3 Down Down Down Up Right]"; fmt.Sprint(sendCalls(f)) != want {
		t.Errorf("keys = %v, want %s — one key per call, boxes first, and never Enter", sendCalls(f), want)
	}
	if hasKey(f, "C-m") {
		t.Error("Enter was pressed on a multi-select question")
	}
	for _, w := range []string{"review screen", "NOT handed over", "6 byte(s)", "free-text row (option 4)"} {
		if !strings.Contains(got.Reason, w) {
			t.Errorf("receipt %q lacks %q", got.Reason, w)
		}
	}
	if len(pastesTo(f)) != 1 || pastesTo(f)[0] != "durian" {
		t.Errorf("pastes = %q", pastesTo(f))
	}
}

// Text with no choices on a multi-select question is the whole answer: the
// set names the end state, so a box that was ticked is cleared.
func TestTextAloneOnAMultiSelectQuestionLeavesNoBoxTicked(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana")
	g.ticks[1] = true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "durian")
	if got.Outcome != fleet.OutcomeSubmitted || fmt.Sprint(g.ticks) != "[false false]" || g.typed != "durian" {
		t.Fatalf("respond = %q (%s), ticks %v typed %q", got.Outcome, got.Reason, g.ticks, g.typed)
	}
	if !strings.Contains(got.Reason, "ticked no box") {
		t.Errorf("receipt = %q", got.Reason)
	}
}

// Inside a multi-question dialog the same walk moves on to the NEXT question,
// not to the review screen.
func TestTextOnAMultiSelectQuestionMovesToTheNextQuestion(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana")
	g.next = true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "durian", 1)
	if got.Outcome != fleet.OutcomeSubmitted || g.stage != 1 || !strings.Contains(got.Reason, "next question") {
		t.Fatalf("respond = %q (%s), stage %d", got.Outcome, got.Reason, g.stage)
	}
}

// A paste the runtime drops on a multi-select question stops the sequence
// before Right: the dialog is not moved on with an empty field, and the
// boxes already flipped are reported.
func TestTextOnAMultiSelectQuestionStopsWhenThePasteDidNotLand(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana")
	g.noPaste = true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "durian", 2)
	if got.Outcome != fleet.OutcomeUnknown || g.stage != 0 {
		t.Fatalf("respond = %q (%s), stage %d", got.Outcome, got.Reason, g.stage)
	}
	if hasKey(f, "Right") || hasKey(f, "C-m") {
		t.Errorf("keys %v: nothing may follow a paste that did not read back", sendCalls(f))
	}
}

// When the free-text row already holds text, no box, digit or paste is sent:
// the row cannot be found by its placeholder, so the prompt does not offer it.
func TestTextOnAMultiSelectQuestionIsRefusedWhenTheRowAlreadyHoldsText(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana")
	g.typed = "left by an earlier attempt"
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondText(t, d, currentNonce(t, f), "durian", 1)
	if got.Outcome != fleet.OutcomeRefused || len(sendCalls(f)) != 0 || len(pastesTo(f)) != 0 {
		t.Fatalf("respond = %q (%s), keys %v", got.Outcome, got.Reason, sendCalls(f))
	}
}

// From any row below the boxes the walk still gets to the free-text row and
// back, one press at a time.
func TestTextOnAMultiSelectQuestionFromEveryStartingRow(t *testing.T) {
	for _, start := range []int{1, 2, 3, 4, 5, 6} {
		t.Run(fmt.Sprint("from row ", start), func(t *testing.T) {
			f := twoSessions()
			g := newFakeMultiSelect("Apple", "Banana", "Cherry")
			g.sel = start
			armDialog(f, "%1", g)
			d := newTestDriver(f)

			got := respondText(t, d, currentNonce(t, f), "durian", 2)
			if got.Outcome != fleet.OutcomeSubmitted || g.stage != 2 || g.typed != "durian" {
				t.Fatalf("respond = %q (%s), stage %d typed %q, keys %v", got.Outcome, got.Reason, g.stage, g.typed, sendCalls(f))
			}
			if fmt.Sprint(g.ticks) != "[false true false]" {
				t.Errorf("ticks = %v", g.ticks)
			}
		})
	}
}

// ----- the freeText flag ----------------------------------------------------

func TestFreeTextIsReportedOnlyOnTheShapesItWasMeasuredOn(t *testing.T) {
	multiNoTabs := func() string {
		g := newFakeMultiSelect("Apple", "Banana")
		var kept []string
		for _, l := range strings.Split(g.screen(), "\n") {
			if !strings.HasPrefix(l, "←") {
				kept = append(kept, l)
			}
		}
		return strings.Join(kept, "\n")
	}
	held := oneQuestion("Tea", "Coffee")
	held.typed[0] = "typed already"
	heldMulti := newFakeMultiSelect("Apple", "Banana")
	heldMulti.typed = "typed already"
	review := newFakeFreeText(
		ftQuestion{header: "A", question: "One?", options: []string{"x", "y"}},
		ftQuestion{header: "B", question: "Two?", options: []string{"x", "y"}},
	)
	review.press("1")
	review.press("1")

	for name, tc := range map[string]struct {
		screen string
		want   bool
	}{
		"single question":                     {oneQuestion("Tea", "Coffee").screen(), true},
		"tab of a multi-question dialog":      {newFakeFreeText(ftQuestion{header: "A", question: "One?", options: []string{"x", "y"}}, ftQuestion{header: "B", question: "Two?", options: []string{"x", "y"}}).screen(), true},
		"multi-select":                        {newFakeMultiSelect("Apple", "Banana").screen(), true},
		"row already holds text":              {held.screen(), false},
		"multi-select row already holds text": {heldMulti.screen(), false},
		"review screen":                       {review.screen(), false},
		"no free-text row":                    {newFakeDialog([]string{"Yes", "No"}).screen(), false},
		"boxed list with no tab bar":          {multiNoTabs(), false},
		"unnumbered menu":                     {fixtureTrustMenuUnnumbered, false},
		"an idle composer":                    {idleFixtureFor("x"), false},
	} {
		t.Run(name, func(t *testing.T) {
			p := parsePrompt(newScreen(tc.screen))
			got := p != nil && p.FreeText
			if got != tc.want {
				t.Errorf("freeText = %v, want %v for:\n%s", got, tc.want, tc.screen)
			}
		})
	}
}

// The nonce digests the options, so typing into the row changes it: nothing
// after the first read may be judged by "same nonce", and the caller's nonce
// is checked once, up front. This pins that the row's label really is what
// moves it.
func TestTypingIntoTheFreeTextRowChangesTheNonce(t *testing.T) {
	g := oneQuestion("Tea", "Coffee")
	g.sel = 3
	before := parsePrompt(newScreen(g.screen()))
	g.paste("chamomile")
	after := parsePrompt(newScreen(g.screen()))
	if before == nil || after == nil || before.Nonce == after.Nonce {
		t.Fatalf("nonce before %v after %v; typing must change it", before, after)
	}
	if after.FreeText {
		t.Error("a row holding text still reports freeText")
	}
}
