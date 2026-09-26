package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// The runtime's feedback-draft card (colab-fleet #215). The geometry that
// matters is measured, not modelled: a fleet-created pane is 24 rows by 80, the
// card is seven of them, and what is left shows the composer's opening rule and
// its ❯ row with nothing below — no closing rule, no mode row.

// cardOpts describes one screen of a session showing the card.
type cardOpts struct {
	// tall paints the composer's closing rule and mode row too, as a pane tall
	// enough to hold them does.
	tall bool
	// row is what the ❯ row holds after the marker.
	row string
	// dimRow renders row dim, the way the composer's placeholder hint is.
	dimRow bool
	// keyRow overrides the card's key row (default: the measured one).
	keyRow string
	// status, when set, replaces the key row with these rows, one card row each:
	// the shape of a status text that wraps (the send confirmation and the send
	// error do, at 80 columns).
	status []string
	// queued appends the queued suffix to the default key row, joined by sep.
	queued, sep string
	// title is the draft's title row (default: a plain one).
	title string
	// body are the detail rows under the title.
	body []string
	// indent shifts the whole card right by this many columns.
	indent int
	// noPromptRow drops the composer's ❯ row: the screen ends on the opening
	// rule, the geometry a wrapped status text leaves a 24-row pane in.
	noPromptRow bool
	// gap is the rows between the card's bottom border and the fence
	// (default: the blank margin and the hint row).
	gap []string
	// transcript replaces the six plain rows above the card.
	transcript []string
}

const cardWidth = 80

func cardRow(indent int, inner string) string {
	pad := cardWidth - 2 - 1 - len([]rune(inner)) - indent
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", indent) + "│ " + inner + strings.Repeat(" ", pad) + "│"
}

func cardScreen(o cardOpts) string {
	inner := cardWidth - 2 - o.indent
	top := strings.Repeat(" ", o.indent) + "╭" + strings.Repeat("─", inner) + "╮"
	bottom := strings.Repeat(" ", o.indent) + "╰" + strings.Repeat("─", inner) + "╯"
	title := o.title
	if title == "" {
		title = "✻ Bug report drafted: something went sideways …"
	}
	key := o.keyRow
	if key == "" {
		key = feedbackFooter
		if o.queued != "" {
			key += o.sep + o.queued
		}
	}
	lines := o.transcript
	if lines == nil {
		for i := 0; i < 6; i++ {
			lines = append(lines, fmt.Sprintf("  transcript line %d", i))
		}
	}
	lines = append(lines, "", "✻ Cogitated for 26s · done 1:11 AM", "", top, cardRow(o.indent, title))
	for _, b := range o.body {
		lines = append(lines, cardRow(o.indent, "│ "+b))
	}
	if len(o.status) > 0 {
		for _, row := range o.status {
			lines = append(lines, cardRow(o.indent, row))
		}
		lines = append(lines, bottom)
	} else {
		lines = append(lines, cardRow(o.indent, key), bottom)
	}
	gap := o.gap
	if gap == nil {
		gap = []string{"", "                                        new task? /clear to save 203.9k tokens"}
	}
	lines = append(lines, gap...)
	lines = append(lines, strings.Repeat("─", 53)+" some-session ─")
	if o.noPromptRow {
		return strings.Join(lines, "\n")
	}
	row := "❯"
	if o.row != "" {
		if o.dimRow {
			row += " " + sgrDimOn + o.row + sgrReset
		} else {
			row += " " + o.row
		}
	}
	lines = append(lines, row)
	if o.tall {
		lines = append(lines, rule, "  ⏵⏵ bypass permissions on")
	}
	return strings.Join(lines, "\n")
}

func cardShort() string {
	return cardScreen(cardOpts{body: []string{"- What happened: it did a thing"}})
}

func classifyAt(raw string, height int) fleet.SessionState {
	st, _ := classifyAgedDetailVisible(raw, height, true, false)
	return st
}

