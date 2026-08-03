package fleet

import (
	"encoding/json"
	"testing"
)

func TestOutcome_JSONRoundTrip(t *testing.T) {
	for _, want := range []Outcome{OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown} {
		b, err := json.Marshal(DeliveryReceipt{Outcome: want})
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got DeliveryReceipt
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got.Outcome != want {
			t.Fatalf("round trip: got %q, want %q", got.Outcome, want)
		}
	}
}

func TestOutcome_RejectsUnknownValue(t *testing.T) {
	var o Outcome
	if err := json.Unmarshal([]byte(`"maybe"`), &o); err == nil {
		t.Fatal("expected an error unmarshaling an unrecognised Outcome, got nil")
	}
}
