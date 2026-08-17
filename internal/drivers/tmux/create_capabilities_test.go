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

// A supervisor's sessions carry three things this API could not express, and a
// session missing any of them starts, looks healthy, and fails later somewhere
// else. These tests are about the three, plus the flag-injection hole found
// while adding them.

// --- environment ------------------------------------------------------------

// The values must not be visible to every process on the machine.
//
// The obvious mechanism — the multiplexer's own `-e NAME=value` — puts them in
// an argv, which is §5.3's rule broken for the payload MOST likely to be a
// credential. This is the same assertion the prompt and context paths already
// carry, and it is the one that must never regress.
func TestEnvValuesNeverReachACommandLine(t *testing.T) {
	f := twoSessions()
	const secret = "s3cr3t-not-for-ps"
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "envy", Cwd: "/work/x",
		Env: map[string]string{"FLEET_TEST_TOKEN": secret},
	})
	_ = argv
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Fatalf("an env VALUE reached a command line: %v", c)
			}
		}
	}
}

// And the session must actually receive them. This runs the real wrapper
// through a real shell, the same way the record tests do — a staging file the
// session never applies would satisfy the test above and deliver nothing.
func TestStagedEnvIsAppliedToTheSessionAndThenUnlinked(t *testing.T) {
	sh := shellForTest(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")
	envFile := filepath.Join(dir, "env")

	// A value with spaces, quotes and shell metacharacters: applied verbatim,
	// never re-parsed. If any of it were evaluated, the value would differ or
	// the wrapper would run something.
	const tricky = `a b "c" $(touch ` + "`" + `pwned` + "`" + `) ; echo no`
	if err := os.WriteFile(envFile, []byte("FLEET_TEST_APPLIED="+tricky+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(sh, "-c", envRecordScript, "colab-fleet", record, envFile,
		"/bin/sh", "-c", `printf '%s' "$FLEET_TEST_APPLIED"`).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v (%s)", err, out)
	}
	if string(out) != tricky {
		t.Errorf("the session received %q, want %q — the value was re-parsed on its way in",
			out, tricky)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("the value was EXECUTED: a command substitution in an env value ran")
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Error("the staging file outlived its read; it may hold a credential")
	}
	// The record is written after the variables are applied, so it is evidence
	// about the environment the agent actually got.
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no record written: %v", err)
	}
	if !strings.Contains(string(raw), "FLEET_TEST_APPLIED") {
		t.Error("the record does not list the injected variable, so it describes an " +
			"environment the agent did not have")
	}
	if strings.Contains(string(raw), tricky) {
		t.Error("the record leaked the VALUE")
	}
}

// The staging format is line-oriented, so a newline in a value would arrive as
// a second variable — inventing a name out of value content, which is the exact
// fabrication the record script was already hardened against. Refuse, loudly.
func TestEnvThatTheFormatCannotCarryIsRefused(t *testing.T) {
	cases := map[string]map[string]string{
		"newline in value":           {"OK": "fine", "BAD": "one\nFAKE=injected"},
		"NUL in value":               {"BAD": "one\x00two"},
		"empty name":                 {"": "x"},
		"name with a dash":           {"NOT-A-NAME": "x"},
		"name starting with a digit": {"1ST": "x"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			_, err := d.Create(context.Background(), testCaller, "k-"+name,
				fleet.SessionSpec{Name: "e", Cwd: "/work/x", Env: env})
			if err == nil {
				t.Fatal("create accepted an env the staging format cannot carry faithfully")
			}
			if !strings.Contains(err.Error(), "env") {
				t.Errorf("the refusal does not say which input was wrong: %v", err)
			}
		})
	}
}

// A driver with no shell to apply the file in must refuse rather than start a
// session silently missing its identity.
func TestEnvIsRefusedWhenThereIsNoChannelForIt(t *testing.T) {
	f := twoSessions()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }), WithBareExec())
	_, err := d.Create(context.Background(), testCaller, "k-bare",
		fleet.SessionSpec{Name: "e", Cwd: "/work/x", Env: map[string]string{"A": "b"}})
	if err == nil {
		t.Fatal("a create carrying env succeeded on a driver that cannot deliver it")
	}
}

// --- resume, permission mode, and flag injection ----------------------------

func TestResumeAndPermissionModeReachTheAgentArgv(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "resumed", Cwd: "/work/x",
		Resume:         "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70",
		PermissionMode: fleet.PermissionModeBypass,
	})
	agent := agentArgv(argv)
	got, ok := flagValue(agent, "--resume")
	if !ok || got != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("--resume = %q (present=%v); a session that cannot continue a "+
			"conversation is a different kind of session", got, ok)
	}
	found := false
	for _, a := range agent {
		if a == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Error("permissionMode bypass did not reach the argv")
	}
}

