package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// The card's other states (colab-fleet #217): the send confirmation, the
// in-flight line, the send error, the question about turning drafts off, and the
// feedback panel `1` opens. Every text below was measured on a real pane; the
// screens are built around them the way the corpus cases are, with the draft's
// words replaced by plain ones.

// The wrapped rows the runtime prints at 80 columns, as measured.
var (
	confirmRows = []string{
		"Send without reviewing (full draft + env, no transcript)? 2 to send · Esc to",
		"back",
	}
	errorRows = []string{
		"✘ Couldn't send feedback (couldn't reach the service). The draft is still",
		"queued. Try again later. 1 to review & retry · Esc to dismiss",
	}
	sendingRows = []string{"Sending…"}
)

// feedbackPanelScreen is the panel `1` opens, list or editor view, over a pane
// whose composer it has replaced.
func feedbackPanelScreen(editor bool) string {
	lines := []string{"", "⏺ Test complete.", "", "✻ Brewed for 5s · done 12:56 PM", strings.Repeat("▔", 80), "   Feedback drafts", ""}
	if editor {
		lines = append(lines,
			"     Type: bug",
			"     Title:",
			"       a title",
			"     Details:",
			"       │ - a detail",
			"     Send transcript: yes · sends this conversation",
			"",
			"   ❯ Send feedback",
			"",
			"   Turn off Claude-drafted feedback anytime in /config.")
	} else {
		lines = append(lines,
			"     This session",
			"   ❯ a draft   bug · 1m",
			"     Other sessions",
			"     another draft   idea",
			"",
			"     + Write new feedback",
			"",
			"   Drafts live only on this machine and are never sent without you. Unsent",
			"   drafts expire after 30 days.",
			"",
			"   "+feedbackPanelHint)
	}
	return strings.Join(lines, "\n")
}

// feedbackQuestionScreen is the question about turning drafts off, a plain row
// above a composer that reads whole.
func feedbackQuestionScreen() string {
	return strings.Join([]string{
		"⏺ Test complete.", "", "✻ Cogitated for 5s · done 1:03 PM", "",
		feedbackQuestionRow, "", "",
		rule, "❯", rule, "  ⏸ manual mode on · ? for shortcuts · 3 feedback drafts",
	}, "\n")
}

func TestFeedbackCardStatesAreReadFromTheirWholeTexts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status []string
		want   feedbackState
		queued string
	}{
		{"the key row", []string{feedbackFooter}, feedbackKeys, ""},
		{"the key row with a queue", []string{feedbackFooter + " · +2 more queued"}, feedbackKeys, "+2 more queued"},
		{"the confirmation, wrapped", confirmRows, feedbackConfirm, ""},
		{"the confirmation, unwrapped", []string{strings.Join(confirmRows, " ")}, feedbackConfirm, ""},
		{"sending", sendingRows, feedbackSending, ""},
		{"the error, wrapped", errorRows, feedbackError, ""},
		{"the error, wrapped narrower", []string{
			"✘ Couldn't send feedback (couldn't reach", "the service). The draft is still queued.", "Try again later. 1 to review & retry ·", "Esc to dismiss"}, feedbackError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cardScreen(cardOpts{tall: true, body: []string{"- a detail"}, status: tc.status})
			c, ok := liveFeedbackCard(newScreen(raw))
			if !ok {
				t.Fatalf("no card read from:\n%s", raw)
			}
			if c.state != tc.want || c.queued != tc.queued {
				t.Errorf("state %v queued %q, want %v %q", c.state, c.queued, tc.want, tc.queued)
			}
			if len(c.body) != 2 {
				t.Errorf("body %q: the title and one detail row are the draft, the status text is not", c.body)
			}
		})
	}
}

// The agent writes the title and the preview. Whatever it writes, the status
// text is the runtime's, read from the bottom of the box.
func TestFeedbackStatusTextInTheDraftIsNotTheCardsStatus(t *testing.T) {
	// The draft's own last row says what the send error says; the runtime's key
	// row is still the status, and it is the key row that is read.
	raw := cardScreen(cardOpts{tall: true, body: []string{"- " + errorRows[0], "- " + errorRows[1]}})
	c, ok := liveFeedbackCard(newScreen(raw))
	if !ok || c.state != feedbackKeys {
		t.Fatalf("state %v ok %v, want the key row", c.state, ok)
	}
	// And a box whose tail is not one of the four is not a card at all.
	for _, tail := range []string{"Sending", "Sent", "Sending… done", "Sending…!"} {
		if _, ok := liveFeedbackCard(newScreen(cardScreen(cardOpts{tall: true, status: []string{tail}}))); ok {
			t.Errorf("read a card whose status is %q", tail)
		}
	}
}

