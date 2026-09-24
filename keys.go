package fleet

import (
	"encoding/json"
	"fmt"
)

// KeyName is the closed vocabulary of raw key events a caller may deliver
// (api-http.md §3.3, POST …/keys).
//
// # Why this exists at all, and why it is seven values
//
// Some full-screen dialogs are navigated with arrow keys and confirmed with a
// bare Enter. `respond` cannot express that: it answers a prompt the classifier
// RECOGNISED, by index, and refuses when it sees no prompt — which is exactly
// the state an unrecognised full-screen dialog leaves a session in. `input`
// cannot express it either, and must not learn to: its whole guarantee is that
// a message containing control characters never becomes a keystroke (§3 of the
// abstraction). So a consumer facing such a dialog had no move inside this API
// and kept a direct handle on the substrate to make one.
//
// The set is move, accept, dismiss — and one key that is none of those,
// KeyBTab, admitted by a ruling (colab-fleet #188, option A) rather than by
// the argument above.
//
// # BTab is not a dialog key, and it changes what the session may do
//
// Shift+Tab is how the runtime cycles its permission mode (default, accept
// edits, plan, auto…) from an idle composer, and for most of those modes it is
// the ONLY way to reach them: only one has a typed command. It is admitted so
// that a client with no terminal in front of it can move a live session
// between modes.
//
// The consequence is stated here and in the docs on purpose. Cycling toward
// accept-edits or auto ESCALATES what the agent may do unattended, and which
// mode a press lands in is the runtime's cycle order, which this service
// neither reads nor controls. So BTab under the `keys` grant means any
// principal holding `keys` can escalate any session that principal can reach.
// The ruling on #188 chose that over a grant of its own (or separate
// escalate/de-escalate grants); it was a decision, not an oversight, and
// reversing it is a change to the grants table, not to this file.
//
// Deliberately absent: every CHARACTER key, which is what `input` is for; and
// every CONTROL key — C-c is `interrupt`, C-u is `discard` — each of which
// carries corroboration and confirmation a blind keypress cannot. Plain Tab is
// absent too: it is a completion key, and BTab is admitted for the one thing
// it does that nothing else here can, not as the first step of a key-name
// free-for-all. An endpoint accepting arbitrary key names would quietly become
// a second, unreviewed way to do everything else in this API, and nobody
// reviewing a grants table would see it happen.
type KeyName string

const (
	KeyUp     KeyName = "Up"
	KeyDown   KeyName = "Down"
	KeyLeft   KeyName = "Left"
	KeyRight  KeyName = "Right"
	KeyEnter  KeyName = "Enter"
	KeyEscape KeyName = "Escape"
	// KeyBTab is Shift+Tab, spelled as the multiplexer spells it. It cycles
	// the runtime's permission mode — see the section above before relying on
	// it for anything else.
	KeyBTab KeyName = "BTab"
)

// Valid reports whether this is a key the API accepts. The set is closed:
// anything else is `invalid`, never passed through to a substrate that might
// have its own opinion about what the string means.
func (k KeyName) Valid() bool {
	switch k {
	case KeyUp, KeyDown, KeyLeft, KeyRight, KeyEnter, KeyEscape, KeyBTab:
		return true
	default:
		return false
	}
}

// KeyNames lists the vocabulary, for an error message that tells a caller what
// it may say rather than only that it said something wrong.
func KeyNames() []KeyName {
	return []KeyName{KeyUp, KeyDown, KeyLeft, KeyRight, KeyEnter, KeyEscape, KeyBTab}
}

func (k KeyName) MarshalJSON() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("fleet: %q is not a key this API delivers", string(k))
	}
	return json.Marshal(string(k))
}

// UnmarshalJSON rejects anything outside the closed set, the empty string
// included. A key that decoded to "" and was then sent would be a keystroke
// nobody named.
func (k *KeyName) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := KeyName(raw)
	if !v.Valid() {
		return fmt.Errorf("fleet: %q is not a key this API delivers", raw)
	}
	*k = v
	return nil
}
