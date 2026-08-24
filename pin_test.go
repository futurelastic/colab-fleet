package fleet

import (
	"encoding/json"
	"testing"
)

// colab-fleet #84: an unresolved pin must round-trip with Honoured, Source
// and Applied all absent — never asserting a fate this driver has not
// established.
func TestPinResult_UnresolvedRoundTrips(t *testing.T) {
	out := PinUnresolved("opus", "this driver passes pins on the command line and cannot read them back")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back PinResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Requested != "opus" {
		t.Errorf("Requested = %q, want opus", back.Requested)
	}
	if back.Honoured != nil {
		t.Errorf("Honoured = %v, want nil", back.Honoured)
	}
	if back.Applied != "" {
		t.Errorf("Applied = %q, want empty (never populated from the request)", back.Applied)
	}
	if back.Source != "" {
		t.Errorf("Source = %q, want empty when Honoured is unresolved", back.Source)
	}
}

// A resolved outcome round-trips faithfully in both directions — honoured
// and not — carrying the value actually requested either way.
func TestPinResult_ResolvedRoundTrips(t *testing.T) {
	for _, honoured := range []bool{true, false} {
		out := PinResolved("opus", "sonnet", honoured, PinObserved, "evidence")
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("honoured=%v: Marshal: %v", honoured, err)
		}
		var back PinResult
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("honoured=%v: Unmarshal: %v", honoured, err)
		}
		if back.Requested != "opus" {
			t.Errorf("honoured=%v: Requested = %q, want opus", honoured, back.Requested)
		}
		if back.Honoured == nil || *back.Honoured != honoured {
			t.Errorf("honoured=%v: round trip gave %v", honoured, back.Honoured)
		}
		if back.Applied != "sonnet" {
			t.Errorf("honoured=%v: Applied = %q, want sonnet", honoured, back.Applied)
		}
		if back.Source != PinObserved {
			t.Errorf("honoured=%v: Source = %q, want observed", honoured, back.Source)
		}
	}
}

// A resolved-but-not-honoured outcome may have no idea what replaced the
// request — Applied empty alongside Honoured false must round-trip, not be
// treated as incoherent.
func TestPinResult_NotHonouredWithNoKnownReplacement(t *testing.T) {
	out := &PinResult{Requested: "opus", Honoured: boolPtr(false), Source: PinDeclared, Evidence: "e"}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back PinResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Applied != "" {
		t.Errorf("Applied = %q, want empty", back.Applied)
	}
	if back.Honoured == nil || *back.Honoured {
		t.Errorf("Honoured = %v, want false", back.Honoured)
	}
}

// A PinResult that cannot support what it claims must neither encode nor
// decode — the same discipline ConversationRef, ResumeOutcome and
// PromptDelivery all hold themselves to.
func TestPinResult_RefusesToPresentSomethingItCannotSupport(t *testing.T) {
	honoured := true
	bad := []struct {
		name string
		out  PinResult
	}{
		{"no requested value", PinResult{Honoured: &honoured, Source: PinObserved, Evidence: "e"}},
		{"no evidence", PinResult{Requested: "opus", Honoured: &honoured, Source: PinObserved}},
		{"honoured known with no source", PinResult{Requested: "opus", Honoured: &honoured, Evidence: "e"}},
		{"unresolved but carries a source anyway", PinResult{Requested: "opus", Source: PinObserved, Evidence: "e"}},
		{"unresolved but carries an applied value anyway", PinResult{Requested: "opus", Applied: "sonnet", Evidence: "e"}},
		{"zero value", PinResult{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.out); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
	}

	raw := []byte(`{"requested":"opus","honoured":true,"source":"not-a-real-source","evidence":"e"}`)
	var back PinResult
	if err := json.Unmarshal(raw, &back); err == nil {
		t.Errorf("an unrecognised source must not decode, got %+v", back)
	}

	good := PinUnresolved("opus", "not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed result must encode: %v", err)
	}
	var got PinResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Requested != good.Requested || got.Evidence != good.Evidence || got.Honoured != nil {
		t.Errorf("round trip changed the result: %+v vs %+v", got, *good)
	}
}

// PinOutcome carries one field per pin, independently — requesting only
// model must not manufacture agent/effort entries.
func TestPinOutcome_OnlyCarriesWhatWasRequested(t *testing.T) {
	out := PinOutcome{Model: PinUnresolved("opus", "e")}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back PinOutcome
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Agent != nil || back.Effort != nil {
		t.Errorf("PinOutcome = %+v, want agent and effort both absent", back)
	}
	if back.Model == nil || back.Model.Requested != "opus" {
		t.Errorf("Model = %+v, want requested=opus", back.Model)
	}
}