// Only the key row over an empty composer is a prompt; every other state is one
// a person answers at the terminal, and reads as unknown with the state named.
func TestFeedbackNonKeyStatesNeverArePrompts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   []string
		noPrompt bool
		mention  string
	}{
		{"confirmation, ❯ row hidden", confirmRows, true, "confirm sending"},
		{"confirmation, ❯ row visible", confirmRows, false, "confirm sending"},
		{"sending, ❯ row hidden", sendingRows, true, "\"Sending…\""},
		{"sending, ❯ row visible", sendingRows, false, "\"Sending…\""},
		{"error, ❯ row hidden", errorRows, true, "sending the draft failed"},
		{"error, ❯ row visible", errorRows, false, "sending the draft failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cardScreen(cardOpts{body: []string{"- a detail"}, status: tc.status, noPromptRow: tc.noPrompt})
			st, amb := classifyAgedDetailVisible(raw, 24, true, false)
			if amb != ambNone {
				t.Errorf("ambiguity %v", amb)
			}
			if st.Status != fleet.StatusUnknown || st.Prompt != nil {
				t.Fatalf("status %q prompt %+v (%s), want unknown with no prompt — never idle, never something to answer", st.Status, st.Prompt, st.Evidence)
			}
			if !strings.Contains(st.Evidence, "feedback-draft card") || !strings.Contains(st.Evidence, tc.mention) {
				t.Errorf("evidence %q does not name the card and %q", st.Evidence, tc.mention)
			}
			if p, _ := parsePromptShape(newScreen(raw)); p != nil {
				t.Errorf("parsed as a prompt: %+v", p)
			}
		})
	}
}

// A hidden ❯ row: whether the composer holds text cannot be read, so even the
// key row is not answerable there (a digit could be appended to hidden text).
func TestFeedbackKeyRowWithTheComposerRowHiddenIsNotAPrompt(t *testing.T) {
	raw := cardScreen(cardOpts{body: []string{"- a detail"}, noPromptRow: true})
	st, _ := classifyAgedDetailVisible(raw, 24, true, false)
	if st.Status != fleet.StatusUnknown || st.Prompt != nil {
		t.Fatalf("status %q prompt %+v (%s)", st.Status, st.Prompt, st.Evidence)
	}
	if !strings.Contains(st.Evidence, "❯ row included") {
		t.Errorf("evidence %q should say the ❯ row is off the bottom", st.Evidence)
	}
}

func TestFeedbackNonKeyStatesOverAReadableComposerAreIdle(t *testing.T) {
	for name, status := range map[string][]string{"confirmation": confirmRows, "sending": sendingRows, "error": errorRows} {
		raw := cardScreen(cardOpts{tall: true, body: []string{"- a detail"}, status: status})
		st, _ := classifyAgedDetailVisible(raw, 40, true, false)
		if st.Status != fleet.StatusIdle || st.Prompt != nil {
			t.Errorf("%s: status %q prompt %+v (%s), want idle: the card blocks nothing here", name, st.Status, st.Prompt, st.Evidence)
		}
		// A message still goes through; a lone digit is the card's shortcut.
		if got, f, _ := sendCardSession(t, raw, "please carry on"); got.Outcome == fleet.OutcomeRefused {
			t.Errorf("%s: an ordinary message was refused: %s", name, got.Reason)
		} else {
			pasted := false
			for _, c := range f.callsSnapshot() {
				if len(c) > 0 && c[0] == "load-buffer" {
					pasted = true
				}
			}
			if !pasted {
				t.Errorf("%s: nothing was pasted", name)
			}
		}
		if got, f, _ := sendCardSession(t, raw, "2"); got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "single digit") {
			t.Errorf("%s: a lone 2 (send, or confirm the send): outcome %q reason %q", name, got.Outcome, got.Reason)
		} else {
			nothingWritten(t, f)
		}
	}
}

