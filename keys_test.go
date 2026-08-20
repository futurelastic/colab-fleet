package fleet

import (
	"encoding/json"
	"strings"
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
	for _, raw := range []string{`"C-c"`, `"C-u"`, `"a"`, `"F1"`, `"enter"`, `""`} {
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
	joined := ""
	for _, k := range KeyNames() {
		joined += string(k) + " "
	}
	for _, forbidden := range []string{"C-c", "C-u", "C-d", "Tab"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("%q is in the vocabulary; interrupt, discard and input own those", forbidden)
		}
	}
	if len(KeyNames()) != 6 {
		t.Errorf("vocabulary has %d keys; it is deliberately six", len(KeyNames()))
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
