package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// The decisive check for the login-shell wrap, and the reason the obvious
// end-to-end check is not decisive.
//
// # Why creating a session on a developer's machine proves nothing
//
// The multiplexer SERVER holds an environment of its own, and every session it
// creates inherits it. On a machine where a human started that server from a
// terminal, the server is already holding everything the startup files export
// — so a created session has the credentials whether or not this driver wraps
// anything, and a check that merely looks for them passes against the
// unwrapped code.
//
// Measured on a live machine while verifying this work: the server's global
// environment carried the agent's tool-server credentials directly. The
// end-to-end run therefore could not distinguish "the wrap delivered these"
// from "the server already had them", and would have reported a confident pass
// for a change that did nothing.
//
// # What the wrap is actually for
//
// It makes a session's environment independent of WHO STARTED THE SERVER.
// That distinction is the whole defect: when a human starts the server, it is
// rich; when the service starts it — first session after a reboot, or on a
// machine nobody has attached to — the server inherits the SERVICE's
// environment, and then every session for the lifetime of that server is
// credential-less. Same code, same machine, opposite outcome, decided by
// something that happened days earlier. That is exactly the shape of failure
// that gets diagnosed as an agent fault.
//
// So this test reproduces the failing condition rather than the convenient
// one: a private server started from a DELIBERATELY STERILE environment, which
// is what the service's own server looks like.
func TestCreatedSessionGetsStartupEnvironmentEvenOnASterileServer(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}

	dir := t.TempDir()
	// The socket lives OUTSIDE the temp dir: a unix socket path is capped at
	// ~104 bytes and the per-test temp directory is named after the test, which
	// on its own overruns it. Measured, not guessed — the first run failed with
	// "File name too long" and skipped, which would have looked like "no
	// multiplexer here" rather than a harness bug.
	socket := filepath.Join("/tmp", "fl-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })

	// A wrapper pinning every invocation to our own private server. The driver
	// takes a binary, not a socket, so this is how the test gets one without
	// widening the driver's configuration surface for a test's benefit.
	wrapper := filepath.Join(dir, "mux")
	script := "#!/bin/sh\nexec " + bin + " -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Start the server with a sterile environment — no credentials, minimal
	// PATH. This is the service's own situation, and it is the situation the
	// convenient end-to-end check never reproduces.
	sterile := []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin", "FLEET_STERILE_MARKER=1"}
	boot := exec.Command(wrapper, "new-session", "-d", "-s", "sterile-boot", "sleep", "600")
	boot.Env = sterile
	if out, err := boot.CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	// Confirm the premise before relying on it: the server really is sterile.
	env, err := exec.Command(wrapper, "show-environment", "-g").Output()
	if err != nil {
		t.Skipf("could not read the server environment: %v", err)
	}
	serverNames := map[string]bool{}
	for _, line := range strings.Split(string(env), "\n") {
		if name, _, ok := strings.Cut(line, "="); ok {
			serverNames[strings.TrimPrefix(name, "-")] = true
		}
	}
	if len(serverNames) > 12 {
		t.Skipf("the private server is not sterile (%d variables); this platform seeds it "+
			"from elsewhere and the test cannot isolate the wrap", len(serverNames))
	}

	// Create through the real driver, against the sterile server. The agent is
	// replaced with a sleep: what is under test is the ENVIRONMENT the wrapper
	// hands over, not any particular CLI.
	d := New("testbox",
		WithBinary(wrapper),
		WithCommandBuilder(func(fleet.SessionSpec, string) []string {
			return []string{"sleep", "600"}
		}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := d.Create(ctx, testCaller, "sterile-1", fleet.SessionSpec{
		Name: "sterile-probe", Cwd: fleet.AbsolutePath(dir),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wait for the record the wrapper writes on its way to exec.
	var rec fleet.SessionEnvironment
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ = d.Environment(ctx, testCaller, ref.SessionRef)
		if rec.Known {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !rec.Known {
		t.Fatalf("no environment record was captured: %s", rec.Reason)
	}

	// THE ASSERTION. The session must hold variables the sterile server could
	// not have supplied — which can only have come from the startup files the
	// wrap reads.
	var fromStartup []string
	for _, n := range rec.Names {
		if !serverNames[n] {
			fromStartup = append(fromStartup, n)
		}
	}
	if len(fromStartup) == 0 {
		t.Fatal("the session inherited nothing beyond the sterile server's own environment. " +
			"The login shell contributed NOTHING, which is the defect: an agent needing " +
			"credentials would start normally and fail at its first tool call.")
	}
	t.Logf("startup files contributed %d variables the sterile server did not have", len(fromStartup))

	if !rec.Interactive {
		t.Error("the record reports a non-interactive shell; the interactive startup file " +
			"is where credentials are exported")
	}
	if len(rec.Path) <= 2 {
		t.Errorf("session PATH has %d entries, barely more than the sterile /usr/bin:/bin — "+
			"the startup files did not contribute a path either", len(rec.Path))
	}
}
