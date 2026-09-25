package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// fakePreviewDialog models the runtime's question dialog with a preview pane
// as measured live for colab-fleet#204, and not as the numbered menus without
// one behave:
//
//   - a digit MOVES the highlight (and the pane) and commits nothing — the tab
//     bar keeps its ☐;
//   - Enter (C-m) commits the HIGHLIGHTED option and advances: to the next
//     question, to the review screen after the last, or out of the dialog on a
//     single question;
//   - of several keys sent in ONE send-keys only one registers. The measured
//     cases were `2` + Enter, which recorded the default and not 2, and
//     `Down Down Down`, which moved one row; the model keeps the last key.
//
// digitsMove=false is a runtime on which a digit does nothing at all, and
// digitsCommit=true is one where it commits as it does on a menu without a
// pane — both are the drift the receipt has to survive without a wrong answer.
type fakePreviewDialog struct {
	questions [][]string // option labels per question
	cur       int        // question on screen; len(questions) is the review screen
	sel       int        // 1-based highlighted option of the current question
	answers   []int      // 1-based option committed per question; 0 = unanswered
	submitted bool

	digitsMove   bool
	digitsCommit bool
	noHighlight  bool // paint no ❯ on any option: the highlight is off the list
}

func newFakePreviewDialog(questions ...[]string) *fakePreviewDialog {
	return &fakePreviewDialog{
		questions:  questions,
		sel:        1,
		answers:    make([]int, len(questions)),
		digitsMove: true,
	}
}

func (g *fakePreviewDialog) press(key string) {
	if g.submitted {
		return
	}
	n := 0
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		n = int(key[0] - '0')
	}
	if g.cur >= len(g.questions) {
		// The review confirm widget has no pane, and a digit commits there.
		if n == 1 || key == "C-m" {
			g.submitted = true
		}
		return
	}
	opts := g.questions[g.cur]
	commit := func(n int) {
		g.answers[g.cur] = n
		g.cur++
		g.sel = 1
		if len(g.questions) == 1 {
			g.submitted = true // a single question has no review screen
		}
	}
	switch {
	case n > 0 && n <= len(opts) && g.digitsCommit:
		commit(n)
	case n > 0 && n <= len(opts) && g.digitsMove:
		g.sel = n
	case key == "C-m":
		commit(g.sel)
	case key == "Down":
		g.sel = min(g.sel+1, len(opts))
	case key == "Up":
		g.sel = max(g.sel-1, 1)
	}
}

func (g *fakePreviewDialog) pressBurst(keys []string) { g.press(keys[len(keys)-1]) }

func (g *fakePreviewDialog) screen() string {
	if g.submitted {
		return idleFixtureFor("answered")
	}
	if g.cur >= len(g.questions) {
		var b strings.Builder
		b.WriteString("  transcript line\n" + rule + "\n")
		b.WriteString(previewTabBar(len(g.questions), g.tabs()...) + "\n\nReview your answers\n\n")
		b.WriteString(reviewQuestion + "\n\n")
		fmt.Fprintf(&b, "❯ 1. %s\n  2. %s\n", reviewOptions[0], reviewOptions[1])
		return b.String()
	}
	var options [][]string
	for _, o := range g.questions[g.cur] {
		options = append(options, []string{o})
	}
	s := previewScreen{
		prose:    previewProse,
		question: fmt.Sprintf("Pick for question %d", g.cur+1),
		options:  options,
		selected: g.sel,
		preview:  []string{"a mockup of " + g.questions[g.cur][g.sel-1]},
	}
	if g.noHighlight {
		s.selected = 0
	}
	if len(g.questions) == 1 {
		s.header = previewChip("☐ Q1")
	} else {
		s.header = previewTabBar(g.cur, g.tabs()...)
	}
	return s.String()
}

func (g *fakePreviewDialog) tabs() []string {
	var tabs []string
	for i := range g.questions {
		box := "☐"
		if g.answers[i] > 0 {
			box = "☒"
		}
		tabs = append(tabs, fmt.Sprintf("%s Q%d", box, i+1))
	}
	return append(tabs, "✔ Submit")
}

func respondOnPreview(t *testing.T, d *Driver, f *fakeMux, resp fleet.Response) fleet.DeliveryReceipt {
	t.Helper()
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, resp)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func previewNonce(t *testing.T, f *fakeMux) string {
	t.Helper()
	p := parsePrompt(newScreen(f.captures["%1"]))
	if p == nil {
		t.Fatalf("not a recognised prompt:\n%s", f.captures["%1"])
	}
	return p.Nonce
}

