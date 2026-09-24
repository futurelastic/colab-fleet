package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// ttyPS answers `ps -o pid=,pgid=,tpgid=,comm= -t <tty>` with fixed output,
// and fails anything else.
func ttyPS(out string) execFunc {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "-t" {
				return []byte(out), nil
			}
		}
		return nil, errors.New("ttyPS: not a tty query")
	}
}

func pastesIn(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if c[0] == "load-buffer" {
			n++
		}
	}
	return n
}

// #180 H2: a shell in the pane's foreground — under a composer frame the
// runtime left behind — is refused before anything is pasted, whatever the
// screen shows.
func TestSendRefusesWhenTheForegroundProcessIsAShell(t *testing.T) {
	for _, cmd := range []string{"zsh", "-zsh", "/bin/bash", "fish"} {
		t.Run(cmd, func(t *testing.T) {
			f := twoSessions()
			f.currentCommand = map[string]string{"%1": cmd}
			d := newTestDriver(f)
			got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
				"touch a-file", driver.SendOptions{Submit: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "shell") {
				t.Fatalf("outcome = %s (%s), want refused naming the shell", got.Outcome, got.Reason)
			}
			if pastesIn(f.callsSnapshot()) != 0 || submitsIn(f.callsSnapshot()) != 0 {
				t.Fatal("something was pasted or submitted into a shell")
			}
		})
	}
}

// The multiplexer's name for the foreground can lag; ps's foreground process
// group is checked too. A shell leading it refuses.
func TestSendRefusesWhenAShellLeadsTheForegroundGroup(t *testing.T) {
	f := twoSessions()
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		withPSExec(ttyPS("  100   100   500 claude\n  500   500   500 zsh\n")))
	got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"touch a-file", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused || pastesIn(f.callsSnapshot()) != 0 {
		t.Fatalf("outcome = %s (%s), want refused with nothing pasted", got.Outcome, got.Reason)
	}
}

// The runtime exits between the paste and the submit: the last look before
// Enter sees a shell in the foreground and does not press it.
func TestSendDoesNotPressEnterWhenTheRuntimeExitedMidSend(t *testing.T) {
	f := twoSessions()
	wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "load-buffer" {
			f.mu.Lock()
			f.currentCommand = map[string]string{"%1": "zsh"}
			f.mu.Unlock()
		}
		return out, err
	}
	d := New("testbox", withExec(wrapped), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		"hello there", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if submitsIn(f.callsSnapshot()) != 0 {
		t.Fatalf("Enter was pressed with a shell in the foreground (%s: %s)", got.Outcome, got.Reason)
	}
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "foreground") {
		t.Fatalf("outcome = %s (%s), want unknown naming the foreground", got.Outcome, got.Reason)
	}
}

// With the runtime's per-process records configured, a foreground process
// whose record matches the session's working directory is positive identity.
func TestForegroundRecordIsPositiveIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "100.json"),
		[]byte(`{"pid":100,"sessionId":"s","cwd":"/work/alpha","procStart":"Mon Jan  2 15:04:05 2026"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := twoSessions()
	d := New("testbox", withExec(f.exec), withPSExec(ttyPS("  100   100   100 claude\n")), WithProcessSessionsRoot(root))
	ok, why := d.foregroundIsRuntime(context.Background(), &paneRow{paneID: "%1", cwd: "/work/alpha"})
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	if n := d.counters.Snapshot()[counterForegroundVerified]; n != 1 {
		t.Fatalf("foreground.verified = %d, want 1", n)
	}
}

func TestParseForegroundGroup(t *testing.T) {
	got := parseForegroundGroup("  100   100   100 claude\n  101   100   100 node helper\n  200   200   100 zsh\nnonsense\n")
	if len(got) != 2 || got[0].pid != 100 || !got[0].leader || got[1].comm != "node helper" || got[1].leader {
		t.Fatalf("got %+v", got)
	}
}