// The state is part of the draft's identity: an answer read against the key row
// must not be applied to the confirmation of the same draft.
func TestFeedbackNonceFollowsTheState(t *testing.T) {
	nonce := func(status []string) string {
		c, ok := liveFeedbackCard(newScreen(cardScreen(cardOpts{body: []string{"- a detail"}, status: status})))
		if !ok {
			t.Fatalf("no card for %q", status)
		}
		return c.nonce()
	}
	keys, confirm, sending, errored := nonce([]string{feedbackFooter}), nonce(confirmRows), nonce(sendingRows), nonce(errorRows)
	seen := map[string]string{}
	for name, n := range map[string]string{"keys": keys, "confirm": confirm, "sending": sending, "error": errored} {
		if other, dup := seen[n]; dup {
			t.Errorf("%s and %s share the nonce %s", name, other, n)
		}
		seen[n] = name
	}
	// The key row's nonce is what it was before the other states were read.
	c, _ := liveFeedbackCard(newScreen(cardScreen(cardOpts{body: []string{"- a detail"}, status: []string{feedbackFooter}})))
	legacy := feedbackCard{feedbackCardBox: feedbackCardBox{body: c.body, queued: c.queued, state: feedbackKeys}}
	if legacy.nonce() != keys {
		t.Errorf("the key row's nonce moved")
	}
}

func TestSendAndKeysAndRespondNameTheStateOfACardTheyCannotWriteUnder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status []string
		want   string
	}{
		{"confirmation", confirmRows, "confirm sending"},
		{"sending", sendingRows, "Sending…"},
		{"error", errorRows, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cardScreen(cardOpts{body: []string{"- a detail"}, status: tc.status, noPromptRow: true})

			got, f, d := sendCardSession(t, raw, "do the thing")
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "feedback-draft card") || !strings.Contains(got.Reason, tc.want) {
				t.Errorf("send: outcome %q reason %q", got.Outcome, got.Reason)
			}
			if strings.Contains(got.Reason, "no composer has been painted") {
				t.Errorf("send blamed startup: %q", got.Reason)
			}
			nothingWritten(t, f)
			if n := d.Counters()[counterFeedbackCardRefusedSend]; n != 1 {
				t.Errorf("%s = %d, want 1", counterFeedbackCardRefusedSend, n)
			}

			f2 := twoSessions()
			f2.captures["%1"] = raw
			d2 := newTestDriver(f2)
			ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
			if r, err := d2.Keys(context.Background(), testCaller, ref, fleet.KeyEnter, digestOf(t, d2, "alpha💬")); err != nil ||
				r.Outcome != fleet.OutcomeRefused || !strings.Contains(r.Reason, "feedback-draft card") {
				t.Errorf("keys Enter: %+v %v", r, err)
			}
			if r := respondCard(t, d2, fleet.Response{Choice: 3, Nonce: "0000000000000000"}); r.Outcome != fleet.OutcomeRefused ||
				!strings.Contains(r.Reason, "feedback-draft card") {
				t.Errorf("respond: %+v", r)
			}
			if _, err := d2.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{}); err == nil ||
				!strings.Contains(err.Error(), "feedback-draft card") {
				t.Errorf("discard: %v, want a refusal naming the card — the composer's row cannot be seen", err)
			}
			if calls := sendCalls(f2); len(calls) != 0 {
				t.Errorf("keys were sent: %q", calls)
			}
		})
	}
}

