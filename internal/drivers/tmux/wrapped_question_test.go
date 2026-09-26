package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet#219: a multi-select question tall enough to outgrow the window
// the classifier reads (promptScanDepth rows from the bottom) was reported
// without multiSelect — and a wrapped question kept its `│` rail in the text.
//
// The height is what broke the first, not the rail: a question that wraps over
// many rows, or a description under every option, pushes the dialog's tab bar —
// or its first option — above the window, and the tab bar is the one thing that
// tells a checkbox question from an agent's own `[ ]` list. The rail is what
// the runtime draws a WRAPPED question with, so it is where tall dialogs come
// from, but a tall dialog without one reads the same way.

const wrappedTabBar = "←  ☐ Q1 pick  ✔ Submit  →"

// wrappedMultiSelect is a single-question multi-select dialog of four boxes,
// drawn the way the runtime draws one whose question does not fit a row: the
// question is questionRows rows, each behind the rail when rail is set, and
// every option carries descRows rows of description.
func wrappedMultiSelect(rail bool, questionRows, descRows int) string {
	var b strings.Builder
	b.WriteString("  transcript line\n" + rule + "\n" + wrappedTabBar + "\n\n")
	for i := 1; i <= questionRows; i++ {
		lead := ""
		if rail {
			lead = "│ "
		}
		fmt.Fprintf(&b, "%squestion row %d\n", lead, i)
	}
	b.WriteString("\n")
	for i, name := range []string{"Alpha", "Bravo", "Charlie", "Delta"} {
		mark := "  "
		if i == 0 {
			mark = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. [ ] %s\n", mark, i+1, name)
		for d := 1; d <= descRows; d++ {
			fmt.Fprintf(&b, "         description row %d\n", d)
		}
	}
	b.WriteString("  5. [ ] Type something\n     Submit\n" + rule + "\n  6. Chat about this\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel")
	return b.String()
}

// depthOf is how many rows above the bottom of the classifier's screen the
// first row containing needle sits, counting the bottom row as 1. A row deeper
// than promptScanDepth is one the fixed window does not reach.
func depthOf(t *testing.T, screen, needle string) int {
	t.Helper()
	lines := newScreen(screen).lines
	for i, l := range lines {
		if strings.Contains(l, needle) {
			return len(lines) - i
		}
	}
	t.Fatalf("no row contains %q", needle)
	return 0
}

func TestAMultiSelectQuestionTallerThanTheWindowIsStillRecognised(t *testing.T) {
	cases := []struct {
		name                      string
		rail                      bool
		questionRows, descRows    int
		headerOut, firstOptionOut bool // what the case must exercise, asserted below
		wantQuestion              string
	}{
		{name: "the issue's shape: a rail, fits the window", rail: true, questionRows: 3, descRows: 1,
			wantQuestion: "question row 1 question row 2 question row 3"},
		{name: "a wrapped question pushes the tab bar above the window", rail: true, questionRows: 10, descRows: 1,
			headerOut: true, wantQuestion: wrappedQuestion(1, 10)},
		{name: "the same height without a rail reads the same", rail: false, questionRows: 10, descRows: 1,
			headerOut: true, wantQuestion: wrappedQuestion(1, 10)},
		{name: "descriptions push the first option above the window", rail: true, questionRows: 3, descRows: 4,
			headerOut: true, firstOptionOut: true, wantQuestion: "question row 1 question row 2 question row 3"},
		{name: "a question the size of the pane", rail: true, questionRows: 30, descRows: 4,
			headerOut: true, firstOptionOut: true, wantQuestion: wrappedQuestion(1, 30)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			screen := wrappedMultiSelect(c.rail, c.questionRows, c.descRows)

			// The case must be the one it names, or it proves nothing about the window.
			if got := depthOf(t, screen, "←") > promptScanDepth; got != c.headerOut {
				t.Fatalf("tab bar beyond the window = %v, want %v: the case does not exercise what it names", got, c.headerOut)
			}
			if got := depthOf(t, screen, "1. [ ] Alpha") > promptScanDepth; got != c.firstOptionOut {
				t.Fatalf("first option beyond the window = %v, want %v: the case does not exercise what it names", got, c.firstOptionOut)
			}

			p := parsePrompt(newScreen(screen))
			if p == nil {
				t.Fatal("not recognised as a prompt at all")
			}
			if !p.MultiSelect {
				t.Fatalf("multiSelect = false; options = %q", p.Options)
			}
			if !p.FreeText {
				t.Error("freeText = false; the row directly after the boxes is the free-text row")
			}
			if got := multiSelectBoxes(p); got != 4 {
				t.Errorf("boxes = %d, want 4", got)
			}
			if strings.Contains(p.Question, "│") {
				t.Errorf("question keeps the rail: %q", p.Question)
			}
			if p.Question != c.wantQuestion {
				t.Errorf("question = %q, want %q", p.Question, c.wantQuestion)
			}
			if p.Selected != 1 {
				t.Errorf("selected = %d, want 1", p.Selected)
			}
			// The header is part of the nonce, and only found when the window
			// reaches it: a nonce without it is a prompt that answers to the wrong
			// question when another dialog is drawn with the same options.
			want := promptNonceWithHeader(&fleet.SessionPrompt{Question: p.Question, Options: p.Options}, wrappedTabBar, tabPosition{})
			if p.Nonce != want {
				t.Error("the nonce does not carry the dialog's header")
			}
		})
	}
}

