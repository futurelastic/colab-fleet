package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// The record must never contain a VALUE.
//
// The whole reason a created session goes through a login shell is that the
// shell exports credentials. A record of that environment which carried values
// would put those credentials into an API response, into whatever logs it, and
// into any client that caches one — which is a worse defect than the one being
// fixed.
//
// So this runs the real record script through a real shell, with a real
// secret-shaped variable exported, and asserts the value does not appear.
func TestEnvironmentRecordNeverContainsAValue(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")

	const secret = "s3cr3t-value-that-must-not-appear"
	cmd := exec.Command(sh, "-c", envRecordScript, "colab-fleet", record, "", "", "/bin/echo", "ran")
	cmd.Env = append(os.Environ(), "FLEET_TEST_TOKEN="+secret)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no record written: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, secret) {
		t.Fatal("the record contains a variable's VALUE; this would publish credentials " +
			"through an ordinary read")
	}
	if !strings.Contains(body, "FLEET_TEST_TOKEN") {
		t.Error("the record does not list the variable's NAME, which is the thing it is for: " +
			"a reader has to be able to see that a credential is present")
	}
	// Nothing after the PATH line may carry an '=' at all — that is the
	// structural guarantee, stronger than "this one secret is absent".
	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if i == 0 {
			continue // PATH, which is deliberately a value
		}
		if strings.Contains(line, "=") {
			t.Errorf("line %d %q carries an '='; only names belong here", i, line)
		}
	}
}

// A value containing a newline must not fabricate a variable name.
//
// The naive form of this extraction — `env | sed 's/=.*//'` — reads each line
// of a multi-line value as another variable, so a value containing the text
// "FAKE=injected" produces a variable named FAKE that does not exist. Verified
// by measurement before choosing the NUL-separated form; this pins the
// difference, because the naive form looks correct and is cheaper.
func TestEnvironmentRecordDoesNotInventNamesFromMultilineValues(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")

	cmd := exec.Command(sh, "-c", envRecordScript, "colab-fleet", record, "", "", "/bin/echo", "ran")
	cmd.Env = append(os.Environ(), "FLEET_TEST_MULTI=first\nPHANTOM=injected\nlast")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}

	_, names := parseEnvRecord(readFile(t, record))
	for _, n := range names {
		if n == "PHANTOM" {
			t.Fatal("a variable name was invented out of another variable's value; the record " +
				"is reporting an environment the session does not have")
		}
	}
	if !contains(names, "FLEET_TEST_MULTI") {
		t.Error("the real variable is missing from the record")
	}
}

// The wrapper must run the agent even when recording is impossible. A
// diagnostic that can cost a session is not worth having.
func TestSessionStartsEvenWhenTheRecordCannotBeWritten(t *testing.T) {
	sh := shellForTest(t)
	unwritable := filepath.Join(t.TempDir(), "no-such-dir", "rec")

	cmd := exec.Command(sh, "-c", envRecordScript, "colab-fleet", unwritable, "", "", "/bin/echo", "AGENT-RAN")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the agent did not run when the record was unwritable: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "AGENT-RAN") {
		t.Errorf("agent output = %q, want it to have run regardless", out)
	}
}

