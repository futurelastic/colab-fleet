package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// fixtureMultiSelectTicked is a single-question multi-select dialog as captured
// live for colab-fleet#176 (one runtime build), with options 1 and 3 ticked
// and the highlight on option 1. Verbatim from the question's rule down,
// apart from the transcript above it.
const fixtureMultiSelectTicked = `  transcript line
────────────────────────────────────────────────────────────────────────────────
←  ☒ Fruit  ✔ Submit  →

Which fruits do you like?

❯ 1. [✔] Apple
         A crisp, sweet fruit available in many varieties
  2. [ ] Banana
         A soft, creamy tropical fruit rich in potassium
  3. [✔] Cherry
         A small, juicy stone fruit
  4. [ ] Type something
     Submit
────────────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel`

// fixtureMultiSelectFirstOfTwo is the first, multi-select, question of a
// two-question dialog, captured live the same day: the in-menu row reads
// "Next", and the footer is the tabbed dialog's.
const fixtureMultiSelectFirstOfTwo = `  transcript line
────────────────────────────────────────────────────────────────────────────────
←  ☐ Fruit  ☐ Size  ✔ Submit  →

Which fruits do you like?

❯ 1. [ ] Apple
         A crisp, sweet fruit
  2. [ ] Banana
         A soft, creamy tropical fruit
  3. [ ] Cherry
         A small, juicy stone fruit
  4. [ ] Type something
     Next
────────────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// fixtureMultiSelectOnSubmitRow is the same question with the highlight on
// the unnumbered Submit row — no numbered option carries the marker, so the
// prompt is read through its footer with no selected option.
const fixtureMultiSelectOnSubmitRow = `  transcript line
────────────────────────────────────────────────────────────────────────────────
←  ☒ Fruit  ✔ Submit  →

Which fruits do you like?

  1. [✔] Apple
  2. [ ] Banana
  3. [✔] Cherry
  4. [ ] Type something
❯    Submit
────────────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel`

func TestMultiSelectIsRecognisedOnTheMeasuredScreens(t *testing.T) {
	cases := map[string]struct {
		screen string
		boxes  int
		ticks  string
	}{
		"single question, two ticked": {fixtureMultiSelectTicked, 3, "[true false true]"},
		"first of two, none ticked":   {fixtureMultiSelectFirstOfTwo, 3, "[false false false]"},
		"highlight on the Submit row": {fixtureMultiSelectOnSubmitRow, 3, "[true false true]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := parsePrompt(newScreen(c.screen))
			if p == nil {
				t.Fatal("not recognised as a prompt at all")
			}
			if !p.MultiSelect {
				t.Fatalf("MultiSelect = false; options = %q", p.Options)
			}
			if got := multiSelectBoxes(p); got != c.boxes {
				t.Errorf("boxes = %d, want %d", got, c.boxes)
			}
			if got := fmt.Sprint(promptTicks(p, c.boxes)); got != c.ticks {
				t.Errorf("ticks = %s, want %s", got, c.ticks)
			}
			// The flag is derived from the options, so it must not move the
			// nonce: it is the question, the options and the dialog's header
			// (#204 — the header used to ride inside the question) and nothing
			// else. These fixtures carry no escapes, so no tab is read as
			// current.
			header := ""
			for _, l := range strings.Split(c.screen, "\n") {
				if isDialogTabBar(strings.TrimSpace(l)) {
					header = l
					break
				}
			}
			if want := promptNonceWithHeader(&fleet.SessionPrompt{Question: p.Question, Options: p.Options}, header, tabPosition{}); p.Nonce != want {
				t.Error("MultiSelect leaked into the nonce")
			}
			if p.Kind != "" || classifyPromptKind(p) != "" {
				t.Errorf("kind = %q; an agent-asked question has no kind", classifyPromptKind(p))
			}
		})
	}
}

// Every shape that resembles one part of the measured screen and not the
// rest must NOT read as multi-select: the flag licenses a keystroke sequence,
// so a false positive sends keys into something that is not a checkbox list.
func TestMultiSelectIsNotGuessed(t *testing.T) {
	g := newFakeDialog([]string{"Red", "Green", "Type something."}, []string{"Cat", "Dog", "Type something."})
	cases := map[string]string{
		"single-select tab of a tabbed dialog": g.screen(),
		"the review screen": `  transcript line
