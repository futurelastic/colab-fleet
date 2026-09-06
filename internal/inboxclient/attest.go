package inboxclient

import "strings"

// ModeClass is the permission-mode class a sender asserts about the session
// it is sending TO, drawn from the receiving runtime's own closed two-value
// vocabulary. The zero value means "not asserted" and is a first-class,
// expected state — this service is a conduit and has no permission mode of
// its own, so it can only assert a class when an operator-supplied index
// tells it which one the target runs in.
//
// # Why a sender must assert this at all (colab-fleet #148)
//
// The receiving runtime's inbound policy defaults to MODE PARITY. Reduced to
// the two facts that matter here: a message whose sender asserts a class is
// accepted when that class equals the receiver's own and held otherwise; a
// message whose sender asserts NO class is accepted only when the receiver
// prompts for permissions, and held when the receiver runs with them
// bypassed. Held means parked for a human, never given to the model, then
// dropped when the receiver's own hold deadline passes.
//
// This service asserted nothing, and an unattended session is normally the
// bypassed kind, so every message to one was held. #148 measured the result
// on a live fleet: 206 sends reported delivered that were not, one session
// unreachable for three and a half days.
//
// # Why the assertion MIRRORS the receiver instead of naming a class of our own
//
// Parity is SYMMETRIC. Asserting one fixed class does not fix this — it moves
// the hold to the other half of the fleet, because a mismatch is held just as
// firmly as a missing assertion. Only an assertion equal to the receiver's own
// class is accepted in every case, so the class is a per-target fact this
// service must be TOLD, never a constant it can compile in. That is the whole
// reason ModeClass travels on InboxAddress, beside the socket and the token,
// rather than living here as a default.
type ModeClass string

const (
	// ModeBypass names a receiver running with permission prompts bypassed.
	ModeBypass ModeClass = "bypass"
	// ModePrompting names a receiver that still prompts for permission.
	ModePrompting ModeClass = "prompting"
)

// Valid reports whether c is one of the two classes the receiving runtime
// recognises. The zero value is deliberately NOT valid: "not asserted" is a
// real state a caller must handle by falling back, never a value to put on
// the wire.
func (c ModeClass) Valid() bool { return c == ModeBypass || c == ModePrompting }

// The envelope's literals. Per this package's own doc comment the values the
// receiving runtime requires live in code, never restated in prose.
const (
	envelopeTag      = "cross-session-message"
	envelopeModeAttr = "from-mode"
)

// openLookalikes is every rune the receiving runtime treats as an
// opening-angle-bracket lookalike, transcribed from its own table: the ASCII
// one plus the fifteen confusables it folds onto the ASCII one.
//
// This set is the entire reason Attest can PROVE its output survives the
// receiver's round-trip check — see Attest's own doc comment. It is
// transcribed rather than derived, so it is a fact about one version of one
// runtime; docs/gotchas.d records how to re-derive it if the envelope ever
// changes shape.
const openLookalikes = "<" +
	"＜﹤〈⟨〈‹˂ᐸ" +
	"❬❮❰⧼≮≺⋖"

// Attest wraps text in the envelope that carries class to the receiving
// runtime, returning ok=false when it cannot do so in a form guaranteed to
// survive intact.
//
// # The guarantee, and why it is a guarantee rather than a hope
//
// The receiver does not simply parse this envelope. It parses it, RE-BUILDS
// the envelope from what it parsed, and compares the rebuild to what actually
// arrived BYTE FOR BYTE; any difference and it discards the envelope
// wholesale — the asserted class is lost, the raw text reaches the model
// unwrapped, and the message is held exactly as if nothing had been asserted.
// So a wrapper that is merely usually right is worse than none: it fails in
// the silent direction this issue is about.
//
// The rebuild passes its own body through an escaper, which is the only
// transform that can make the two sides differ. That escaper cannot fire on a
// body containing no opening-bracket lookalike — its pattern must match one
// as its very first character. So: no lookalike in the body implies the
// escaper is the identity function on it, which implies the rebuild is
// byte-identical, which implies the envelope is honoured. That is a proof,
// not a measurement, and it is why ok=false here is generous rather than
// clever — a body carrying any of the sixteen runes is refused outright
// rather than escaped correctly.
//
// # ok=false is not a failure
//
// It means this service cannot attest THIS send, which — per ADR 119's rule
// that the honest response to half a capability is the same as to none of it
// — makes the inbox path unavailable for it. The caller falls back to the
// pane path, which is the behaviour that predates the inbox default and which
// #148 records running at a 100% delivery rate. Reporting delivered without
// attesting is the bug; falling back is the fix.
//
// Only the class attribute is emitted. A reply address is deliberately NOT
// asserted: this service has no socket bound in the receiver's own namespace
// to receive one over (colab-fleet #120), and advertising an address that
// cannot be honoured would be a second lie in the same envelope.
func Attest(text string, class ModeClass) (string, bool) {
	if !class.Valid() {
		return "", false
	}
	if strings.ContainsAny(text, openLookalikes) {
		return "", false
	}
	var b strings.Builder
	b.WriteString("<" + envelopeTag + " " + envelopeModeAttr + `="`)
	b.WriteString(string(class))
	b.WriteString("\">\n")
	b.WriteString(text)
	b.WriteString("\n</" + envelopeTag + ">")
	return b.String(), true
}