// The measured shape: the card over a composer whose closing rule is off the
// bottom. The state read used to say idle; it is now the prompt the card is.
func TestFeedbackCardOverAClippedComposerIsAPrompt(t *testing.T) {
	for _, h := range []int{0, 24} {
		st, amb := classifyAgedDetailVisible(cardShort(), h, true, false)
		if amb != ambNone {
			t.Errorf("height %d: ambiguity %v — the kind must stand on the first read", h, amb)
		}
		if st.Status != fleet.StatusWaitingInput || st.WaitingOn != fleet.WaitingPrompt {
			t.Fatalf("height %d: status %q waitingOn %q (%s), want waiting_input on a prompt", h, st.Status, st.WaitingOn, st.Evidence)
		}
		p := st.Prompt
		if p == nil {
			t.Fatalf("height %d: no prompt", h)
		}
		if p.Kind != fleet.PromptFeedbackReview {
			t.Errorf("kind = %q, want %q", p.Kind, fleet.PromptFeedbackReview)
		}
		if fmt.Sprint(p.Options) != "[review send dismiss]" {
			t.Errorf("options = %q", p.Options)
		}
		if p.Selected != 0 || p.MultiSelect || p.FreeText {
			t.Errorf("selected %d multiSelect %v freeText %v: the card highlights nothing and is neither", p.Selected, p.MultiSelect, p.FreeText)
		}
		if p.Nonce == "" {
			t.Error("no nonce")
		}
		if want := "Bug report drafted: something went sideways …"; p.Question != want {
			t.Errorf("question = %q, want the draft's title %q", p.Question, want)
		}
		if !strings.Contains(st.Evidence, "feedback-draft card") {
			t.Errorf("evidence = %q; it must name the card", st.Evidence)
		}
	}
}

// The composer's placeholder hint is not text a key would be appended to.
func TestFeedbackCardOverAPlaceholderIsStillAPrompt(t *testing.T) {
	st := classifyAt(cardScreen(cardOpts{row: "Try \"fix the failing test\"", dimRow: true}), 24)
	if st.Status != fleet.StatusWaitingInput || st.Prompt == nil || st.Prompt.Kind != fleet.PromptFeedbackReview {
		t.Fatalf("status %q prompt %+v (%s)", st.Status, st.Prompt, st.Evidence)
	}
}

// Fork A: on a pane tall enough to show the composer whole, the card blocks
// nothing — the session takes messages — so no prompt is reported.
func TestFeedbackCardAboveAReadableComposerIsNotAPrompt(t *testing.T) {
	raw := cardScreen(cardOpts{tall: true})
	if _, scan := composerText(newScreen(raw)); scan != composerFound {
		t.Fatalf("fixture: composer scan = %v, want found", scan)
	}
	st := classifyAt(raw, 0)
	if st.Status != fleet.StatusIdle || st.Prompt != nil {
		t.Fatalf("status %q prompt %+v (%s); a card above a readable composer is not a wait", st.Status, st.Prompt, st.Evidence)
	}
	if p, _ := parsePromptShape(newScreen(raw)); p != nil {
		t.Errorf("parsePromptShape read a prompt from a card above a readable composer: %+v", p)
	}
}

// Text on the ❯ row would have a digit appended to it, so nothing is answerable;
// and idle would be false, because the composer was not read.
func TestFeedbackCardOverTypedTextInAClippedComposerIsUnknown(t *testing.T) {
	st := classifyAt(cardScreen(cardOpts{row: "merge it once CI is green"}), 24)
	if st.Status != fleet.StatusUnknown || st.Prompt != nil {
		t.Fatalf("status %q prompt %+v, want unknown with no prompt", st.Status, st.Prompt)
	}
	if !strings.Contains(st.Evidence, "feedback-draft card") {
		t.Errorf("evidence = %q; it must name the card", st.Evidence)
	}
}

