package tmux

import "strings"

// The refusal seam for text this driver's runtime does not treat as a
// message (#53).
//
// # Why this belongs to the driver
//
// `send` delivers caller text into a runtime that may read the SAME bytes
// two different ways: as a message, or as its own local syntax — and which
// one is a property of the runtime, not of this API. §2.1 draws the
// identical line for naming a session: a rule enforced by one caller is not
// a rule, it is a convention that holds until a second caller forgets it.
// The runtime a driver drives is the one thing only that driver knows for
// certain, so the pattern list lives here, not in the service that merely
// forwards `text` on to whichever driver is registered for the request.
//
// # Fail closed, and mean it
//
// A pattern that matches refuses outright — never escaped, never mangled,
// never delivered and hoped about. There is no repair here that turns
// hazardous text into safe text and still says what the caller meant; the
// only honest move is the one `respond` already models for prompts: refuse,
// and say why (§2.4, §5.7 — a refusal must not render like a delivery, and
// DeliveryReceipt already keeps the two apart by Outcome).
//
// # The leading-whitespace trap
//
// #53 measured this against seven runtimes: every one of them trims a
// message before testing its first character, so `" !rm -rf ..."` is read
// exactly as `"!rm -rf ..."` — a caller "defusing" the pattern with a
// leading space defuses nothing, because the trim happens on the runtime's
// side, not the sender's. A check here that tested text[0] directly would
// be exactly as blind as the composer-echo bug this package has already
// paid for once (F55): correct against the bytes a human typed, wrong
// against what the runtime actually reads. matchesRuntimeSyntax reproduces
// the SAME liberty the runtime takes, so a pattern here sees what the
// runtime sees.

// nonMessageInput is one shape of caller text this driver's runtime treats
// as its own local syntax rather than as a message to relay.
type nonMessageInput struct {
	// name distinguishes one refusal from another for a test's benefit; it
	// is never shown to a caller — reason is what explains the refusal.
	name string
	// matches reports whether text — after the same leading-whitespace trim
	// the runtime itself applies before reading its first character — is
	// this runtime's own syntax.
	matches func(trimmed string) bool
	reason  string
}

// nonMessagePatterns is what THIS driver's runtime — the one it actually
// drives, not any runtime a future driver might — reads as something other
// than a message.
//
// #53's own scope note: deciding a pattern list for a runtime nobody here
// drives is explicitly NOT this issue's job. "The driver that adds a
// runtime brings its own patterns." So this list holds exactly one entry,
// because exactly one hazard is established for this runtime: a message
// beginning with "!" is read as a shell command to run directly, no
// approval prompt, the same shape #53 measured elsewhere and the reason
// this seam exists at all. Nothing else is added on suspicion.
var nonMessagePatterns = []nonMessageInput{
	{
		name: "bash-mode",
		matches: func(trimmed string) bool {
			return strings.HasPrefix(trimmed, "!")
		},
		reason: "this runtime reads a message beginning with \"!\" as a shell " +
			"command to run directly, with no approval prompt — refusing rather " +
			"than delivering it (#53); resend without the leading \"!\" if a " +
			"message was intended",
	},
}

// refuseAsRuntimeSyntax reports the reason for the first declared pattern
// that text matches, if any — checked against the SAME leading-whitespace
// trim the runtime applies, per the package comment above.
func refuseAsRuntimeSyntax(text string) (reason string, refused bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, p := range nonMessagePatterns {
		if p.matches(trimmed) {
			return p.reason, true
		}
	}
	return "", false
}
