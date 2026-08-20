package tmux

import (
	"context"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// #53: a message this runtime reads as a shell command to run directly is
// refused, not delivered — even into a session whose composer is empty and
// which would otherwise happily accept the text.
func TestSendRefusesBashModeSyntax(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "!rm -rf /", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("want refused, got %q (%s)", got.Outcome, got.Reason)
	}
	if got.Reason == "" {
		t.Error("a refusal must explain itself (§2.4)")
	}
	// The substrate must never see text that was always going to be
	// refused — this is a decision about the bytes, checked before the
	// session is even looked up.
	if calls := f.callsSnapshot(); len(calls) != 0 {
		t.Errorf("hazardous text reached the substrate: %v", calls)
	}
}

// #53's own measurement: every surveyed runtime trims a message before
// testing its first character, so a leading space does not "defuse" the
// pattern — it is read exactly as if the space were not there.
func TestSendRefusesBashModeDespiteLeadingWhitespace(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	for _, text := range []string{" !rm -rf /", "\t!rm -rf /", "\n!rm -rf /", "  \t !rm -rf /"} {
		got, err := d.Send(context.Background(), testCaller,
			fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, text, driver.SendOptions{Submit: true})
		if err != nil {
			t.Fatalf("%q: a refusal is a domain outcome, not an error: %v", text, err)
		}
		if got.Outcome != fleet.OutcomeRefused {
			t.Errorf("%q: leading whitespace must not defuse the pattern; want refused, got %q", text, got.Outcome)
		}
	}
}

// An exclamation mark that is not the runtime's own bash-mode prefix — one
// that does not lead the message — is ordinary text and must be delivered.
// A matcher that refused any "!" anywhere would refuse a caller for
// punctuation, which is not what #53 asks for.
func TestSendDeliversTextContainingButNotStartingWithBang(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello! how are you", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Errorf("ordinary text was refused: %s", got.Reason)
	}
}

// The pattern is checked ahead of every session-state gate: it refuses a
// session that does not even exist, exactly as it would refuse a live one,
// because the decision is about the text and not about the target.
func TestSendRefusesBashModeSyntaxEvenForAMissingSession(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "nope"}, "!ls", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("want refused, got %q", got.Outcome)
	}
	if calls := f.callsSnapshot(); len(calls) != 0 {
		t.Errorf("a text-level refusal must never reach the substrate to look the session up: %v", calls)
	}
}

// A bare "!" with nothing after it is still the runtime's own syntax, not a
// message with zero content worth relaying.
func TestSendRefusesBareBang(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, _ := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "!", driver.SendOptions{Submit: true})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("want refused, got %q", got.Outcome)
	}
}

// The matcher itself, independent of Send's plumbing: exercises the same
// leading-whitespace liberty the runtime takes, and confirms the seam does
// not overreach into text that merely contains the character.
func TestRefuseAsRuntimeSyntax(t *testing.T) {
	cases := []struct {
		text    string
		refused bool
	}{
		{"!ls -la", true},
		{"!", true},
		{" !ls -la", true},
		{"\t\t!ls -la", true},
		{"\n !ls -la", true},
		{"", false},
		{"ls -la", false},
		{"hello!", false},
		{"a ! b", false},
		{"   ", false},
	}
	for _, c := range cases {
		reason, refused := refuseAsRuntimeSyntax(c.text)
		if refused != c.refused {
			t.Errorf("refuseAsRuntimeSyntax(%q) refused = %v, want %v (reason %q)", c.text, refused, c.refused, reason)
		}
		if refused && reason == "" {
			t.Errorf("refuseAsRuntimeSyntax(%q) refused with no reason", c.text)
		}
	}
}
