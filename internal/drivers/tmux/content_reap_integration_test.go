package tmux

import (
	"context"
	"os"
	"os/exec"
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
	// The private server, the keeper session and the probe are the same setup
	// every integration test in this package needs; it lives in one place so a
	// second test cannot drift from it. See privateMux.
	wrapper, probeDir := privateMux(t)

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
	startProbe(t, wrapper, probeDir)

	// A process that has exited and been waited on is gone from the table; one
	// that has exited and not been waited on reads as a zombie for as long as
	// nobody waits on it. Give the reap time to happen, then read the answer.
	stat := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stat = procStat(pid); stat == "" {
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
