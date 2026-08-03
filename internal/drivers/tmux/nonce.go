package tmux

import (
	"crypto/rand"
	"encoding/hex"
)

// randomNonce returns an unguessable token used to delimit records and
// fields in the multiplexer's batched output.
//
// It is cryptographically random rather than a counter or a timestamp, and
// that is a security property rather than fastidiousness. The text being
// delimited is written by an agent — it is arbitrary attacker-influenced
// content in the general case, and merely arbitrary human content in the
// common one. A predictable delimiter can be emitted by the very text it is
// meant to delimit, at which point the driver parses a session name, a
// working directory, or a whole extra session out of a transcript.
//
// The cost is one 64-bit read from the OS entropy pool per call, which does
// not register against a subprocess spawn.
func randomNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read does not fail on any platform this runs on;
		// if it ever does, failing loudly beats silently falling back to
		// a predictable value, which would defeat the entire point.
		panic("tmux: no entropy available for delimiter nonce: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