// #151: the pane must end up in the requested directory even when the process
// that starts it has a working directory that no longer exists.
//
// That is the exact state a long-running multiplexer server hands every new pane
// once its own directory is deleted — and `-c` did not rescue it. This rebuilds
// the state for real: an outer shell enters a directory, deletes it, and only
// then execs the wrapper, so the wrapper inherits a dead cwd exactly as a pane
// does. The control run without a cwd argument proves the state is reproduced
// rather than assumed; without it this test would pass on a machine where the
// reproduction silently failed.
func TestWrapperEntersTheRequestedDirectoryFromADeadInheritedOne(t *testing.T) {
	sh := shellForTest(t)
	base := t.TempDir()
	dead := filepath.Join(base, "dead")
	target := filepath.Join(base, "target dir with spaces")
	for _, d := range []string{dead, target} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}

	run := func(cwdArg string) (string, error) {
		// A fresh dead directory per run; the outer shell leaves before rmdir
		// could matter to anything but its own child.
		if err := os.MkdirAll(dead, 0o700); err != nil {
			t.Fatal(err)
		}
		outer := `cd -- "$1" && rmdir -- "$1" && shift && exec "$@"`
		cmd := exec.Command(sh, "-c", outer, "outer", dead,
			sh, "-c", envRecordScript, "colab-fleet", "", "", cwdArg,
			"/bin/pwd", "-P")
		// stdout only: a shell starting in a dead directory complains about it
		// on stderr (getcwd failures) — which is the reproduction showing
		// itself, not the agent's answer.
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return strings.TrimSpace(string(out) + stderr.String()), err
		}
		return strings.TrimSpace(string(out)), nil
	}

	if out, err := run(""); err == nil && out == want {
		t.Fatalf("control run landed in the target without being asked to (%q); the dead-cwd "+
			"state was not reproduced, so the real assertion below would prove nothing", out)
	}

	out, err := run(target)
	if err != nil {
		t.Fatalf("the wrapper did not run the agent from a dead inherited cwd: %v (%s)", err, out)
	}
	if out != want {
		t.Errorf("agent started in %q, want %q — the explicit cd did not take", out, want)
	}
}

// A directory that cannot be entered ends the pane rather than starting the
// agent somewhere the caller did not name.
func TestWrapperRefusesToRunTheAgentOutsideTheRequestedDirectory(t *testing.T) {
	sh := shellForTest(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	out, err := exec.Command(sh, "-c", envRecordScript, "colab-fleet", "", "", missing,
		"/bin/echo", "AGENT-RAN").CombinedOutput()
	if err == nil {
		t.Fatalf("the wrapper exited 0 for a directory that does not exist (%s)", out)
	}
	if strings.Contains(string(out), "AGENT-RAN") {
		t.Fatal("the agent ran even though its working directory could not be entered; it " +
			"would be working on something the caller never named")
	}
	if !strings.Contains(string(out), missing) {
		t.Errorf("the failure does not name the directory: %q", out)
	}
}

// parseEnvRecord's contract: first line is PATH, the rest are names.
func TestParseEnvRecordSplitsPathFromNames(t *testing.T) {
	path, names := parseEnvRecord("/usr/bin:/bin\nHOME\nPATH\nSHELL\n")
	if len(path) != 2 || path[0] != "/usr/bin" || path[1] != "/bin" {
		t.Errorf("path = %v", path)
	}
	if len(names) != 3 {
		t.Errorf("names = %v, want three", names)
	}
}

// AddedByShell is the field a reader actually acts on: empty means the login
// shell contributed nothing, which means the credentials are not there, which
// means the session will start fine and fail at its first tool call.
func TestAddedByShellNamesWhatTheShellContributed(t *testing.T) {
	env := fleetEnv(
		[]string{"HOME", "PATH", "AGENT_TOKEN"},
		[]string{"HOME", "PATH"},
	)
	added := env.AddedByShell()
	if len(added) != 1 || added[0] != "AGENT_TOKEN" {
		t.Errorf("added = %v, want exactly the variable the service did not have", added)
	}
}

// An unknown record must not report an empty difference as though it were a
// measured one — §5.7 applied to this type: "we did not look" and "the shell
// added nothing" are opposite answers.
func TestUnknownEnvironmentReportsNothingRatherThanAnEmptyDiff(t *testing.T) {
	var env = fleetEnv([]string{"A"}, []string{})
	env.Known = false
	if got := env.AddedByShell(); got != nil {
		t.Errorf("an unknown record answered %v; it must answer nothing at all", got)
	}
}

func shellForTest(t *testing.T) string {
	t.Helper()
	// /bin/sh, not the login shell: this exercises the SCRIPT, and doing so
	// through the tester's own configured shell would drag their startup files
	// into a unit test.
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this platform")
	}
	return "/bin/sh"
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no record written: %v", err)
	}
	return string(raw)
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func fleetEnv(session, service []string) fleet.SessionEnvironment {
	return fleet.SessionEnvironment{Known: true, Names: session, ServiceNames: service}
}