// Escape is the one key accepted through keys over a card this driver cannot
// read past, and only where it measurably leaves: the key row and the error are
// dismissed, and the confirmation returns to the key row.
func TestKeysEscapeStepsBackFromTheConfirmationAndTheError(t *testing.T) {
	for name, status := range map[string][]string{"key row": {feedbackFooter}, "confirmation": confirmRows, "error": errorRows} {
		f := twoSessions()
		f.captures["%1"] = cardScreen(cardOpts{body: []string{"- a detail"}, status: status, noPromptRow: true})
		d := newTestDriver(f)
		ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
		got, err := d.Keys(context.Background(), testCaller, ref, fleet.KeyEscape, digestOf(t, d, "alpha💬"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Outcome == fleet.OutcomeRefused {
			t.Errorf("%s: Escape was refused: %s", name, got.Reason)
		}
		if calls := sendCalls(f); len(calls) != 1 || calls[0] != "Escape" {
			t.Errorf("%s: send-keys %q, want one Escape", name, calls)
		}
		// Anything else is refused, and says what is accepted.
		f2 := twoSessions()
		f2.captures["%1"] = f.captures["%1"]
		d2 := newTestDriver(f2)
		r, err := d2.Keys(context.Background(), testCaller, ref, fleet.KeyEnter, digestOf(t, d2, "alpha💬"))
		if err != nil || r.Outcome != fleet.OutcomeRefused || !strings.Contains(r.Reason, "only Escape is accepted") {
			t.Errorf("%s: Enter: %+v %v", name, r, err)
		}
		nothingWritten(t, f2)
	}
	// Not while the send is in flight: what Escape does to a request nobody has
	// measured is not known.
	f := twoSessions()
	f.captures["%1"] = cardScreen(cardOpts{body: []string{"- a detail"}, status: sendingRows, noPromptRow: true})
	d := newTestDriver(f)
	got, _ := d.Keys(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEscape, digestOf(t, d, "alpha💬"))
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("Escape while sending: %+v", got)
	}
	nothingWritten(t, f)
}

// keys(Escape) over a card and a composer that both read is not refused: it
// dismisses the card, the draft stays queued, and the receipt says so.
func TestKeysEscapeOverAReadableComposerIsDeliveredAndSaysWhatItDid(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = cardScreen(cardOpts{tall: true, body: []string{"- a detail"}})
	d := newTestDriver(f)
	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEscape, digestOf(t, d, "alpha💬"))
	if err != nil || got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("outcome %q (%s) err %v: Escape must reach the card", got.Outcome, got.Reason, err)
	}
	if calls := sendCalls(f); len(calls) != 1 || calls[0] != "Escape" {
		t.Errorf("send-keys %q, want one Escape", calls)
	}
	for _, want := range []string{"dismisses the notice", "stays queued"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("receipt %q lacks %q", got.Reason, want)
		}
	}
	// With text in the composer Escape does nothing to the card, and says nothing.
	f2 := twoSessions()
	f2.captures["%1"] = cardScreen(cardOpts{tall: true, row: "half a thought"})
	d2 := newTestDriver(f2)
	if r, _ := d2.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEscape, composerDigestOf(t, d2, "alpha💬")); strings.Contains(r.Reason, "dismisses") {
		t.Errorf("Escape over typed text claimed to dismiss the card: %q", r.Reason)
	}
}

// interrupt is not guarded: a stop must not be refused by a notice, and the card
// is not on screen while a turn runs. What it does to a card is dismiss it.
func TestInterruptIsNotGuardedByAFeedbackCard(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = cardScreen(cardOpts{tall: true, body: []string{"- a detail"}})
	d := newTestDriver(f)
	ack, err := d.Interrupt(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("interrupt: %+v %v", ack, err)
	}
	if calls := sendCalls(f); len(calls) != 1 || calls[0] != "Escape" {
		t.Errorf("send-keys %q, want one Escape", calls)
	}
}

// --- the question about turning drafts off ---

func TestFeedbackQuestionIsIdleAndNeverAPrompt(t *testing.T) {
	raw := feedbackQuestionScreen()
	if !liveFeedbackQuestion(newScreen(raw)) {
		t.Fatalf("question not read from:\n%s", raw)
	}
	st, _ := classifyAgedDetailVisible(raw, 24, true, false)
	if st.Status != fleet.StatusIdle || st.Prompt != nil {
		t.Errorf("status %q prompt %+v (%s): the question hides nothing and answers to a person", st.Status, st.Prompt, st.Evidence)
	}
	// The question is the runtime's whole row. Words in the agent's output do not count.
	for _, row := range []string{
		"⏺ " + feedbackQuestionRow,
		"  Turn off Claude-drafted feedback? 0 to turn off · Esc to keep more",
		"Turn off Claude-drafted feedback? 1 to turn off · Esc to keep",
	} {
		other := strings.Replace(raw, feedbackQuestionRow, row, 1)
		if liveFeedbackQuestion(newScreen(other)) {
			t.Errorf("read the question from %q", row)
		}
	}
	// Not when something sits between it and the composer.
	between := strings.Replace(raw, feedbackQuestionRow+"\n\n\n", feedbackQuestionRow+"\n\n⏺ and then I said\n", 1)
	if liveFeedbackQuestion(newScreen(between)) {
		t.Error("read the question with agent output between it and the composer")
	}
}

