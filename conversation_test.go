package fleet_test

import (
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// The distinction this whole type exists for: "nobody looked" and "we looked
// and could not tell" are different answers (§5.7), and a caller that cannot
// tell them apart will read a driver with no record store as a session whose
// record is missing.
func TestConversationAbsenceIsDistinguishableFromAFailedLookup(t *testing.T) {
	never, err := json.Marshal(fleet.Session{SessionRef: fleet.SessionRef{Machine: "m", ID: "s"},
		State: fleet.UnknownState(fleet.ConfidenceObserved, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(never), "conversation") {
		t.Errorf("a session nobody looked up must carry no conversation field at all; got %s", never)
	}

	looked, err := json.Marshal(fleet.Session{SessionRef: fleet.SessionRef{Machine: "m", ID: "s"},
		State:        fleet.UnknownState(fleet.ConfidenceObserved, "x"),
		Conversation: fleet.UnresolvedConversation("two records could both be this conversation")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(looked), `"known":false`) {
		t.Errorf("a lookup that failed must say so explicitly, not by absence; got %s", looked)
	}
	if !strings.Contains(string(looked), "two records") {
		t.Errorf("a failed lookup must carry its own evidence; got %s", looked)
	}
}

func TestConversationSourceIsAClosedSet(t *testing.T) {
	for _, raw := range []string{`"guessed"`, `""`, `"observed"`} {
		var got fleet.ConversationSource
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("decoding %s as a ConversationSource must fail, got %q", raw, got)
		}
	}
	for _, ok := range []fleet.ConversationSource{fleet.ConversationDerived, fleet.ConversationCaptured} {
		b, err := json.Marshal(ok)
		if err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
		var back fleet.ConversationSource
		if err := json.Unmarshal(b, &back); err != nil || back != ok {
			t.Errorf("round trip of %q gave %q, %v", ok, back, err)
		}
	}
}

// A ref that says it knows an identifier and does not carry one, or carries one
// without saying how it was obtained, is the exact confusion this field exists
// to prevent — so it must not be encodable, and must not decode from a peer.
func TestConversationRefRefusesToPresentSomethingItCannotSupport(t *testing.T) {
	bad := []struct {
		name string
		ref  fleet.ConversationRef
	}{
		{"known with no identifier", fleet.ConversationRef{Known: true, Source: fleet.ConversationDerived, Evidence: "e"}},
		{"known with no source", fleet.ConversationRef{Known: true, ID: "abc", Evidence: "e"}},
		{"known with an invented source", fleet.ConversationRef{Known: true, ID: "abc", Source: "psychic", Evidence: "e"}},
		{"unresolved but carrying an identifier", fleet.ConversationRef{ID: "abc", Evidence: "e"}},
		{"unresolved with no evidence", fleet.ConversationRef{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.ref); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
		raw, _ := json.Marshal(struct {
			Known    bool   `json:"known"`
			ID       string `json:"id,omitempty"`
			Source   string `json:"source,omitempty"`
			Evidence string `json:"evidence"`
		}{c.ref.Known, c.ref.ID, string(c.ref.Source), c.ref.Evidence})
		var back fleet.ConversationRef
		if err := json.Unmarshal(raw, &back); err == nil {
			t.Errorf("%s: must not decode from a peer either, got %+v", c.name, back)
		}
	}

	good := fleet.ResolvedConversation("abc", fleet.ConversationDerived, "the only record carrying this session's name")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed ref must encode: %v", err)
	}
	var back fleet.ConversationRef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back != *good {
		t.Errorf("round trip changed the ref: %+v vs %+v", back, *good)
	}
}
