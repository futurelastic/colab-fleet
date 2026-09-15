package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// keyedScreen is a pane model that redraws under send-keys: fakeDialog for a
// numbered menu (#168), fakeMenu for an unnumbered one (#171).
type keyedScreen interface {
	press(key string)
	screen() string
}

// fixtureTrustMenuUnnumbered is the folder-trust question as measured through
// the service for colab-fleet#171, on a session created in a directory the
// runtime had never seen. Verbatim apart from the directory line. Note the
// order (the decline first) and that no row carries a number.
const fixtureTrustMenuUnnumbered = `
────────────────────────────────────────────────────────────────────────────────
 Accessing workspace:

 /work/alpha

 Quick safety check: Is this a project you created or one you trust? (Like your
 own code, a well-known open source project, or work from your team). If not,
 take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel




`

// fakeMenu models the unnumbered menu as measured for #171: a digit and
// Space change nothing, Up/Down move the highlight, C-m confirms whatever is
// highlighted. swallowArrows models a menu that drops the arrow presses.
type fakeMenu struct {
	options       []string
	sel           int // 0-based highlighted row
	chosen        int // 1-based confirmed row; 0 while the menu is up
	swallowArrows bool
}

func newTrustMenu() *fakeMenu {
	return &fakeMenu{options: []string{"No, exit", "Yes, I trust this folder"}}
}

func (m *fakeMenu) press(key string) {
	if m.chosen != 0 {
		return
	}
	switch key {
	case "Down":
		if !m.swallowArrows && m.sel < len(m.options)-1 {
			m.sel++
		}
	case "Up":
		if !m.swallowArrows && m.sel > 0 {
			m.sel--
		}
	case "C-m", "Enter":
		m.chosen = m.sel + 1
	}
}

func (m *fakeMenu) screen() string {
	if m.chosen != 0 {
		return idleFixtureFor("answered")
	}
	var b strings.Builder
	b.WriteString("\n" + rule + "\n Quick safety check: Is this a project you created or one you trust?\n\n Security guide\n\n")
	for i, o := range m.options {
		if i == m.sel {
			fmt.Fprintf(&b, " ❯ %s\n", o)
		} else {
			fmt.Fprintf(&b, "   %s\n", o)
		}
	}
	b.WriteString("\n Enter to confirm · Esc to cancel\n")
	return b.String()
}

func TestUnnumberedTrustMenuIsAPrompt(t *testing.T) {
	p, unnumbered := parsePromptMenu(newScreen(fixtureTrustMenuUnnumbered))
	if p == nil {
		t.Fatal("the measured folder-trust menu is not recognised as a prompt")
	}
	if !unnumbered {
		t.Error("unnumbered = false; Respond would send a digit, which this menu ignores")
	}
	if want := []string{"No, exit", "Yes, I trust this folder"}; fmt.Sprint(p.Options) != fmt.Sprint(want) {
		t.Errorf("options = %q, want %q", p.Options, want)
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1", p.Selected)
	}
	if !strings.Contains(p.Question, "Security guide") || p.Nonce == "" {
		t.Errorf("question = %q, nonce = %q; both must be published", p.Question, p.Nonce)
	}
	p.Kind = classifyPromptKind(p)
	if p.Kind != fleet.PromptFolderTrust {
		t.Errorf("kind = %q, want %q", p.Kind, fleet.PromptFolderTrust)
	}
	if got, ok := affirmativeOption(p); !ok || got != 2 {
		t.Errorf("affirmativeOption = %d, %v; want 2 — the trust row is second on this menu", got, ok)
	}
}

// The nonce covers the question and options, not the highlight, so a caller
// who read the menu before the highlight moved can still answer it.
func TestUnnumberedMenuNonceIgnoresTheHighlight(t *testing.T) {
	m := newTrustMenu()
	a, _ := parsePromptMenu(newScreen(m.screen()))
	m.press("Down")
	b, _ := parsePromptMenu(newScreen(m.screen()))
	if a == nil || b == nil {
		t.Fatal("both highlights must parse")
	}
	if b.Selected != 2 || a.Nonce != b.Nonce {
		t.Errorf("after Down: selected = %d (want 2), nonce %s -> %s (want unchanged)", b.Selected, a.Nonce, b.Nonce)
	}
}

