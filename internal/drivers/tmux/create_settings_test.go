package tmux

import (
	"context"
	"encoding/json"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// muster #247: launch-time CLI settings reach the agent's argv as one
// `--settings <json>` element, for a bypass session only.

func TestSettingsReachTheAgentArgvForABypassSession(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "inbound", Cwd: "/work/x", PermissionMode: fleet.PermissionModeBypass,
		Settings: json.RawMessage(`{ "crossSessionInbound": "accept" }`),
	})
	got, ok := flagValue(agentArgv(argv), "--settings")
	if !ok {
		t.Fatalf("no --settings in the agent argv: %v", agentArgv(argv))
	}
	if got != `{"crossSessionInbound":"accept"}` {
		t.Errorf("--settings value = %q, want the compact JSON object", got)
	}
}

func TestBypassSessionWithoutSettingsEmitsNoSettingsFlag(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "plain", Cwd: "/work/x", PermissionMode: fleet.PermissionModeBypass,
	})
	if _, ok := flagValue(agentArgv(argv), "--settings"); ok {
		t.Errorf("--settings appeared with no settings requested: %v", agentArgv(argv))
	}
}

func TestSettingsOnANonBypassSessionAreRefusedBeforeTheSpawn(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-s", fleet.SessionSpec{
		Name: "asks", Cwd: "/work/x", Settings: json.RawMessage(`{"a":1}`),
	})
	if err == nil {
		t.Fatal("settings were accepted on a session that is not in bypass mode")
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must precede the spawn")
	}
}

func TestInvalidSettingsAreRefusedRatherThanStarted(t *testing.T) {
	for _, raw := range []string{`{"a":`, `["a"]`, `"x"`} {
		f := twoSessions()
		d := newTestDriver(f)
		_, err := d.Create(context.Background(), testCaller, "k-s", fleet.SessionSpec{
			Name: "bad", Cwd: "/work/x", PermissionMode: fleet.PermissionModeBypass,
			Settings: json.RawMessage(raw),
		})
		if err == nil {
			t.Errorf("settings %s were accepted", raw)
		}
		if newSessionArgv(f) != nil {
			t.Errorf("settings %s: the session was started anyway", raw)
		}
	}
}

// A non-bypass session is unchanged: no --settings, whatever else is set.
func TestNonBypassSessionArgvCarriesNoSettingsFlag(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "ordinary", Cwd: "/work/x"})
	if _, ok := flagValue(agentArgv(argv), "--settings"); ok {
		t.Errorf("--settings appeared on an ordinary session: %v", agentArgv(argv))
	}
}
