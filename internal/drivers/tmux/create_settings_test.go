package tmux

import (
	"context"
	"encoding/json"
	"strings"
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

// muster #254: an allow-listed key rides on a session that is NOT in bypass
// mode, and carries no --dangerously-skip-permissions with it.
func TestAllowListedSettingsReachTheArgvOfANonBypassSession(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "inbound-default", Cwd: "/work/x",
		Settings: json.RawMessage(`{ "crossSessionInbound": "accept" }`),
	})
	agent := agentArgv(argv)
	got, ok := flagValue(agent, "--settings")
	if !ok || got != `{"crossSessionInbound":"accept"}` {
		t.Fatalf("--settings = %q (present=%v) in %v", got, ok, agent)
	}
	for _, a := range agent {
		if a == "--dangerously-skip-permissions" {
			t.Errorf("settings must not turn a default session into a bypass one: %v", agent)
		}
	}
}

// muster #254's acceptance: create + resume of a session originally booted with
// the flag reproduces its argv, so a relaunching client never has to fall back.
func TestResumeOfADefaultModeSessionKeepsItsSettings(t *testing.T) {
	const conv = "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "relaunched", Cwd: "/work/x", Resume: conv,
		Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`),
	})
	agent := agentArgv(argv)
	if got, ok := flagValue(agent, "--resume"); !ok || got != conv {
		t.Errorf("--resume = %q (present=%v)", got, ok)
	}
	if got, ok := flagValue(agent, "--settings"); !ok || got != `{"crossSessionInbound":"accept"}` {
		t.Errorf("--settings = %q (present=%v) in %v", got, ok, agent)
	}
}

// A key outside the boundary on a non-bypass session is refused naming the key,
// before the spawn — even when an allow-listed key sits beside it.
func TestUnlistedSettingsKeyOnANonBypassSessionNamesTheKey(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-s", fleet.SessionSpec{
		Name: "wide", Cwd: "/work/x",
		Settings: json.RawMessage(`{"crossSessionInbound":"accept","permissions":{"allow":["*"]}}`),
	})
	if err == nil || !strings.Contains(err.Error(), `"permissions"`) {
		t.Fatalf("err = %v, want a refusal naming \"permissions\"", err)
	}
	if newSessionArgv(f) != nil {
		t.Error("the session was started anyway; the refusal must precede the spawn")
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
