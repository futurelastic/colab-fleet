package fleet

import "testing"

// A stamp is only reported when it reads as a describe of a release tag.
// Everything else is unstamped: a value a floor comparison cannot parse is
// worse than none, because it looks like an answer (colab-fleet #161).
func TestStampedVersion(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string // "" = unstamped
	}{
		{"", ""},
		{"   ", ""},
		{"v", ""},
		{"3ce7e27", ""},  // describe --always on a history with no tag
		{"(devel)", ""},  // what the toolchain itself says for a checkout
		{"version1", ""}, // v-prefixed but not a release
		{"v0.1.0", "v0.1.0"},
		{"v0.1.0-2-g3ce7e27", "v0.1.0-2-g3ce7e27"},
		{"v0.1.0-2-g3ce7e27-dirty", "v0.1.0-2-g3ce7e27-dirty"},
		{" v1.2.3\n", "v1.2.3"},
	} {
		got := stampedVersion(tc.raw)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("stampedVersion(%q) = %q, want unstamped", tc.raw, *got)
		case tc.want != "" && (got == nil || *got != tc.want):
			t.Errorf("stampedVersion(%q) = %v, want %q", tc.raw, got, tc.want)
		}
	}
}

// SelfBuild must carry the link-time stamp through to the wire type.
func TestSelfBuildReportsTheLinkTimeStamp(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = "v0.1.0-2-g3ce7e27"
	if b := SelfBuild(); b.Version == nil || *b.Version != "v0.1.0-2-g3ce7e27" {
		t.Errorf("SelfBuild().Version = %v, want the stamp", b.Version)
	}
	version = ""
	if b := SelfBuild(); b.Version != nil {
		t.Errorf("SelfBuild().Version = %q with no stamp, want nil", *b.Version)
	}
}
