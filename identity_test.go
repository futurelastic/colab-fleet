package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// colab-fleet #102: "not yet corroborated" and "no identity ever asserted"
// must round-trip as distinguishable values — the field being absent from
// Session's JSON is the latter; a present IdentityAssertion with Drifted nil
// is the former. Collapsing them would read an adopted/foreign session as
// one this machine has an opinion about.
func TestIdentityAssertion_UncorroboratedRoundTrips(t *testing.T) {
	out := IdentityUncorroborated("alpha", Timestamp{}, "asserted, not yet corroborated")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back IdentityAssertion
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Drifted != nil {
		t.Errorf("Drifted = %v, want nil (unresolved, never a settled value)", back.Drifted)
	}
	if back.Asserted != "alpha" {
		t.Errorf("Asserted = %q, want %q", back.Asserted, "alpha")
	}
	if back.Carried != "" {
		t.Errorf("an uncorroborated assertion carries Carried anyway: %+v", back)
	}
	if back.Evidence == "" {
		t.Error("an uncorroborated assertion must still explain itself")
	}
	if strings.Contains(string(b), `"assertedAt"`) {
		t.Errorf("a zero AssertedAt must be omitted, not encoded as a zero time: %s", b)
	}
}

func TestIdentityAssertion_HeldRoundTrips(t *testing.T) {
	at := Timestamp(mustParseRFC3339(t, "2026-08-24T00:00:00Z"))
	out := IdentityHeld("alpha", at, "the runtime carries what this machine asserted")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back IdentityAssertion
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Drifted == nil || *back.Drifted {
		t.Errorf("Drifted = %v, want false (settled: held)", back.Drifted)
	}
	if back.Carried != "" {
		t.Errorf("a held assertion carries Carried anyway: %+v", back)
	}
	if back.AssertedAt == nil || !back.AssertedAt.Equal(at) {
		t.Errorf("AssertedAt = %v, want %v", back.AssertedAt, at)
	}
}

func TestIdentityAssertion_DriftedRoundTrips(t *testing.T) {
	out := IdentityDrifted("beta-x", "beta", Timestamp{}, "this machine last asserted \"beta-x\" and the runtime now carries \"beta\"")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back IdentityAssertion
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Drifted == nil || !*back.Drifted {
		t.Errorf("Drifted = %v, want true", back.Drifted)
	}
	if back.Asserted != "beta-x" || back.Carried != "beta" {
		t.Errorf("Asserted/Carried = %q/%q, want beta-x/beta", back.Asserted, back.Carried)
	}
}

// The field being absent from Session's own JSON is the fourth state — never
// a claim the identity agrees. Mirrors resume_test.go's
// TestResumeOutcomeAbsenceIsDistinguishableFromUnresolved for the identical
// reason (§5.7).
func TestIdentityAssertion_AbsenceIsDistinguishableFromUncorroborated(t *testing.T) {
	never, err := json.Marshal(Session{SessionRef: SessionRef{Machine: "m", ID: "s"},
		State: UnknownState(ConfidenceObserved, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(never), "identityAssertion") {
		t.Errorf("a session this machine never asserted an identity for must carry no identityAssertion field at all; got %s", never)
	}

	pending, err := json.Marshal(Session{SessionRef: SessionRef{Machine: "m", ID: "s"},
		State:             UnknownState(ConfidenceObserved, "x"),
		IdentityAssertion: IdentityUncorroborated("alpha", Timestamp{}, "not yet corroborated")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pending), `"drifted":true`) || strings.Contains(string(pending), `"drifted":false`) {
		t.Errorf("an uncorroborated assertion must not assert drifted either way; got %s", pending)
	}
	if !strings.Contains(string(pending), "alpha") {
		t.Errorf("an uncorroborated assertion must still say what was asserted; got %s", pending)
	}
}

// A value that cannot support what it claims must neither encode nor decode
// — the same discipline every other §5.7 type in this package holds itself
// to (surface_test.go's RefusesToPresentSomethingItCannotSupport).
func TestIdentityAssertion_RefusesToPresentSomethingItCannotSupport(t *testing.T) {
	drifted := true
	notDrifted := false
	bad := []struct {
		name string
		a    IdentityAssertion
	}{
		{"no asserted identity", IdentityAssertion{Drifted: &notDrifted, Evidence: "e"}},
		{"no evidence at all", IdentityAssertion{Asserted: "a", Drifted: &notDrifted}},
		{"carried set with drifted false", IdentityAssertion{Asserted: "a", Drifted: &notDrifted, Carried: "b", Evidence: "e"}},
		{"carried set with drifted nil", IdentityAssertion{Asserted: "a", Carried: "b", Evidence: "e"}},
		{"drifted true with no carried", IdentityAssertion{Asserted: "a", Drifted: &drifted, Evidence: "e"}},
		{"drifted true with carried equal to asserted", IdentityAssertion{Asserted: "a", Drifted: &drifted, Carried: "a", Evidence: "e"}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.a); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
	}

	raw := []byte(`{"asserted":"a","drifted":true,"carried":"a","evidence":"e"}`)
	var back IdentityAssertion
	if err := json.Unmarshal(raw, &back); err == nil {
		t.Errorf("carried equal to asserted must not decode, got %+v", back)
	}

	good := IdentityUncorroborated("alpha", Timestamp{}, "not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed assertion must encode: %v", err)
	}
	var got IdentityAssertion
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Evidence != good.Evidence || got.Drifted != nil {
		t.Errorf("round trip changed the assertion: %+v vs %+v", got, *good)
	}
}

func mustParseRFC3339(t *testing.T, s string) Timestamp {
	t.Helper()
	var ts Timestamp
	if err := ts.UnmarshalJSON([]byte(`"` + s + `"`)); err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ts
}
