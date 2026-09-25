package tmux

import (
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Escapes as the runtime painted them on the measured build: the current tab
// is a dark foreground on a background colour, and nothing else marks it.
const (
	previewBG    = "\x1b[38;5;16m\x1b[48;5;153m"
	previewBGOff = "\x1b[39m\x1b[49m"
)

// previewScreen renders a question dialog with a preview pane the way one
// runtime build was measured drawing it (colab-fleet#204): the list column and
// the box side by side, the box's top border on the first option's row, its
// left edge in one column all the way down, and below it the notes hint, a
// rule, the chat row and the footer.
//
// The list column is a width in SCREEN cells, so a wide character takes two —
// a label in Japanese leaves fewer spaces before the box than one in ASCII,
// which is the case a column-number reading of the pane gets wrong.
type previewScreen struct {
	prose    []string   // transcript rows above the dialog's opening rule
	header   string     // the header row as painted, escapes included; "" for none
	question string     // the question row
	options  [][]string // per option, the rows of its cell: the label, then any wrapped rows
	selected int        // 1-based highlighted option; 0 puts the highlight on no option
	preview  []string   // rows inside the box; one starting with "├" is a whole divider row

	listCells  int // width of the list column in cells; 0 means 34
	innerWidth int // cells between the box's corners; 0 means 42
}

// cellWidth counts screen cells: East Asian wide characters take two.
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) ||
			(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xff00 && r <= 0xff60)) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// previewDivider is the row the runtime paints where it clamped a pane.
func previewDivider(innerWidth, hidden int) string {
	text := fmt.Sprintf("─── ✂ ─── %d lines hidden ", hidden)
	return "├" + text + strings.Repeat("─", innerWidth-cellWidth(text)) + "┤"
}

func (p previewScreen) String() string {
	list, inner := p.listCells, p.innerWidth
	if list == 0 {
		list = 34
	}
	if inner == 0 {
		inner = 42
	}
	var left []string
	for i, rows := range p.options {
		for j, text := range rows {
			if j > 0 {
				left = append(left, "    "+text)
				continue
			}
			marker := "  "
			if i+1 == p.selected {
				marker = "❯ "
			}
			left = append(left, fmt.Sprintf("%s%d. %s", marker, i+1, text))
		}
	}
	box := []string{"┌" + strings.Repeat("─", inner) + "┐"}
	for _, row := range p.preview {
		if strings.HasPrefix(row, "├") {
			box = append(box, row)
			continue
		}
		box = append(box, "│ "+row+strings.Repeat(" ", max(inner-1-cellWidth(row), 0))+"│")
	}
	box = append(box, "└"+strings.Repeat("─", inner)+"┘")

	var b strings.Builder
	for _, l := range p.prose {
		b.WriteString(l + "\n")
	}
	b.WriteString(rule + "\n")
	if p.header != "" {
		b.WriteString(p.header + "\n")
	}
	b.WriteString("\n" + p.question + "\n\n")
	for k := 0; k < max(len(left), len(box)); k++ {
		l := ""
		if k < len(left) {
			l = left[k]
		}
		if k < len(box) {
			l += strings.Repeat(" ", max(list-cellWidth(l), 2)) + box[k]
		}
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + strings.Repeat(" ", list) + "Notes: press n to add notes\n\n" + rule +
		"\n  Chat about this\n\nEnter to select · ↑/↓ to navigate · n to add notes · Esc to cancel")
	return b.String()
}

// previewTabBar paints a tab bar as measured: each tab padded by a space on
// both sides, the current one on a background colour.
func previewTabBar(current int, tabs ...string) string {
	var b strings.Builder
	b.WriteString("← ")
	for i, t := range tabs {
		if i == current {
			b.WriteString(previewBG + " " + t + " " + previewBGOff)
		} else {
			b.WriteString(" " + t + " ")
		}
	}
	b.WriteString(" →")
	return b.String()
}