// Everything the recogniser refuses to read as the card. Each is one mutation of
// the measured screen, so a pass here means the detector is not matching on
// something looser than what was measured.
func TestFeedbackCardFooterMustBeExact(t *testing.T) {
	long := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, tc := range []struct {
		name string
		o    cardOpts
	}{
		{"reordered items", cardOpts{keyRow: "2 to send · 1 to review · 0 to dismiss"}},
		{"an extra item", cardOpts{keyRow: feedbackFooter + " · 3 to snooze"}},
		{"the wrong key", cardOpts{keyRow: "1 to review · 2 to send · 3 to dismiss"}},
		{"a capitalised verb", cardOpts{keyRow: "1 to Review · 2 to send · 0 to dismiss"}},
		{"a missing item", cardOpts{keyRow: "1 to review · 0 to dismiss"}},
		{"a queued suffix with no count", cardOpts{keyRow: feedbackFooter + " · more queued"}},
		{"a queued suffix that is not a number", cardOpts{keyRow: feedbackFooter + " · +x more queued"}},
		// The card's other states are recognised now (colab-fleet#217) — but only
		// as whole texts. Their tails alone, or the question boxed as if it were
		// a status, are not any of the four.
		{"the error's tail alone", cardOpts{keyRow: "1 to review & retry · Esc to dismiss"}},
		{"the turn-off question boxed", cardOpts{keyRow: "Turn off Claude-drafted feedback? 0 to turn off · Esc to keep"}},
		{"the confirmation with the wrong key", cardOpts{keyRow: "Send without reviewing (full draft + env, no transcript)? 1 to send · Esc to back"}},
		{"the confirmation with a different scope", cardOpts{keyRow: "Send without reviewing (full draft + env + transcript)? 2 to send · Esc to back"}},
		{"an error that is not the runtime's", cardOpts{keyRow: "✘ Couldn't send feedback 1 to review & retry · Esc to dismiss"}},
		{"an in-flight line with more words", cardOpts{keyRow: "Sending… please wait"}},
		{"more body rows than a draft has", cardOpts{body: long}},
		{"a gap taller than the runtime leaves", cardOpts{gap: []string{"", "", "", "", "", "hint"}}},
		{"a rule in the gap", cardOpts{gap: []string{"", rule}}},
		{"agent output in the gap", cardOpts{gap: []string{"", "⏺ and then I said"}}},
		{"a box row in the gap", cardOpts{gap: []string{"│ stray │", ""}}},
		{"a card shifted off the fence's column", cardOpts{indent: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := cardScreen(tc.o)
			if _, ok := liveFeedbackCard(newScreen(raw)); ok {
				t.Fatalf("read a card from:\n%s", raw)
			}
			st := classifyAt(raw, 24)
			if st.Prompt != nil && st.Prompt.Kind == fleet.PromptFeedbackReview {
				t.Errorf("classified as a feedback card: %+v", st.Prompt)
			}
		})
	}
	// No top border, and no borders at all: the box must close on both ends.
	raw := cardShort()
	noTop := strings.Replace(raw, "╭", " ", 1)
	if _, ok := liveFeedbackCard(newScreen(noTop)); ok {
		t.Error("read a card with no top border")
	}
	plain := strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ").Replace(raw)
	if _, ok := liveFeedbackCard(newScreen(plain)); ok {
		t.Error("read a card with no borders")
	}
}

func TestFeedbackCardQueuedSuffixKeepsTheOptionsAndChangesTheNonce(t *testing.T) {
	nonce := func(o cardOpts) string {
		t.Helper()
		p, _ := parsePromptShape(newScreen(cardScreen(o)))
		if p == nil {
			t.Fatalf("no prompt for %+v", o)
		}
		if fmt.Sprint(p.Options) != "[review send dismiss]" {
			t.Errorf("options = %q; the suffix is not an option", p.Options)
		}
		return p.Nonce
	}
	none := nonce(cardOpts{})
	dot := nonce(cardOpts{queued: "+1 more queued", sep: " · "})
	space := nonce(cardOpts{queued: "+1 more queued", sep: " "})
	two := nonce(cardOpts{queued: "+2 more queued", sep: " · "})
	if none == dot || dot == two {
		t.Errorf("a draft arriving behind this one must be a changed prompt: none %s, +1 %s, +2 %s", none, dot, two)
	}
	if dot != space {
		t.Errorf("the separator is not identity: %s vs %s", dot, space)
	}
}