// The window is widened to the dialog's opening rule and no further: the prose
// above a dialog is the agent's, and a numbered list in it is not an option.
func TestTheWindowStopsAtTheDialogsOpeningRule(t *testing.T) {
	screen := "  1. first, read the issue\n  2. then write the test\n  3. then fix it\n" + wrappedMultiSelect(true, 10, 1)
	p := parsePrompt(newScreen(screen))
	if p == nil || !p.MultiSelect {
		t.Fatalf("not read as multi-select: %+v", p)
	}
	want := "[ ] Alpha [ ] Bravo [ ] Charlie [ ] Delta [ ] Type something Chat about this"
	if got := strings.Join(p.Options, " "); got != want {
		t.Errorf("options = %q\nwant      %q", got, want)
	}
}

// Every shape that resembles the tall dialog and lacks its tab bar directly
// under its own opening rule must still NOT read as multi-select: the flag
// licenses a keystroke sequence, and widening the window is what could have
// carried a stray tab bar into it.
func TestWideningTheWindowDoesNotInventAMultiSelect(t *testing.T) {
	tall := wrappedMultiSelect(true, 10, 1)
	noHeader := strings.Replace(tall, wrappedTabBar+"\n", "", 1)
	cases := map[string]string{
		"a tall boxed list with no tab bar":                  noHeader,
		"a tab bar in the prose above the dialog's own rule": wrappedTabBar + "\n" + rule + "\n" + noHeader,
		"a tab bar on the far side of an inner rule": strings.Replace(tall, "\n\n│ question row 1\n",
			"\n\n"+rule+"\n│ question row 1\n", 1),
	}
	for name, screen := range cases {
		t.Run(name, func(t *testing.T) {
			if depthOf(t, screen, "1. [ ] Alpha") > promptScanDepth {
				t.Log("first option is beyond the fixed window: the widening is what is being tested")
			}
			if p := parsePrompt(newScreen(screen)); p != nil && p.MultiSelect {
				t.Fatalf("read as multi-select: options = %q", p.Options)
			}
		})
	}
}

// A window that already reaches a rule above the first option is not widened,
// and neither is a screen that has nothing to read.
func TestDialogTopLeavesAWindowThatAlreadyReachesTheDialog(t *testing.T) {
	short := newScreen(wrappedMultiSelect(true, 3, 1)).lines
	if from := max(len(short)-promptScanDepth, 0); dialogTop(short, from) != from {
		t.Error("a dialog that fits the window moved the window")
	}
	idle := newScreen(strings.Repeat("  transcript line\n", 40) + rule + "\n❯ <input>\n" + rule).lines
	if from := len(idle) - promptScanDepth; dialogTop(idle, from) != from {
		t.Error("a screen with no option and no footer moved the window")
	}
}

func TestQuestionRowSetsAsideTheRailAndOnlyTheRail(t *testing.T) {
	for in, want := range map[string]string{
		"│ Which fruits do you like?":  "Which fruits do you like?",
		"  │   indented past the rail": "indented past the rail",
		"│":                            "", // the rail alone, between two paragraphs
		"Which fruits │ do you like?":  "Which fruits │ do you like?",
		"no rail at all":               "no rail at all",
	} {
		if got := questionRow(in); got != want {
			t.Errorf("questionRow(%q) = %q, want %q", in, got, want)
		}
	}

	// A rail-only row is not an empty row of the question: it adds no gap to the text.
	screen := strings.Replace(wrappedMultiSelect(true, 3, 1), "│ question row 2\n", "│\n│ question row 2\n", 1)
	if p := parsePrompt(newScreen(screen)); p == nil || p.Question != "question row 1 question row 2 question row 3" {
		t.Fatalf("question = %+v", p)
	}
}