func previewChip(text string) string { return previewBG + " " + text + " " + previewBGOff }

var previewProse = []string{
	"  the tail of the agent's own prose, written just above the dialog",
	"  and a second line of it",
}

func twoQuestionPreview(selected int, tabs ...string) previewScreen {
	if len(tabs) == 0 {
		tabs = []string{"☐ Layout", "☐ Theme", "✔ Submit"}
	}
	return previewScreen{
		prose:    previewProse,
		header:   previewTabBar(0, tabs...),
		question: "Which layout do you prefer?",
		options:  [][]string{{"Option A"}, {"Option B"}, {"Option C"}},
		selected: selected,
		preview:  []string{fmt.Sprintf("a mockup of option %d", selected), "second line", "third line"},
	}
}

// The measured defect, on the shapes it was measured on and the ones a real
// agent will produce: every option read as its label plus padding plus the
// box's row, the question read as the tail of the agent's prose plus the tab
// bar plus the question, and a pane taller than the scan window hid the whole
// dialog.
func TestPreviewPaneOptionsAreTheListColumnAlone(t *testing.T) {
	tall := previewScreen{
		prose: previewProse, header: previewChip("☐ Snippet"), question: "Which snippet?",
		options:  [][]string{{"First"}, {"Second"}, {"Third"}},
		selected: 1,
	}
	for i := 1; i <= 24; i++ {
		tall.preview = append(tall.preview, fmt.Sprintf("line %02d: const a%02d = %d;", i, i, i))
	}
	tall.preview = append(tall.preview, previewDivider(42, 16))

	cases := []struct {
		name     string
		screen   previewScreen
		options  []string
		question string
		tab      tabPosition
	}{
		{"two questions, first current", twoQuestionPreview(1),
			[]string{"Option A", "Option B", "Option C"}, "Which layout do you prefer?", tabPosition{1, 2}},
		{"two questions, second current", func() previewScreen {
			s := twoQuestionPreview(1, "☒ Layout", "☐ Theme", "✔ Submit")
			s.header = previewTabBar(1, "☒ Layout", "☐ Theme", "✔ Submit")
			s.question = "Which theme do you prefer?"
			s.options = [][]string{{"Light"}, {"Dark"}}
			return s
		}(), []string{"Light", "Dark"}, "Which theme do you prefer?", tabPosition{2, 2}},
		{"single question, lone chip", previewScreen{
			prose: previewProse, header: previewChip("☐ Pick"), question: "Which card do you want?",
			options: [][]string{{"One"}, {"Two"}, {"Three"}}, selected: 2,
			preview: []string{"a mockup"},
		}, []string{"One", "Two", "Three"}, "Which card do you want?", tabPosition{1, 1}},
		{"a label in Japanese", previewScreen{
			prose: previewProse, header: previewChip("☐ Pick"), question: "Which card do you want?",
			options: [][]string{{"日本語のレイアウトA"}, {"Plain"}}, selected: 1,
			preview: []string{"┌──────────┐", "│ カード   │", "└──────────┘"},
		}, []string{"日本語のレイアウトA", "Plain"}, "Which card do you want?", tabPosition{1, 1}},
		{"a label that wraps inside its column", previewScreen{
			prose: previewProse, header: previewChip("☐ Pick"), question: "Which card do you want?",
			options: [][]string{
				{"A very long option label", "that goes on and on to", "test how the list column"},
				{"Plain"},
			}, selected: 1,
			preview: []string{"one", "two", "three", "four", "five"},
		}, []string{"A very long option label that goes on and on to test how the list column", "Plain"},
			"Which card do you want?", tabPosition{1, 1}},
		{"a pane shorter than the list", previewScreen{
			prose: previewProse, header: previewChip("☐ Pick"), question: "Which?",
			options: [][]string{{"One"}, {"Two"}, {"Three"}, {"Four"}}, selected: 1,
			preview: []string{"only line"},
		}, []string{"One", "Two", "Three", "Four"}, "Which?", tabPosition{1, 1}},
		// The highlight below a short pane's bottom border sits between two
		// rule-like rows, which once read as a fenced composer and hid it.
		{"the highlight below a short pane", previewScreen{
			prose: previewProse, header: previewChip("☐ Pick"), question: "Which?",
			options: [][]string{{"One"}, {"Two"}, {"Three"}, {"Four"}}, selected: 4,
			preview: []string{"only line"},
		}, []string{"One", "Two", "Three", "Four"}, "Which?", tabPosition{1, 1}},
		{"a pane taller than the scan window", tall,
			[]string{"First", "Second", "Third"}, "Which snippet?", tabPosition{1, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := c.screen.String()
			p, shape := parsePromptShape(newScreen(text))
			if p == nil {
				t.Fatalf("not recognised as a prompt at all:\n%s", text)
			}
			if !shape.preview {
				t.Error("the preview pane was not recognised")
			}
			if fmt.Sprintf("%q", p.Options) != fmt.Sprintf("%q", c.options) {
				t.Errorf("options = %q, want %q", p.Options, c.options)
			}
			if p.Question != c.question {
				t.Errorf("question = %q, want %q", p.Question, c.question)
			}
			if p.Selected != c.screen.selected {
				t.Errorf("selected = %d, want %d", p.Selected, c.screen.selected)
			}
			if shape.tab != c.tab {
				t.Errorf("tab = %+v, want %+v", shape.tab, c.tab)
			}
			// The whole classifier must agree, not only the parser: a tall pane
			// used to read as idle, and a blocked session reading as idle is
			// worse than one reading as unknown.
			st, _ := classifyPaneRemembering(text, true, true, false, paneMemory{}, time.Now())
			if st.Status != fleet.StatusWaitingInput {
				t.Errorf("status = %q (%s), want waiting_input", st.Status, st.Evidence)
			}
			if st.Prompt == nil || st.Prompt.Nonce != p.Nonce {
				t.Errorf("the state read and the parser disagree about the prompt: %+v", st.Prompt)
			}
		})
	}
}

