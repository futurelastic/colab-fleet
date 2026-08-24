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

// colab-fleet #86: a pending delivery must round-trip with Outcome absent —
// never asserting an outcome that has not resolved.
func TestPromptDelivery_PendingRoundTrips(t *testing.T) {
	out := PromptPending("accepted at creation, not yet delivered")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `{"outcome":null,"evidence":"accepted at creation, not yet delivered"}` {
		t.Errorf("unexpected wire shape: %s", b)
	}
	var back PromptDelivery
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Outcome != nil {
		t.Errorf("Outcome = %v, want nil", back.Outcome)
	}
	if back.Evidence != out.Evidence {
		t.Errorf("Evidence = %q, want %q", back.Evidence, out.Evidence)
	}
}

// A resolved delivery round-trips carrying the same outcome send() would
// have returned for the same delivery.
func TestPromptDelivery_ResolvedRoundTrips(t *testing.T) {
	for _, want := range []Outcome{OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown} {
		out := PromptDelivered(want, "evidence")
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("outcome=%q: Marshal: %v", want, err)
		}
		var back PromptDelivery
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("outcome=%q: Unmarshal: %v", want, err)
		}
		if back.Outcome == nil || *back.Outcome != want {
			t.Errorf("outcome=%q: round trip gave %v", want, back.Outcome)
		}
		if back.Evidence != "evidence" {
			t.Errorf("outcome=%q: Evidence = %q, want %q", want, back.Evidence, "evidence")
		}
	}
}

// A PromptDelivery that cannot support what it claims — no evidence, or an
// outcome outside the closed set — must neither encode nor decode. Same
// discipline ConversationRef and ResumeOutcome hold themselves to: silently
// accepting it would let a caller compare two malformed values and conclude
// they describe the same thing.
func TestPromptDelivery_RefusesToPresentSomethingItCannotSupport(t *testing.T) {
	submitted := OutcomeSubmitted
	bad := []struct {
		name string
		out  PromptDelivery
	}{
		{"no evidence, resolved", PromptDelivery{Outcome: &submitted}},
		{"no evidence, pending", PromptDelivery{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.out); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
	}

	// An outcome outside the closed set must not decode, even with evidence
	// present — the same rule Outcome's own decoder already enforces, now
	// reached through the field that embeds it.
	raw := []byte(`{"outcome":"maybe","evidence":"e"}`)
	var back PromptDelivery
	if err := json.Unmarshal(raw, &back); err == nil {
		t.Errorf("an unrecognised outcome must not decode, got %+v", back)
	}

	good := PromptPending("not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed value must encode: %v", err)
	}
	var got PromptDelivery
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Evidence != good.Evidence || got.Outcome != nil {
		t.Errorf("round trip changed the value: %+v vs %+v", got, *good)
	}
}
