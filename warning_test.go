package fleet

import (
	"encoding/json"
	"testing"
)

// Absent must stay absent on the wire, the same discipline
// TestAbsentControlChannelIsOmittedEntirely already holds ControlChannel to:
// "nothing to report" must not serialise as an empty list.
func TestAbsentWarningsIsOmittedEntirely(t *testing.T) {
	st := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred, Evidence: "settled"}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); contains(got, "warnings") {
		t.Errorf("no notices found, but warnings appeared on the wire: %s", got)
	}

	st.Warnings = []Warning{{Kind: WarningTranscriptUnreliable, Text: "Transcript writes are failing (disk full — ENOSPC) · recent messages may …"}}
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Warnings) != 1 || back.Warnings[0].Kind != WarningTranscriptUnreliable {
		t.Errorf("warning lost in transit: %s", b)
	}
}

// An unrecognised notice still reaches the wire — Kind empty is a real
// answer, not a reason to drop the finding (see WarningKind's own doc). Only
// the per-item Kind is omitempty; Text always rides along.
func TestUnrecognisedWarningKindStillCarriesText(t *testing.T) {
	st := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	st.Warnings = []Warning{{Text: "✻ Something new the driver has never named"}}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); contains(got, `"kind"`) {
		t.Errorf("an empty Kind must not appear on the wire: %s", got)
	}
	var back SessionState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Warnings) != 1 || back.Warnings[0].Kind != "" ||
		back.Warnings[0].Text != "✻ Something new the driver has never named" {
		t.Errorf("unrecognised notice not preserved: %s", b)
	}
}

// A notice appearing or clearing changes nothing else on SessionState — same
// status, same composer, same prompt — so it must still fire, the identical
// argument TestAChannelChangeIsAMaterialChange makes for ControlChannel one
// field over.
func TestAWarningAppearingOrClearingIsAMaterialChange(t *testing.T) {
	a := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	b := a
	b.Warnings = []Warning{{Kind: WarningTranscriptUnreliable, Text: "Transcript writes are failing…"}}
	if !b.MateriallyDiffers(a) {
		t.Error("a notice appearing would reach no subscriber; that is the invisibility " +
			"this field exists to end, moved to the event plane")
	}
	c := a
	if b.MateriallyDiffers(b) {
		t.Error("an unchanged set of warnings must not fire an event")
	}
	if !a.MateriallyDiffers(b) {
		t.Error("a notice clearing must be reported too — a mirror that never learns the " +
			"transcript recovered stays wrong in the opposite direction")
	}
	_ = c
}

// Same Kind, different Text is still a second finding worth an event — the
// same call sameTurnEnd and ControlChannel.Reason already make for their own
// runtime-reported prose.
func TestAWarningTextChangeIsAMaterialChangeEvenWithTheSameKind(t *testing.T) {
	a := SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}
	a.Warnings = []Warning{{Kind: WarningTranscriptUnreliable, Text: "Transcript writes are failing (disk full — ENOSPC) · recent messages may …"}}
	b := a
	b.Warnings = []Warning{{Kind: WarningTranscriptUnreliable, Text: "Transcript writes are failing (disk full — ENOSPC) · older messages may …"}}
	if !b.MateriallyDiffers(a) {
		t.Error("a reworded notice under the same Kind must still fire")
	}
	c := b
	if c.MateriallyDiffers(b) {
		t.Error("an unchanged notice must not fire an event")
	}
}
