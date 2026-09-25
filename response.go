package fleet

import (
	"errors"
	"fmt"
	"strings"
)

// Response answers a prompt a session is blocked on (§3).
//
// # Why this is not just send()
//
// send() delivers a message, and delivers it through a paste buffer
// specifically so the bytes are never interpreted as control input — a message
// containing "C-c" must not interrupt the session receiving it. That property
// is correct for messages and is exactly what makes a prompt unanswerable:
// a menu wants a keypress, and send() is built to guarantee it never sends one.
//
// So answering is a different operation, not a flag on delivery. The
// separation also means the two can be authorised differently and refused for
// different reasons, which they should be: delivering text to a working
// session is harmless, while answering a prompt commits to whatever the prompt
// was asking.
//
// # Why a choice and not a key
//
// §5.1 says the interface expresses questions, never mechanisms — state()
// rather than readScreen(), send() rather than typeKeys(). A "press Enter"
// operation would bind every future driver to this substrate's idea of
// confirmation. A choice is what the caller actually means; how a driver
// produces it is the driver's business.
//
// # Why the fields carry tags
//
// Field names are tagged rather than left to Go's defaults. Decoding happened
// to work without them — encoding/json matches field names case-insensitively
// — so the omission was invisible from the server side while making this the
// one wire type in the package that would MARSHAL as "Choice". A type that
// reads one way and writes another is a trap for the next client.
type Response struct {
	// Choice selects a numbered option, 1-based. Zero means "accept
	// whatever is highlighted", which is what a caller usually wants and
	// what a human pressing Enter would get.
	Choice int `json:"choice,omitempty"`

	// Choices answers a multi-select question (SessionPrompt.MultiSelect):
	// the 1-based options that must be ticked when the answer is handed on,
	// and — by omission — every other checkbox, which must be clear. It
	// cannot be combined with Choice or Cancel, and it cannot be empty
	// (see Validate).
	//
	// # Why a set and not a toggle
	//
	// On a multi-select question a single index does not answer anything: it
	// flips one box. A caller that answered by toggling would need one call
	// per box, each one unsafe to resend — a retried toggle undoes itself —
	// and a sequence that failed half-way would leave a tick state nobody
	// chose. A set names the END state, so the driver works out which boxes
	// differ from what is on screen and flips only those. Sending the same
	// set twice asks for the same result twice.
	//
	// # Why it stops short of submitting
	//
	// Choices sets the boxes and then moves the dialog ONE step on, exactly
	// as a digit does on a single-select tab of a multi-question dialog: to
	// the next question, or — after the last one — to the dialog's review
	// screen. It never confirms that review screen. "Submit the answers" is
	// its own prompt with its own nonce, answered with Choice like any other,
	// so the step that actually hands the answers to the agent is never taken
	// on the strength of an earlier read.
	Choices []int `json:"choices,omitempty"`

	// Text answers a question in the caller's own words, through the row the
	// runtime appends to every question an agent asks — "Type something" —
	// instead of through one of the agent's options (SessionPrompt.FreeText).
	// The driver puts the highlight on that row, types Text into it, reads the
	// row back to prove the text arrived, and only then confirms; the receipt
	// is `submitted` only once the answered question has left the screen.
	//
	// # Why a pointer
	//
	// So that an empty string and an absent field stay different things on
	// their way through a relay. Every field here is `omitempty`, and a plain
	// string would drop `"text": ""` when a peer-relaying driver marshalled it
	// onward: the peer would receive a body with no answer in it, and a body
	// with no answer means "accept whatever is highlighted". That is the same
	// trap an empty Choices set is refused for (see Validate), and it is closed
	// the same way — a pointer marshals `""` as `""`, so the emptiness reaches
	// the one place that refuses it.
	//
	// # Why an empty or blank Text is refused rather than sent
	//
	// Measured live: confirming the free-text field while it is empty does not
	// answer the question with nothing — the runtime treats it as declining the
	// WHOLE dialog, every question in it. So an empty answer would be reported
	// as a submission and would cost the caller answers it never meant to
	// withdraw.
	//
	// # Combining it with the other fields
	//
	// Text cannot be combined with Choice (which selects a listed option) or
	// Cancel. On a multi-select question it is combined with Choices on
	// purpose: Choices names the boxes to leave ticked and Text is the
	// free-text row's own content, so together they name the whole end state,
	// exactly as Choices alone names a set. Text without Choices there means
	// no box is ticked and the text is the whole answer.
	Text *string `json:"text,omitempty"`

	// Cancel dismisses the prompt instead of answering it. A caller that
	// does not like any of the options needs a way to say so that is not
	// "pick one anyway".
	Cancel bool `json:"cancel,omitempty"`

	// Nonce is the SessionPrompt.Nonce the caller was answering.
	//
	// A caller reads a prompt, shows it to a human, and answers seconds or
	// minutes later. In between the session may have moved on and be showing
	// a DIFFERENT question in the same place — and an answer submitted by
	// index would be applied to it, silently. Supplying the nonce turns that
	// into a refusal.
	//
	// Optional, and its absence is not free: a driver must say, in the
	// receipt, that it answered without checking. An automated caller should
	// always send it; a human at a terminal answering immediately reasonably
	// may not.
	Nonce string `json:"nonce,omitempty"`
}

// Validate reports a Response whose fields contradict each other — a fault in
// the request, as opposed to a refusal, which is a fact about the session.
//
// # Why an empty Choices is refused here and not left to a driver
//
// `"choices": []` decodes to an empty, non-nil slice, and a driver that
// forwards the Response to a peer marshals it with omitempty — which drops
// the field. The peer then receives `{}`, and `{}` means "accept whatever is
// highlighted": a request to submit NOTHING would arrive as a request to
// submit something nobody chose. The only place that can still tell the two
// apart is the one that decoded the caller's own bytes, so the check lives
// here, where every receiving handler calls it.
func (r Response) Validate() error {
	if r.Text != nil {
		if strings.TrimSpace(*r.Text) == "" {
			return errors.New("text is empty or blank; an empty free-text answer is not an " +
				"answer — the runtime reads it as declining the whole dialog, so it is never sent")
		}
		if r.Choice != 0 {
			return errors.New("text and choice cannot be combined: text answers through the " +
				"free-text row, choice selects a listed option")
		}
		if r.Cancel {
			return errors.New("text and cancel cannot be combined")
		}
	}
	if r.Choices == nil {
		return nil
	}
	if len(r.Choices) == 0 {
		return errors.New("choices is empty; to leave every box clear is not supported, " +
			"and an empty set must not be read as \"accept the highlighted option\"")
	}
	if r.Choice != 0 {
		return errors.New("choices and choice cannot be combined: choices answers a " +
			"multi-select question, choice a single-select one")
	}
	if r.Cancel {
		return errors.New("choices and cancel cannot be combined")
	}
	seen := make(map[int]bool, len(r.Choices))
	for _, c := range r.Choices {
		if c <= 0 {
			return fmt.Errorf("choices: %d is not an option index (they are 1-based)", c)
		}
		if seen[c] {
			return fmt.Errorf("choices: %d is listed twice", c)
		}
		seen[c] = true
	}
	return nil
}
