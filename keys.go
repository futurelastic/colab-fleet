package fleet

import (
	"encoding/json"
	"fmt"
)

// KeyName is the closed vocabulary of raw key events a caller may deliver
// (api-http.md §3.3, POST …/keys).
//
// # Why this exists at all, and why it is six values
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
// The set is move, accept, dismiss, and nothing else.
//
// Deliberately absent: every CHARACTER key, which is what `input` is for; and
// every CONTROL key — C-c is `interrupt`, C-u is `discard` — each of which
// carries corroboration and confirmation a blind keypress cannot. An endpoint
// accepting arbitrary key names would quietly become a second, unreviewed way
// to do everything else in this API, and nobody reviewing a grants table would
// see it happen.
type KeyName string

const (
	KeyUp     KeyName = "Up"
	KeyDown   KeyName = "Down"
	KeyLeft   KeyName = "Left"
	KeyRight  KeyName = "Right"
	KeyEnter  KeyName = "Enter"
	KeyEscape KeyName = "Escape"
)

// Valid reports whether this is a key the API accepts. The set is closed:
// anything else is `invalid`, never passed through to a substrate that might
// have its own opinion about what the string means.
func (k KeyName) Valid() bool {
	switch k {
	case KeyUp, KeyDown, KeyLeft, KeyRight, KeyEnter, KeyEscape:
		return true
	default:
		return false
	}
}

// KeyNames lists the vocabulary, for an error message that tells a caller what
// it may say rather than only that it said something wrong.
func KeyNames() []KeyName {
	return []KeyName{KeyUp, KeyDown, KeyLeft, KeyRight, KeyEnter, KeyEscape}
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
