package inboxclient

import (
	"strings"
	"unicode"
)

// nameCap is the receiving runtime's own length cap on a sender name, in code
// points. Past it, the receiver truncates to this many and appends an ellipsis
// — so a longer name comes back 65 code points long. SenderName truncates to
// nameCap INCLUDING its own ellipsis, which the receiver then leaves alone.
const nameCap = 64

// nameEllipsis is what both sides append to a truncated name.
const nameEllipsis = "…"

// SenderName returns raw as the receiving runtime will rebuild it, or "" when
// that cannot be guaranteed (colab-fleet #158).
//
// # Why this has to be a fixed point, not merely "cleaned up"
//
// The sender name travels in the envelope's from-name attribute, and the
// receiver validates the envelope the same way it validates the rest of it:
// parse, RE-BUILD, compare byte for byte (see Attest). On the rebuild it runs
// the parsed name through its own normaliser. If that normaliser changes a
// single byte, the envelope is discarded wholesale — and the mode class goes
// with it, so the message is held silently: #148's failure, reached through a
// cosmetic field.
//
// So the name emitted must be one the receiver's normaliser leaves untouched.
// The receiver's normaliser is idempotent, so running the SAME steps here
// first yields such a name by construction:
//
//  1. drop '"', '<' and '>';
//  2. drop every rune in Cf, Cc, Cs, Zl, Zp — format, control, surrogate, line
//     and paragraph separators. Cf includes the zero-width joiner, so a
//     joiner-based compound emoji comes out as its component emoji, side by
//     side. That is what the receiver would show anyway;
//  3. trim leading and trailing Zs (after step 2, the only whitespace left
//     for the receiver's trim to find);
//  4. cap the length at nameCap code points.
//
// # Why "" rather than a best effort
//
// The name is optional to the receiver; the mode class is not. When the name
// cannot be guaranteed, dropping it keeps the delivery, while emitting a
// guess risks the whole envelope. A degraded label must never cost a
// delivery, so every doubtful case resolves to "":
//
//   - nothing survives normalisation;
//   - the name holds a rune this build's Unicode tables do not assign. The
//     receiver's tables may be newer, and a rune assigned there as a format
//     character would be stripped on its side and not on ours — a byte
//     difference neither side would report;
//   - the result is somehow not a fixed point of receiverNormalise. That
//     cannot happen for a name built above, and is checked anyway, because a
//     mistake here fails silently in exactly the direction #148 exists to
//     prevent.
func SenderName(raw string) string {
	name := receiverNormalise(raw)
	if name == "" {
		return ""
	}
	if r := []rune(name); len(r) > nameCap {
		// The receiver would cut to nameCap and add an ellipsis, giving
		// nameCap+1. Cut one shorter so the ellipsis lands inside the cap; a
		// name ending in the ellipsis has no trailing space for the
		// receiver's trim to remove, so it stays put.
		name = strings.TrimRightFunc(string(r[:nameCap-1]), isReceiverSpace) + nameEllipsis
	}
	for _, r := range name {
		if !assigned(r) {
			return ""
		}
	}
	if receiverNormalise(name) != name || strings.ContainsAny(name, "\"<>\n\r") {
		return ""
	}
	return name
}

// receiverNormalise is the receiving runtime's own name normaliser, transcribed:
// drop quote and angle-bracket characters, drop invisible runes, trim, and
// truncate past nameCap with an ellipsis. Like openLookalikes, it is a fact
// about one version of one runtime; docs/gotchas.d/148 says how to re-derive it.
func receiverNormalise(raw string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '<' || r == '>':
			return -1
		case isInvisible(r):
			return -1
		}
		return r
	}, raw)
	s = strings.TrimFunc(s, isReceiverSpace)
	if r := []rune(s); len(r) > nameCap {
		s = string(r[:nameCap]) + nameEllipsis
	}
	return s
}

// isInvisible matches the receiver's strip class: Cf, Cc, Cs, Zl, Zp.
func isInvisible(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Cc, unicode.Cs, unicode.Zl, unicode.Zp)
}

// isReceiverSpace is what the receiver's trim can still find once isInvisible
// runes are gone: its whitespace set is the Zs category plus a handful of
// control and format runes, and those are already stripped.
func isReceiverSpace(r rune) bool { return unicode.Is(unicode.Zs, r) }

// assigned reports whether this build's Unicode tables give r a category other
// than Cn (unassigned).
//
// The C subcategories are listed one by one ON PURPOSE: Go's major table
// unicode.C also covers Cn, so unicode.In(r, ..., unicode.C) is true for every
// unassigned rune and would make this check a no-op. That was the first
// version of this function; the test for an unassigned rune caught it.
func assigned(r rune) bool {
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z,
		unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}
