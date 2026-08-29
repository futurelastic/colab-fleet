package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// TestLiveEnumerationSurvivesTheCommandLengthWall is colab-fleet#141's own
// reproduction, run against a REAL multiplexer server rather than a mock.
//
// # What #141 reported
//
// On one machine carrying 85 sessions, every single one of them classified
// as "driver failed to capture this pane's screen" — a total, machine-wide
// failure that cleared only once the session count dropped back under a
// wall nobody had measured yet. On the healthy peer (22 sessions) nothing
// was wrong at all.
//
// # What this test establishes, and why it does not hardcode the wall
//
// The wall's exact position (measured separately, off this repo, by
// bisection against tmux 3.7: ~995 args survives, ~1007 fails) is a
// property of the MULTIPLEXER BUILD running this test, not a documented
// contract — a different version or platform could move it. So rather than
// asserting a specific session count fails, this test:
//
//  1. Builds the exact UNCHUNKED single invocation the pre-#141 driver used
//     to build — every session's display-message+capture-pane pair chained
//     into one call — for a fleet sized comfortably past what any
//     reasonable single invocation should survive, and confirms THIS HOST,
//     right now, actually refuses it. If it does not, this host's wall sits
//     somewhere this test's fleet size does not reach, and the rest of the
//     test cannot demonstrate anything — it skips rather than passing
//     vacuously.
//  2. Runs the SAME fleet through the real driver (which chunks per
//     captureChunkMaxArgs/captureChunkMaxBytes) and asserts every session
//     comes back with a real classification — proving the chunking the
//     driver actually ships with survives a wall this host was just shown
//     to have.
//
// Runs against a PRIVATE, isolated server (own socket) precisely so it can
// create the ~200 throwaway sessions this needs without touching anything a
// human is using — the same reasoning sterile_integration_test.go documents
// for its own private server.
func TestLiveEnumerationSurvivesTheCommandLengthWall(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}

	dir := t.TempDir()
	// Socket lives outside the temp dir — see sterile_integration_test.go's
	// own comment: a unix socket path is capped at ~104 bytes and a
	// per-test temp dir overruns it.
	socket := filepath.Join("/tmp", "fl-capchunk-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })

	wrapper := filepath.Join(dir, "mux")
	script := "#!/bin/sh\nexec " + bin + " -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	boot := exec.Command(wrapper, "new-session", "-d", "-s", "capchunk-boot", "sleep", "600")
	if out, err := boot.CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	const n = 200 // comfortably past the ~84-session cliff measured off-repo
	for i := 0; i < n; i++ {
		name := "capchunk-" + strconv.Itoa(i)
		cmd := exec.Command(wrapper, "new-session", "-d", "-s", name, "sleep", "600")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("could not create probe session %d: %v (%s)", i, err, out)
		}
	}

	// Step 1: confirm THIS host actually refuses the old, unchunked shape
	// at this size — the same invocation enumerate() built before #141's
	// fix, constructed by hand against real pane ids.
	listOut, err := exec.Command(wrapper, "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	panes := strings.Fields(string(listOut))
	if len(panes) < n {
		t.Fatalf("only %d panes visible, expected at least %d", len(panes), n)
	}

	mark := "capchunkP"
	var unchunked []string
	for i, p := range panes {
		if i > 0 {
			unchunked = append(unchunked, ";")
		}
		unchunked = append(unchunked, "display-message", "-p", mark+strconv.Itoa(i), ";")
		unchunked = append(unchunked, classifyCaptureArgs(p, defaultCaptureLines)...)
	}
	wallOut, wallErr := exec.Command(wrapper, unchunked...).CombinedOutput()
	if wallErr == nil {
		t.Skipf("this host's multiplexer accepted an unchunked %d-pane invocation "+
			"(%d args) without complaint; the wall #141 hit sits above what this test "+
			"reaches here, so chunking cannot be demonstrated on this host — output: %q",
			n, len(unchunked), string(wallOut))
	}
	t.Logf("confirmed: this host's multiplexer refuses an unchunked %d-arg invocation (%v)",
		len(unchunked), wallErr)

	// Step 2: the real driver, over the SAME fleet, must not reproduce
	// #141 — every session should classify for real, not "unknown".
	d := New("capchunk-host", WithBinary(wrapper))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("List against the chunked driver failed: %v", err)
	}
	// +1 for the boot session, which List sees too.
	if len(got.Items()) != n+1 {
		t.Fatalf("want %d sessions (incl. the boot session), got %d", n+1, len(got.Items()))
	}

	// A bare "sleep 600" pane has no recognizable UI at all, so the
	// classifier may honestly call it StatusUnknown on CONTENT grounds —
	// that is not the failure this test guards against. What #141 actually
	// produced is a specific, distinguishing evidence string
	// (classify.go's classifyPaneRemembering/classifyPane: "driver failed
	// to capture this pane's screen") emitted ONLY when a capture is
	// altogether missing, never when a capture succeeded and merely didn't
	// match a known screen shape. Matching on that string is what tells the
	// two apart.
	const driverMalfunctionEvidence = "driver failed to capture this pane's screen"
	var malfunctioned []string
	for _, s := range got.Items() {
		if s.State.Status == fleet.StatusUnknown && strings.Contains(s.State.Evidence, driverMalfunctionEvidence) {
			malfunctioned = append(malfunctioned, s.ID)
		}
	}
	if len(malfunctioned) != 0 {
		t.Errorf("%d/%d sessions came back as a DRIVER MALFUNCTION (capture never happened) "+
			"on a host with a confirmed command-length wall — chunking did not protect this "+
			"fleet: %v", len(malfunctioned), len(got.Items()), malfunctioned)
	}
}