func TestFeedbackCardNonceFollowsTheDraftNotTheGlyph(t *testing.T) {
	nonce := func(title string, body ...string) string {
		p, _ := parsePromptShape(newScreen(cardScreen(cardOpts{title: title, body: body})))
		if p == nil {
			t.Fatal("no prompt")
		}
		return p.Nonce
	}
	if nonce("✻ Bug report drafted: one", "- a") != nonce("✽ Bug report drafted: one", "- a") {
		t.Error("the title's leading glyph animates; it must not change the nonce")
	}
	if nonce("✻ Bug report drafted: one", "- a") == nonce("✻ Bug report drafted: two", "- a") {
		t.Error("a different draft must have a different nonce")
	}
	if nonce("✻ Bug report drafted: one", "- a") == nonce("✻ Bug report drafted: one", "- b") {
		t.Error("a different body must have a different nonce")
	}
}

// The card's rows are the agent's own words, and an agent can print anything.
func TestFeedbackFooterPrintedByTheAgentNeverBecomesAPrompt(t *testing.T) {
	fake := func(indent int) []string {
		inner := cardWidth - 2 - indent
		return []string{
			strings.Repeat(" ", indent) + "╭" + strings.Repeat("─", inner) + "╮",
			cardRow(indent, "✻ Bug report drafted: looks real"),
			cardRow(indent, feedbackFooter),
			strings.Repeat(" ", indent) + "╰" + strings.Repeat("─", inner) + "╯",
		}
	}
	// In the agent's own output, indented under its bullet, above a composer that
	// reads fine: nothing is wrong with the composer, so there is no card.
	transcript := append([]string{"⏺ Here is what the card looks like:"}, fake(2)...)
	tall := cardScreenNoCard(transcript, true)
	if st := classifyAt(tall, 0); st.Status != fleet.StatusIdle || st.Prompt != nil {
		t.Errorf("a fake card in the transcript above a readable composer: status %q prompt %+v", st.Status, st.Prompt)
	}
	// Even against an unreadable composer: printed at the chrome's own column but
	// far above the fence, with the agent's output between, it is not the card.
	far := append(append([]string{"⏺ and here it is at column 0:"}, fake(0)...),
		"", "", "⏺ and that was the end of it", "", "  more output", "")
	short := cardScreenNoCard(far, false)
	if st := classifyAt(short, 24); st.Prompt != nil {
		t.Errorf("a fake card far above an unclipped fence became a prompt: %+v", st.Prompt)
	}
}

// cardScreenNoCard is the composer over the given transcript, tall or short.
func cardScreenNoCard(transcript []string, tall bool) string {
	lines := append([]string{}, transcript...)
	lines = append(lines, "", "✻ Cogitated for 26s · done 1:11 AM", "", strings.Repeat("─", 53)+" some-session ─", "❯")
	if tall {
		lines = append(lines, rule, "  ⏵⏵ bypass permissions on")
	}
	return strings.Join(lines, "\n")
}

// The options' words are ordinary, so option text alone must never yield the
// kind: an agent's own numbered list could say the same three words.
func TestNumberedReviewSendDismissMenuIsNotAFeedbackCard(t *testing.T) {
	p := &fleet.SessionPrompt{Options: []string{"review", "send", "dismiss"}, Selected: 1}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("kind = %q from option text alone", k)
	}
	raw := "  transcript line\n" + rule + "\n  Pick one\n❯ 1. review\n  2. send\n  3. dismiss\n" + rule + "\nEnter to select · Esc to cancel"
	st := classifyAt(raw, 0)
	if st.Prompt != nil && st.Prompt.Kind == fleet.PromptFeedbackReview {
		t.Errorf("an agent's numbered menu was labelled as the card: %+v", st.Prompt)
	}
}