// Each required piece of the layout, removed on its own, must make the
// screen not a menu — every one of them alone is paintable by transcript.
func TestUnnumberedMenuNeedsEveryPieceOfItsLayout(t *testing.T) {
	cases := map[string]string{
		"no footer": " Pick one\n\n ❯ No, exit\n   Yes, I trust this folder\n",
		"misaligned row": " Pick one\n\n ❯ No, exit\n  Yes, I trust this folder\n\n" +
			" Enter to confirm · Esc to cancel\n",
		"two highlights": " Pick one\n\n ❯ No, exit\n ❯ Yes, I trust this folder\n\n" +
			" Enter to confirm · Esc to cancel\n",
		"one row":      " Pick one\n\n ❯ No, exit\n\n Enter to confirm · Esc to cancel\n",
		"no highlight": " Pick one\n\n   No, exit\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel\n",
		"transcript row between": " Pick one\n\n ❯ No, exit\n some agent prose here\n   Yes, I trust this folder\n\n" +
			" Enter to confirm · Esc to cancel\n",
	}
	for name, screenText := range cases {
		t.Run(name, func(t *testing.T) {
			if p := parsePrompt(newScreen(screenText)); p != nil {
				t.Errorf("parsed as a prompt: %+v", p)
			}
		})
	}
}

// A numbered menu must keep the numbered path, so Respond keeps sending the
// digit that #168 measured committing there.
func TestNumberedMenuIsNotReportedUnnumbered(t *testing.T) {
	p, unnumbered := parsePromptMenu(newScreen(fixtureTrustPrompt))
	if p == nil || unnumbered {
		t.Fatalf("numbered trust prompt: prompt = %v, unnumbered = %v", p, unnumbered)
	}
}

func keySends(f *fakeMux) [][]string {
	var out [][]string
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			out = append(out, sentKeys(c))
		}
	}
	return out
}

// #171's fix: choosing the second row walks the highlight there, reads that
// it arrived, and only then confirms. No digit is ever sent.
func TestRespondWalksAnUnnumberedMenuThenConfirms(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	got := respondWithNonce(t, d, f, 2)
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("respond = %q (%s), want submitted", got.Outcome, got.Reason)
	}
	if m.chosen != 2 {
		t.Errorf("confirmed row = %d, want 2", m.chosen)
	}
	if want := "[[Down] [C-m]]"; fmt.Sprint(keySends(f)) != want {
		t.Errorf("key sends = %v, want %s", keySends(f), want)
	}
	if !strings.Contains(got.Reason, "Yes, I trust this folder") {
		t.Errorf("receipt = %q; must name the option chosen", got.Reason)
	}
}

func TestRespondWalksUpOnAnUnnumberedMenu(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	m.options = append(m.options, "Third row")
	m.sel = 2
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	if got := respondWithNonce(t, d, f, 1); got.Outcome != fleet.OutcomeSubmitted || m.chosen != 1 {
		t.Fatalf("respond = %q (%s), chosen = %d; want submitted and row 1", got.Outcome, got.Reason, m.chosen)
	}
	if want := "[[Up Up] [C-m]]"; fmt.Sprint(keySends(f)) != want {
		t.Errorf("key sends = %v, want %s", keySends(f), want)
	}
}

// The row already highlighted needs no walk.
func TestRespondOnTheHighlightedUnnumberedRowSendsNoArrow(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	if got := respondWithNonce(t, d, f, 1); got.Outcome != fleet.OutcomeSubmitted || m.chosen != 1 {
		t.Fatalf("respond = %q (%s), chosen = %d", got.Outcome, got.Reason, m.chosen)
	}
	for _, keys := range keySends(f) {
		for _, k := range keys {
			if k == "Up" || k == "Down" {
				t.Errorf("an arrow was sent for the row already highlighted: %v", keySends(f))
			}
		}
	}
}

// If the highlight does not arrive, nothing may be confirmed: C-m would
// accept the row it stopped on. On the trust menu that row is "No, exit".
func TestRespondConfirmsNothingWhenTheHighlightDoesNotArrive(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	m.swallowArrows = true
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	got := respondWithNonce(t, d, f, 2)
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("respond = %q (%s), want unknown", got.Outcome, got.Reason)
	}
	if m.chosen != 0 {
		t.Fatalf("row %d was confirmed although the highlight never reached row 2", m.chosen)
	}
	if sends := newlineSends(f); len(sends) != 0 {
		t.Errorf("a confirm key was sent: %v", sends)
	}
	if !strings.Contains(got.Reason, "did not arrive") {
		t.Errorf("receipt = %q; must say the highlight did not arrive", got.Reason)
	}
}

// End to end through the create path: a caller that consented to trust gets
// its session past the unnumbered menu with the trust row, not the decline.
func TestTrustConsentAnswersTheUnnumberedMenu(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha", TrustCwd: true})

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		chosen := m.chosen
		f.mu.Unlock()
		if chosen != 0 {
			if chosen != 2 {
				t.Fatalf("consent confirmed row %d (%s), want 2 (Yes, I trust this folder)",
					chosen, m.options[chosen-1])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the trust consent never answered the unnumbered menu")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The respond path uses context only through Respond; keep the import honest.
var _ = context.Background
