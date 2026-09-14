package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet#157: a machine whose multiplexer has no server has zero
// sessions, and says so completely. Every other listing failure is still a
// read that did not happen — §5.7's "a failed read is never an empty result"
// is not what this weakens.
func TestListNoServerIsCompleteAndEmpty(t *testing.T) {
	exitWith := func(stderr string) error {
		return &exec.ExitError{Stderr: []byte(stderr)}
	}
	cases := []struct {
		name   string
		err    error
		wantOK bool
	}{
		{"no socket file", exitWith("error connecting to /tmp/tmux-501/default (No such file or directory)\n"), true},
		{"stale socket file", exitWith("no server running on /tmp/tmux-501/default\n"), true},
		{"permission denied is not no-server", exitWith("error connecting to /tmp/tmux-501/default (Permission denied)\n"), false},
		{"path too long is not no-server", exitWith("error connecting to /a/very/long/path (File name too long)\n"), false},
		{"killed with no stderr", exitWith(""), false},
		{"the words outside a process exit do not count", errors.New("no server running on /tmp/tmux-501/default"), false},
		{"wrapped exit error still counts", fmt.Errorf("wrapper: %w", exitWith("no server running on /tmp/x\n")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			var calls int
			failing := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls++
				if len(args) > 0 && args[0] == "list-panes" {
					return nil, tc.err
				}
				return f.exec(ctx, name, args...)
			}
			d := New("testbox",
				withExec(failing),
				withNonce(func() string { return testNonce }),
				withClock(func() time.Time { return time.Unix(1785760000, 0) }),
			)

			got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
			if err != nil {
				t.Fatalf("a source outcome belongs in the envelope, not in err: %v", err)
			}
			if len(got.Sources()) != 1 {
				t.Fatalf("want exactly one source, got %+v", got.Sources())
			}
			src := got.Sources()[0]
			if len(got.Items()) != 0 {
				t.Errorf("want no sessions, got %d", len(got.Items()))
			}

			if tc.wantOK {
				if src.Status != fleet.SourceOK {
					t.Fatalf("a machine with no server answered fully; want ok, got %q (%s)", src.Status, src.Error)
				}
				if !got.Complete() {
					t.Error("an empty machine is a complete answer — Complete() must be true")
				}
				if src.Count == nil || *src.Count != 0 {
					t.Errorf("want count 0, got %v", src.Count)
				}
				if calls != 1 {
					t.Errorf("nothing to capture after an empty listing; want 1 subprocess, got %d", calls)
				}
				return
			}
			if src.Status != fleet.SourceUnreachable {
				t.Fatalf("want unreachable, got %q", src.Status)
			}
			if got.Complete() {
				t.Error("a failed read must not report complete")
			}
			if src.Error == "" {
				t.Error("an unreachable source must carry why (§9)")
			}
		})
	}
}

// The same property against the real binary, which is the only way to learn
// whether the signatures above are what the multiplexer actually prints. It
// needs no live server and touches no existing session: it points the
// multiplexer's socket directory at a fresh, empty one. Skipped where no
// multiplexer is installed.
func TestLiveListNoServerIsCompleteAndEmpty(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no multiplexer on PATH")
	}
	// A short fixed root, not t.TempDir(): a unix socket path has a length
	// cap (docs/gotchas.d/118), and a long TMPDIR would turn this into the
	// "File name too long" case instead of the one under test.
	root, err := os.MkdirTemp("/tmp", "cf157-")
	if err != nil {
		t.Skipf("cannot create a short socket root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	list := func(t *testing.T, socketRoot string) fleet.Collection[fleet.Session] {
		t.Helper()
		t.Setenv("TMUX", "") // never follow the socket of a session this test runs inside
		t.Setenv("TMUX_TMPDIR", socketRoot)
		got, err := New("local").List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got.Sources()) != 1 {
			t.Fatalf("want one source, got %+v", got.Sources())
		}
		return got
	}

	t.Run("no server", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
			t.Fatal(err)
		}
		got := list(t, filepath.Join(root, "empty"))
		if src := got.Sources()[0]; src.Status != fleet.SourceOK {
			t.Fatalf("want ok, got %q (%s)", src.Status, src.Error)
		}
		if !got.Complete() || len(got.Items()) != 0 {
			t.Fatalf("want a complete, empty answer; complete=%v items=%d", got.Complete(), len(got.Items()))
		}
	})

	t.Run("unreadable socket directory stays unreachable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		locked := filepath.Join(root, "locked")
		sockDir := filepath.Join(locked, fmt.Sprintf("tmux-%d", os.Getuid()))
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sockDir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sockDir, 0o700) })

		got := list(t, locked)
		if src := got.Sources()[0]; src.Status != fleet.SourceUnreachable {
			t.Fatalf("a socket the driver may not open is not an empty machine; want unreachable, got %q (%s)",
				src.Status, src.Error)
		}
		if got.Complete() {
			t.Error("must not report complete")
		}
	})
}
