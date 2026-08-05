package fleet_test

import (
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// The predicate exists to raise a skew warning, so every case that cannot be
// trusted must answer false. A false "same" suppresses the one warning worth
// having; a false "different" costs a log line.
func TestBuildSameAsRefusesWhatItCannotVerify(t *testing.T) {
	known := fleet.Build{Known: true, Revision: "abc123"}
	other := fleet.Build{Known: true, Revision: "def456"}
	dirty := fleet.Build{Known: true, Revision: "abc123", Modified: true}
	unknown := fleet.Build{}

	if !known.SameAs(known) {
		t.Error("two clean builds at the same revision must compare equal")
	}
	if known.SameAs(other) {
		t.Error("different revisions must not compare equal")
	}
	if dirty.SameAs(dirty) {
		t.Error("a modified build has no identity and must not equal itself")
	}
	if known.SameAs(dirty) || dirty.SameAs(known) {
		t.Error("a modified build must not equal a clean one at the same revision")
	}
	if unknown.SameAs(unknown) {
		t.Error("unknown builds must not compare equal — absence is not a match (§5.7)")
	}
	if known.SameAs(unknown) || unknown.SameAs(known) {
		t.Error("an unknown build must not equal a known one")
	}
}

// An operator needs to know WHICH of the three causes produced a non-match:
// "different revisions" sends someone looking for a deploy that lagged, and
// saying that about an unverifiable comparison wastes exactly the diagnosis
// this type exists to save.
func TestBuildDifferenceNamesTheCause(t *testing.T) {
	known := fleet.Build{Known: true, Revision: "abc123"}
	other := fleet.Build{Known: true, Revision: "def456"}
	dirty := fleet.Build{Known: true, Revision: "abc123", Modified: true}
	unknown := fleet.Build{}

	if got := known.DifferenceFrom(known); got != "" {
		t.Errorf("identical clean builds should report no difference, got %q", got)
	}
	if got := known.DifferenceFrom(other); got != "different revisions" {
		t.Errorf("want a revision mismatch, got %q", got)
	}
	for _, tc := range []struct {
		name     string
		a, b     fleet.Build
		contains string
	}{
		{"theirs unstamped", known, unknown, "theirs is not stamped"},
		{"ours unstamped", unknown, known, "ours is not stamped"},
		{"neither stamped", unknown, unknown, "neither build is stamped"},
		{"theirs dirty", known, dirty, "theirs was built from uncommitted"},
		{"ours dirty", dirty, known, "ours was built from uncommitted"},
		{"both dirty", dirty, dirty, "both were built from uncommitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.DifferenceFrom(tc.b)
			if !strings.Contains(got, tc.contains) {
				t.Errorf("DifferenceFrom = %q, want it to mention %q", got, tc.contains)
			}
		})
	}
}

func TestBuildShort(t *testing.T) {
	cases := []struct {
		name string
		in   fleet.Build
		want string
	}{
		{"unknown", fleet.Build{}, "unknown"},
		{"short revision", fleet.Build{Known: true, Revision: "abc123"}, "abc123"},
		{"truncated", fleet.Build{Known: true, Revision: "0123456789abcdef0123"}, "0123456789ab"},
		{"dirty is visible", fleet.Build{Known: true, Revision: "abc123", Modified: true}, "abc123+dirty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Short(); got != tc.want {
				t.Errorf("Short() = %q, want %q", got, tc.want)
			}
		})
	}
}

// SelfBuild must never report Known with nothing behind it. Under `go test`
// the toolchain supplies no VCS stamp, so this is the unknown path — which is
// exactly the one that must not masquerade as knowledge.
func TestSelfBuildNeverClaimsAnEmptyRevision(t *testing.T) {
	b := fleet.SelfBuild()
	if b.Known && b.Revision == "" {
		t.Error("Known with an empty revision reports ignorance as a fact (§5.7)")
	}
	if b.Go == "" {
		t.Error("toolchain version should always be available")
	}
}
