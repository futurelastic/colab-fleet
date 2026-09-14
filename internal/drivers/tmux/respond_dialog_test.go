package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// fakeDialog models the runtime's numbered menus as measured live for
// colab-fleet#168, rather than as this driver once assumed them to be:
//
//   - a digit alone COMMITS the answer — no confirm key is needed — on a
//     tabbed question, on the review confirm widget, and on a single-question
//     menu;
//   - on a tabbed question that commit also ADVANCES to the next tab;
//   - C-m commits the HIGHLIGHTED option (the first, here) and advances the
//     same way, which is what turned the old digit+C-m pair into an answer to
//     a question nobody chose for.
//
// The measured burst went further than this model (the digit itself was lost
// and the default recorded in its place); the model keeps the simpler
// sequential reading, which already fails the old key shape.
type fakeDialog struct {
	questions [][]string // option labels per question; the first is highlighted
	cur       int        // index of the question on screen; len(questions) = review
	answers   []int      // 1-based option chosen per question; 0 = unanswered
	submitted bool
}

func newFakeDialog(questions ...[]string) *fakeDialog {
	return &fakeDialog{questions: questions, answers: make([]int, len(questions))}
}

func (g *fakeDialog) press(key string) {
	if g.submitted {
		return
	}
	n := 0
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		n = int(key[0] - '0')
	}
	if g.cur >= len(g.questions) {
		// The review confirm widget: option 1 is "Submit answers".
		if n == 1 || key == "C-m" {
			g.submitted = true
		}
		return
	}
	switch {
	case n > 0 && n <= len(g.questions[g.cur]):
		g.answers[g.cur] = n
	case key == "C-m":
		g.answers[g.cur] = 1
	default:
		return
	}
	g.cur++
	if len(g.questions) == 1 && g.cur == 1 {
		// A single-question menu has no review screen: the answer is final.
		g.submitted = true
	}
}

func (g *fakeDialog) screen() string {
	if g.submitted {
		return idleFixtureFor("answered")
	}
	var b strings.Builder
	b.WriteString("  transcript line\n" + rule + "\n")
	if len(g.questions) > 1 {
		b.WriteString("←")
		for i := range g.questions {
			box := "☐"
			if g.answers[i] > 0 {
				box = "☒"
			}
			fmt.Fprintf(&b, "  %s Q%d", box, i+1)
		}
		b.WriteString("  ✔ Submit  →\n")
	}
	if g.cur >= len(g.questions) {
		b.WriteString("Review your answers\n\n" + reviewQuestion + "\n\n")
		fmt.Fprintf(&b, "❯ 1. %s\n  2. %s\n", reviewOptions[0], reviewOptions[1])
		return b.String()
	}
	fmt.Fprintf(&b, "Pick for question %d\n", g.cur+1)
	for i, o := range g.questions[g.cur] {
		marker := "  "
		if i == 0 {
			marker = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. %s\n", marker, i+1, o)
	}
	b.WriteString(rule + "\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel")
	return b.String()
}

// sendKeysPane returns the -t target of a send-keys invocation.
func sendKeysPane(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-t" {
			return argv[i+1]
		}
	}
	return ""
}

func armDialog(f *fakeMux, pane string, g *fakeDialog) {
	if f.dialog == nil {
		f.dialog = map[string]*fakeDialog{}
	}
	f.dialog[pane] = g
	f.captures[pane] = g.screen()
}