// The card's body is the agent's draft, prose about whatever went wrong. It sits
// where the live-bottom scans read runtime notices from, and must not be one.
func TestFeedbackCardBodyIsNotReadAsARuntimeNotice(t *testing.T) {
	body := []string{
		"- What happened: the run ended with API Error: 500 overloaded",
		"- and the account said You've hit your usage limit · resets 3pm",
	}
	for _, tall := range []bool{true, false} {
		raw := cardScreen(cardOpts{tall: tall, body: body})
		st := classifyAt(raw, 0)
		if st.Quota != nil || st.Status == fleet.StatusQuotaBlocked {
			t.Errorf("tall=%v: the draft's words were read as a usage limit: %q %+v", tall, st.Status, st.Quota)
		}
		if st.LastTurn != nil {
			t.Errorf("tall=%v: the draft's words were read as a failed turn: %+v", tall, st.LastTurn)
		}
	}
	// And the masking hides nothing real: a notice ABOVE the card is still read.
	real := cardScreen(cardOpts{tall: true, transcript: []string{
		"  transcript line", "  You've hit your usage limit · resets 3pm", "  /usage-credits to finish",
	}})
	if st := classifyAt(real, 0); st.Status != fleet.StatusQuotaBlocked {
		t.Errorf("a real notice above the card: status %q (%s), want quota_blocked", st.Status, st.Evidence)
	}
}

func TestFeedbackReviewIsNotConsentable(t *testing.T) {
	if _, ok := consentableKinds[fleet.PromptFeedbackReview]; ok {
		t.Fatal("feedback-review is consentable: a create could answer a person's decision about what leaves their machine")
	}
	d := newTestDriver(twoSessions())
	_, err := d.Create(context.Background(), testCaller, "key-feedback",
		fleet.SessionSpec{Name: "gamma", Cwd: "/work/gamma", Consents: []fleet.PromptKind{fleet.PromptFeedbackReview}})
	if err == nil || !strings.Contains(err.Error(), "not a consentable question") {
		t.Fatalf("create with that consent: %v, want a refusal", err)
	}
}

// --- Send, Keys, Discard ---

func sendCardSession(t *testing.T, raw string, text string) (fleet.DeliveryReceipt, *fakeMux, *Driver) {
	t.Helper()
	f := twoSessions()
	f.captures["%1"] = raw
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	return got, f, d
}

func nothingWritten(t *testing.T, f *fakeMux) {
	t.Helper()
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && (c[0] == "send-keys" || c[0] == "load-buffer" || c[0] == "paste-buffer") {
			t.Errorf("nothing may be typed or pasted; saw %v", c)
		}
	}
}

// The refusal used to read "no composer has been painted ... a full-screen
// interface", which blamed startup and pointed the caller at keys(). It now
// names the card, and answers to respond.
func TestSendToAFeedbackCardOverAClippedComposerNamesTheCard(t *testing.T) {
	got, f, d := sendCardSession(t, cardShort(), "do the thing")
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome %q (%s), want refused", got.Outcome, got.Reason)
	}
	for _, want := range []string{"feedback-draft card", "respond"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q lacks %q", got.Reason, want)
		}
	}
	for _, unwanted := range []string{"no composer has been painted", "keys()"} {
		if strings.Contains(got.Reason, unwanted) {
			t.Errorf("reason %q still says %q", got.Reason, unwanted)
		}
	}
	nothingWritten(t, f)
	if n := d.Counters()[counterFeedbackCardRefusedSend]; n != 1 {
		t.Errorf("%s = %d, want 1", counterFeedbackCardRefusedSend, n)
	}
}

