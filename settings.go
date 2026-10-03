package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// launchSettingsOutsideBypass is the allow-list of top-level keys a session
// that is NOT in bypass mode may carry in SessionSpec.Settings (muster #254).
// Every other key stays bypass-only, exactly as #247 left it.
//
// # Why an allow-list and not "any key, minus the ones that widen"
//
// The service deliberately holds no opinion about which of the CLI's settings
// widen a session (see the Shape note on ValidateLaunchSettings), so a
// deny-list would be a claim it cannot keep: the CLI adds a key, the key
// widens, and the deny-list silently admits it. An allow-list fails the other
// way — a new switch is refused until someone adds it here on purpose — which
// is the direction #247's guarantee ("cannot be widened by accident") needs.
//
// # Why crossSessionInbound is on it
//
// It is the switch this issue exists for: a default-mode session relaunched
// through `resume` must boot with the argv it originally had. The key controls
// whether the session ACCEPTS messages other sessions send it; it grants the
// session no authority to act without asking, so it does not move a session
// toward bypass. Adding a key here is a security decision and needs its own
// reason recorded next to it.
var launchSettingsOutsideBypass = map[string]bool{
	"crossSessionInbound": true,
}

// LaunchSettingsOutsideBypass returns the keys a non-bypass session may carry,
// sorted. A peer advertises this list on /v1/health so a relaying client can
// tell a peer that carries #254 from one that only carries #247, and so an
// operator can read the boundary without reading the source.
func LaunchSettingsOutsideBypass() []string {
	keys := make([]string, 0, len(launchSettingsOutsideBypass))
	for k := range launchSettingsOutsideBypass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// LaunchSettingsKeys returns the top-level keys of a settings object, sorted,
// or nil when raw is absent or not an object. It does not validate; callers
// that need a verdict use ValidateLaunchSettings.
func LaunchSettingsKeys(raw json.RawMessage) []string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &obj); err != nil || obj == nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	// Shape first, so a malformed value is told it is malformed rather than
	// told about a posture it may not have been going to be refused on.
	if !json.Valid(trimmed) {
		return "", errors.New("settings is not valid JSON")
	}
	if trimmed[0] != '{' {
		return "", errors.New("settings must be a JSON object")
	}
	if permissionMode != PermissionModeBypass {
		// muster #254: outside bypass only the allow-listed keys pass; the
		// refusal names the first offender so the caller knows what to drop.
		for _, k := range LaunchSettingsKeys(trimmed) {
			if !launchSettingsOutsideBypass[k] {
				return "", fmt.Errorf("settings key %q is accepted only together with permissionMode \"bypass\": "+
					"outside bypass the only launch-time settings keys are %s, so a session that still asks before acting cannot be widened by accident",
					k, strings.Join(LaunchSettingsOutsideBypass(), ", "))
			}
		}
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
