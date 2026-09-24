package fleet

import (
	"encoding/json"
	"testing"
)

func TestKeyName_JSONRoundTrip(t *testing.T) {
	for _, want := range KeyNames() {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got KeyName
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip: %q -> %q", want, got)
		}
	}
}

// The set is closed at the decoder, not at the driver. A key name this API
// never defined must not reach a substrate whose own key vocabulary is far
// larger — that is how a narrow endpoint becomes a second way to do everything.
func TestKeyName_RejectsAnythingOutsideTheVocabulary(t *testing.T) {
	// "Tab", "btab" and "S-Tab" are here on purpose (#188): BTab was admitted
	// for the one thing it does, and neighbours that merely LOOK like it —
	// plain Tab, a wrong-case spelling, the other multiplexer spelling — must
	// stay outside a vocabulary that is matched exactly.
	for _, raw := range []string{`"C-c"`, `"C-u"`, `"a"`, `"F1"`, `"enter"`, `""`, `"Tab"`, `"btab"`, `"S-Tab"`, `"ShiftTab"`} {
		var got KeyName
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("Unmarshal(%s) was accepted as %q", raw, got)
		}
	}
}

// A closed set that can be marshalled outside itself would let a bad value
// travel on the wire and be rejected only at the far end, or not at all.
func TestKeyName_MarshalRejectsAnInvalidValue(t *testing.T) {
	if _, err := json.Marshal(KeyName("C-c")); err == nil {
		t.Error("marshalling a key outside the set must fail")
	}
}

// The vocabulary is move, accept, dismiss — and specifically not the control
// keys, each of which has an operation of its own that carries corroboration a
// blind keypress cannot.
func TestKeyNames_ExcludesWhatOtherOperationsOwn(t *testing.T) {
	// Exact-name comparison, not a substring scan: this used to be
	// strings.Contains over the joined names, which "BTab" (#188) trips on
	// "Tab" even though plain Tab is still, correctly, outside the set.
	have := map[string]bool{}
	for _, k := range KeyNames() {
		have[string(k)] = true
	}
	for _, forbidden := range []string{"C-c", "C-u", "C-d", "Tab"} {
		if have[forbidden] {
			t.Errorf("%q is in the vocabulary; interrupt, discard and input own those", forbidden)
		}
	}
	if len(KeyNames()) != 7 {
		t.Errorf("vocabulary has %d keys; it is deliberately seven "+
			"(move, accept, dismiss, and BTab by the #188 ruling)", len(KeyNames()))
	}
}

// BTab is the one member of the vocabulary that is not a dialog key: it cycles
// the runtime's permission mode. It is pinned by name so that dropping it, or
// renaming it on the wire, is a deliberate edit to this test and not a quiet
// side effect of tidying the list (#188).
func TestKeyName_BTabIsAdmittedAndSpelledAsTheMultiplexerSpellsIt(t *testing.T) {
	if !KeyBTab.Valid() {
		t.Fatal("KeyBTab must be a valid key (ruled on #188)")
	}
	if string(KeyBTab) != "BTab" {
		t.Errorf("wire spelling is %q; it is deliberately the multiplexer's own name, BTab", KeyBTab)
	}
	var got KeyName
	if err := json.Unmarshal([]byte(`"BTab"`), &got); err != nil || got != KeyBTab {
		t.Errorf(`Unmarshal("BTab") = %q, %v; want KeyBTab`, got, err)
	}
	b, err := json.Marshal(KeyBTab)
	if err != nil || string(b) != `"BTab"` {
		t.Errorf("Marshal(KeyBTab) = %s, %v", b, err)
	}
}

// ScreenDigest is the corroboration token for a raw key, and it changes on
// every repaint. Treating it as a material change would emit an event per
// character an agent prints.
func TestScreenDigestIsNotAMaterialChange(t *testing.T) {
	a := SessionState{Status: StatusWorking, Confidence: ConfidenceInferred, ScreenDigest: "aaaa"}
	b := a
	b.ScreenDigest = "bbbb"
	if b.MateriallyDiffers(a) {
		t.Error("a repainted screen must not fire an event; that is one per keystroke")
	}
}
