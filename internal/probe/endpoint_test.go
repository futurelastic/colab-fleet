// Package probe holds standalone, self-contained experiments run for a
// spike (colab-fleet #118) and kept as reproducible evidence rather than
// prose alone. Nothing here talks to any real session or any real
// runtime-owned directory — see the package doc comment below for why.
package probe

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestNonOwnerProcessCanCreateEndpointInSharedDirectory answers the first
// half of #118: "can a non-session process create an addressable endpoint
// inside [the runtime's inbox] namespace at all, or is creation gated on
// being the runtime?"
//
// This does NOT touch the real namespace. That directory is a real,
// machine-local path belonging to another application's runtime, and this
// repo is PUBLIC and Tier 1 for privacy (CLAUDE.local.md) — a committed
// test that hardcodes its path or naming convention would both (a) be
// flaky, since it depends on exactly one machine's live process table, and
// (b) leak an implementation detail of a system this repo is not allowed
// to name. What's committed instead is the general mechanism, observed to
// be identical in shape during a one-time interactive check against the
// real directory (recorded in the issue, not here): a directory that is
// only writable by one OS user, containing one file per address, named
// after a numeric identifier that need not correspond to anything alive.
//
// Interactive finding this test generalizes (colab-fleet #118): binding a
// new unix-domain-socket file inside such a directory, under a name that
// does not correspond to any real running process, succeeded with zero
// privilege beyond ordinary same-user filesystem access — no companion
// registry, token or lock file was required or consulted. Creation is
// gated on filesystem permission alone, never on being "the runtime".
func TestNonOwnerProcessCanCreateEndpointInSharedDirectory(t *testing.T) {
	// Deliberately NOT t.TempDir(): on this OS, the per-test temp path it
	// hands back routinely exceeds the ~104-byte sun_path limit a
	// unix-domain-socket address is allowed, and bind fails with a generic
	// "invalid argument" that has nothing to do with the claim under test.
	// This is itself consistent with what the real namespace shows: it
	// lives at a short, fixed path rather than under a per-process/per-run
	// temp directory, for exactly this reason.
	dir, err := os.MkdirTemp("/tmp", "colab-fleet-probe-118-")
	if err != nil {
		t.Fatalf("could not create a short-path sandbox dir under /tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// A name shaped like the real convention (a numeric identifier) but
	// deliberately not backed by any process — the interesting case, since
	// it is the one a genuine runtime session could never produce itself.
	addr := filepath.Join(dir, "999999.sock")

	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("a same-user process could not create an endpoint in a "+
			"directory it does not own any entry in yet: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(addr)
	if err != nil {
		t.Fatalf("endpoint file vanished immediately after creation: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("created file is not a socket: mode=%v", info.Mode())
	}

	// No registry, token, or lock file exists beside it — the directory
	// holds exactly what was asked for and nothing else, matching what the
	// real namespace showed on inspection (only per-address files, no
	// companion state).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not list the directory after creating the endpoint: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the one endpoint file, found %d entries", len(entries))
	}
}
