package tmux

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// What a created session must be, and what it must not be.
//
// The defect these tests pin: a session created through the API was not the
// same KIND of session an on-machine launcher produces, and nothing said so.
// It had no remote-control binding, so it could not be reached from a remote
// client; it had no shell, so it had none of the credentials the agent's tool
// servers need; and it took the requested name verbatim, because the naming
// rules lived in a client rather than here.
//
// All three failed the same way — the session started perfectly and the
// consequence appeared later, elsewhere, looking like an agent problem rather
// than a creation problem. So they are asserted at creation, on the argv, which
// is the last place the three are still one decision.

// newSessionArgv returns the argv handed to the multiplexer's new-session, or
// nil if none was.
func newSessionArgv(f *fakeMux) []string {
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == "new-session" {
			return c
		}
	}
	return nil
}

// agentArgv returns the part of a new-session call after the "--" separator —
// what actually gets executed.
func agentArgv(argv []string) []string {
	for i, a := range argv {
		if a == "--" {
			return argv[i+1:]
		}
	}
	return nil
}

// sessionNameOf returns the -s value from a new-session call.
func sessionNameOf(argv []string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-s" {
			return argv[i+1]
		}
	}
	return ""
}

// flagValue returns the value following a flag in argv.
func flagValue(argv []string, flag string) (string, bool) {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1], true
		}
	}
	return "", false
}

func createWith(t *testing.T, f *fakeMux, spec fleet.SessionSpec, opts ...Option) (*Driver, []string) {
	t.Helper()
	all := append([]Option{withExec(f.exec), withNonce(func() string { return testNonce })}, opts...)
	d := New("testbox", all...)
	if _, err := d.Create(context.Background(), testCaller, "key-1", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	argv := newSessionArgv(f)
	if argv == nil {
		t.Fatal("no new-session call was made")
	}
	return d, argv
}

// THE invariant the three gaps share.
//
// The launcher passes the identical string to the session name, the
// remote-control binding and the agent's own name, so the name matches
// everywhere from birth. A driver that resolved the name AFTER building the
// argv would bind remote control to a name the session does not have — and
// that failure is invisible until somebody tries to reach it from a phone.
//
// This is why naming and the flags could not be fixed separately: they are one
// decision, made once, at one point in Create.
func TestCreatedSessionCarriesOneNameEverywhere(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "review", Cwd: "/work/x"})

	name := sessionNameOf(argv)
	if name == "" {
		t.Fatal("new-session carried no -s name")
	}
	agent := agentArgv(argv)
	rc, ok := flagValue(agent, "--remote-control")
	if !ok {
		t.Fatal("no --remote-control in the agent argv; the session cannot be reached " +
			"from a remote client, which is most of the value of creating one remotely")
	}
	an, ok := flagValue(agent, "-n")
	if !ok {
		t.Fatal("no -n (agent name) in the agent argv")
	}
	if rc != name || an != name {
		t.Errorf("three names that must be one: session=%q remote-control=%q agent=%q",
			name, rc, an)
	}
}

// The requested name is not necessarily the resolved name, and the builder must
// see the RESOLVED one. A driver that sanitizes the session name but hands the
// builder spec.Name unchanged produces exactly the split this pins against.
func TestRemoteControlBindsTheResolvedNameNotTheRequestedOne(t *testing.T) {
	f := twoSessions()
	// A name the rules must rewrite: leading '-' (read as a flag downstream),
	// a ':' (the multiplexer's target separator) and a '.' (silently mangled).
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "-a:b.c", Cwd: "/work/x"})

	name := sessionNameOf(argv)
	if name == "-a:b.c" {
		t.Fatal("the requested name was used verbatim; the naming rules did not run")
	}
	if want := "ab-c"; name != want {
		t.Errorf("resolved name = %q, want %q", name, want)
	}
	rc, _ := flagValue(agentArgv(argv), "--remote-control")
	if rc != name {
		t.Errorf("remote control bound %q while the session is %q — a session bound to a "+
			"name it does not have is unreachable and looks healthy", rc, name)
	}
}

// A colliding name is numbered, not fatal. The launcher numbers; this used to
// be a hard failure, which is a different behaviour for the same request
// depending on which client made it.
func TestCollidingNameIsNumberedRatherThanRefused(t *testing.T) {
	f := twoSessions() // holds "alpha💬" and "beta"
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "beta", Cwd: "/work/x"})
	if got := sessionNameOf(argv); got != "beta-2" {
		t.Errorf("second session named %q, want %q", got, "beta-2")
	}
}

// The counter goes BEFORE the trailing marker, because the marker is what
// tooling keys on. A name whose marker has a number after it is no longer a
// marked name — and that would break exactly at the moment there are two
// sessions of the same type, which is the ordinary case.
func TestCollisionNumberGoesBeforeTheMarkerNotAfterIt(t *testing.T) {
	f := twoSessions() // holds "alpha💬"
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "alpha", Marker: "💬", Cwd: "/work/x"})
	got := sessionNameOf(argv)
	if got != "alpha-2💬" {
		t.Errorf("numbered name = %q, want %q", got, "alpha-2💬")
	}
	if strings.HasSuffix(got, "2") {
		t.Error("the counter landed after the marker; the session is no longer recognisable " +
			"as its type by anything reading a trailing marker")
	}
}