// The nonce is what makes an answer refusable when the screen moved. It used
// to change every time the highlight moved, because the options held the
// box's row for the highlighted option; and it depended on the tab bar only by
// accident, because the tab bar sat inside the question.
func TestPreviewPaneNonceFollowsThePromptAndNotTheHighlight(t *testing.T) {
	nonce := func(s previewScreen) string {
		t.Helper()
		p := parsePrompt(newScreen(s.String()))
		if p == nil {
			t.Fatalf("not a prompt:\n%s", s.String())
		}
		return p.Nonce
	}
	first := nonce(twoQuestionPreview(1))
	for _, sel := range []int{2, 3} {
		if got := nonce(twoQuestionPreview(sel)); got != first {
			t.Errorf("moving the highlight to option %d changed the nonce: the box shows the "+
				"highlighted option's preview, which is not part of the question", sel)
		}
	}

	answered := twoQuestionPreview(1, "☒ Layout", "☐ Theme", "✔ Submit")
	if nonce(answered) == first {
		t.Error("the first tab reading ☒ (answered) did not change the nonce")
	}

	// Two tabs that ask the same thing, neither answered: only WHICH tab is
	// current tells them apart, and an answer meant for one must not land on
	// the other.
	a, b := twoQuestionPreview(1), twoQuestionPreview(1)
	b.header = previewTabBar(1, "☐ Layout", "☐ Theme", "✔ Submit")
	if nonce(a) == nonce(b) {
		t.Error("two tabs with the same question and options share a nonce")
	}
}

