package tmux

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// #180 M7: the C1 controls are dropped too. U+009B is the 8-bit form of CSI,
// so "\u009b201~" spells the paste-END sequence another way, and tmux passes
// it into a bracketed paste unchanged; U+009D and U+0090 open OSC and DCS.
func TestSanitiserDropsC1Controls(t *testing.T) {
	for in, want := range map[string]string{
		"A\u009b201~B":            "A201~B",
		"\u009dOSC payload\u009c": "OSC payload",
		"\u0090DCS\u009c":         "DCS",
		"keep\nnew\tlines":        "keep\nnew\tlines",
		"Việt ✓  nbsp":            "Việt ✓  nbsp",
	} {
		if got := sanitizeForBracketedPaste(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// #180 M7: whatever renders as nothing in front of "!" is skipped before the
// guard decides what the first visible character is.
func TestGuardSkipsInvisibleCharactersBeforeBang(t *testing.T) {
	for _, text := range []string{"\u0080!id", "\u009b!id", "\u200b!id", "\u2060!id", "\u00ad!id", "\ufeff!id", "\ufe0f!id"} {
		if _, refused := refuseAsRuntimeSyntax(text, false); !refused {
			t.Errorf("%q passed the guard", text)
		}
	}
	if _, refused := refuseAsRuntimeSyntax("say hi!", false); refused {
		t.Error("a message merely containing \"!\" was refused")
	}
}

// #180 L6: a leading "/" is a command to this runtime. Refused, unless it is
// one of the session-management commands consumers already send, or the
// caller relays a human.
func TestLeadingSlashIsRefusedUnlessAllowedOrHumanRelay(t *testing.T) {
	for _, tc := range []struct {
		text       string
		humanRelay bool
		refused    bool
	}{
		{"/clear", false, true},
		{"  /exit", false, true},
		{"\u200b/logout", false, true},
		{"/custom-command do things", false, true},
		{"/rename my-session", false, false},
		{"/rc", false, false},
		{"/remote-control", false, false},
		{"/clear", true, false},
		{"a message about /tmp", false, false},
	} {
		_, refused := refuseAsRuntimeSyntax(tc.text, tc.humanRelay)
		if refused != tc.refused {
			t.Errorf("%q (humanRelay=%v): refused = %v, want %v", tc.text, tc.humanRelay, refused, tc.refused)
		}
	}
}

// The slash guard sees the same sanitised bytes the paste would carry: a
// control byte the sanitiser drops cannot hide a leading "/".
func TestSlashGuardSeesSanitisedText(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"\x0b/clear", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "/clear") {
		t.Fatalf("outcome = %s (%s), want refused naming the command", got.Outcome, got.Reason)
	}
	if pastesIn(f.callsSnapshot()) != 0 {
		t.Fatal("a refused command was pasted")
	}
}