// Carry a marker, never stack it. Without the guard a name that already ends in
// one gains a second on every pass, and the doubled form is a DIFFERENT session
// name — so the next lookup misses and creates yet another session.
func TestMarkerIsCarriedNotStacked(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "release📋", Marker: "💬", Cwd: "/work/x"})
	if got := sessionNameOf(argv); got != "release📋" {
		t.Errorf("name = %q, want the existing marker kept and nothing appended", got)
	}
}

// A session created with no marker still gets one if the caller asks.
func TestMarkerIsAppliedWhenAsked(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "fresh", Marker: "💬", Cwd: "/work/x"})
	if got := sessionNameOf(argv); got != "fresh💬" {
		t.Errorf("name = %q, want %q", got, "fresh💬")
	}
}

// # The environment half
//
// The agent must not be exec'd directly by the multiplexer. With no shell there
// is no startup file, and with no startup file there are no credentials — so
// the agent starts perfectly and fails at its first tool call.
func TestAgentRunsUnderALoginShellNotDirectly(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "x", Cwd: "/work/x"},
		WithLoginShell("/bin/testsh"))

	agent := agentArgv(argv)
	if len(agent) == 0 {
		t.Fatal("nothing after the -- separator")
	}
	if agent[0] != "/bin/testsh" {
		t.Fatalf("the multiplexer runs %q directly; with no shell in front of it the agent "+
			"inherits the SERVICE's environment, which holds no agent credentials", agent[0])
	}
}

// The flag, not just the shell. `-lc` is a login shell that is NOT interactive,
// and a non-interactive shell does not read the interactive startup file — the
// file that on a normal machine exports the credentials. Measured on this
// runtime: `-lc` produced none of them and `-lic` produced all of them.
//
// So this asserts the measured form, and would fail a "tidy" change to `-lc`
// that looks more conventional and ships a session that starts fine and cannot
// call a tool.
func TestLoginShellIsInteractiveBecauseNonInteractiveMissesTheRcFile(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "x", Cwd: "/work/x"},
		WithLoginShell("/bin/testsh"))

	agent := agentArgv(argv)
	if len(agent) < 2 {
		t.Fatal("no shell flags at all")
	}
	flags := agent[1]
	if !strings.HasPrefix(flags, "-") {
		t.Fatalf("second argv element %q is not a flag bundle", flags)
	}
	for _, want := range []struct {
		ch     byte
		reason string
	}{
		{'l', "login: without it the login startup files are never read"},
		{'i', "interactive: without it the INTERACTIVE startup file is never read, which is " +
			"where credentials are exported — measured, 6 variables vs none"},
		{'c', "command: the agent argv has to actually run"},
	} {
		if !strings.ContainsRune(flags, rune(want.ch)) {
			t.Errorf("shell flags %q lack -%c (%s)", flags, want.ch, want.reason)
		}
	}
}

// The agent argv is bound as POSITIONAL PARAMETERS, never spliced into the
// shell script. A spliced argv makes every session name, working directory and
// model string a shell-injection surface, and quoting it correctly by hand is a
// bug waiting for the first name with a quote in it.
func TestAgentArgvIsBoundPositionallyNeverSplicedIntoTheScript(t *testing.T) {
	f := twoSessions()
	nasty := "/work/'; touch /tmp/pwned; '"
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "x", Cwd: fleet.AbsolutePath(nasty), Model: "'; rm -rf /; '",
	}, WithLoginShell("/bin/testsh"))

	agent := agentArgv(argv)
	// The script is one argv element. Nothing the caller supplied may appear
	// inside it — it must arrive as separate elements after it.
	script := ""
	for _, a := range agent {
		if strings.Contains(a, "exec") && strings.Contains(a, "$@") {
			script = a
		}
	}
	if script == "" {
		t.Fatal("no wrapper script element found")
	}
	if strings.Contains(script, "rm -rf") || strings.Contains(script, "pwned") {
		t.Fatalf("caller-supplied text was spliced into the shell script: %q", script)
	}
	// And the model still reached the agent, as its own element.
	if _, ok := flagValue(agent, "--model"); !ok {
		t.Error("the model never reached the agent argv")
	}
}

// Opting out is expressible, and is the only thing that turns it off. The zero
// value of a bool would silently mean "off", which is the second-class shape
// this work exists to stop being the default.
func TestRemoteControlIsOnUnlessExplicitlyRefused(t *testing.T) {
	off := false
	on := true
	for _, tc := range []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset means parity", nil, true},
		{"explicit true", &on, true},
		{"explicit false opts out", &off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			_, argv := createWith(t, f, fleet.SessionSpec{
				Name: "rc", Cwd: "/work/x", RemoteControl: tc.set,
			})
			_, got := flagValue(agentArgv(argv), "--remote-control")
			if got != tc.want {
				t.Errorf("--remote-control present = %v, want %v", got, tc.want)
			}
		})
	}
}

// The old direct-exec behaviour is still reachable, and is not the default.
func TestBareExecIsAvailableAndNotTheDefault(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{Name: "x", Cwd: "/work/x"}, WithBareExec())
	if got := agentArgv(argv); len(got) == 0 || got[0] != "claude" {
		t.Errorf("with bare exec the agent must be run directly, got %v", got)
	}
}
