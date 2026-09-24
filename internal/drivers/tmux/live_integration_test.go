package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// Live tests for #180 against a PRIVATE multiplexer server (its own socket),
// never a real session. Gated like the other integration tests here:
// FLEET_TMUX_INTEGRATION=1. The panes run synthetic programs from testdata/,
// not the runtime.

type liveMux struct {
	t       *testing.T
	wrapper string
	env     []string
}

func newLiveMux(t *testing.T) *liveMux {
	t.Helper()
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the synthetic panes")
	}
	// Outside the temp dir: a unix socket path is capped near 104 bytes.
	socket := filepath.Join("/tmp", "fl180-"+randomNonce()[:10])
	wrapper := filepath.Join(t.TempDir(), "mux")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+bin+" -u -f /dev/null -S "+socket+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &liveMux{t: t, wrapper: wrapper, env: append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")}
	t.Cleanup(func() { m.run("kill-server"); _ = os.Remove(socket) })
	return m
}

func (m *liveMux) run(args ...string) string {
	c := exec.Command(m.wrapper, args...)
	c.Env = m.env
	out, _ := c.CombinedOutput()
	return string(out)
}

func testdataPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// #180 H2, live: a composer frame left behind by a program that exited, and an
// interactive shell under it. The message must not run as a command.
func TestLiveShellUnderLeftoverFrameIsNotExecuted(t *testing.T) {
	m := newLiveMux(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "shell-ran")
	paneCmd := "python3 " + testdataPath(t, "frame_then_exit.py") + "; exec env PS1='sh%% ' zsh -f -i"
	if _, err := exec.LookPath("zsh"); err != nil {
		paneCmd = "python3 " + testdataPath(t, "frame_then_exit.py") + "; exec env PS1='sh$ ' bash --norc -i"
	}
	m.run("new-session", "-d", "-x", "80", "-y", "24", "-s", "shellpane", "-c", dir, "sh", "-c", paneCmd)
	time.Sleep(1500 * time.Millisecond)

	d := New("livebox", WithBinary(m.wrapper))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "livebox", ID: "shellpane"},
		"touch "+marker, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the message ran as a shell command (receipt %s: %s)", got.Outcome, got.Reason)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s (%s), want refused", got.Outcome, got.Reason)
	}
}

// #180 H1, live: a synthetic composer that collapses long pastes and swallows
// the first Enter. The fresh send strands; the resume the receipt asks for
// finishes it, and the TUI records the whole text submitted exactly once.
func TestLiveCollapsedPasteStrandResumes(t *testing.T) {
	m := newLiveMux(t)
	logf := filepath.Join(t.TempDir(), "submits.log")
	m.run("new-session", "-d", "-x", "80", "-y", "30", "-s", "tuipane",
		"env FAKE_SWALLOW=1 FAKE_LOG="+logf+" python3 "+testdataPath(t, "faketui.py"))

	d := New("livebox", WithBinary(m.wrapper))
	ref := fleet.SessionRef{Machine: "livebox", ID: "tuipane"}
	waitForEmptyComposer(t, d, "tuipane")

	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("brief line %d: pick up the issue and report back", i)
	}
	text := strings.Join(lines, "\n")

	send := func(opts driver.SendOptions) fleet.DeliveryReceipt {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := d.Send(ctx, testCaller, ref, text, opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := send(driver.SendOptions{Submit: true})
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("first: %s (%s), want unknown — the synthetic TUI swallowed its Enter", first.Outcome, first.Reason)
	}
	resumed := send(driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume: %s (%s), want queued", resumed.Outcome, resumed.Reason)
	}
	time.Sleep(300 * time.Millisecond)
	logged, _ := os.ReadFile(logf)
	if n := strings.Count(string(logged), "\n"); n != 1 || !strings.Contains(string(logged), "brief line 11") {
		t.Fatalf("submitted turns = %q, want the whole text exactly once", logged)
	}
}

// #180 L2, live: a collapsed-paste marker in the history margin above the
// visible pane is never credited to a delivery that pasted nothing.
func TestLiveMarkerInHistoryMarginIsNotCredited(t *testing.T) {
	m := newLiveMux(t)
	script := `printf '\342\235\257 [Pasted text #10 +12 lines]\n'; for i in $(seq 1 15); do echo filler $i; done; sleep 60`
	m.run("new-session", "-d", "-x", "80", "-y", "10", "-s", "histpane", "sh", "-c", script)
	time.Sleep(500 * time.Millisecond)

	d := New("livebox", WithBinary(m.wrapper))
	sc, _ := d.captureForClassify(context.Background(), "histpane")
	before := composerMarkers(sc)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, landed := d.confirmLandedV2(ctx, "histpane", "a text that was never pasted", before, false, false); landed {
		t.Fatal("a residue marker in the history margin was credited to this delivery")
	}
}

func waitForEmptyComposer(t *testing.T, d *Driver, pane string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if sc, ok := d.captureForClassify(context.Background(), pane); ok {
			if txt, scan := composerText(sc); scan == composerFound && txt == "" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the synthetic composer never became ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
