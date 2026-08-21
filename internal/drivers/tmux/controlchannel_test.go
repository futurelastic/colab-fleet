package tmux

import (
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// A pane with the footer the runtime actually renders. The label shares the
// footer row, right-aligned, which is why the matcher looks for it anywhere on
// a footer line rather than at a column.
func paneWithFooter(footer string) string {
	return "  transcript line\n" +
		"✻ Brewed for 1m 0s\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)" + footer
}

func TestControlChannelReadsEachOfTheRuntimesFourLabels(t *testing.T) {
	cases := []struct {
		footer string
		want   fleet.ControlChannelState
	}{
		{"                    /rc active", fleet.ControlChannelActive},
		{"                    /rc failed", fleet.ControlChannelFailed},
		{"              /rc reconnecting", fleet.ControlChannelReconnecting},
		{"               /rc connecting…", fleet.ControlChannelConnecting},
	}
	for _, tc := range cases {
		got := controlChannelOf(newScreen(paneWithFooter(tc.footer)))
		if got == nil {
			t.Errorf("footer %q reported no channel at all", tc.footer)
			continue
		}
		if got.State != tc.want {
			t.Errorf("footer %q -> %q, want %q", tc.footer, got.State, tc.want)
		}
	}
}

// THE test. The measured incident: a supervisor grepping panes for the
// disconnection notice classified itself as disconnected, because its own tool
// output contained the strings it was searching for.
//
// This pane is a healthy session whose TRANSCRIPT is full of the exact tokens —
// which is what any session doing recovery work looks like — and its footer says
// the channel is fine. The transcript must not be able to overrule the chrome.
func TestATranscriptFullOfTheseStringsCannotForgeAChannelState(t *testing.T) {
	contaminated := "  Searching panes for disconnected bridges…\n" +
		"⏺ found: /rc failed\n" +
		"  Remote Control disconnected — this session was ended or archived\n" +
		"  from another device or app (code 4090)\n" +
		"  — run /remote-control to reconnect\n" +
		"✻ Brewed for 1m 0s\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		"  ⏵⏵ auto mode on                    /rc active"

	got := controlChannelOf(newScreen(contaminated))
	if got == nil {
		t.Fatal("the footer said active and was ignored")
	}
	if got.State != fleet.ControlChannelActive {
		t.Errorf("state = %q; the transcript forged it. Only the chrome below the "+
			"composer fence may decide this — that is the entire safety property",
			got.State)
	}
}

// The same shape with nothing in the footer: a transcript that talks about
// disconnection must produce no claim at all, not a disconnection.
func TestTranscriptAloneProducesNoClaim(t *testing.T) {
	s := newScreen("⏺ the fleet report says /rc failed on 37 sessions\n" +
		"✻ Brewed for 1m 0s\n" +
		rule + "\n" +
		"❯\n" +
		rule + "\n" +
		"  ⏵⏵ auto mode on")
	if got := controlChannelOf(s); got != nil {
		t.Errorf("state = %+v; a transcript mention is not a status label", *got)
	}
}

// A session started without remote control renders no label. Reporting that as
// connected would invent a channel out of a configuration; reporting it as
// failed would invent a fault (§5.7).
func TestNoLabelIsNilAndNotAVerdict(t *testing.T) {
	if got := controlChannelOf(newScreen(paneWithFooter(""))); got != nil {
		t.Errorf("state = %+v, want nil for a session with no control channel", *got)
	}
}

// A full-screen dialog takes the composer away, so this driver cannot locate
// the footer at all. It must say nothing rather than read a label out of a
// layout it does not recognise.
func TestNoComposerMeansNoFooterToRead(t *testing.T) {
	s := newScreen("  Remote Control\n\n    ▸ Disconnect\n      Cancel\n\n  /rc active")
	if got := controlChannelOf(s); got != nil {
		t.Errorf("state = %+v; with no composer fence there is no footer region "+
			"this driver can identify", *got)
	}
}

// A fifth label must produce nothing, not the nearest of the four. Guessing
// which one a new word resembles is how a wrong claim gets made confidently.
func TestAnUnknownLabelIsNotRoundedToTheNearestKnownOne(t *testing.T) {
	if got := controlChannelOf(newScreen(paneWithFooter("   /rc suspended"))); got != nil {
		t.Errorf("state = %+v, want nil for a label this vocabulary does not contain", *got)
	}
}

// The label reaches SessionState through every classification path, not only
// the one that happened to be wired. A branch that forgot it would report a
// dead channel as a healthy one, which is indistinguishable from the bug this
// field exists to end.
func TestEveryClassificationPathCarriesTheChannel(t *testing.T) {
	cases := map[string]string{
		"idle": paneWithFooter("   /rc failed"),
		"working": "  transcript\n✻ Thinking… (5s · ↓ 1.2k tokens)\n" +
			rule + "\n❯\n" + rule + "\n  ⏵⏵ auto mode on   /rc failed",
		"unsent": "  transcript\n✻ Brewed for 1m 0s\n" +
			rule + "\n❯ half a thought\n" + rule + "\n  ⏵⏵ auto mode on   /rc failed",
	}
	for name, raw := range cases {
		st := classify(raw, true)
		if st.ControlChannel == nil {
			t.Errorf("%s: classified as %q and reported no control channel", name, st.Status)
			continue
		}
		if st.ControlChannel.State != fleet.ControlChannelFailed {
			t.Errorf("%s: channel = %q, want failed", name, st.ControlChannel.State)
		}
	}
}

// A dead pane has no runtime left to describe itself. Reporting a channel state
// for one would be quoting a process that is gone.
func TestADeadPaneReportsNoChannel(t *testing.T) {
	st := classify(paneWithFooter("   /rc active"), false)
	if st.Status != fleet.StatusDead {
		t.Fatalf("status = %q, want dead", st.Status)
	}
	if st.ControlChannel != nil {
		t.Errorf("channel = %+v; a pane whose process is gone reports nothing about itself",
			*st.ControlChannel)
	}
}

// The channel is orthogonal to what the session is doing and must never rewrite
// it — a session nothing outside can reach is still running and still able to
// work. Folding one into the other is the precedence mistake #10 already named.
func TestAFailedChannelDoesNotChangeTheStatus(t *testing.T) {
	withChannel := classify(paneWithFooter("   /rc failed"), true)
	without := classify(paneWithFooter(""), true)
	if withChannel.Status != without.Status {
		t.Errorf("status = %q with a failed channel and %q without; the channel must "+
			"not move the status", withChannel.Status, without.Status)
	}
}