// The hole found while adding the above: a pin is caller data and lands beside
// real flags, so a value beginning with "-" is read as a flag. A create grant
// would otherwise mean "run the agent with arguments of my choosing" — most
// pointedly, the very permission mode that is gated behind a second grant.
func TestAFlagShapedPinIsNotPassedThroughAsAFlag(t *testing.T) {
	f := twoSessions()
	_, argv := createWith(t, f, fleet.SessionSpec{
		Name: "sneaky", Cwd: "/work/x",
		Model:  "--dangerously-skip-permissions",
		Agent:  "-n",
		Effort: "--resume",
	})
	for _, a := range agentArgv(argv) {
		switch a {
		case "--dangerously-skip-permissions", "--resume":
			t.Fatalf("a pin was passed through as a flag: %v", agentArgv(argv))
		}
	}
}

func TestFlagShapedResumeIsRefusedRatherThanDropped(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/x", Resume: "--dangerously-skip-permissions"})
	if err == nil {
		t.Fatal("a resume id that is really a flag was accepted")
	}
}

func TestUnknownPermissionModeIsRefused(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-p",
		fleet.SessionSpec{Name: "p", Cwd: "/work/x", PermissionMode: "yolo"})
	if err == nil {
		t.Fatal("an unrecognised permission mode was accepted and handed to the CLI")
	}
}

// --- consents ---------------------------------------------------------------

// The wording this fixture uses is the one the CLASSIFIER recognises, and it is
// deliberately not the repo's captured `fixtureBypassPrompt`.
//
// Those two disagree, which is a finding filed separately: the captured screen's
// options are "No, exit" / "Yes, I accept", and classifyPromptKind needs
// "bypass" and "permissions" to appear in one option, so the captured screen
// classifies as NOTHING. Using it here would test that gap rather than this
// consent. What this file is entitled to assert is that a screen the classifier
// DOES recognise gets the accepting option chosen — the rest is #8's territory.
const fixtureBypassRecognised = `  By proceeding, you accept all responsibility for actions taken.
❯ 1. No, exit
  2. Yes, I accept the bypass permissions mode
Enter to confirm · Esc to cancel`

// The second consentable question. Same machinery as folder-trust, and the
// index matters more here: on this screen the HIGHLIGHTED option is "No, exit",
// so a consent that accepted the default would kill the session it was starting.
func TestBypassAcceptanceIsAnsweredByTheOptionThatAccepts(t *testing.T) {
	h, d := newSettleHarness(t, fixtureBypassRecognised, true)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{
			Cwd: "/work/alpha", Prompt: "start working",
			PermissionMode: fleet.PermissionModeBypass,
			Consents:       []fleet.PromptKind{fleet.PromptBypassAcceptance},
		})

	waitFor(t, "the work to be delivered after the acceptance screen", func() bool {
		return h.sawDelivery("start working")
	})
	got := h.keysPressed()
	if len(got) == 0 || got[0] != "2" {
		t.Errorf("keys pressed = %v, want option 2 first — option 1 on this screen is "+
			"\"No, exit\", which would have killed the session", got)
	}
}

// A consent is per kind: consenting to one question does not answer another.
func TestAConsentDoesNotSpendItselfOnADifferentQuestion(t *testing.T) {
	h, d := newSettleHarness(t, fixtureTrustPrompt, false)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{
			Cwd: "/work/alpha",
			// Consent to the OTHER boot question only.
			Consents: []fleet.PromptKind{fleet.PromptBypassAcceptance},
		})

	// Give the loop several polls at a folder-trust screen it was not told about.
	time.Sleep(3 * promptPollInterval)
	if got := h.keysPressed(); len(got) != 0 {
		t.Errorf("keys pressed = %v; a consent to one question answered another", got)
	}
}

// The resume chooser has no safe affirmative option — its choices are summaries
// of somebody's prior sessions, and nothing in the text identifies the one the
// caller named. Consenting to it must be refused at the boundary rather than
// answered with a guess.
func TestTheResumeChooserIsNotConsentable(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Create(context.Background(), testCaller, "k-rc",
		fleet.SessionSpec{
			Name: "rc", Cwd: "/work/x",
			Consents: []fleet.PromptKind{fleet.PromptResumeChooser},
		})
	if err == nil {
		t.Fatal("a consent was accepted for a question whose affirmative option " +
			"cannot be identified; answering it is a coin flip")
	}
}

// The older boolean still means what it meant. It shipped, so it keeps working.
func TestTrustCwdStillMeansTheFolderTrustConsent(t *testing.T) {
	spec := fleet.SessionSpec{TrustCwd: true}
	if !spec.ConsentsTo(fleet.PromptFolderTrust) {
		t.Error("trustCwd stopped meaning consent to the folder-trust question")
	}
	if spec.ConsentsTo(fleet.PromptBypassAcceptance) {
		t.Error("trustCwd leaked into consent for a different question")
	}
	both := fleet.SessionSpec{TrustCwd: true, Consents: []fleet.PromptKind{fleet.PromptFolderTrust}}
	if !both.ConsentsTo(fleet.PromptFolderTrust) {
		t.Error("saying the same thing twice was treated as a conflict")
	}
}