// Typed text under the card: not a prompt, but still not a screen to write into.
func TestSendUnderAFeedbackCardOverTypedTextIsRefusedByName(t *testing.T) {
	got, f, _ := sendCardSession(t, cardScreen(cardOpts{row: "half a thought"}), "do the thing")
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "feedback-draft card") {
		t.Fatalf("outcome %q reason %q", got.Outcome, got.Reason)
	}
	nothingWritten(t, f)
}

// A message that is only 1, 2 or 0 is the card's shortcut, not a message.
func TestSendOfALoneDigitUnderAFeedbackCardIsRefused(t *testing.T) {
	tall := cardScreen(cardOpts{tall: true})
	for _, text := range []string{"1", "2", "0", " 1 "} {
		got, f, d := sendCardSession(t, tall, text)
		if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "single digit") {
			t.Errorf("%q: outcome %q reason %q, want a lone-digit refusal", text, got.Outcome, got.Reason)
		}
		nothingWritten(t, f)
		if n := d.Counters()[counterFeedbackCardRefusedDigit]; n != 1 {
			t.Errorf("%q: %s = %d, want 1", text, counterFeedbackCardRefusedDigit, n)
		}
	}
	// Anything longer is an ordinary message; and a digit with no card is too.
	for _, tc := range []struct{ raw, text string }{
		{tall, "12"}, {tall, "1 please"}, {tall, "3"},
		{idleFixtureFor("alpha"), "1"},
	} {
		got, _, _ := sendCardSession(t, tc.raw, tc.text)
		if strings.Contains(got.Reason, "single digit") {
			t.Errorf("%q was refused as a lone digit: %q", tc.text, got.Reason)
		}
	}
}

// On a pane that shows the composer whole the card is ambient: a message is
// delivered as it always was.
func TestSendUnderAFeedbackCardAboveAReadableComposerDelivers(t *testing.T) {
	got, f, _ := sendCardSession(t, cardScreen(cardOpts{tall: true}), "please carry on")
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("refused (%s); the card blocks nothing here", got.Reason)
	}
	pasted := false
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == "load-buffer" {
			pasted = true
		}
	}
	if !pasted {
		t.Error("nothing was pasted")
	}
}

func TestDiscardRefusesWhenAFeedbackCardHidesTypedText(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = cardScreen(cardOpts{row: "half a thought"})
	d := newTestDriver(f)
	_, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "", driver.DiscardOptions{})
	if err == nil || !strings.Contains(err.Error(), "feedback-draft card") {
		t.Fatalf("discard: %v, want a refusal naming the card — \"already clear\" would be false", err)
	}
	if n := d.Counters()[counterFeedbackCardRefusedDiscard]; n != 1 {
		t.Errorf("%s = %d, want 1", counterFeedbackCardRefusedDiscard, n)
	}
	// An empty row is refused too (colab-fleet#216). This used to answer "already
	// clear" — #215 read "nothing on the row" as "nothing to discard" — but the
	// composer's closing rule is below the pane and what is under the row cannot
	// be read, which is what a clipped composer is: the same answer the verb
	// gives for any other clipped composer. It is also the only answer that
	// agrees with what send says of this screen (the card is a prompt, and a
	// message is refused until a person answers it).
	f.captures["%1"] = cardShort()
	_, err = d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "", driver.DiscardOptions{})
	if err == nil || !strings.Contains(err.Error(), "feedback-draft card") {
		t.Fatalf("discard on an empty row: %v, want a refusal naming the card", err)
	}
	if n := d.Counters()[counterFeedbackCardRefusedDiscard]; n != 2 {
		t.Errorf("%s = %d, want 2", counterFeedbackCardRefusedDiscard, n)
	}
}

func TestKeysRefusesAtAFeedbackCardOverTypedText(t *testing.T) {
	f := twoSessions()
	raw := cardScreen(cardOpts{row: "half a thought"})
	f.captures["%1"] = raw
	d := newTestDriver(f)
	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, digestOf(t, d, "alpha💬"))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "feedback-draft card") {
		t.Fatalf("outcome %q reason %q", got.Outcome, got.Reason)
	}
	nothingWritten(t, f)
}

