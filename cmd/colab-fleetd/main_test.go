package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// The rule under test: after this function, the machine can always reach its
// own service. See withLoopback's comment for the incident — a tunnel-only
// bind makes the service unreachable from the host running the diagnostics,
// where it is indistinguishable from a wedged process.
func TestWithLoopback(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{{
		name: "a tunnel address gains loopback on the same port",
		in:   []string{"10.0.0.5:9999"},
		want: []string{"10.0.0.5:9999", "127.0.0.1:9999"},
	}, {
		name: "an explicit loopback bind is left alone",
		in:   []string{"127.0.0.1:9999"},
		want: []string{"127.0.0.1:9999"},
	}, {
		name: "localhost counts as loopback",
		in:   []string{"localhost:9999"},
		want: []string{"localhost:9999"},
	}, {
		name: "a wildcard already includes loopback",
		in:   []string{"0.0.0.0:9999"},
		want: []string{"0.0.0.0:9999"},
	}, {
		name: "loopback anywhere in the list satisfies the rule",
		in:   []string{"10.0.0.5:9999", "127.0.0.1:9999"},
		want: []string{"10.0.0.5:9999", "127.0.0.1:9999"},
	}, {
		name: "an ephemeral port has nothing to mirror",
		in:   []string{"10.0.0.5:0"},
		want: []string{"10.0.0.5:0"},
	}, {
		name: "the port comes from the first address that has a real one",
		in:   []string{"10.0.0.5:0", "10.0.0.6:9999"},
		want: []string{"10.0.0.5:0", "10.0.0.6:9999", "127.0.0.1:9999"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withLoopback(slices.Clone(tc.in))
			if !slices.Equal(got, tc.want) {
				t.Errorf("withLoopback(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a=1 , b=2 ,, ")
	want := []string{"a=1", "b=2"}
	if !slices.Equal(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
}

// colab-fleet #95: the startup doc comment on FLEET_CONFIG says that when a
// principal table is present, FLEET_TOKEN is ignored — but the fatal check
// used to run before the config file was even opened, so a fully-specified
// principal table could never satisfy it. requireToken is the extracted
// decision so this regression has a direct, os.Exit-free test: a token must
// still be required whenever no validated principal table is in hand, and
// only then.
func TestRequireToken(t *testing.T) {
	if !requireToken(nil) {
		t.Error("requireToken(nil) = false, want true — no config loaded means a token is the only auth left")
	}
	if requireToken(&fileConfig{}) {
		t.Error("requireToken(non-nil) = true, want false — a loaded config is never empty (loadConfig refuses that), so the token becomes optional")
	}
}

// colab-fleet #98: starting from a principal table alone left the peer
// credential empty, so this pins which states now proceed (a non-empty
// credential is handed to SetPeerCredential) and which still refuse
// (svc.SetPeerCredential("") — internal/drivers/remote.Driver.bearerFor and
// the peer's own principalFor both then have nothing to authenticate,
// exactly as before #98). A regression that made any REFUSE case below start
// presenting an unauthenticated peer subscription is precisely the bug #98's
// ruling required stay impossible.
func TestPeerCredentialFailsClosedWithoutASelfPrincipal(t *testing.T) {
	writeConfig := func(t *testing.T, body string) *fileConfig {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	const self = fleet.MachineId("machine-a")

	cases := []struct {
		name    string
		token   string
		cfgFile *fileConfig
		want    string
	}{
		{
			name:  "single-token mode — no config at all — REFUSE",
			token: "",
			want:  "",
		},
		{
			name:  "single-token mode with a token set — the token is the credential",
			token: "shared-secret",
			want:  "shared-secret",
		},
		{
			name:    "table-only, no self principal configured — REFUSE (the #98 gap, still refused for a deployment that hasn't opted in)",
			token:   "",
			cfgFile: writeConfig(t, `{"principals": [{"name": "op", "token": "op-tok"}]}`),
			want:    "",
		},
		{
			name:    "table-only, self principal configured — the #98 fix: its token becomes the credential",
			token:   "",
			cfgFile: writeConfig(t, `{"principals": [{"name": "op", "token": "op-tok"}, {"name": "system:machine-a", "token": "self-tok"}]}`),
			want:    "self-tok",
		},
		{
			name:    "table-only, a DIFFERENT machine's self principal is configured — REFUSE, this is not a wildcard",
			token:   "",
			cfgFile: writeConfig(t, `{"principals": [{"name": "system:machine-b", "token": "other-tok"}]}`),
			want:    "",
		},
		{
			name:    "config and token both set — FLEET_TOKEN still wins, unchanged by #98",
			token:   "shared-secret",
			cfgFile: writeConfig(t, `{"principals": [{"name": "system:machine-a", "token": "self-tok"}]}`),
			want:    "shared-secret",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := peerCredential(tc.token, tc.cfgFile, self)
			if got != tc.want {
				t.Errorf("peerCredential(%q, cfgFile, %q) = %q, want %q", tc.token, self, got, tc.want)
			}
		})
	}
}

// TestStartupAuthGateNeverStartsUnauthenticated composes loadConfig and
// requireToken exactly as main() does, and is the regression test the issue
// asked for: it must be impossible to reach a state where the daemon would
// start with neither a token nor a validated principal table — including
// when FLEET_CONFIG names a file that is unreadable, malformed, or carries
// an empty principal table. Each case says which guard is the one that
// refuses it, per the issue's request.
func TestStartupAuthGateNeverStartsUnauthenticated(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name       string
		configPath string // "" means FLEET_CONFIG unset
		token      string
		wantStart  bool
		guard      string
	}{
		{
			name:      "no config, no token — refused by requireToken",
			token:     "",
			wantStart: false,
			guard:     "requireToken(nil)",
		},
		{
			name:       "config path names a file that does not exist — refused by loadConfig's ReadFile",
			configPath: filepath.Join(t.TempDir(), "missing.json"),
			token:      "",
			wantStart:  false,
			guard:      "loadConfig (unreadable)",
		},
		{
			name:       "config file is present but malformed — refused by loadConfig's Decode",
			configPath: writeConfig(t, `{"principals": [{"name": "op", "token": "tok"}`),
			token:      "",
			wantStart:  false,
			guard:      "loadConfig (malformed JSON)",
		},
		{
			name:       "config file is present but names no principals — refused by loadConfig's empty-table check",
			configPath: writeConfig(t, `{"principals": []}`),
			token:      "",
			wantStart:  false,
			guard:      "loadConfig (empty principal table)",
		},
		{
			name:      "no config, token set — allowed by single-token mode",
			token:     "shared-secret",
			wantStart: true,
			guard:     "none — single-token mode",
		},
		{
			name:       "valid config, no token — allowed, principal table is authoritative",
			configPath: writeConfig(t, `{"principals": [{"name": "op", "token": "tok"}]}`),
			token:      "",
			wantStart:  true,
			guard:      "none — principal table is authoritative",
		},
		{
			name:       "valid config, token also set — allowed, both present",
			configPath: writeConfig(t, `{"principals": [{"name": "op", "token": "tok"}]}`),
			token:      "shared-secret",
			wantStart:  true,
			guard:      "none — both present",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfgFile *fileConfig
			var loadErr error
			if tc.configPath != "" {
				cfgFile, loadErr = loadConfig(tc.configPath)
			}

			// Mirror main()'s ordering exactly: a loadConfig error is
			// fatal on its own, before requireToken is ever consulted.
			started := loadErr == nil && !(requireToken(cfgFile) && tc.token == "")

			if started != tc.wantStart {
				t.Errorf("started = %v, want %v (guard: %s, loadErr: %v)", started, tc.wantStart, tc.guard, loadErr)
			}
		})
	}
}