func TestSendOfALoneDigitUnderTheTurnOffQuestionIsRefused(t *testing.T) {
	for _, text := range []string{"0", "1", "2"} {
		got, f, _ := sendCardSession(t, feedbackQuestionScreen(), text)
		if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "single digit") {
			t.Errorf("%q: outcome %q reason %q", text, got.Outcome, got.Reason)
		}
		nothingWritten(t, f)
	}
	if got, _, _ := sendCardSession(t, feedbackQuestionScreen(), "please carry on"); got.Outcome == fleet.OutcomeRefused {
		t.Errorf("an ordinary message was refused under the question: %s", got.Reason)
	}
}

func TestKeysEscapeUnderTheTurnOffQuestionKeepsDraftsAndSaysSo(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = feedbackQuestionScreen()
	d := newTestDriver(f)
	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEscape, digestOf(t, d, "alpha💬"))
	if err != nil || got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("outcome %q (%s) err %v", got.Outcome, got.Reason, err)
	}
	if !strings.Contains(got.Reason, "keeps them on") {
		t.Errorf("receipt %q", got.Reason)
	}
}

// --- the panel ---

func TestFeedbackPanelIsUnknownAndNamed(t *testing.T) {
	for name, raw := range map[string]string{"list": feedbackPanelScreen(false), "editor": feedbackPanelScreen(true)} {
		if _, ok := liveFeedbackPanel(newScreen(raw)); !ok {
			t.Errorf("%s: panel not read from:\n%s", name, raw)
		}
		st, _ := classifyAgedDetailVisible(raw, 24, true, false)
		if st.Status != fleet.StatusUnknown || st.Prompt != nil || !strings.Contains(st.Evidence, "feedback panel") {
			t.Errorf("%s: status %q prompt %+v (%s), want unknown naming the panel — the finished-turn line above it read as an idle composer that is not there",
				name, st.Status, st.Prompt, st.Evidence)
		}
	}
}

func TestFeedbackPanelMustBeTheLiveScreen(t *testing.T) {
	// A composer's fence below it: the agent printed this, or it is history.
	withComposer := feedbackPanelScreen(false) + "\n" + rule + "\n❯\n" + rule
	if _, ok := liveFeedbackPanel(newScreen(withComposer)); ok {
		t.Error("read a panel with a composer beneath it")
	}
	for name, raw := range map[string]string{
		"the title without the rule": strings.Replace(feedbackPanelScreen(false), strings.Repeat("▔", 80), "", 1),
		"a short rule":               strings.Replace(feedbackPanelScreen(false), strings.Repeat("▔", 80), strings.Repeat("▔", 10), 1),
		"an indented rule":           strings.Replace(feedbackPanelScreen(false), strings.Repeat("▔", 80), "  "+strings.Repeat("▔", 78), 1),
		"another title":              strings.Replace(feedbackPanelScreen(false), "Feedback drafts", "Feedback drafts (2)", 1),
		"the title indented deeper":  strings.Replace(feedbackPanelScreen(false), "   Feedback drafts", "     Feedback drafts", 1),
	} {
		if _, ok := liveFeedbackPanel(newScreen(raw)); ok {
			t.Errorf("%s: read a panel", name)
		}
	}
}

