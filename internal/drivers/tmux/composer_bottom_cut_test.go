package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// A composer whose closing rule is cut off by the bottom of the pane
// (colab-fleet#216). The measured shape is #215's feedback-draft card on a
// 24-row pane, but the cause is general: anything tall enough above the composer
// leaves the screen ending on the composer's opening rule and its ❯ row, with no
// row below them for the closing rule or the mode row. These tests use a plain
// bordered notice, not the card, so they pin the general rule and not #215's
// recogniser.

// noticeScreen is a full pane whose last two rows are the composer's opening
// rule and its prompt row. The notice above them is a bordered box of
// `noticeRows` rows, the shape whose bottom border the old walk took for the
// composer's opening fence. tail is appended verbatim after the prompt row.
func noticeScreen(noticeRows int, row string, tail ...string) string {
	const width = 60
	var lines []string
	lines = append(lines, "  transcript line", "✻ Brewed for 1m 0s", "")
	lines = append(lines, "╭"+strings.Repeat("─", width-2)+"╮")
	for i := 0; i < noticeRows; i++ {
		lines = append(lines, fmt.Sprintf("│ %-*s│", width-3, fmt.Sprintf("notice row %d", i)))
	}
	lines = append(lines, "╰"+strings.Repeat("─", width-2)+"╯", "")
	lines = append(lines, strings.Repeat("─", 53)+" some-session ─", strings.TrimRight("❯ "+row, " "))
	lines = append(lines, tail...)
	return strings.Join(lines, "\n")
}

const noticeFullPane = 16 // rows of notice that put the prompt row on row 24 of the screen

func rowsOf(raw string) int { return strings.Count(raw, "\n") + 1 }

