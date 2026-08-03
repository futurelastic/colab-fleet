package fleet

import (
	"encoding/json"
	"testing"
)

func TestStatus_JSONRoundTrip(t *testing.T) {
	all := []Status{
		StatusStarting, StatusWorking, StatusWaitingInput, StatusIdle,
		StatusQuotaBlocked, StatusDead, StatusUnknown,
	}
	for _, want := range all {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got Status
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Fatalf("round trip: got %q, want %q", got, want)
		}
	}
}

func TestStatus_UnknownSurvivesAsLiteralString(t *testing.T) {
	// §2.3: "unknown" must never be conflated with an absent/empty field.
	state := SessionState{Status: StatusUnknown, Confidence: ConfidenceObserved, Evidence: "driver said so"}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, ok := raw["status"]
	if !ok {
		t.Fatalf("status field missing from %s — omitted rather than emitted as a literal value", b)
	}
	if got != "unknown" {
		t.Fatalf("status = %v (%T), want literal string \"unknown\"", got, got)
	}
}

func TestStatus_RejectsEmptyString(t *testing.T) {
	var s Status
	if err := json.Unmarshal([]byte(`""`), &s); err == nil {
		t.Fatal("expected an error unmarshaling an empty Status, got nil")
	}
}

func TestStatus_RejectsUnknownValue(t *testing.T) {
	var s Status
	if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
		t.Fatal("expected an error unmarshaling an unrecognised Status, got nil")
	}
}

func TestStatus_MarshalRejectsInvalidValue(t *testing.T) {
	s := Status("bogus")
	if _, err := json.Marshal(s); err == nil {
		t.Fatal("expected an error marshaling an invalid Status, got nil")
	}
}

func TestConfidence_JSONRoundTrip(t *testing.T) {
	for _, want := range []Confidence{ConfidenceObserved, ConfidenceInferred} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got Confidence
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Fatalf("round trip: got %q, want %q", got, want)
		}
	}
}

func TestConfidence_RejectsEmptyString(t *testing.T) {
	var c Confidence
	if err := json.Unmarshal([]byte(`""`), &c); err == nil {
		t.Fatal("expected an error unmarshaling an empty Confidence, got nil")
	}
}

func TestConfidence_RejectsUnknownValue(t *testing.T) {
	var c Confidence
	if err := json.Unmarshal([]byte(`"guessing"`), &c); err == nil {
		t.Fatal("expected an error unmarshaling an unrecognised Confidence, got nil")
	}
}

func TestUnknownState_TakesConfidenceAsGiven(t *testing.T) {
	// The spec never fixes a Confidence for StatusUnknown — see doc.go's
	// findings list. Both are legal; the constructor must not silently
	// override either.
	observed := UnknownState(ConfidenceObserved, "api replied: unknown")
	if observed.Confidence != ConfidenceObserved {
		t.Fatalf("Confidence = %q, want %q", observed.Confidence, ConfidenceObserved)
	}
	inferred := UnknownState(ConfidenceInferred, "could not classify pane text")
	if inferred.Confidence != ConfidenceInferred {
		t.Fatalf("Confidence = %q, want %q", inferred.Confidence, ConfidenceInferred)
	}
}
