package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// These tests run the compat harness against a real multiplexer with a
// synthetic runtime: no real agent, no network, no tokens. They are gated the
// same way the driver's other multiplexer tests are.

func compatIntegration(t *testing.T) (tmuxBin, python string) {
	t.Helper()
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	var err error
	if tmuxBin, err = exec.LookPath("tmux"); err != nil {
		t.Skip("no multiplexer on PATH")
	}
	if python, err = exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the synthetic runtime")
	}
	return tmuxBin, python
}

// fakeRuntime writes an executable stand-in for the agent: it answers
// --version and otherwise becomes the synthetic composer. Knobs are set INSIDE
// the launcher because the harness clears the environment — which is exactly
// what a knob passed the ordinary way would not survive.
func fakeRuntime(t *testing.T, python string, knobs map[string]string) string {
	t.Helper()
	tui, err := filepath.Abs(filepath.Join("testdata", "faketui.py"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$1\" in --version) echo \"0.0.0-synthetic (Synthetic Runtime)\"; exit 0 ;; esac\n")
	for k, v := range knobs {
		b.WriteString("export " + k + "=" + shQuote(v) + "\n")
	}
	b.WriteString("exec " + shQuote(python) + " " + shQuote(tui) + "\n")
	p := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// harnessFor builds a harness whose whole store lives in a temp directory, so
// a test can never touch the real runtime's files.
func harnessFor(t *testing.T, candidate string) *compatHarness {
	t.Helper()
	home := t.TempDir()
	getenv := env(
		"HOME", home,
		"FLEET_RECORD_ROOT", filepath.Join(home, "projects"),
		"FLEET_PROCESS_SESSIONS_ROOT", filepath.Join(home, "sessions"),
		"FLEET_TRUST_STATE_PATH", filepath.Join(home, "state.json"),
	)
	for _, d := range []string{"projects", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h := &compatHarness{getenv: getenv}
	h.cand.Resolved = candidate
	t.Cleanup(h.teardown) // never leave a server behind, whatever the test did
	return h
}

func cfcDirs(t *testing.T) map[string]bool {
	t.Helper()
	m, _ := filepath.Glob("/tmp/cfc-*")
	out := map[string]bool{}
	for _, p := range m {
		out[p] = true
	}
	return out
}

// waitFor polls until fn is true.
func cfcWaitFor(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// assertGone proves a torn-down world left nothing behind.
func assertGone(t *testing.T, tmuxBin string, w *compatWorld, pids []int) {
	t.Helper()
	if _, err := os.Stat(w.scratch); err == nil {
		t.Errorf("the scratch directory %s was left behind", w.scratch)
	}
	if out, err := exec.Command(tmuxBin, "-S", w.socket, "list-sessions").CombinedOutput(); err == nil {
		t.Errorf("the private server is still answering on its socket:\n%s", out)
	}
	if _, err := os.Stat(w.socket); err == nil {
		t.Errorf("the socket %s was left behind", w.socket)
	}
	for _, pid := range pids {
		cfcWaitFor(t, "pane process "+strconv.Itoa(pid)+" to end", 5*time.Second, func() bool {
			return syscall.Kill(pid, 0) == syscall.ESRCH
		})
	}
	for _, d := range []string{"projects", "sessions"} {
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(w.store.projects), d, "*cfc-"+w.nonce+"*"))
		if len(left) > 0 {
			t.Errorf("leftovers in the store's %s: %v", d, left)
		}
	}
}

func TestCompatWorldIsolationAndTeardown(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	// A canary in the caller's own environment: it must never reach the
	// candidate. And $TMUX set to a socket that does not exist: a stray command
	// with an implicit "current server" would fail loudly here.
	t.Setenv("CFC_CANARY", "leaked-from-the-caller")
	t.Setenv("TMUX", "/nonexistent/socket,1,0")
	before := cfcDirs(t)

	cand := fakeRuntime(t, python, nil)
	h := harnessFor(t, cand)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.world

	// The private server holds only the keeper, and its environment is the
	// minimal set: the two update locks, and nothing from the caller.
	if out, _ := w.tmux(ctx, "list-sessions", "-F", "#{session_name}"); strings.TrimSpace(out) != compatKeeper {
		t.Fatalf("sessions = %q, want only the keeper", out)
	}
	envOut, err := w.tmux(ctx, "show-environment", "-g")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DISABLE_UPDATES=1", "DISABLE_AUTOUPDATER=1", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"} {
		if !strings.Contains(envOut, want) {
			t.Errorf("the server's environment lacks %s:\n%s", want, envOut)
		}
	}
	for _, leak := range []string{"CFC_CANARY", "leaked-from-the-caller", "CLAUDE_", "TMUX="} {
		if strings.Contains(envOut, leak) {
			t.Errorf("the server's environment leaks %q:\n%s", leak, envOut)
		}
	}

	// A session is created through the driver's real Create, launched by the
	// resolved candidate, on the private server only.
	ref, err := w.create(ctx, "a", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfcWaitFor(t, "the synthetic composer", 15*time.Second, func() bool {
		out, _ := w.tmux(ctx, "capture-pane", "-p", "-t", ref.ID)
		return strings.Contains(out, "fake tui ready")
	})
	cmdline, _ := w.tmux(ctx, "list-panes", "-a", "-F", "#{session_name} #{pane_start_command}")
	var row string
	for _, l := range strings.Split(cmdline, "\n") {
		if strings.HasPrefix(l, ref.ID+" ") {
			row = l
		}
	}
	if !strings.Contains(row, cand) || !strings.Contains(row, "-n "+ref.ID) || !strings.Contains(row, "--model") {
		t.Errorf("the pane was not started as the candidate with its name and pins: %q", row)
	}
	if strings.Contains(row, "--remote-control") {
		t.Errorf("remote control must stay off: %q", row)
	}
	// The default server must not have heard of it.
	if out, _ := exec.Command(tmuxBin, "list-sessions", "-F", "#{session_name}").CombinedOutput(); strings.Contains(string(out), w.label) {
		t.Errorf("the session appeared on a server that is not ours:\n%s", out)
	}
	pids := w.paneProcesses(ctx)
	if len(pids) < 2 {
		t.Fatalf("expected the keeper's and the candidate's pane processes, got %v", pids)
	}

	h.teardown()
	if len(h.tearErrs) != 0 {
		t.Errorf("teardown reported problems: %v", h.tearErrs)
	}
	assertGone(t, tmuxBin, w, pids)
	for p := range cfcDirs(t) {
		if !before[p] {
			t.Errorf("a new scratch directory survived: %s", p)
		}
	}
}

// A run cancelled by a signal must still clean up: teardown runs on its own
// deadline, not the cancelled context.
func TestCompatTeardownSurvivesACancelledContext(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.world
	if _, err := w.create(ctx, "a", "a", nil); err != nil {
		t.Fatal(err)
	}
	pids := w.paneProcesses(context.Background())
	cancel() // the signal arrives
	h.teardown()
	if len(h.tearErrs) != 0 {
		t.Errorf("teardown reported problems: %v", h.tearErrs)
	}
	assertGone(t, tmuxBin, w, pids)
}

// The service's own sessions are untouched. A second private server stands in
// for it, with a session of its own and $TMUX pointing at it — the very trap
// an implicit "current server" would fall into — and its session ids and its
// pane must be identical after a whole world has been built and torn down.
func TestCompatNeverTouchesAnotherServer(t *testing.T) {
	tmuxBin, python := compatIntegration(t)
	// The socket lives under /tmp, not the per-test directory: a unix socket
	// path is capped near 104 bytes and the test's own directory overruns it.
	other := filepath.Join("/tmp", "svc-standin-"+randomNonce()[:8]+".sock")
	t.Cleanup(func() { _ = os.Remove(other) })
	otherMux := func(args ...string) (string, error) {
		out, err := exec.Command(tmuxBin, append([]string{"-S", other, "-f", "/dev/null"}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := otherMux("new-session", "-d", "-s", "service-own", "--", "sleep", "300"); err != nil {
		// Not a skip: integration is explicitly enabled, and a skip here would
		// hide the one assertion this test exists for.
		t.Fatalf("could not start a stand-in server: %v %s", err, out)
	}
	defer otherMux("kill-server")
	snapshot := func() string {
		ids, _ := otherMux("list-sessions", "-F", "#{session_id} #{session_name}")
		panes, _ := otherMux("list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_dead}")
		return ids + "|" + panes
	}
	before := snapshot()
	// Everything a stray command could latch onto points at the stand-in.
	t.Setenv("TMUX", other+",1,0")
	t.Setenv("TMUX_TMPDIR", filepath.Dir(other))

	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.world.create(ctx, "a", "a", nil); err != nil {
		t.Fatal(err)
	}
	h.teardown()

	if after := snapshot(); after != before {
		t.Errorf("the stand-in server changed while the harness ran:\n before: %s\n after:  %s", before, after)
	}

	// Positive control: the comparison above is only worth anything if it can
	// see a change. Touch the stand-in on purpose and confirm it does.
	if out, err := otherMux("new-session", "-d", "-s", "control", "--", "sleep", "300"); err != nil {
		t.Fatalf("control session: %v %s", err, out)
	}
	if snapshot() == before {
		t.Error("the snapshot did not notice a new session: this test could not have caught a leak")
	}
}

// A static-only run never starts a server or creates a scratch directory.
func TestCompatStaticOnlyRunTouchesNoMultiplexer(t *testing.T) {
	_, python := compatIntegration(t)
	before := cfcDirs(t)
	cand := fakeRuntime(t, python, nil)
	rep, err := RunCompat(context.Background(), CompatOptions{Claude: cand, Only: []string{"H-RC"}, Getenv: env("HOME", t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 1 {
		t.Fatalf("checks = %+v", rep.Checks)
	}
	for p := range cfcDirs(t) {
		if !before[p] {
			t.Errorf("a static-only run created %s", p)
		}
	}
}

// However a caller shapes the spec, a created session lands on the private
// server: the wrapper, not the spec, decides where a session goes.
func TestCompatCreatedSessionIsOnThePrivateServer(t *testing.T) {
	_, python := compatIntegration(t)
	h := harnessFor(t, fakeRuntime(t, python, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startWorld(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := h.world.create(ctx, "a", "a", func(s *fleet.SessionSpec) { s.Name = "someone-elses-name" })
	if err != nil {
		t.Fatal(err)
	}
	// Whatever the caller asked for, the created session is on OUR server.
	out, _ := h.world.tmux(ctx, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, ref.ID) {
		t.Errorf("session %q not on the private server: %s", ref.ID, out)
	}
}