// colab-fleet#219, end to end through respond: the tall dialog answers to
// `choices` like the one-row question of #176 — every flip read back, one Right,
// and a stop on the review screen.
func TestChoicesOnAWrappedQuestionTallerThanTheWindow(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry", "Date")
	for i := 1; i <= 10; i++ {
		g.question = append(g.question, fmt.Sprintf("Which fruits do you like? (row %d of the wrapped question)", i))
	}
	g.ticks[1] = true // Banana already ticked; the caller wants Apple and Cherry
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	screen := f.captures["%1"]
	if depthOf(t, screen, "←") <= promptScanDepth {
		t.Fatal("the tab bar is inside the fixed window: this dialog does not exercise the widening")
	}
	if p := parsePrompt(newScreen(screen)); p == nil || !p.MultiSelect || strings.Contains(p.Question, "│") {
		t.Fatalf("prompt = %+v; want multiSelect with a rail-free question", p)
	}

	got := respondChoices(t, d, f, currentNonce(t, f), 1, 3)
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
	}
	if want := "[1 2 3 Right]"; fmt.Sprint(sendCalls(f)) != want {
		t.Errorf("key sends = %v, want %s — each flip its own call, then one Right", sendCalls(f), want)
	}
	if fmt.Sprint(g.ticks) != "[true false true false]" {
		t.Errorf("ticks = %v, want exactly Apple and Cherry", g.ticks)
	}
	if g.stage != 2 {
		t.Fatalf("stage = %d, want the review screen — choices must never hand the answers over", g.stage)
	}
}

// colab-fleet#219, live: a real multiplexer, its real capture, and the driver's
// own State and Respond against a synthetic dialog whose wrapped question puts
// the tab bar beyond the window. Gated like the other live tests here
// (FLEET_TMUX_INTEGRATION=1); the pane runs testdata/multiselect_tui.py, not the
// runtime.
func TestLiveWrappedMultiSelectIsReadAndAnswered(t *testing.T) {
	m := newLiveMux(t)
	logf := filepath.Join(t.TempDir(), "answers.log")
	m.run("new-session", "-d", "-x", "110", "-y", "50", "-s", "askpane",
		"env FAKE_LOG="+logf+" python3 "+testdataPath(t, "multiselect_tui.py"))

	d := New("livebox", WithBinary(m.wrapper))
	ref := fleet.SessionRef{Machine: "livebox", ID: "askpane"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var prompt *fleet.SessionPrompt
	deadline := time.Now().Add(15 * time.Second)
	for prompt == nil {
		st, err := d.State(ctx, testCaller, ref)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		prompt = st.Prompt
		if prompt == nil {
			if time.Now().After(deadline) {
				t.Fatalf("the dialog was never read as a prompt; state = %s", st.Status)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// The capture must really hold a tab bar the fixed window cannot reach, or
	// the run proves nothing about the widening.
	if pane := m.run("capture-pane", "-p", "-t", "askpane"); depthOf(t, pane, "←") <= promptScanDepth {
		t.Fatalf("the tab bar is inside the fixed window on this pane:\n%s", pane)
	}
	if !prompt.MultiSelect || !prompt.FreeText {
		t.Fatalf("prompt = multiSelect %v, freeText %v (options %q); want both", prompt.MultiSelect, prompt.FreeText, prompt.Options)
	}
	if strings.Contains(prompt.Question, "│") {
		t.Errorf("question keeps the rail: %q", prompt.Question)
	}
	// colab-fleet#220: the whole wrapped question reaches the reader — all ten
	// rows the dialog draws, not the last three of them.
	var wantRows []string
	for i := 1; i <= 10; i++ {
		wantRows = append(wantRows, fmt.Sprintf("row %d of a question long enough that the runtime wraps it", i))
	}
	if want := strings.Join(wantRows, " "); prompt.Question != want {
		t.Errorf("question = %q\nwant       %q", prompt.Question, want)
	}

	got, err := d.Respond(ctx, testCaller, ref, fleet.Response{Choices: []int{1, 3}, Nonce: prompt.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %s (%s), want submitted", got.Outcome, got.Reason)
	}

	// choices stops on the review screen; confirming it is the caller's own answer.
	st, err := d.State(ctx, testCaller, ref)
	if err != nil || st.Prompt == nil {
		t.Fatalf("after choices: %v, prompt %+v; want the review screen", err, st.Prompt)
	}
	if fin, err := d.Respond(ctx, testCaller, ref, fleet.Response{Choice: 1, Nonce: st.Prompt.Nonce}); err != nil || fin.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("review answer = %+v, %v", fin, err)
	}
	time.Sleep(300 * time.Millisecond)
	logged, _ := os.ReadFile(logf)
	if !strings.Contains(string(logged), `{"answer": ["Alpha", "Charlie"]}`) {
		t.Fatalf("answers logged = %q, want exactly Alpha and Charlie", logged)
	}
}
