package inboxclient

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// receiverStrip and receiverTrim transcribe the receiving runtime's name
// normaliser as REGULAR EXPRESSIONS, the form it is written in there — kept
// deliberately separate from the unicode.In calls SenderName uses, so these
// tests do not check the production code against itself.
var (
	receiverStrip = regexp.MustCompile(`[\p{Cf}\p{Cc}\p{Cs}\p{Zl}\p{Zp}]`)
	receiverDrop  = regexp.MustCompile(`["<>]`)
	receiverTrim  = regexp.MustCompile(`^[\p{Zs}\t\v\f\x{FEFF}]+|[\p{Zs}\t\v\f\x{FEFF}]+$`)
)

// receiverRebuildName is what the receiver writes back into from-name when it
// re-serialises an envelope: drop quotes and angle brackets, strip invisible
// runes, trim, and cut past 64 code points with an ellipsis. "" means it would
// omit the attribute.
func receiverRebuildName(n string) string {
	s := receiverDrop.ReplaceAllString(n, "")
	s = receiverStrip.ReplaceAllString(s, "")
	s = receiverTrim.ReplaceAllString(s, "")
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64]) + "…"
	}
	return s
}

// TestAttestNameGoldenBytes pins the attribute's position: after the tag name,
// BEFORE from-mode. The receiver rebuilds in that order and compares bytes.
func TestAttestNameGoldenBytes(t *testing.T) {
	got, ok := Attest("hello", ModeBypass, "agent-a · s1 · box")
	if !ok {
		t.Fatal("Attest refused a plain body with a plain name")
	}
	want := "<cross-session-message from-name=\"agent-a · s1 · box\" from-mode=\"bypass\">\nhello\n</cross-session-message>"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// hostileNames is #158's acceptance list plus the neighbours of each case.
var hostileNames = []string{
	"agent-a · colab-fleet-158 · box",
	"👨‍👩‍👧 family",            // joiner-based compound emoji
	"🏳️‍🌈 flag",               // joiner plus a variation selector
	"a\x00b\x1bc\x7fd",        // control characters
	"tab\there\r\nand crlf",   // controls that are also whitespace
	`say "hi" <x> & 'y'`,      // quotes and angle brackets
	"   padded both sides   ", // leading and trailing space
	"\u00a0\u3000nbsp and ideographic space\u2003",
	strings.Repeat("a", 100),         // over the cap
	strings.Repeat("a", 64),          // exactly the cap
	strings.Repeat("a", 65),          // one over
	strings.Repeat("a", 63) + "   b", // a cut that would leave trailing space
	strings.Repeat("日本語", 30),        // multi-byte, over the cap
	"\u200d\u200b\u2060",             // nothing but invisibles
	"\"<>\"",                         // nothing but dropped characters
	"line\u2028sep\u2029para",        // line and paragraph separators
	"left\u202eright",                // bidi override (Cf)
	"\U000E0080 unassigned",          // a rune this build does not assign
	"private \uE000 use",             // private use: assigned, kept
	"bad utf8 \xff\xfe here",         // invalid UTF-8
	"",
}

// TestAttestWithHostileNamesStaysAttestedAndRoundTrips is the acceptance
// criterion: whatever the name, the send stays attested, and the envelope the
// receiver parses and rebuilds is byte-identical to what was sent.
func TestAttestWithHostileNamesStaysAttestedAndRoundTrips(t *testing.T) {
	for _, class := range []ModeClass{ModeBypass, ModePrompting} {
		for _, name := range hostileNames {
			assertRoundTrip(t, "Read the brief and follow it end to end.", class, name)
		}
	}
}

func assertRoundTrip(t *testing.T, body string, class ModeClass, name string) {
	t.Helper()
	got, ok := Attest(body, class, name)
	if !ok {
		t.Fatalf("name %q cost the send its attestation", name)
	}
	m := receiverGrammar.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("receiver grammar rejected the envelope for name %q:\n%q", name, got)
	}
	if m[5] != string(class) {
		t.Errorf("name %q: class group = %q, want %q", name, m[5], class)
	}
	if want := SenderName(name); m[4] != want {
		t.Errorf("name %q: name group = %q, want %q", name, m[4], want)
	}
	if rt := rebuild(m[1], m[2], m[3], m[4], m[5], m[6]); rt != got {
		t.Errorf("name %q: receiver rebuild differs, envelope would be DISCARDED:\n got %q\nwant %q", name, rt, got)
	}
}

// TestSenderName pins the normalised form of each class of hostile input.
func TestSenderName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"agent-a · s1 · box", "agent-a · s1 · box"},
		{"👨‍👩‍👧 family", "👨👩👧 family"},
		{`say "hi" <x>`, "say hi x"},
		{"   padded   ", "padded"},
		{"a\x00b\x1bc", "abc"},
		{"line\u2028sep", "linesep"},
		{"\u200d\u200b\u2060", ""},
		{"\"<>\"", ""},
		{"\U000E0080 unassigned", ""},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), strings.Repeat("a", 63) + "…"},
		{strings.Repeat("a", 62) + " bbbb", strings.Repeat("a", 62) + "…"},
	} {
		if got := SenderName(tc.in); got != tc.want {
			t.Errorf("SenderName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSenderNameIsAFixedPointWithinTheCap is the property the whole #158
// mechanism rests on, checked over generated names drawn from the runes most
// likely to break it: whatever SenderName returns, the receiver leaves it
// untouched, it fits the cap, and it never costs the send its attestation.
func TestSenderNameIsAFixedPointWithinTheCap(t *testing.T) {
	pool := []rune{
		'a', 'Z', '0', ' ', '-', '·', '"', '<', '>', '\'', '&', '/',
		'\t', '\n', '\r', 0x00, 0x1b, 0x7f, 0x85,
		0x00a0, 0x2003, 0x3000, 0x202f, 0x1680, // Zs
		0x200b, 0x200c, 0x200d, 0x2060, 0xfeff, 0x202e, 0x00ad, // Cf
		0x2028, 0x2029, // Zl, Zp
		0xfe0f, 0x0301, // variation selector, combining mark
		'👨', '🏳', '日', 'ệ', 0xe000, 0x10ffff, 0xe0080, 0x1f3fb,
		0xff1c, 0x2329, // bracket lookalikes: fine in a name, not in a body
	}
	rng := rand.New(rand.NewSource(158))
	for i := 0; i < 20000; i++ {
		n := rng.Intn(90)
		rs := make([]rune, n)
		for j := range rs {
			rs[j] = pool[rng.Intn(len(pool))]
		}
		raw := string(rs)
		got := SenderName(raw)
		if got == "" {
			continue
		}
		if rt := receiverRebuildName(got); rt != got {
			t.Fatalf("SenderName(%q) = %q, which the receiver rebuilds as %q", raw, got, rt)
		}
		if c := utf8.RuneCountInString(got); c > 64 {
			t.Fatalf("SenderName(%q) = %q is %d code points, over the cap", raw, got, c)
		}
		if strings.ContainsAny(got, "\"<>\n\r") {
			t.Fatalf("SenderName(%q) = %q holds a character the receiver's grammar forbids", raw, got)
		}
		if i%10 == 0 {
			assertRoundTrip(t, "hello", ModeBypass, raw)
		}
	}
}