func TestFeedbackPanelRefusesEveryDeliveryButEscape(t *testing.T) {
	for _, editor := range []bool{false, true} {
		raw := feedbackPanelScreen(editor)
		ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

		got, f, _ := sendCardSession(t, raw, "do the thing")
		if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "feedback panel") {
			t.Errorf("editor=%v send: outcome %q reason %q", editor, got.Outcome, got.Reason)
		}
		nothingWritten(t, f)

		f2 := twoSessions()
		f2.captures["%1"] = raw
		d2 := newTestDriver(f2)
		digest := digestOf(t, d2, "alpha💬")
		for _, key := range []fleet.KeyName{fleet.KeyEnter, fleet.KeyUp, fleet.KeyDown, fleet.KeyLeft, fleet.KeyRight, fleet.KeyBTab} {
			r, err := d2.Keys(context.Background(), testCaller, ref, key, digest)
			if err != nil || r.Outcome != fleet.OutcomeRefused || !strings.Contains(r.Reason, "feedback panel") {
				t.Errorf("editor=%v keys %s: %+v %v — Enter here opens a draft or sends it", editor, key, r, err)
			}
		}
		if r := respondCard(t, d2, fleet.Response{Choice: 1, Nonce: "0000000000000000"}); r.Outcome != fleet.OutcomeRefused ||
			!strings.Contains(r.Reason, "feedback panel") {
			t.Errorf("editor=%v respond: %+v", editor, r)
		}
		if _, err := d2.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{}); err == nil ||
			!strings.Contains(err.Error(), "feedback panel") {
			t.Errorf("editor=%v discard: %v — \"already clear\" would be a claim about a composer that is not there", editor, err)
		}
		if calls := sendCalls(f2); len(calls) != 0 {
			t.Errorf("editor=%v keys sent: %q", editor, calls)
		}

		f3 := twoSessions()
		f3.captures["%1"] = raw
		d3 := newTestDriver(f3)
		r, err := d3.Keys(context.Background(), testCaller, ref, fleet.KeyEscape, digestOf(t, d3, "alpha💬"))
		if err != nil || r.Outcome == fleet.OutcomeRefused {
			t.Errorf("editor=%v Escape: %+v %v — Escape is how the panel is left", editor, r, err)
		}
		if calls := sendCalls(f3); len(calls) != 1 || calls[0] != "Escape" {
			t.Errorf("editor=%v send-keys %q, want one Escape", editor, calls)
		}
		if !strings.Contains(r.Reason, "closes the panel") {
			t.Errorf("editor=%v receipt %q", editor, r.Reason)
		}
	}
}

// --- what respond says the session moved to ---

func TestRespondNamesWhatTheAnswerOpened(t *testing.T) {
	for _, tc := range []struct {
		key    string
		screen string
		want   string
	}{
		{"1", feedbackPanelScreen(false), "feedback panel"},
		{"2", cardScreen(cardOpts{body: []string{"- a detail"}, status: confirmRows, noPromptRow: true}), "asks to confirm"},
		{"2", cardScreen(cardOpts{body: []string{"- a detail"}, status: sendingRows, noPromptRow: true}), "sending the draft"},
		{"0", feedbackQuestionScreen(), "whether to turn drafts off"},
		{"0", idleFixtureFor("alpha"), "dismissed"},
	} {
		got := feedbackNext(tc.key, newScreen(tc.screen), true)
		if !strings.Contains(got, tc.want) {
			t.Errorf("key %s over %q: %q lacks %q", tc.key, tc.want, got, tc.want)
		}
	}
	// A read that never happened is worded as an expectation.
	if got := feedbackNext("2", screen{}, false); !strings.Contains(got, "should now ask") {
		t.Errorf("unread: %q", got)
	}
}

// --- redaction ---

func TestRedactKeepsTheStatusTextOfEveryStateAndDropsTheDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status []string
		keep   string
	}{
		{"key row", []string{feedbackFooter + " · +2 more queued"}, feedbackFooter + " · +2 more queued"},
		{"confirmation", confirmRows, "Send without reviewing (full draft + env, no transcript)? 2 to send · Esc to"},
		{"sending", sendingRows, "Sending…"},
		{"error", errorRows, "queued. Try again later. 1 to review & retry · Esc to dismiss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cardScreen(cardOpts{tall: true, title: "✻ Bug report drafted: SECRET-TITLE", body: []string{"- SECRET-DETAIL"}, status: tc.status})
			got := RedactCapture(raw)
			if strings.Contains(got, "SECRET") {
				t.Errorf("the draft survived:\n%s", got)
			}
			if !strings.Contains(got, tc.keep) {
				t.Errorf("the status text was dropped (want %q):\n%s", tc.keep, got)
			}
			if again := RedactCapture(got); again != got {
				t.Errorf("not a fixed point:\n%s\n---\n%s", got, again)
			}
			if c, ok := liveFeedbackCard(newScreen(got)); !ok || c.state == 0 {
				t.Errorf("the redacted screen no longer reads as a card:\n%s", got)
			}
		})
	}
}

