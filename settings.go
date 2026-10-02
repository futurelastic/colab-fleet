package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxLaunchSettingsBytes bounds SessionSpec.Settings after compaction (muster
// #247). The value lands on a command line, and a command line is a bounded,
// world-readable resource; a few hundred bytes covers every launch-time
// switch this field exists for, so a caller sending kilobytes is sending
// something that belongs in a settings FILE, which this field is not.
const MaxLaunchSettingsBytes = 4096

// ValidateLaunchSettings checks SessionSpec.Settings together with the
// PermissionMode it is scoped to (muster #247), and returns the COMPACT JSON a
// driver puts on argv.
//
// # Why the pairing is checked here and not left to each driver
//
// Settings are accepted only on a session created with PermissionModeBypass.
// The issue that added the field asked for it "scoped so it cannot widen a
// non-bypass session by accident", and a runtime-settings object is the kind of
// input that can: it is handed to the agent CLI as configuration, and the CLI
// owns what its configuration may switch on. Restricting the field to the one
// posture that already acts without asking, and that already requires the
// `send` grant, means a session that still asks before acting cannot be given
// launch-time settings through this door at all. The refusal is a refusal, not
// a silent drop: a caller that sent settings and got a session without them
// would have a healthy-looking session that quietly lacks what it was created
// for — the failure this API refuses everywhere else it can.
//
// # Shape
//
// A JSON object, never null, an array or a scalar: the CLI's `--settings`
// takes an object, and a mis-shaped value should be refused here with a reason
// rather than surface as a session whose CLI silently ignored it. Contents are
// otherwise not read — which keys exist is the CLI's business, and a service
// that vetted them would be holding an opinion about the runtime (§1's
// non-goal).
//
// # Not for secrets
//
// The result is passed as an argv element, which every process table on the
// machine can read. That is why McpConfig takes paths and Env travels out of
// band. This field is for launch-time SWITCHES; a value that would be a
// credential does not belong in it, and the API documentation says so.
//
// An absent field (nil, empty, or the JSON literal null) returns ("", nil).
func ValidateLaunchSettings(permissionMode string, raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if permissionMode != PermissionModeBypass {
		return "", errors.New("settings is accepted only together with permissionMode \"bypass\": " +
			"launch-time settings are scoped to that posture so they cannot widen a session that still asks before acting")
	}
	if !json.Valid(trimmed) {
		return "", errors.New("settings is not valid JSON")
	}
	if trimmed[0] != '{' {
		return "", errors.New("settings must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", fmt.Errorf("settings is not valid JSON: %v", err)
	}
	if buf.Len() > MaxLaunchSettingsBytes {
		return "", fmt.Errorf("settings is %d bytes, over the %d-byte limit: it lands on a command line, "+
			"so it is for a few launch-time switches, not a configuration file", buf.Len(), MaxLaunchSettingsBytes)
	}
	return buf.String(), nil
}
