package inboxclient

import (
	"regexp"
	"strings"
	"testing"
)

// receiverGrammar is the receiving runtime's OWN parse grammar, transcribed
// into RE2. It is expressible here because the parse side uses no lookahead
// (only the escaper does, and Attest is built to never provoke it).
//
// Transcribing it is the point: a test asserting our output merely CONTAINS
// the class attribute would pass against a string the receiver rejects. The
// receiver's contract is "matches this grammar AND rebuilds byte-identically",
// so the test has to check both halves.
var receiverGrammar = regexp.MustCompile(
	`^<cross-session-message` +
		`(?: from="([^"]+)")?` +
		`(?: from-session="([^"]+)")?` +
		`(?: hop-chain="([^"]+)")?` +
		`(?: from-name="([^"<>\n\r]+)")?` +
		`(?: from-mode="(bypass|prompting)")?` +
		`>\n([\s\S]*)\n</cross-session-message>$`)

// rebuild is the receiver's own re-serialisation step, for a body that needs
// no escaping — which is precisely the set Attest allows. Attribute ORDER is
// load-bearing: the receiver emits them in this order and compares bytes, so
// a wrapper that emitted the same attributes in a different order would parse
// and then fail the rebuild check.
func rebuild(from, fromSession, hopChain, fromName, mode, body string) string {
	var attrs []string
	if from != "" {
		attrs = append(attrs, `from="`+from+`"`)
	}
	if fromSession != "" {
		attrs = append(attrs, `from-session="`+fromSession+`"`)
	}
	if hopChain != "" {
		attrs = append(attrs, `hop-chain="`+hopChain+`"`)
	}
	if fromName != "" {
		attrs = append(attrs, `from-name="`+fromName+`"`)
	}
	if mode != "" {
		attrs = append(attrs, `from-mode="`+mode+`"`)
	}
	head := ""
	if len(attrs) > 0 {
		head = " " + strings.Join(attrs, " ")
	}
	return "<cross-session-message" + head + ">\n" + body + "\n</cross-session-message>"
}

// TestAttestGoldenBytes pins the exact bytes, both newlines included, for
// both classes. If this test has to be updated, the wire format changed and
// every claim in Attest's doc comment needs re-deriving against the runtime —
// see docs/gotchas.d.
func TestAttestGoldenBytes(t *testing.T) {
	for _, tc := range []struct {
		class ModeClass
		want  string
	}{
		{ModeBypass, "<cross-session-message from-mode=\"bypass\">\nhello\n</cross-session-message>"},
		{ModePrompting, "<cross-session-message from-mode=\"prompting\">\nhello\n</cross-session-message>"},
	} {
		got, ok := Attest("hello", tc.class)
		if !ok {
			t.Fatalf("Attest(%q) refused a plain body", tc.class)
		}
		if got != tc.want {
			t.Errorf("Attest(%q):\n got %q\nwant %q", tc.class, got, tc.want)
		}
	}
}

// TestAttestSatisfiesReceiverGrammar is the test that actually proves the
// envelope works: it parses our output with the receiver's grammar, checks the
// class lands in the class group, and checks the receiver's rebuild is
// byte-identical to what we produced. A failure of the last assertion is the
// silent-hold bug reappearing.
func TestAttestSatisfiesReceiverGrammar(t *testing.T) {
	bodies := []string{
		"hello",
		"",
		"line one\nline two",
		"Read the brief in this repo and follow it end to end.",
		"unicode: 日本語 tiếng Việt — em dash, \"quotes\", 'apostrophes'",
		"trailing spaces   ",
		"a > b & c",
		strings.Repeat("long ", 400),
	}
	for _, class := range []ModeClass{ModeBypass, ModePrompting} {
		for _, body := range bodies {
			got, ok := Attest(body, class)
			if !ok {
				t.Fatalf("Attest(%q, %q) refused an attestable body", body, class)
			}
			m := receiverGrammar.FindStringSubmatch(got)
			if m == nil {
				t.Fatalf("receiver grammar rejected our envelope for body %q", body)
			}
			if m[5] != string(class) {
				t.Errorf("class group = %q, want %q", m[5], class)
			}
			if m[6] != body {
				t.Errorf("body group = %q, want %q", m[6], body)
			}
			if rt := rebuild(m[1], m[2], m[3], m[4], m[5], m[6]); rt != got {
				t.Errorf("receiver rebuild differs, envelope would be DISCARDED:\n got %q\nwant %q", rt, got)
			}
		}
	}
}

// TestAttestRefuses is the refusal table. Every row here is a send that takes
// the pane path instead — correct behaviour, not a degradation.
func TestAttestRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		class ModeClass
	}{
		{"zero class", "hello", ""},
		{"unknown class", "hello", ModeClass("bypassPermissions")},
		{"another unknown class", "hello", ModeClass("plan")},
		{"ascii open bracket", "if a < b then", ModeBypass},
		{"a closing tag in the body", "x\n</cross-session-message>", ModeBypass},
		{"fullwidth lookalike", "a ＜ b", ModeBypass},
		{"small lookalike", "a ﹤ b", ModeBypass},
		{"angle bracket lookalike", "a 〈 b", ModeBypass},
		{"math lookalike", "a ⟨ b", ModePrompting},
		{"cjk lookalike", "a 〈 b", ModeBypass},
		{"guillemet lookalike", "a ‹ b", ModeBypass},
		{"modifier lookalike", "a ˂ b", ModeBypass},
		{"syllabics lookalike", "a ᐸ b", ModeBypass},
		{"ornament lookalike", "a ❬ b", ModeBypass},
		{"not-less-than lookalike", "a ≮ b", ModeBypass},
		{"precedes lookalike", "a ≺ b", ModeBypass},
		{"less-dot lookalike", "a ⋖ b", ModeBypass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Attest(tc.text, tc.class)
			if ok {
				t.Fatalf("Attest attested a body it cannot guarantee: %q", got)
			}
			if got != "" {
				t.Errorf("a refusal must return no envelope, got %q", got)
			}
		})
	}
}

// TestAttestRefusesEveryOpenLookalike guards the transcription itself: every
// rune in the table must be refused. A rune dropped from openLookalikes by a
// careless edit would otherwise only show up as messages silently held again.
func TestAttestRefusesEveryOpenLookalike(t *testing.T) {
	n := 0
	for _, r := range openLookalikes {
		n++
		if _, ok := Attest("before "+string(r)+" after", ModeBypass); ok {
			t.Errorf("rune %U is in openLookalikes but was not refused", r)
		}
	}
	if n != 16 {
		t.Errorf("openLookalikes holds %d runes, want 16 (the ASCII one plus fifteen confusables)", n)
	}
}

// TestModeClassValid pins the closed set. The zero value must never be valid:
// "not asserted" is the state the whole fallback path keys on.
func TestModeClassValid(t *testing.T) {
	for _, c := range []ModeClass{ModeBypass, ModePrompting} {
		if !c.Valid() {
			t.Errorf("%q should be valid", c)
		}
	}
	for _, c := range []ModeClass{"", "bypassPermissions", "acceptEdits", "default", "plan", "BYPASS"} {
		if c.Valid() {
			t.Errorf("%q should not be valid", c)
		}
	}
}
