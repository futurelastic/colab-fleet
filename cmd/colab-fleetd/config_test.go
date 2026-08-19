package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #47: trustRoots is a plain, optional field on the same file principals and
// peers already live in — this asserts it round-trips and that a config
// with no trustRoots at all (the common case, and every config that existed
// before #47) still loads exactly as it did.
func TestLoadConfigReadsTrustRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"principals": [{"name": "op", "token": "tok", "grants": ["read"]}],
		"trustRoots": ["/Users/someone/workspace", "/Users/someone/tools"]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Users/someone/workspace", "/Users/someone/tools"}
	if len(cfg.TrustRoots) != len(want) {
		t.Fatalf("TrustRoots = %v, want %v", cfg.TrustRoots, want)
	}
	for i, w := range want {
		if cfg.TrustRoots[i] != w {
			t.Errorf("TrustRoots[%d] = %q, want %q", i, cfg.TrustRoots[i], w)
		}
	}
}

func TestLoadConfigWithoutTrustRootsLeavesItEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"principals": [{"name": "op", "token": "tok"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustRoots) != 0 {
		t.Errorf("TrustRoots = %v, want empty", cfg.TrustRoots)
	}
}

func TestTrustSeedIntervalDefaultsAndValidates(t *testing.T) {
	t.Setenv("FLEET_TRUST_SEED_INTERVAL", "")
	if got := trustSeedInterval(); got != 2*time.Minute {
		t.Errorf("default = %s, want 2m", got)
	}

	t.Setenv("FLEET_TRUST_SEED_INTERVAL", "30s")
	if got := trustSeedInterval(); got != 30*time.Second {
		t.Errorf("30s = %s, want 30s", got)
	}

	// An invalid or non-positive value falls back to the default rather
	// than producing a zero or negative ticker interval, which
	// time.NewTicker panics on.
	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		t.Setenv("FLEET_TRUST_SEED_INTERVAL", bad)
		if got := trustSeedInterval(); got != 2*time.Minute {
			t.Errorf("bad value %q = %s, want the 2m default", bad, got)
		}
	}
}