// The current tab is found by where the highlight sits.
func TestDialogTabPositionReadsTheHighlight(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want tabPosition
	}{
		{"background on the first tab", previewTabBar(0, "☐ A", "☐ B", "✔ Submit"), tabPosition{1, 2}},
		{"background on the second tab", previewTabBar(1, "☐ A", "☐ B", "✔ Submit"), tabPosition{2, 2}},
		{"the Submit tab is current", previewTabBar(2, "☒ A", "☒ B", "✔ Submit"), tabPosition{0, 2}},
		{"reverse video", "← \x1b[7m ☐ A \x1b[27m ☐ B  ✔ Submit  →", tabPosition{1, 2}},
		{"a basic background", "←  ☐ A \x1b[44m ☐ B \x1b[49m ✔ Submit  →", tabPosition{2, 2}},
		{"a lone chip", previewChip("☐ Pick"), tabPosition{1, 1}},
		// The 7 and the 5 in an extended colour are arguments, not attributes.
		{"a foreground colour only", "← \x1b[38;5;7m ☐ A \x1b[39m ☐ B  ✔ Submit  →", tabPosition{0, 2}},
		{"no escapes at all", "←  ☐ A  ☐ B  ✔ Submit  →", tabPosition{0, 2}},
		{"two highlighted runs", "← \x1b[7m ☐ A \x1b[27m \x1b[7m ☐ B \x1b[27m ✔ Submit  →", tabPosition{0, 2}},
		// A redacted screen gives every tab the same text, so matching the
		// highlighted label would name the wrong one.
		{"identical labels, second current", previewTabBar(1, "☐ Q", "☐ Q", "✔ Submit"), tabPosition{2, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dialogTabPosition(c.raw); got != c.want {
				t.Errorf("dialogTabPosition = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A `┌…┐` and a `│` in an agent's own output must not make an ordinary menu
// read as one with a pane — a pane found in transcript would cut real rows.
func TestAPaneIsOnlyBelievedWhenItClosesBesideAnOption(t *testing.T) {
	plainMenu := func(tail string) string {
		return strings.Join([]string{
			"  " + tail,
			rule,
			" Do you want to proceed?",
			"❯ 1. Yes",
			"  2. No, and tell it what to do differently",
			"",
			"Enter to select · ↑/↓ to navigate · Esc to cancel",
		}, "\n")
	}
	table := "┌────────┬────────┐\n  │ a      │ b      │\n  └────────┴────────┘"
	cases := map[string]string{
		"a table above an ordinary menu": plainMenu(table),
		"a box with no option beside it": strings.Join([]string{
			rule, "  ┌────────────┐", "  │ some text  │", "  └────────────┘",
			"Enter to select · Esc to cancel",
		}, "\n"),
		"borders of different widths": func() string {
			s := twoQuestionPreview(1).String()
			return strings.Replace(s, "└"+strings.Repeat("─", 42)+"┘", "└"+strings.Repeat("─", 30)+"┘", 1)
		}(),
		"a row inside the box that is not closed": func() string {
			s := twoQuestionPreview(1).String()
			return strings.Replace(s, "│ second line"+strings.Repeat(" ", 30)+"│", "│ second line, cut off", 1)
		}(),
		"a bottom border with no top": func() string {
			s := twoQuestionPreview(1).String()
			return strings.Replace(s, "┌"+strings.Repeat("─", 42)+"┐", "", 1)
		}(),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			lines := newScreen(text).lines
			if pane, ok := findPreviewPane(lines); ok {
				t.Errorf("found a pane at rows %d-%d where there is none", pane.top, pane.bottom)
			}
		})
	}

	t.Run("an ordinary menu keeps its options whole", func(t *testing.T) {
		p := parsePrompt(newScreen(plainMenu(table)))
		if p == nil || len(p.Options) != 2 || p.Options[1] != "No, and tell it what to do differently" {
			t.Errorf("prompt = %+v", p)
		}
	})
}

// The pane belongs to the nearest dialog: a box far above the bottom of the
// screen is scrollback.
func TestAPaneAboveTheDialogIsScrollback(t *testing.T) {
	old := twoQuestionPreview(1).String()
	lines := strings.Split(old, "\n")
	for i := 0; i < paneFooterSlack+2; i++ {
		lines = append(lines, fmt.Sprintf("  later transcript row %d", i))
	}
	if pane, ok := findPreviewPane(newScreen(strings.Join(lines, "\n")).lines); ok {
		t.Errorf("found a pane at rows %d-%d, %d rows above the end of the screen", pane.top, pane.bottom, paneFooterSlack+2)
	}
}

// Every secret-looking string below must be gone from a redacted pane, and
// what stays must still replay as the same dialog: the shape is the point.
func TestRedactCaptureKeepsAPreviewPaneAsAShape(t *testing.T) {
	secretive := func(s previewScreen) previewScreen {
		s.prose = []string{"  SECRET-PROSE the agent wrote"}
		s.question = "SECRET-QUESTION?"
		s.preview = []string{"SECRET-PREVIEW one", "SECRET-PREVIEW two"}
		return s
	}
	base := secretive(twoQuestionPreview(1))
	base.options = [][]string{{"SECRET-LABEL-A"}, {"SECRET-LABEL-B"}, {"SECRET-LABEL-C"}}
	base.header = previewTabBar(0, "☐ SECRET-HDR", "☐ SECRET-HDR", "✔ Submit")

	wide := secretive(previewScreen{
		header: previewChip("☐ SECRET-HDR"), selected: 1,
		options: [][]string{{"SECRET-日本語のラベル"}, {"SECRET-LABEL", "SECRET-WRAPPED-ROW"}, {"C"}},
	})
	tall := secretive(previewScreen{header: previewChip("☐ SECRET-HDR"), selected: 1,
		options: [][]string{{"SECRET-A"}, {"SECRET-B"}}})
	for i := 0; i < 24; i++ {
		tall.preview = append(tall.preview, fmt.Sprintf("SECRET-LINE %d", i))
	}
	tall.preview = append(tall.preview, previewDivider(42, 16))

	cases := []struct {
		name    string
		screen  previewScreen
		options int
		tab     tabPosition
	}{
		{"two questions", base, 3, tabPosition{1, 2}},
		{"a wide label and a wrapped one", wide, 3, tabPosition{1, 1}},
		{"a tall pane with a divider", tall, 2, tabPosition{1, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			red := RedactCapture(c.screen.String())
			if strings.Contains(red, "SECRET") {
				t.Errorf("redaction let content through:\n%s", red)
			}
			if again := RedactCapture(red); again != red {
				t.Errorf("not a fixed point of redaction; second pass:\n%s\nfirst pass:\n%s", again, red)
			}
			p, shape := parsePromptShape(newScreen(red))
			if p == nil || !shape.preview {
				t.Fatalf("the redacted screen no longer replays as a preview pane (prompt %+v):\n%s", p, red)
			}
			if len(p.Options) != c.options {
				t.Errorf("options = %q, want %d of them", p.Options, c.options)
			}
			for _, o := range p.Options {
				if !strings.HasPrefix(o, placeholderToken) || strings.ContainsAny(o, "┌│└├┐┤┘─") {
					t.Errorf("option %q still carries the pane, or is not the placeholder", o)
				}
			}
			if p.Question != placeholderToken {
				t.Errorf("question = %q, want the bare placeholder", p.Question)
			}
			if shape.tab != c.tab {
				t.Errorf("tab = %+v, want %+v — the highlight has to survive redaction", shape.tab, c.tab)
			}
		})
	}

	t.Run("box drawing that is not a pane is left to the ordinary rules", func(t *testing.T) {
		text := strings.Join([]string{
			"  │ SECRET-TABLE-CELL │",
			rule,
			"❯ 1. Yes",
			"  2. No",
			"",
			"Enter to select · Esc to cancel",
		}, "\n")
		if red := RedactCapture(text); strings.Contains(red, "SECRET") {
			t.Errorf("a row that is not part of a pane kept its content:\n%s", red)
		}
	})
}