func TestBottomCutComposerReadsClippedNotAbsent(t *testing.T) {
	full := noticeScreen(noticeFullPane, "")
	if got := rowsOf(full); got != 24 {
		t.Fatalf("fixture: %d rows, want the 24 of a created session's pane", got)
	}
	for _, tc := range []struct {
		name string
		raw  string
		want composerScan
	}{
		{"an empty prompt row on the last row of the pane", full, composerClipped},
		{"text on that row", noticeScreen(noticeFullPane, "half a thought"), composerClipped},
		{"a capture that ends with a newline, as capture-pane's does", full + "\n", composerClipped},
		// What keeps the rule narrow, each one a mutation of the measured screen.
		{"blank rows under the prompt row: the pane had room, nothing was cut", noticeScreen(noticeFullPane-3, "", "", "", ""), composerAbsent},
		{"a box border directly above the prompt row, not the composer's own rule",
			strings.Replace(full, strings.Repeat("─", 53)+" some-session ─\n", "", 1), composerAbsent},
		{"a numbered option on the last row: a menu, not a composer",
			strings.Replace(full, "\n❯", "\n❯ 1. Yes", 1), composerAbsent},
		{"a whole composer, its closing rule drawn", noticeScreen(noticeFullPane-3, "", rule, "  ⏵⏵ bypass permissions on"), composerFound},
	} {
		s := newScreenVisible(tc.raw, 24)
		if _, _, got := composerSpan(s); got != tc.want {
			t.Errorf("%s: composerSpan = %v, want %v", tc.name, got, tc.want)
		}
		if _, got := composerText(s); got != tc.want {
			t.Errorf("%s: composerText = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A screen that is clipped only because of the pane's bottom edge names that
// cause, and says nothing of a capture window it is not taller than.
func TestBottomCutComposerNamesItsOwnCause(t *testing.T) {
	cut := newScreenVisible(noticeScreen(noticeFullPane, ""), 24)
	if got := composerClippedCause(cut); !strings.Contains(got, "bottom edge of the pane") || strings.Contains(got, "capture window") {
		t.Errorf("bottom-cut cause = %q", got)
	}
	tall := newScreenVisible(clippedComposerFixture(), 0)
	if _, _, scan := composerSpan(tall); scan != composerClipped {
		t.Fatalf("fixture: a tall composer's tail reads %v, want clipped", scan)
	}
	if got := composerClippedCause(tall); !strings.Contains(got, "capture window") || strings.Contains(got, "bottom edge") {
		t.Errorf("tall-composer cause = %q, its wording must be unchanged", got)
	}
	// The rows the pane cuts off are not scrollback: the #169 counter is for
	// fences above the visible pane and must not claim this refusal.
	if clippedOnlyAboveVisiblePane(cut) {
		t.Error("a bottom-cut composer was counted as one clipped only by the visible-pane rule")
	}
}

// The state read used to say idle here: the finished-spinner branch never
// noticed the composer had not been read.
func TestStateOfABottomCutComposerIsNotIdle(t *testing.T) {
	for _, row := range []string{"", "half a thought"} {
		st := classifyAt(noticeScreen(noticeFullPane, row), 24)
		if st.Status != fleet.StatusUnknown {
			t.Fatalf("row %q: status %q (%s), want unknown", row, st.Status, st.Evidence)
		}
		if !strings.Contains(st.Evidence, "bottom edge of the pane") {
			t.Errorf("row %q: evidence %q does not name what was cut off", row, st.Evidence)
		}
	}
}

// A finished spinner with no composer read at all is not "composer empty".
func TestFinishedSpinnerWithNoComposerIsNotIdle(t *testing.T) {
	for name, raw := range map[string]string{
		"no rule anywhere":                  "  transcript line\n✻ Brewed for 1m 0s\n  some other screen",
		"a rule with no composer inside it": "  transcript line\n✻ Brewed for 1m 0s\n" + rule + "\n  not a composer\n" + rule,
	} {
		st, _ := classifyAgedDetailVisible(raw, 24, true, false)
		if st.Status == fleet.StatusIdle {
			t.Errorf("%s: idle (%s) — no composer was read, so nothing says the session takes input", name, st.Evidence)
		}
		if st.Status != fleet.StatusUnknown || !strings.Contains(st.Evidence, "no composer found") {
			t.Errorf("%s: status %q evidence %q, want unknown naming the missing composer", name, st.Status, st.Evidence)
		}
	}
	// The healthy shape is untouched: finished spinner, composer found, empty.
	if st := classifyAt(idleFixtureFor("x"), 24); st.Status != fleet.StatusIdle {
		t.Errorf("a composer that reads whole and empty: status %q (%s), want idle", st.Status, st.Evidence)
	}
}

// send, keys and discard each refuse a bottom-cut composer with the clipped
// refusals, name the real cause, write nothing and are counted.
func TestVerbsRefuseABottomCutComposer(t *testing.T) {
	for _, row := range []string{"", "half a thought"} {
		raw := noticeScreen(noticeFullPane, row)
		ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
		ctx := context.Background()

		f := twoSessions()
		f.captures["%1"] = raw
		d := newTestDriver(f)
		got, err := d.Send(ctx, testCaller, ref, "carry on", driver.SendOptions{Submit: true})
		if err != nil || got.Outcome != fleet.OutcomeRefused {
			t.Fatalf("row %q send: %+v %v, want a refusal", row, got, err)
		}
		if !strings.Contains(got.Reason, "bottom edge of the pane") {
			t.Errorf("row %q send reason %q does not name the cause", row, got.Reason)
		}
		if strings.Contains(got.Reason, "no composer has been painted") {
			t.Errorf("row %q send blamed startup for a composer that is there: %q", row, got.Reason)
		}
		assertClippedRemedy(t, got.Reason)
		if n := d.Counters()[counterComposerClippedRefusedSend]; n != 1 {
			t.Errorf("row %q %s = %d, want 1", row, counterComposerClippedRefusedSend, n)
		}
		if n := d.Counters()[counterComposerClippedBottomCut]; n != 1 {
			t.Errorf("row %q %s = %d, want 1: the rate of this rule is what the Issue asked to be readable", row, counterComposerClippedBottomCut, n)
		}
		if n := d.Counters()[counterComposerClippedAboveVisiblePane]; n != 0 {
			t.Errorf("row %q %s = %d, want 0: the rows are cut off below, not above", row, counterComposerClippedAboveVisiblePane, n)
		}
		nothingWritten(t, f)

		kf := twoSessions()
		kf.captures["%1"] = raw
		kd := newTestDriver(kf)
		kr, err := kd.Keys(ctx, testCaller, ref, fleet.KeyEnter, digestOf(t, kd, "alpha💬"))
		if err != nil || kr.Outcome != fleet.OutcomeRefused {
			t.Fatalf("row %q keys: %+v %v, want a refusal", row, kr, err)
		}
		if !strings.Contains(kr.Reason, "bottom edge of the pane") {
			t.Errorf("row %q keys reason %q does not name the cause", row, kr.Reason)
		}
		assertClippedRemedy(t, kr.Reason)
		if n := kd.Counters()[counterComposerClippedRefusedKeys]; n != 1 || kd.Counters()[counterComposerClippedBottomCut] != 1 {
			t.Errorf("row %q %s = %d, %s = %d, want 1 and 1", row, counterComposerClippedRefusedKeys, n,
				counterComposerClippedBottomCut, kd.Counters()[counterComposerClippedBottomCut])
		}
		nothingWritten(t, kf)

		df := twoSessions()
		df.captures["%1"] = raw
		dd := newTestDriver(df)
		// "Already clear" was the answer for a row this could not read.
		_, err = dd.Discard(ctx, testCaller, ref, "", driver.DiscardOptions{})
		if err == nil || !strings.Contains(err.Error(), "bottom edge of the pane") {
			t.Fatalf("row %q discard: %v, want a refusal naming the cause", row, err)
		}
		assertClippedRemedy(t, err.Error())
		if n := dd.Counters()[counterComposerClippedRefusedDiscard]; n != 1 || dd.Counters()[counterComposerClippedBottomCut] != 1 {
			t.Errorf("row %q %s = %d, %s = %d, want 1 and 1", row, counterComposerClippedRefusedDiscard, n,
				counterComposerClippedBottomCut, dd.Counters()[counterComposerClippedBottomCut])
		}
		nothingWritten(t, df)
	}
}
