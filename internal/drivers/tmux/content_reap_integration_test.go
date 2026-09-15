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

// The real-substrate half of reaping a content client whose session exited.
//
// The unit test proves the driver calls Close. This proves that calling it is
// what stands between a session exit and a zombie: the real transport's
// client is a child process of this one, it exits when its session does, and
// only Close waits on it. The observation is the kernel's, not the driver's —
// the child's process table entry, read with ps.
//
// It runs against a PRIVATE server, never the machine's own, because it kills
// a session: nothing here may touch somebody's live work.
func TestLiveContentClientOfARecreatedSessionIsNotLeftAZombie(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
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

	wrapper := filepath.Join(dir, "mux")
	script := "#!/bin/sh\nexec " + bin + " -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two sessions: one the probe, one that outlives it so the server does
	// not exit with the probe and take every client down with it.
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "keeper", "-c", "/", "sleep", "600").CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })
	probeDir := filepath.Join(dir, "probe")
	if err := os.Mkdir(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "probe", "-c", probeDir, "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("could not create the probe session: %v (%s)", err, out)
	}

	d := New("testbox", WithBinary(wrapper))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	defer func() { _ = s.Close(); <-drained }()

	es := s.(*eventStream)
	es.mu.Lock()
	conn, ok := es.conns["probe"].(*realCtlConn)
	es.mu.Unlock()
	if !ok {
		t.Fatal("subscription opened no real content client for the probe session")
	}
	pid := conn.cmd.Process.Pid

	// Kill the probe and re-create it under the same name at once, inside one
	// coalesce window. That is the case that leaked: a plain exit is seen by
	// the engine's next read, which detaches — and reaps — the client, so a
	// test that only killed the session passed against the unfixed code
	// (measured). A session that is back by the time the read happens is
	// never seen to close, and only the pump itself knows its client died.
	if out, err := exec.Command(wrapper, "kill-session", "-t", "probe").CombinedOutput(); err != nil {
		t.Fatalf("kill-session: %v (%s)", err, out)
	}
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "probe", "-c", probeDir, "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("could not re-create the probe session: %v (%s)", err, out)
	}

	// A process that has exited and been waited on is gone from the table; one
	// that has exited and not been waited on reads as a zombie for as long as
	// nobody waits on it. Give the reap time to happen, then read the answer.
	stat := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "stat=", "-p", itoa(pid)).Output()
		stat = strings.TrimSpace(string(out))
		if err != nil || stat == "" {
			stat = ""
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stat != "" {
		t.Fatalf("content client pid %d still in the process table (stat %q) 5s after its session exited, "+
			"with the subscription open", pid, stat)
	}
	t.Logf("content client pid %d reaped after its session exited", pid)
}