// --- Respond ---

// fakeFeedbackCard models the card the way the runtime's source reads: a digit
// acts on it only while the composer is empty. "1" opens a review, "2" turns the
// card into a confirm line, "0" dismisses it. The last two knobs model the
// failures a read-back has to tell apart: a key the card swallows, and one that
// is typed into the composer instead of being read.
type fakeFeedbackCard struct {
	tall      bool
	row       string
	state     string // card | review | confirm | gone
	swallow   bool
	typeIntoC bool
}

func (g *fakeFeedbackCard) press(key string) {
	if g.swallow {
		return
	}
	if g.typeIntoC {
		g.row += key
		return
	}
	if g.state != "card" || g.row != "" {
		return
	}
	switch key {
	case "1":
		g.state = "review"
	case "2":
		g.state = "confirm"
	case "0":
		g.state = "gone"
	}
}

func (g *fakeFeedbackCard) screen() string {
	switch g.state {
	case "review":
		return "  Draft for review\n\n  Title: something\n  Enter to send · Esc to close"
	case "confirm":
		return cardScreen(cardOpts{tall: g.tall, keyRow: "Send without reviewing (full draft + env, no transcript)? 2 to send · Esc to back"})
	case "gone":
		return idleFixtureFor("dismissed")
	}
	return cardScreen(cardOpts{tall: g.tall, row: g.row})
}

func armCard(g *fakeFeedbackCard) (*fakeMux, *Driver) {
	f := twoSessions()
	if g.state == "" {
		g.state = "card"
	}
	armDialog(f, "%1", g)
	return f, newTestDriver(f)
}

func cardNonce(t *testing.T, f *fakeMux) string {
	t.Helper()
	p, _ := parsePromptShape(newScreen(f.captures["%1"]))
	if p == nil {
		t.Fatalf("not a prompt:\n%s", f.captures["%1"])
	}
	return p.Nonce
}

func respondCard(t *testing.T, d *Driver, resp fleet.Response) fleet.DeliveryReceipt {
	t.Helper()
	got, err := d.Respond(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, resp)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	return got
}

// Choice 3 is the key 0: the options are the runtime's verbs, and the keys are
// not their positions. One key, alone, and nothing after it.
func TestRespondToAFeedbackCardSendsTheCardsOwnKey(t *testing.T) {
	for choice, want := range map[int]string{1: "1", 2: "2", 3: "0"} {
		g := &fakeFeedbackCard{}
		f, d := armCard(g)
		got := respondCard(t, d, fleet.Response{Choice: choice, Nonce: cardNonce(t, f)})
		if got.Outcome != fleet.OutcomeSubmitted {
			t.Errorf("choice %d: outcome %q (%s), want submitted", choice, got.Outcome, got.Reason)
		}
		if calls := sendCalls(f); len(calls) != 1 || calls[0] != want {
			t.Errorf("choice %d: send-keys calls %q, want exactly one, %q", choice, calls, want)
		}
		if choice == 2 && (!strings.Contains(got.Reason, "asks to confirm") || !strings.Contains(got.Reason, "nothing has been sent")) {
			t.Errorf("choosing send must say the runtime asks to confirm first: %q", got.Reason)
		}
		if choice == 3 && !strings.Contains(got.Reason, "dismissed") {
			t.Errorf("receipt %q", got.Reason)
		}
	}
}