func respondWithNonce(t *testing.T, d *Driver, f *fakeMux, choice int) fleet.DeliveryReceipt {
	t.Helper()
	p := parsePrompt(newScreen(f.captures["%1"]))
	if p == nil {
		t.Fatalf("fixture is not a recognised prompt:\n%s", f.captures["%1"])
	}
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.Response{Choice: choice, Nonce: p.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func newlineSends(f *fakeMux) [][]string {
	var out [][]string
	for _, c := range f.callsSnapshot() {
		if c[0] != "send-keys" {
			continue
		}
		for _, k := range sentKeys(c) {
			if isNewlineKey(k) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// colab-fleet#168: two respond calls on a three-question tabbed dialog must
// answer exactly the two questions whose nonces were quoted, with exactly the
// options chosen. With the old digit+C-m pair the first call's C-m answered
// question 2 with its default and moved the dialog to question 3 — the
// caller, quoting question 3's nonce believing it was question 2, then
// answered the wrong one, and every receipt said "submitted".
func TestRespondAnswersOnlyTheQuotedQuestionOnATabbedDialog(t *testing.T) {
	f := twoSessions()
	g := newFakeDialog(
		[]string{"Red", "Green", "Blue", "Type something."},
		[]string{"Cat", "Dog", "Fish", "Type something."},
		[]string{"Rice", "Bread", "Soup", "Type something."},
	)
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	first := respondWithNonce(t, d, f, 2)
	if first.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("first respond = %q (%s), want submitted", first.Outcome, first.Reason)
	}
	if g.cur != 1 {
		t.Fatalf("after one respond the dialog is on question %d, want 2 — a second "+
			"key answered a question nobody chose for; answers = %v", g.cur+1, g.answers)
	}
	if !strings.Contains(first.Reason, "Green") {
		t.Errorf("receipt = %q; must name the option chosen on the quoted question", first.Reason)
	}

	second := respondWithNonce(t, d, f, 3)
	if second.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("second respond = %q (%s), want submitted", second.Outcome, second.Reason)
	}
	if want := []int{2, 3, 0}; fmt.Sprint(g.answers) != fmt.Sprint(want) {
		t.Errorf("answers = %v, want %v — question 3 must be left for the caller to answer", g.answers, want)
	}
	if !strings.Contains(second.Reason, "Fish") {
		t.Errorf("receipt = %q; must name question 2's option 3, the one the quoted nonce belongs to", second.Reason)
	}
	if sends := newlineSends(f); len(sends) != 0 {
		t.Errorf("a confirm key was sent on a menu the digit had already answered: %v", sends)
	}
}

// The review screen and a single-question menu commit on the digit too —
// measured — so neither may receive a trailing confirm either. On the
// single-question menu that confirm has nowhere to go but the composer the
// session returns to.
func TestRespondSendsNoConfirmWhereTheDigitCommits(t *testing.T) {
	cases := map[string]*fakeDialog{
		"single question": newFakeDialog([]string{"Tea", "Coffee", "Water", "Type something."}),
		"review screen": func() *fakeDialog {
			g := newFakeDialog([]string{"A", "B"}, []string{"C", "D"})
			g.answers = []int{1, 2}
			g.cur = 2
			return g
		}(),
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			armDialog(f, "%1", g)
			d := newTestDriver(f)
			got := respondWithNonce(t, d, f, 1)
			if got.Outcome != fleet.OutcomeSubmitted {
				t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
			}
			if !g.submitted {
				t.Fatal("the dialog was not answered")
			}
			if sends := newlineSends(f); len(sends) != 0 {
				t.Errorf("a confirm key was sent after the digit had committed: %v", sends)
			}
		})
	}
}

// The other half: a menu on which the digit changes nothing (the plain fake —
// no dialog model armed). respond must press exactly one key and report
// unknown. Neither follow-up is safe: a re-sent digit answers whatever
// replaces the question once the first repaints, and a lone confirm accepts
// the highlighted option, which here is not necessarily the one chosen.
func TestRespondPressesTheDigitOnceWhenThePromptStays(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = fixtureTrustPrompt
	d := newTestDriver(f)

	got := respondWithNonce(t, d, f, 2)

	var sends [][]string
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			sends = append(sends, sentKeys(c))
		}
	}
	if len(sends) != 1 || fmt.Sprint(sends[0]) != "[2]" {
		t.Fatalf("key sends = %v, want the digit alone, exactly once", sends)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown — the prompt never left", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "No, continue without these permissions") {
		t.Errorf("receipt = %q; must still name the option pressed on the quoted prompt", got.Reason)
	}
}
