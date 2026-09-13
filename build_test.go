package fleet_test

import (
	"encoding/json"
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

// A plain `go build` — and `go test`, which links the same way — carries no
// link-time stamp, so the release version must read as unstamped: explicitly
// null on the wire, never omitted and never a fabricated "v0.0.0" that a
// version floor would compare against on no evidence (colab-fleet #161).
func TestSelfBuildVersionIsNullWhenUnstamped(t *testing.T) {
	b := fleet.SelfBuild()
	if b.Version != nil {
		t.Fatalf("unstamped build reports version %q, want nil", *b.Version)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, present := wire["version"]
	if !present {
		t.Fatalf("version omitted from %s; an unstamped build must say null, not nothing", raw)
	}
	if v != nil {
		t.Errorf("version = %v on the wire, want null", v)
	}
}

// Version is not identity: two builds at one clean revision are the same
// code whatever their stamps say.
func TestBuildSameAsIgnoresVersion(t *testing.T) {
	v1 := "v0.1.0"
	a := fleet.Build{Known: true, Revision: "abc123", Version: &v1}
	b := fleet.Build{Known: true, Revision: "abc123"}
	if !a.SameAs(b) || !b.SameAs(a) {
		t.Error("a version stamp must not change whether two builds are the same code")
	}
}
