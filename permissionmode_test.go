package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// The closed set is exactly six values, and bypass is the create-time word.
func TestPermissionModeStateIsTheSixValuesAndBypassIsTheCreateTimeWord(t *testing.T) {
	all := []PermissionModeState{
		PermissionModeDefault, PermissionModeAcceptEdits, PermissionModePlan,
		PermissionModeAuto, PermissionModeBypass, PermissionModeUnknown,
	}
	seen := map[PermissionModeState]bool{}
	for _, m := range all {
		if !m.valid() {
			t.Errorf("%q is a declared member and reads as invalid", m)
		}
		if seen[m] {
			t.Errorf("%q declared twice", m)
		}
		seen[m] = true
		if _, err := json.Marshal(m); err != nil {
			t.Errorf("marshal %q: %v", m, err)
		}
	}
	// A session created with permissionMode "bypass" must read back as it. They
	// are the same constant, so this cannot drift — the assertion is the
	// tripwire for somebody giving the state enum its own spelling.
	if string(PermissionModeBypass) != "bypass" {
		t.Errorf("bypass is spelled %q; the create-time spelling is what state must read back", PermissionModeBypass)
	}
}

// A value nobody defined is refused in both directions. A client branching on
// five names must never meet a sixth by surprise, and this service must never
// emit one.
func TestPermissionModeStateRefusesAnythingOutsideTheSet(t *testing.T) {
	for _, bad := range []PermissionModeState{"", "yolo", "Plan", "accept edits", "bypassPermissions"} {
		if _, err := json.Marshal(bad); err == nil {
			t.Errorf("marshalled %q; a value outside the set must be refused", bad)
		}
		var m PermissionModeState
		if err := json.Unmarshal([]byte(`"`+string(bad)+`"`), &m); err == nil {
			t.Errorf("unmarshalled %q into %q; a value outside the set must be refused", bad, m)
		}
	}
	var m PermissionModeState
	if err := json.Unmarshal([]byte(`7`), &m); err == nil {
		t.Error("a non-string decoded into the enum")
	}
}

// Absent stays absent on the wire — the field is omitted, never emitted as an
// empty string or null — and a value round-trips. This is the difference
// between "nothing was read" and "unknown" surviving a hop.
func TestPermissionModeIsOmittedWhenNothingWasReadAndRoundTripsOtherwise(t *testing.T) {
	st := UnknownState(ConfidenceInferred, "x")
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "permissionMode") {
		t.Errorf("a state that read no mode carries the key: %s", b)
	}

	for _, want := range []PermissionModeState{PermissionModePlan, PermissionModeUnknown, PermissionModeBypass} {
		st.PermissionMode = want
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"permissionMode":"`+string(want)+`"`) {
			t.Errorf("%q missing from the wire form: %s", want, b)
		}
		var back SessionState
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.PermissionMode != want {
			t.Errorf("round trip: %q -> %q", want, back.PermissionMode)
		}
	}

	// A peer that predates the field sends none; that decodes to "nothing read".
	var old SessionState
	if err := json.Unmarshal([]byte(`{"status":"idle","confidence":"inferred","evidence":"e"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.PermissionMode != "" {
		t.Errorf("an absent key decoded to %q", old.PermissionMode)
	}
}

// A mode change moves nothing else on the state: same status, same composer,
// same prompt. If it were not material a watcher would keep showing the mode a
// session had before somebody pressed the key that changes it — the exact
// invisibility the event plane was widened to end.
func TestPermissionModeIsMaterialToTheEventPlane(t *testing.T) {
	a := UnknownState(ConfidenceInferred, "x")
	b := a
	if a.MateriallyDiffers(b) {
		t.Fatal("identical states differ")
	}
	b.PermissionMode = PermissionModePlan
	if !a.MateriallyDiffers(b) || !b.MateriallyDiffers(a) {
		t.Error("a mode appearing is not a material change")
	}
	a.PermissionMode = PermissionModeAcceptEdits
	if !a.MateriallyDiffers(b) {
		t.Error("a mode changing is not a material change")
	}
	a.PermissionMode = PermissionModePlan
	if a.MateriallyDiffers(b) {
		t.Error("the same mode is a material change; that would emit an event per poll")
	}
}

// An absent flag on the wire is false, the same as every other capability, and a
// present one round-trips under the name the docs give it.
func TestObservesPermissionModeIsACapabilityFlag(t *testing.T) {
	b, err := json.Marshal(DriverCapabilities{Source: CapabilitiesObserved, ObservesPermissionMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"observesPermissionMode":true`) {
		t.Errorf("capability missing from the wire form: %s", b)
	}
	var c DriverCapabilities
	if err := json.Unmarshal([]byte(`{}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.ObservesPermissionMode {
		t.Error("an absent capability decoded as true")
	}
}