// colab-fleet#204. The model has to reproduce the measured failure for the
// tests below to prove anything: the old key shape — the digit and Enter in one
// send-keys — must record the default, exactly as the live run did.
func TestFakePreviewDialogReproducesTheMeasuredBurst(t *testing.T) {
	g := newFakePreviewDialog([]string{"Light", "Dark"})
	g.pressBurst([]string{"2", "C-m"})
	if g.answers[0] != 1 {
		t.Errorf("answers = %v; the measured burst recorded the highlighted default (1), not the digit", g.answers)
	}
	g = newFakePreviewDialog([]string{"A", "B", "C", "D"})
	g.pressBurst([]string{"Down", "Down", "Down"})
	if g.sel != 2 {
		t.Errorf("highlight = %d after a burst of three Down; the measured run moved one row (to 2)", g.sel)
	}
	g = newFakePreviewDialog([]string{"A", "B"})
	g.press("2")
	if g.sel != 2 || g.answers[0] != 0 {
		t.Errorf("a digit alone: highlight %d, answers %v; it must only move the highlight", g.sel, g.answers)
	}
}

// The measured behaviour, end to end through Respond: two questions, each
// answered with the option chosen — not the default, and not the neighbour.
func TestRespondOnAPreviewPaneMovesTheHighlightThenConfirms(t *testing.T) {
	f := twoSessions()
	g := newFakePreviewDialog([]string{"Red", "Green", "Blue"}, []string{"Cat", "Dog", "Fish", "Bird"})
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	first := respondOnPreview(t, d, f, fleet.Response{Choice: 2, Nonce: previewNonce(t, f)})
	if first.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("first respond = %q (%s), want submitted", first.Outcome, first.Reason)
	}
	if g.cur != 1 || fmt.Sprint(g.answers) != "[2 0]" {
		t.Fatalf("after one respond: on question %d, answers %v; want question 2 and [2 0]", g.cur+1, g.answers)
	}
	if !strings.Contains(first.Reason, "Green") || !strings.Contains(first.Reason, "question 1 of 2") {
		t.Errorf("receipt = %q; must name the option and which question it answered", first.Reason)
	}

	second := respondOnPreview(t, d, f, fleet.Response{Choice: 4, Nonce: previewNonce(t, f)})
	if second.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("second respond = %q (%s), want submitted", second.Outcome, second.Reason)
	}
	if fmt.Sprint(g.answers) != "[2 4]" {
		t.Errorf("answers = %v, want [2 4]", g.answers)
	}
	if !strings.Contains(second.Reason, "Bird") || !strings.Contains(second.Reason, "question 2 of 2") {
		t.Errorf("receipt = %q", second.Reason)
	}

	// Each key its own call, the digit first and one Enter after it — never
	// two keys in one send-keys, which loses one of them.
	want := "[[2] [C-m] [4] [C-m]]"
	if got := fmt.Sprint(keySends(f)); got != want {
		t.Errorf("key sends = %s, want %s", got, want)
	}

	// The review screen is not a pane, and there a digit commits.
	third := respondOnPreview(t, d, f, fleet.Response{Choice: 1, Nonce: previewNonce(t, f)})
	if third.Outcome != fleet.OutcomeSubmitted || !g.submitted {
		t.Errorf("review screen: %q (%s), submitted=%v", third.Outcome, third.Reason, g.submitted)
	}
}

// A single-question dialog has a chip and no tab bar, and Enter closes it.
func TestRespondOnASingleQuestionPreviewPaneClosesTheDialog(t *testing.T) {
	f := twoSessions()
	g := newFakePreviewDialog([]string{"One", "Two", "Three"})
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondOnPreview(t, d, f, fleet.Response{Choice: 3, Nonce: previewNonce(t, f)})
	if got.Outcome != fleet.OutcomeSubmitted || !g.submitted || g.answers[0] != 3 {
		t.Fatalf("respond = %q (%s); submitted=%v answers=%v", got.Outcome, got.Reason, g.submitted, g.answers)
	}
	if strings.Contains(got.Reason, "question 1 of 1") {
		t.Errorf("receipt = %q; naming 'question 1 of 1' on a dialog with one question says nothing", got.Reason)
	}
}

