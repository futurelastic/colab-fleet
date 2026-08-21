package fleet_test

import (
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// §5.7 again: "no resume was requested" and "a resume was requested but
// not yet resolved" are different answers, and a caller that cannot tell
// them apart will read a session that never asked to resume anything as
// one whose resume is still pending.
func TestResumeOutcomeAbsenceIsDistinguishableFromUnresolved(t *testing.T) {
	never, err := json.Marshal(fleet.Session{SessionRef: fleet.SessionRef{Machine: "m", ID: "s"},
		State: fleet.UnknownState(fleet.ConfidenceObserved, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(never), "resumeOutcome") {
		t.Errorf("a session that never asked to resume anything must carry no resumeOutcome field at all; got %s", never)
	}

	pending, err := json.Marshal(fleet.Session{SessionRef: fleet.SessionRef{Machine: "m", ID: "s"},
		State:         fleet.UnknownState(fleet.ConfidenceObserved, "x"),
		ResumeOutcome: fleet.ResumeUnresolved("conv-1", "the session's own conversation has not resolved yet")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pending), `"honoured":true`) || strings.Contains(string(pending), `"honoured":false`) {
		t.Errorf("an unresolved outcome must not assert honoured either way; got %s", pending)
	}
	if !strings.Contains(string(pending), "conv-1") {
		t.Errorf("an unresolved outcome must still say what was requested; got %s", pending)
	}
}

// The verdict #72 exists for: honoured true and false must both round-trip
// faithfully, carrying the id that was actually requested either way.
func TestResumeOutcomeHonouredRoundTrips(t *testing.T) {
	for _, honoured := range []bool{true, false} {
		out := fleet.ResumeResolved("conv-1", honoured, "evidence")
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("honoured=%v: %v", honoured, err)
		}
		var back fleet.ResumeOutcome
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("honoured=%v: unmarshal: %v", honoured, err)
		}
		if back.Requested != "conv-1" {
			t.Errorf("honoured=%v: Requested = %q, want conv-1", honoured, back.Requested)
		}
		if back.Honoured == nil || *back.Honoured != honoured {
			t.Errorf("honoured=%v: round trip gave %v", honoured, back.Honoured)
		}
	}
}

// A ResumeOutcome that says something it cannot support — no requested id,
// or no evidence either way — must not be encodable, and must not decode
// from a peer. The same discipline ConversationRef holds itself to, and for
// the same reason: a caller comparing two malformed outcomes with empty
// Requested fields would conclude two unrelated sessions share one.
func TestResumeOutcomeRefusesToPresentSomethingItCannotSupport(t *testing.T) {
	honoured := true
	bad := []struct {
		name string
		out  fleet.ResumeOutcome
	}{
		{"no requested id", fleet.ResumeOutcome{Honoured: &honoured, Evidence: "e"}},
		{"no evidence", fleet.ResumeOutcome{Requested: "conv-1", Honoured: &honoured}},
		{"zero value", fleet.ResumeOutcome{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.out); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
		raw, _ := json.Marshal(struct {
			Requested string `json:"requested"`
			Honoured  *bool  `json:"honoured"`
			Evidence  string `json:"evidence"`
		}{c.out.Requested, c.out.Honoured, c.out.Evidence})
		var back fleet.ResumeOutcome
		if err := json.Unmarshal(raw, &back); err == nil {
			t.Errorf("%s: must not decode from a peer either, got %+v", c.name, back)
		}
	}

	good := fleet.ResumeUnresolved("conv-1", "not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed outcome must encode: %v", err)
	}
	var back fleet.ResumeOutcome
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.Requested != good.Requested || back.Evidence != good.Evidence || back.Honoured != nil {
		t.Errorf("round trip changed the outcome: %+v vs %+v", back, *good)
	}
}
