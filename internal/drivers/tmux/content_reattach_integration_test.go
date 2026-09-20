package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/driver"
)

// privateMux starts a multiplexer server of this test's own and puts two
// sessions on it: "keeper", which exists only so the server does not exit when
// the probe does and take every client down with it, and "probe", the session a
// test is allowed to kill.
//
// It is PRIVATE, never the machine's own, because these tests kill a session:
// nothing here may touch somebody's live work. Skips rather than fails when the
// machine cannot provide one — an integration test that cannot reach its
// substrate has measured nothing, and saying so is not the same as a red test.
//
// Returns the wrapper to invoke the server through and the probe's working
// directory, which is what a subscription filter can be scoped to.
func privateMux(t *testing.T) (wrapper, probeDir string) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	// Outside the temp dir: a unix socket path is capped at ~104 bytes, and
	// the per-test temp directory alone overruns it.
	socket := filepath.Join("/tmp", "fl-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })

	wrapper = filepath.Join(dir, "mux")
	script := "#!/bin/sh\nexec " + bin + " -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "keeper", "-c", "/", "sleep", "600").CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	probeDir = filepath.Join(dir, "probe")
	if err := os.Mkdir(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	startProbe(t, wrapper, probeDir)
	return wrapper, probeDir
}

// startProbe creates (or re-creates) the probe session under the same id.
func startProbe(t *testing.T, wrapper, probeDir string) {
	t.Helper()
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "probe", "-c", probeDir, "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("could not create the probe session: %v (%s)", err, out)
	}
}

// procStat returns a pid's process-table state, or "" once it is gone.
func procStat(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The real-substrate half of #172: a watched session killed and re-created
// under the same id, inside one coalesce window, gets a NEW live content client
// while the subscription stays open.
//
// The unit test proves the driver re-dials against a fake transport. This proves
// the thing the fake can only model: that the id really is reused by the
// substrate, that the first client really does die with its session, and that
// the replacement is a live process attached to the new incarnation. Only the
// pump knows the first client died — the engine's read finds the session present
// both times, so its diff has nothing to notice, which is the entire defect.
//
// The observation is the kernel's, not the driver's: two pids, one gone and one
// running, read with ps.
func TestLiveRecreatedSessionGetsANewContentClient(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	wrapper, probeDir := privateMux(t)

	d := New("testbox", WithBinary(wrapper))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := d.Subscribe(ctx, testCaller, driver.SubscribeFilter{CwdPrefix: probeDir})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := s.Next(context.Background()); err != nil {
				return
			}
		}
	}()
	defer func() { <-drained }()
	defer func() { _ = s.Close() }()

	es := s.(*eventStream)
	content := func() *realCtlConn {
		es.mu.Lock()
		defer es.mu.Unlock()
		c, _ := es.conns["probe"].(*realCtlConn)
		return c
	}
	first := content()
	if first == nil {
		t.Fatal("subscription opened no real content client for the probe session")
	}
	firstPid := first.cmd.Process.Pid

	// Kill and re-create at once, inside one coalesce window. A session that is
	// back before the next read is never observed to close, so the engine's diff
	// keeps it in `known` and never takes the branch that opens a content client
	// — the gap this closes.
	if out, err := exec.Command(wrapper, "kill-session", "-t", "probe").CombinedOutput(); err != nil {
		t.Fatalf("kill-session: %v (%s)", err, out)
	}
	startProbe(t, wrapper, probeDir)

	var second *realCtlConn
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c := content(); c != nil && c != first {
			second = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == nil {
		t.Fatalf("the probe session is back under the same id and its content client died with the "+
			"old one, but the subscription dialled no replacement (still holding %v); its state "+
			"changes have silently stopped being pushed for the life of the subscription",
			content() != nil)
	}
	secondPid := second.cmd.Process.Pid
	if secondPid == firstPid {
		t.Fatalf("replacement client has the same pid %d as the one that died", secondPid)
	}

	// The replacement is a live process, not a corpse in the map.
	if stat := procStat(secondPid); stat == "" || strings.Contains(stat, "Z") {
		t.Errorf("replacement content client pid %d is not running (stat %q)", secondPid, stat)
	}
	// And the one it replaced was reaped, not merely dropped (#170).
	for time.Now().Before(deadline) && procStat(firstPid) != "" {
		time.Sleep(20 * time.Millisecond)
	}
	if stat := procStat(firstPid); stat != "" {
		t.Errorf("the dead content client pid %d is still in the process table (stat %q)", firstPid, stat)
	}
	t.Logf("content client %d died with the session, %d attached to its replacement", firstPid, secondPid)

	// The subscription itself carried on throughout.
	es.mu.Lock()
	closed := es.closed
	es.mu.Unlock()
	if closed {
		t.Error("a session restarting closed the whole subscription")
	}
}