// The highlight is already on the chosen option, or the caller accepts the
// highlighted one: no digit, and the confirm goes alone.
func TestRespondOnAPreviewPaneNeedsNoDigitWhereTheHighlightAlreadySits(t *testing.T) {
	cases := map[string]fleet.Response{
		"choice equals the highlight": {Choice: 2},
		"accept the highlighted":      {},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			g := newFakePreviewDialog([]string{"One", "Two", "Three"})
			g.sel = 2
			armDialog(f, "%1", g)
			d := newTestDriver(f)
			resp.Nonce = previewNonce(t, f)

			got := respondOnPreview(t, d, f, resp)
			if got.Outcome != fleet.OutcomeSubmitted || g.answers[0] != 2 {
				t.Fatalf("respond = %q (%s); answers=%v", got.Outcome, got.Reason, g.answers)
			}
			if sends := keySends(f); fmt.Sprint(sends) != "[[C-m]]" {
				t.Errorf("key sends = %v, want the confirm alone", sends)
			}
		})
	}
}

// A runtime on which the digit does not move the highlight: Enter would then
// commit whichever row it IS on, which is not the one chosen. Nothing may be
// confirmed, and the receipt must say so.
func TestRespondOnAPreviewPaneConfirmsNothingWhenTheHighlightDoesNotArrive(t *testing.T) {
	f := twoSessions()
	g := newFakePreviewDialog([]string{"One", "Two", "Three"})
	g.digitsMove = false
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondOnPreview(t, d, f, fleet.Response{Choice: 3, Nonce: previewNonce(t, f)})
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "No confirm key was sent") {
		t.Errorf("receipt = %q; must say nothing was confirmed", got.Reason)
	}
	if sends := keySends(f); fmt.Sprint(sends) != "[[3]]" {
		t.Errorf("key sends = %v, want the digit alone", sends)
	}
	if g.answers[0] != 0 {
		t.Errorf("answers = %v; nothing should have been committed", g.answers)
	}
}

// The other drift: a runtime where the digit commits (as it does on a menu
// with no pane). The dialog has moved on, so the highlight is never read
// back on the chosen row — and pressing Enter now would answer the NEXT
// question with its default. It must not be sent.
func TestRespondOnAPreviewPaneNeverConfirmsAQuestionItDidNotAsk(t *testing.T) {
	f := twoSessions()
	g := newFakePreviewDialog([]string{"Red", "Green"}, []string{"Cat", "Dog"})
	g.digitsCommit = true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondOnPreview(t, d, f, fleet.Response{Choice: 2, Nonce: previewNonce(t, f)})
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if fmt.Sprint(g.answers) != "[2 0]" {
		t.Errorf("answers = %v; the second question must be left unanswered", g.answers)
	}
	if sends := keySends(f); fmt.Sprint(sends) != "[[2]]" {
		t.Errorf("key sends = %v, want the digit alone", sends)
	}
}

// With the highlight on no option — on a row below the list — a digit could be
// typed into a field, and there is nothing highlighted to accept.
func TestRespondOnAPreviewPaneRefusesWhenTheHighlightIsOffTheList(t *testing.T) {
	for name, resp := range map[string]fleet.Response{"a choice": {Choice: 2}, "accepting the highlighted": {}} {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			g := newFakePreviewDialog([]string{"One", "Two", "Three"})
			g.noHighlight = true
			armDialog(f, "%1", g)
			d := newTestDriver(f)
			if p, shape := parsePromptShape(newScreen(f.captures["%1"])); p == nil || !shape.preview || p.Selected != 0 {
				t.Fatalf("fixture is not a pane prompt with no highlight: %+v %+v", p, shape)
			}
			resp.Nonce = previewNonce(t, f)

			got := respondOnPreview(t, d, f, resp)
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("respond = %q (%s), want refused", got.Outcome, got.Reason)
			}
			if sends := keySends(f); len(sends) != 0 {
				t.Errorf("keys were sent to a refused respond: %v", sends)
			}
		})
	}
}

// Cancelling is one key on every layout, and a preview pane changes nothing
// about it.
func TestRespondCancelsAPreviewPaneWithOneKey(t *testing.T) {
	f := twoSessions()
	g := newFakePreviewDialog([]string{"One", "Two"})
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	respondOnPreview(t, d, f, fleet.Response{Cancel: true, Nonce: previewNonce(t, f)})
	if sends := keySends(f); fmt.Sprint(sends) != "[[Escape]]" {
		t.Errorf("key sends = %v, want a single Escape", sends)
	}
}