// A reason nobody has measured came from a path nobody has read; it is not kept.
func TestRedactDropsASendErrorWhoseReasonWasNotMeasured(t *testing.T) {
	unread := []string{
		"✘ Couldn't send feedback (HTTP 500 from a private-host.example). The draft is still",
		"queued. Try again later. 1 to review & retry · Esc to dismiss",
	}
	raw := cardScreen(cardOpts{tall: true, body: []string{"- a detail"}, status: unread})
	if _, ok := liveFeedbackCard(newScreen(raw)); !ok {
		t.Fatal("the live read must still recognise the error")
	}
	got := RedactCapture(raw)
	if strings.Contains(got, "private-host") || strings.Contains(got, "HTTP 500") {
		t.Errorf("the unmeasured reason survived:\n%s", got)
	}
	if RedactCapture(got) != got {
		t.Errorf("not a fixed point:\n%s", got)
	}
	// Masked in place: the rows keep their shape and the screen still reads as
	// the error, with the runtime's fixed words around the mask.
	if c, ok := liveFeedbackCard(newScreen(got)); !ok || c.state != feedbackError {
		t.Errorf("the masked screen no longer reads as the send error:\n%s", got)
	}
	if !strings.Contains(got, "(#### ### ####") || !strings.Contains(got, "1 to review & retry · Esc to dismiss") {
		t.Errorf("the reason was not masked in place:\n%s", got)
	}
	// An error with no parenthesised reason has nothing to mask: its middle is
	// dropped whole, which is not a fixed point and is meant to be noticed.
	bare := []string{
		"✘ Couldn't send feedback: something the service said. The draft is still",
		"queued. Try again later. 1 to review & retry · Esc to dismiss",
	}
	if out := RedactCapture(cardScreen(cardOpts{tall: true, body: []string{"- a detail"}, status: bare})); strings.Contains(out, "something the service said") {
		t.Errorf("free text in a send error survived:\n%s", out)
	}
}

func TestRedactKeepsThePanelFrameAndTheQuestionAndNothingElse(t *testing.T) {
	got := RedactCapture(feedbackPanelScreen(false))
	for _, want := range []string{strings.Repeat("▔", 80), "   " + feedbackPanelTitle, "   " + feedbackPanelHint} {
		if !strings.Contains(got, want) {
			t.Errorf("the panel's frame lost %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"a draft", "another draft", "+ Write new feedback", "This session"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived redaction:\n%s", gone, got)
		}
	}
	if RedactCapture(got) != got {
		t.Error("the panel is not a fixed point")
	}
	if _, ok := liveFeedbackPanel(newScreen(got)); !ok {
		t.Errorf("the redacted panel no longer reads as one:\n%s", got)
	}
	q := RedactCapture(feedbackQuestionScreen())
	if !strings.Contains(q, feedbackQuestionRow) || RedactCapture(q) != q {
		t.Errorf("the question was not kept as a fixed point:\n%s", q)
	}
	_ = fmt.Sprint
}

// A tool call is drawn behind the response bullet, and its arguments are the
// agent's. statusLine is a shape (a symbol, a capital, then " for " anywhere), so
// a call whose arguments said "… for …" used to be kept whole as if it were the
// spinner line. The bullet is not a spinner frame.
func TestRedactDoesNotKeepAToolCallWhoseArgumentsLookLikeAStatusLine(t *testing.T) {
	for _, line := range []string{
		"⏺ SendFeedback(a title with the word for in it)",
		"⏺ Bash(grep -rn secret-token for the-customer)",
	} {
		if got := RedactCapture(line); got != "⏺ "+placeholderToken {
			t.Errorf("%q redacted to %q", line, got)
		}
	}
	if got := RedactCapture("✻ Cogitated for 26s · done 1:11 AM"); got != "✻ Cogitated for 26s · done 1:11 AM" {
		t.Errorf("the real status line was redacted: %q", got)
	}
}