` + rule + `
←  ☒ Fruit  ✔ Submit  →
Review your answers
 ● Which fruits do you like?
   → Apple, Cherry
Ready to submit your answers?
❯ 1. Submit answers
  2. Cancel`,
		"checkboxes with no tab bar": strings.Replace(fixtureMultiSelectTicked, "←  ☒ Fruit  ✔ Submit  →", "", 1),
		"a checklist in the transcript above an ordinary menu": `  - [ ] write the tests
  - [✔] read the issue
` + rule + `
Proceed?
❯ 1. Yes
  2. No
` + rule + `
Enter to select · ↑/↓ to navigate · Esc to cancel`,
		"an unknown row after the boxes": strings.Replace(fixtureMultiSelectTicked, "5. Chat about this", "5. Something else", 1),
		"no escape row after the boxes": strings.Replace(strings.Replace(fixtureMultiSelectTicked,
			"  4. [ ] Type something\n", "", 1), "  5. Chat about this\n", "", 1),
		"a gap in the checkbox run": strings.Replace(fixtureMultiSelectTicked, "  2. [ ] Banana\n", "", 1),
	}
	for name, screen := range cases {
		t.Run(name, func(t *testing.T) {
			if p := parsePrompt(newScreen(screen)); p != nil && p.MultiSelect {
				t.Fatalf("read as multi-select: options = %q", p.Options)
			}
		})
	}
}

// With the box in front of the label, the agent-question guard in
// classifyPromptKind no longer saw "type something" at the start of that row,
// so an agent's own checkbox option could be labelled a runtime kind.
func TestCheckboxGlyphDoesNotHideTheAgentQuestionGuard(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{
		"[ ] Yes, allow this for the build step", "[ ] No", "[ ] Type something", "Chat about this",
	}}
	if k := classifyPromptKind(p); k != "" {
		t.Fatalf("kind = %q, want none: this is an agent's own multi-select question", k)
	}
}

// fakeMultiSelect models the multi-select dialog as measured live for
// colab-fleet#176:
//
//   - a digit on a checkbox flips it; the highlight does not move and the
//     dialog does not advance;
//   - C-m flips the highlighted checkbox;
//   - Up/Down move the highlight over the checkboxes, the free-text row, the
//     unnumbered Submit row and the chat row, in that order;
//   - Right on a checkbox row moves to the next tab — the next question when
//     there is one, else the review screen — ticks kept; Right anywhere else
//     is swallowed by the free-text field;
//   - off the checkboxes the free-text field has focus, so a digit is typed
//     into it (and ticks its row) instead of flipping a box — measured by the
//     live end-to-end run, which this model missed at first;
//   - on the review screen, 1 hands the answers over and 2 cancels.
type fakeMultiSelect struct {
	labels   []string
	ticks    []bool
	sel      int  // 1..n boxes, n+1 free text, n+2 Submit row, n+3 chat
	next     bool // a second, single-select question follows
	stage    int  // 0 question, 1 next question, 2 review, 3 submitted, 4 cancelled
	swallow  map[string]int
	answered []string
	typed    string // what reached the free-text field
}

func newFakeMultiSelect(labels ...string) *fakeMultiSelect {
	return &fakeMultiSelect{labels: labels, ticks: make([]bool, len(labels)), sel: 1, swallow: map[string]int{}}
}

func (g *fakeMultiSelect) press(key string) {
	if g.swallow[key] > 0 {
		g.swallow[key]--
		return
	}
	n := len(g.labels)
	switch g.stage {
	case 0:
		switch {
		case len(key) == 1 && key[0] >= '1' && key[0] <= '9' && g.sel > n:
			g.typed += key
		case len(key) == 1 && key[0] >= '1' && key[0] <= '9':
			if i := int(key[0] - '0'); i <= n {
				g.ticks[i-1] = !g.ticks[i-1]
			}
		case key == "C-m" && g.sel <= n:
			g.ticks[g.sel-1] = !g.ticks[g.sel-1]
		case key == "Up" && g.sel > 1:
			g.sel--
		case key == "Down" && g.sel < n+3:
			g.sel++
		case key == "Right" && g.sel <= n:
			if g.next {
				g.stage = 1
			} else {
				g.stage = 2
			}
		}
	case 1:
		if key == "1" || key == "2" {
			g.stage = 2
		}
	case 2:
		switch key {
		case "1":
			g.stage = 3
			for i, t := range g.ticks {
				if t {
					g.answered = append(g.answered, g.labels[i])
				}
			}
		case "2":
			g.stage = 4
		}
	}
}

func (g *fakeMultiSelect) screen() string {
	if g.stage >= 3 {
		return idleFixtureFor("answered")
	}
	n := len(g.labels)
	var b strings.Builder
	b.WriteString("  transcript line\n" + rule + "\n")
	box := "☐"
	for _, t := range g.ticks {
		if t {
			box = "☒"
		}
	}
	if g.next {
		fmt.Fprintf(&b, "←  %s Fruit  ☐ Size  ✔ Submit  →\n\n", box)
	} else {
		fmt.Fprintf(&b, "←  %s Fruit  ✔ Submit  →\n\n", box)
	}
	mark := func(row int) string {
		if row == g.sel {
			return "❯ "
		}
		return "  "
	}
	switch g.stage {
	case 1:
		b.WriteString("What size do you prefer?\n\n❯ 1. Small\n  2. Large\n  3. Type something.\n" + rule +
			"\n  4. Chat about this\n\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel")
		return b.String()
	case 2:
		b.WriteString("Review your answers\n\n ● Which fruits do you like?\n   → picked\n\n" + reviewQuestion + "\n\n")
		fmt.Fprintf(&b, "❯ 1. %s\n  2. %s\n", reviewOptions[0], reviewOptions[1])
		return b.String()
	}
	b.WriteString("Which fruits do you like?\n\n")
	for i, l := range g.labels {
		c := "[ ]"
		if g.ticks[i] {
			c = "[✔]"
		}
		fmt.Fprintf(&b, "%s%d. %s %s\n         a description of %s\n", mark(i+1), i+1, c, l, l)
	}
	if g.typed != "" {
		fmt.Fprintf(&b, "%s%d. [✔] %s\n", mark(n+1), n+1, g.typed)
	} else {
		fmt.Fprintf(&b, "%s%d. [ ] Type something\n", mark(n+1), n+1)
	}
	fmt.Fprintf(&b, "%s   Submit\n%s\n%s%d. Chat about this\n\n", mark(n+2), rule, mark(n+3), n+2)
	b.WriteString("Enter to select · ↑/↓ to navigate · Esc to cancel")
	return b.String()
}

func respondChoices(t *testing.T, d *Driver, f *fakeMux, nonce string, choices ...int) fleet.DeliveryReceipt {
	t.Helper()
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.Response{Choices: choices, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func currentNonce(t *testing.T, f *fakeMux) string {
	t.Helper()
	p := parsePrompt(newScreen(f.captures["%1"]))
	if p == nil {
		t.Fatalf("not a recognised prompt:\n%s", f.captures["%1"])
	}
	return p.Nonce
}

// sendCalls lists every send-keys call's keys, one entry per call.
func sendCalls(f *fakeMux) []string {
	var out []string
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			out = append(out, strings.Join(sentKeys(c), " "))
		}
	}
	return out
}

func TestChoicesTicksExactlyTheSetAndStopsOnTheReviewScreen(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	g.ticks[1] = true // Banana already ticked; the caller wants Apple and Cherry
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondChoices(t, d, f, currentNonce(t, f), 1, 3)
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
	}
	if want := "[1 2 3 Right]"; fmt.Sprint(sendCalls(f)) != want {
		t.Errorf("key sends = %v, want %s — each flip its own call, then one Right", sendCalls(f), want)
	}
	if fmt.Sprint(g.ticks) != "[true false true]" {
		t.Errorf("ticks = %v, want exactly Apple and Cherry", g.ticks)
	}
	if g.stage != 2 {
		t.Fatalf("stage = %d, want the review screen — choices must never hand the answers over", g.stage)
	}
	if !strings.Contains(got.Reason, "review screen") || !strings.Contains(got.Reason, "choice 1") {
		t.Errorf("receipt = %q; must say it stopped on the review screen and how to finish", got.Reason)
	}

	// The follow-up the receipt asks for: the review screen's own nonce.
	fin := respondWithNonce(t, d, f, 1)
	if fin.Outcome != fleet.OutcomeSubmitted || fmt.Sprint(g.answered) != "[Apple Cherry]" {
		t.Fatalf("review answer = %q (%s); answered = %v", fin.Outcome, fin.Reason, g.answered)
	}
}

func TestChoicesAlreadyExactOnlyMovesOn(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	g.ticks[0], g.ticks[2] = true, true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondChoices(t, d, f, currentNonce(t, f), 3, 1)
	if got.Outcome != fleet.OutcomeSubmitted || fmt.Sprint(sendCalls(f)) != "[Right]" {
		t.Fatalf("respond = %q (%s), sends = %v; want only Right", got.Outcome, got.Reason, sendCalls(f))
	}
}

// Idempotence under a nonce: the same request, resent after it succeeded, is
// refused without a key — the question it quoted has moved on.
func TestChoicesResentWithTheSameNonceIsRefused(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	armDialog(f, "%1", g)
	d := newTestDriver(f)
	nonce := currentNonce(t, f)

	if got := respondChoices(t, d, f, nonce, 2); got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("first = %q (%s)", got.Outcome, got.Reason)
	}
	sent := len(sendCalls(f))
	again := respondChoices(t, d, f, nonce, 2)
	if again.Outcome != fleet.OutcomeRefused {
		t.Fatalf("resend = %q (%s), want refused", again.Outcome, again.Reason)
	}
	if len(sendCalls(f)) != sent {
		t.Errorf("the resend pressed keys: %v", sendCalls(f)[sent:])
	}
}

// Someone ticked a box at the pane after the caller read the prompt: the tick
// is in the nonce, so the caller's set is refused, not quietly reconciled.
func TestChoicesAgainstAChangedTickStateIsRefused(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	armDialog(f, "%1", g)
	d := newTestDriver(f)
	nonce := currentNonce(t, f)
	g.press("2")
	f.captures["%1"] = g.screen()

	got := respondChoices(t, d, f, nonce, 1)
	if got.Outcome != fleet.OutcomeRefused || len(sendCalls(f)) != 0 {
		t.Fatalf("respond = %q (%s), sends = %v; want refused with no key", got.Outcome, got.Reason, sendCalls(f))
	}
}

// A flip that does not read back stops the sequence: Right is never sent,
// the receipt names what was flipped, and resending the same set with the
// fresh nonce finishes the job without flipping anything twice.
func TestChoicesStopsAtASwallowedDigitAndResumesFromTheScreen(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	g.swallow["3"] = 1
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondChoices(t, d, f, currentNonce(t, f), 1, 3)
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	for _, k := range sendCalls(f) {
		if k == "Right" {
			t.Fatal("the dialog was moved on after a flip that did not land")
		}
	}
	if !strings.Contains(got.Reason, "1 (Apple)") || !strings.Contains(got.Reason, "3 (Cherry)") {
		t.Errorf("receipt = %q; must name what was flipped and what did not land", got.Reason)
	}

	again := respondChoices(t, d, f, currentNonce(t, f), 1, 3)
	if again.Outcome != fleet.OutcomeSubmitted || fmt.Sprint(g.ticks) != "[true false true]" || g.stage != 2 {
		t.Fatalf("resend = %q (%s); ticks %v stage %d", again.Outcome, again.Reason, g.ticks, g.stage)
	}
}

// Off the checkboxes the free-text field has focus: Right moves its cursor
// and a digit is typed into it (both measured live). So the highlight is
// walked up onto a checkbox before ANY other key, one read-back press at a
// time.
func TestChoicesMovesTheHighlightOffTheRowsThatSwallowRight(t *testing.T) {
	for _, start := range []int{4, 5, 6} {
		t.Run(fmt.Sprint("from row ", start), func(t *testing.T) {
			f := twoSessions()
			g := newFakeMultiSelect("Apple", "Banana", "Cherry")
			g.sel = start
			armDialog(f, "%1", g)
			d := newTestDriver(f)

			got := respondChoices(t, d, f, currentNonce(t, f), 2)
			if got.Outcome != fleet.OutcomeSubmitted || g.stage != 2 {
				t.Fatalf("respond = %q (%s), stage %d, sends %v", got.Outcome, got.Reason, g.stage, sendCalls(f))
			}
			ups := 0
			for _, k := range sendCalls(f) {
				if k == "Up" {
					ups++
				}
			}
			if ups != start-3 {
				t.Errorf("Up presses = %d, want %d (one per row back to the last checkbox)", ups, start-3)
			}
			if sends := sendCalls(f); len(sends) == 0 || sends[0] != "Up" {
				t.Errorf("key sends = %v; the highlight must reach a checkbox before any digit", sends)
			}
			if g.typed != "" || fmt.Sprint(g.ticks) != "[false true false]" {
				t.Errorf("typed %q into the free-text field, ticks %v; want only Banana ticked", g.typed, g.ticks)
			}
		})
	}
}

func TestChoicesOnTheFirstOfTwoQuestionsMovesToTheNext(t *testing.T) {
	f := twoSessions()
	g := newFakeMultiSelect("Apple", "Banana", "Cherry")
	g.next = true
	armDialog(f, "%1", g)
	d := newTestDriver(f)

	got := respondChoices(t, d, f, currentNonce(t, f), 2)
	if got.Outcome != fleet.OutcomeSubmitted || g.stage != 1 {
		t.Fatalf("respond = %q (%s), stage %d", got.Outcome, got.Reason, g.stage)
	}
	if !strings.Contains(got.Reason, "next question") {
		t.Errorf("receipt = %q; must say it moved on to the next question", got.Reason)
	}
}

func TestMultiSelectRefusalsSendNoKeys(t *testing.T) {
	cases := map[string]struct {
		screen func() keyedScreen
		resp   func(nonce string) fleet.Response
	}{
		"choices on a single-select menu": {
			func() keyedScreen { return newFakeDialog([]string{"Tea", "Coffee", "Type something."}) },
			func(n string) fleet.Response { return fleet.Response{Choices: []int{1}, Nonce: n} },
		},
		"choices on the review screen": {
			func() keyedScreen { g := newFakeMultiSelect("Apple", "Banana"); g.stage = 2; return g },
			func(n string) fleet.Response { return fleet.Response{Choices: []int{1}, Nonce: n} },
		},
		"choices naming the free-text row": {
			func() keyedScreen { return newFakeMultiSelect("Apple", "Banana") },
			func(n string) fleet.Response { return fleet.Response{Choices: []int{1, 3}, Nonce: n} },
		},
		"a single choice on a checkbox": {
			func() keyedScreen { return newFakeMultiSelect("Apple", "Banana") },
			func(n string) fleet.Response { return fleet.Response{Choice: 2, Nonce: n} },
		},
		"accepting the highlighted checkbox": {
			func() keyedScreen { return newFakeMultiSelect("Apple", "Banana") },
			func(n string) fleet.Response { return fleet.Response{Nonce: n} },
		},
		"an empty set": {
			func() keyedScreen { return newFakeMultiSelect("Apple", "Banana") },
			func(n string) fleet.Response { return fleet.Response{Choices: []int{}, Nonce: n} },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			armDialog(f, "%1", c.screen())
			d := newTestDriver(f)
			got, err := d.Respond(context.Background(), testCaller,
				fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, c.resp(currentNonce(t, f)))
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("respond = %q (%s), want refused", got.Outcome, got.Reason)
			}
			if s := sendCalls(f); len(s) != 0 {
				t.Errorf("a refusal pressed keys: %v", s)
			}
		})
	}
}

// Cancel still reaches a multi-select question: refusing single choices there
// must not take away the way out.
func TestCancelStillWorksOnAMultiSelectQuestion(t *testing.T) {
	f := twoSessions()
	armDialog(f, "%1", newFakeMultiSelect("Apple", "Banana"))
	d := newTestDriver(f)
	_, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.Response{Cancel: true, Nonce: currentNonce(t, f)})
	if err != nil {
		t.Fatal(err)
	}
	if s := sendCalls(f); fmt.Sprint(s) != "[Escape]" {
		t.Fatalf("key sends = %v, want Escape", s)
	}
}
