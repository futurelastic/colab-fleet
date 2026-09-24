package fleet

import (
	"encoding/json"
	"fmt"
)

// PermissionModeState is the permission mode a session's runtime is in RIGHT
// NOW, as the runtime itself shows it (colab-fleet #194).
//
// It is the read side of `keys`' BTab (#188): Shift+Tab cycles the mode, which
// mode a press lands in is the runtime's own cycle order, and a client that
// wants a named mode has to press, look, and repeat until it is showing. Until
// this field existed the "look" had no answer anywhere in this API.
//
// # A closed set, and why it is these six
//
// The five named members are the modes measured on a real runtime build by
// pressing the cycle key through every one of them and reading what the
// indicator row said. Nothing here was inferred from a settings file:
//
//	default      the indicator reads `manual mode on`
//	acceptEdits  `accept edits on`
//	plan         `plan mode on`
//	auto         `auto mode on`
//	bypass       `bypass permissions on`
//
// `bypass` is the same word SessionSpec.PermissionMode takes at create time
// (PermissionModeBypass), so a session created with it reads back as it. It is
// a member here although the request that filed this field did not list it,
// because it is the mode most of an unattended fleet actually runs in: a set
// without it would report `unknown` for exactly the sessions a mode control
// exists to reach.
//
// # Unknown is an answer, and absent is a different one
//
// A value outside the five is never guessed at and never passed through as text.
// Two different situations look alike from far away and must not be merged
// (§5.7):
//
//   - PermissionModeUnknown: the driver found the runtime's indicator area and
//     could not name the mode from it — wording it does not know (a mode this
//     build has and this list does not, or a reworded label), the area showing
//     something else at that moment (a shell-mode hint, say), or two labels at
//     once. A client cycling toward a target should STOP on this, not press on.
//   - the field absent: nothing was read. Either the driver does not look at
//     all (DriverCapabilities.ObservesPermissionMode says so), or the screen
//     had no composer to anchor the indicator area to — a dialog owns it — or
//     the area was empty. A client should read again.
//
// The value carries no conversation and no screen text, so publishing it does
// not reopen the rule that `state` publishes fingerprints of the screen and
// never the screen.
type PermissionModeState string

const (
	// PermissionModeDefault is the runtime's ordinary mode: it asks before it
	// acts, except where a rule has already allowed the action. The runtime's
	// own indicator calls it "manual".
	PermissionModeDefault PermissionModeState = "default"
	// PermissionModeAcceptEdits: file edits are accepted without asking; other
	// actions still ask. Reaching this from default ESCALATES what the agent
	// may do unattended (docs/adr/188).
	PermissionModeAcceptEdits PermissionModeState = "acceptEdits"
	// PermissionModePlan: the agent may read and plan and may not act.
	PermissionModePlan PermissionModeState = "plan"
	// PermissionModeAuto: the runtime decides for itself what is safe to do
	// without asking. Only available on some models and accounts — a build
	// that does not offer it never lands here.
	PermissionModeAuto PermissionModeState = "auto"
	// PermissionModeUnknown: an indicator area was read and named no mode this
	// build recognises. See the type's own doc — never a guess.
	PermissionModeUnknown PermissionModeState = "unknown"

	// The sixth member is PermissionModeBypass, declared in session.go as the
	// create-time value and reused here unchanged: an untyped constant, so it
	// compares and assigns against this type directly, and the two can never
	// drift apart.
)

func (m PermissionModeState) valid() bool {
	switch m {
	case PermissionModeDefault, PermissionModeAcceptEdits, PermissionModePlan,
		PermissionModeAuto, PermissionModeBypass, PermissionModeUnknown:
		return true
	default:
		return false
	}
}

// MarshalJSON rejects a value outside the closed set rather than emitting it. A
// mode nobody defined is worse than none: a client branching on five names
// silently falls through to its default for a sixth.
func (m PermissionModeState) MarshalJSON() ([]byte, error) {
	if !m.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid PermissionModeState", string(m))
	}
	return json.Marshal(string(m))
}

// UnmarshalJSON rejects anything outside the set, the empty string included —
// the same discipline Status and ControlChannelState follow: an absent value
// and a mode named "" are different facts and must not collapse. (Absent never
// reaches this method; the field is omitempty and a missing key is not decoded.)
func (m *PermissionModeState) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := PermissionModeState(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid PermissionModeState", raw)
	}
	*m = v
	return nil
}