// Refused, each without a single key sent.
func TestRespondToAFeedbackCardRefusesWhatItCannotAnswerSafely(t *testing.T) {
	txt := "nope"
	for _, tc := range []struct {
		name  string
		resp  func(nonce string) fleet.Response
		error string
	}{
		{"cancel", func(n string) fleet.Response { return fleet.Response{Cancel: true, Nonce: n} }, "Escape"},
		{"accepting the highlight", func(n string) fleet.Response { return fleet.Response{Nonce: n} }, "no default"},
		{"choices", func(n string) fleet.Response { return fleet.Response{Choices: []int{1}, Nonce: n} }, "single-choice"},
		{"text", func(n string) fleet.Response { return fleet.Response{Text: &txt, Nonce: n} }, "single-choice"},
		{"no nonce", func(string) fleet.Response { return fleet.Response{Choice: 3} }, "nonce"},
		{"a stale nonce", func(string) fleet.Response { return fleet.Response{Choice: 3, Nonce: "0000000000000000"} }, "changed"},
		{"an option that is not there", func(n string) fleet.Response { return fleet.Response{Choice: 4, Nonce: n} }, "no such option"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d := armCard(&fakeFeedbackCard{})
			got := respondCard(t, d, tc.resp(cardNonce(t, f)))
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, tc.error) {
				t.Errorf("outcome %q reason %q, want a refusal saying %q", got.Outcome, got.Reason, tc.error)
			}
			if calls := sendCalls(f); len(calls) != 0 {
				t.Errorf("keys were sent despite the refusal: %q", calls)
			}
		})
	}
}

// Typed text: not a prompt, so there is nothing to answer, and the refusal says
// why rather than "not waiting on a prompt".
func TestRespondToAFeedbackCardOverTypedTextRefuses(t *testing.T) {
	f, d := armCard(&fakeFeedbackCard{row: "half a thought"})
	got := respondCard(t, d, fleet.Response{Choice: 3, Nonce: "0000000000000000"})
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "feedback-draft card") {
		t.Fatalf("outcome %q reason %q", got.Outcome, got.Reason)
	}
	if calls := sendCalls(f); len(calls) != 0 {
		t.Errorf("keys sent: %q", calls)
	}
}

// A card above a composer that reads fine is not a prompt, so respond has
// nothing to answer.
func TestRespondToAFeedbackCardAboveAReadableComposerRefuses(t *testing.T) {
	f, d := armCard(&fakeFeedbackCard{tall: true})
	got := respondCard(t, d, fleet.Response{Choice: 3, Nonce: "0000000000000000"})
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome %q (%s)", got.Outcome, got.Reason)
	}
	if calls := sendCalls(f); len(calls) != 0 {
		t.Errorf("keys sent: %q", calls)
	}
}

// A key the card swallows leaves the same card up. Unknown, and never resent.
func TestRespondToAFeedbackCardThatStaysIsUnknown(t *testing.T) {
	f, d := armCard(&fakeFeedbackCard{swallow: true})
	got := respondCard(t, d, fleet.Response{Choice: 3, Nonce: cardNonce(t, f)})
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "may not have registered") {
		t.Fatalf("outcome %q reason %q", got.Outcome, got.Reason)
	}
	if calls := sendCalls(f); len(calls) != 1 {
		t.Errorf("the key was sent %d times: %q", len(calls), calls)
	}
}

// A digit that lands in the composer instead of being read by the card is left
// there; the receipt says so.
func TestRespondToAFeedbackCardReportsADigitTypedIntoTheComposer(t *testing.T) {
	f, d := armCard(&fakeFeedbackCard{typeIntoC: true})
	got := respondCard(t, d, fleet.Response{Choice: 3, Nonce: cardNonce(t, f)})
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "typed into the composer") {
		t.Fatalf("outcome %q reason %q", got.Outcome, got.Reason)
	}
}

// The read on a card that has moved on: after "2" the runtime asks to confirm,
// a screen this driver does not recognise, and the next read is idle again with
// no prompt — the caller has been told so.
func TestAfterChoosingSendTheConfirmationIsNotAPrompt(t *testing.T) {
	g := &fakeFeedbackCard{}
	f, d := armCard(g)
	respondCard(t, d, fleet.Response{Choice: 2, Nonce: cardNonce(t, f)})
	if p, _ := parsePromptShape(newScreen(f.captures["%1"])); p != nil {
		t.Errorf("the confirm-send line was read as a prompt: %+v", p)
	}
}
