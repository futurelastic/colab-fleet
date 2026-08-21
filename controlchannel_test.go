package fleet

import (
	"encoding/json"
	"testing"
)

func TestControlChannelState_JSONRoundTrip(t *testing.T) {
	all := []ControlChannelState{
		ControlChannelActive, ControlChannelConnecting,
		ControlChannelReconnecting, ControlChannelFailed,
	}
	for _, want := range all {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", want, err)
		}
		var got ControlChannelState
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip: %q -> %q", want, got)
		}
	}
}

// The set is closed at the decoder. A fifth state decoded silently would fall
// through every client's four-way branch to its default, which is the quiet
// wrong answer this type exists to prevent.
func TestControlChannelState_RejectsAnythingOutsideTheSet(t *testing.T) {
	for _, raw := range []string{`"suspended"`, `"connected"`, `"Active"`, `""`} {
		var got ControlChannelState
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("Unmarshal(%s) was accepted as %q", raw, got)
		}
	}
	if _, err := json.Marshal(ControlChannelState("suspended")); err == nil {
		t.Error("marshalling a state outside the set must fail")
	}
}

// Absent must stay absent on the wire. A field that serialised as an empty
// object or a zero state would turn "nothing to report" into a verdict, which
// is exactly the §5.7 collapse the type's doc comment forbids.
func TestAbsentControlChannelIsOmittedEntirely(t *testing.T) {
	st := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred, Evidence: "settled"}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); contains(got, "controlChannel") {
		t.Errorf("an unreported channel appeared on the wire: %s", got)
	}

	st.ControlChannel = &ControlChannel{State: ControlChannelFailed}
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.ControlChannel == nil || back.ControlChannel.State != ControlChannelFailed {
		t.Errorf("channel lost in transit: %s", b)
	}
}

// The channel says nothing about what the session is doing, so it must not be
// part of what makes a state materially different... except that it IS: a
// channel going down is precisely the transition a subscriber maintaining a
// mirror needs to be told about, and it changes no other field.
func TestAChannelChangeIsAMaterialChange(t *testing.T) {
	a := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	b := a
	b.ControlChannel = &ControlChannel{State: ControlChannelFailed}
	if !b.MateriallyDiffers(a) {
		t.Error("a channel that just failed would reach no subscriber; that is the " +
			"invisibility this field exists to end, moved to the event plane")
	}

	c := b
	c.ControlChannel = &ControlChannel{State: ControlChannelActive}
	if !c.MateriallyDiffers(b) {
		t.Error("failed -> active must be reported; a recovery nobody hears about " +
			"leaves a supervisor believing the session is still unreachable")
	}
	if c.MateriallyDiffers(c) {
		t.Error("an unchanged channel must not fire an event")
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
