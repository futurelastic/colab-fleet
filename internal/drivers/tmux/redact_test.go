package tmux

import "testing"

// #73: a real capture (#70) showed the control-channel label sharing the
// model/plan row with the project name, glued on by alignment padding
// rather than the " · " separator every other footer element uses. The
// prior rule — replace everything after the last " · " — could not tell
// the label apart from the name it shares the row with and discarded both.
// This pins the fix: only the name is sensitive, the label survives.
func TestRedactModelLineKeepsAControlChannelLabelSharingItsRow(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "active renders as a bare label with no trailing word",
			raw:  "  ▸ Opus 5 · src                                                    /rc",
			want: "  ▸ Opus 5 · <project> /rc",
		},
		{
			name: "a state carrying a trailing word survives the same way",
			raw:  "  ▸ Opus 5 · src                                              /rc failed",
			want: "  ▸ Opus 5 · <project> /rc failed",
		},
		{
			name: "no control channel on this row redacts exactly as before",
			raw:  "  ▸ Opus 5 · src",
			want: "  ▸ Opus 5 · <project>",
		},
		{
			name: "a name that merely ends in the same three letters is not mistaken for the label",
			raw:  "  ▸ Opus 5 · marc",
			want: "  ▸ Opus 5 · <project>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCapture(tc.raw)
			if got != tc.want {
				t.Errorf("RedactCapture(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The same fixed-point property TestCorpusIsFullyRedacted holds every
// committed screen to: redacting an already-redacted line must reproduce
// it exactly, or a corpus case built from this row would fail that gate
// the moment it landed.
func TestRedactModelLineWithControlLabelIsAFixedPoint(t *testing.T) {
	cases := []string{
		"  ▸ Opus 5 · src                                                    /rc",
		"  ▸ Opus 5 · src                                              /rc failed",
	}
	for _, raw := range cases {
		once := RedactCapture(raw)
		twice := RedactCapture(once)
		if once != twice {
			t.Errorf("not a fixed point for %q: first pass %q, second pass %q", raw, once, twice)
		}
	}
}
