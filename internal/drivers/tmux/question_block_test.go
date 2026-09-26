package tmux

import (
	"fmt"
	"strings"
	"testing"
)

// colab-fleet#220: the parser kept the last three rows above the options as the
// question, a bound written when the rows above a dialog could be transcript.
// Once the dialog's header row is found nothing between it and the options can
// be, so the bound only cut a question long enough to wrap over more than three
// rows — and reported it starting mid-sentence.
//
// The rule these tests pin: a question whose start is KNOWN (the header was
// found) is reported whole, up to maxQuestionRows, keeping the rows nearest the
// options when it is longer; a question whose start is NOT known (no header)
// keeps the last unanchoredQuestionRows, as before.

// wrappedQuestion is the text wrappedMultiSelect draws as rows first..last of
// its question, joined the way the parser joins rows.
func wrappedQuestion(first, last int) string {
	rows := make([]string, 0, last-first+1)
	for i := first; i <= last; i++ {
		rows = append(rows, fmt.Sprintf("question row %d", i))
	}
	return strings.Join(rows, " ")
}

func TestAQuestionUnderItsHeaderIsReportedWhole(t *testing.T) {
	for _, rail := range []bool{true, false} {
		// 4 is the smallest height the old bound cut; 30 is the height the issue
		// measured; maxQuestionRows is the largest one reported without loss.
		for _, rows := range []int{1, 3, 4, 10, 24, 30, maxQuestionRows} {
			t.Run(fmt.Sprintf("rail=%v rows=%d", rail, rows), func(t *testing.T) {
				p := parsePrompt(newScreen(wrappedMultiSelect(rail, rows, 4)))
				if p == nil {
					t.Fatal("not recognised as a prompt at all")
				}
				if want := wrappedQuestion(1, rows); p.Question != want {
					t.Errorf("question = %q\nwant       %q", p.Question, want)
				}
				if !p.MultiSelect {
					t.Errorf("multiSelect = false; a whole question must not cost the flag")
				}
			})
		}
	}
}

// A question longer than the cap keeps the rows NEAREST the options. The ask
// sits at the end of a question, and the review screen is recognised by its
// closing line (reviewScreenPrompt), so the tail is what must survive.
func TestAQuestionPastTheCapKeepsItsLastRows(t *testing.T) {
	const extra = 8
	rows := maxQuestionRows + extra
	p := parsePrompt(newScreen(wrappedMultiSelect(true, rows, 1)))
	if p == nil || !p.MultiSelect {
		t.Fatalf("prompt = %+v; want a multi-select prompt", p)
	}
	if want := wrappedQuestion(extra+1, rows); p.Question != want {
		t.Errorf("question = %q\nwant       %q", p.Question, want)
	}
	if got := len(strings.Split(p.Question, " question row ")); got != maxQuestionRows {
		t.Errorf("question holds %d rows, want the cap, %d", got, maxQuestionRows)
	}
}

// Without a header the start is not known: the rows above a permission dialog
// are the transcript it was drawn under, and only the last few are its ask.
func TestAQuestionWithNoHeaderKeepsOnlyItsLastRows(t *testing.T) {
	screen := "  transcript 1\n  transcript 2\n  transcript 3\n  transcript 4\n  Do you want to proceed?\n" +
		"  ❯ 1. Yes\n    2. No\n\n  Esc to cancel · Tab to amend"
	p := parsePrompt(newScreen(screen))
	if p == nil {
		t.Fatal("not recognised as a prompt at all")
	}
	if want := "transcript 3 transcript 4 Do you want to proceed?"; p.Question != want {
		t.Errorf("question = %q, want %q", p.Question, want)
	}
}

// question and nonce digest the same rows, and both must read the same on every
// capture of the same screen: the capture is the pane plus a margin of history
// that comes and goes, and none of it is the dialog. Whole or cut, the question
// must not depend on how much history is above the dialog's opening rule.
func TestTheQuestionDoesNotDependOnTheHistoryAboveTheDialog(t *testing.T) {
	for _, rows := range []int{3, 10, 30, maxQuestionRows + 8} {
		t.Run(fmt.Sprintf("rows=%d", rows), func(t *testing.T) {
			dialog := wrappedMultiSelect(true, rows, 2)
			var question, nonce string
			for _, history := range []int{0, 1, 7, 19, 40} {
				screen := strings.Repeat("  history row\n", history) + dialog
				p := parsePrompt(newScreen(screen))
				if p == nil || !p.MultiSelect {
					t.Fatalf("history %d: prompt = %+v; want a multi-select prompt", history, p)
				}
				if question == "" {
					question, nonce = p.Question, p.Nonce
					continue
				}
				if p.Question != question {
					t.Errorf("history %d: question = %q\nwant              %q", history, p.Question, question)
				}
				if p.Nonce != nonce {
					t.Errorf("history %d: nonce = %s, want %s", history, p.Nonce, nonce)
				}
			}
		})
	}
}

// The review screen is recognised by the line that closes its question. Reading
// its whole block must keep it recognised, and put the answers a person is about
// to submit in front of them.
func TestTheReviewScreensWholeBlockStillEndsOnItsConfirmation(t *testing.T) {
	p := parsePrompt(newScreen(fixtureReviewScreen))
	if p == nil || !reviewScreenPrompt(p) {
		t.Fatalf("prompt = %+v; want the review screen", p)
	}
	want := "Review your answers ● Which approach? → The first one ● And the name? → Keep it Ready to submit your answers?"
	if p.Question != want {
		t.Errorf("question = %q\nwant       %q", p.Question, want)
	}
}
