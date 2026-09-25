package tmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/driver"
)

// TestLiveConversationFollowsAProcessReplacedInTheSamePane is #202's own
// reproduction against a REAL multiplexer and the real `ps`: the process in a
// session's pane is replaced by `respawn-pane` — the same pane, the same
// session, the same creation time, a new pid — and the next listing must name
// the conversation of the process that is there now.
//
// The fake-multiplexer tests in conversation_replaced_process_test.go establish
// the rule; this one establishes that what the real multiplexer reports as
// pane_pid actually moves when a pane's process is replaced, which is the
// assumption the rule leans on and a fake can only state.
//
// Runs against a PRIVATE server (own socket), for the reason
// capture_chunk_integration_test.go gives.
func TestLiveConversationFollowsAProcessReplacedInTheSamePane(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}

	dir := t.TempDir()
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, sessions := t.TempDir(), t.TempDir()

	socket := filepath.Join("/tmp", "fl-replaced-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })
	wrapper := filepath.Join(t.TempDir(), "mux")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+bin+" -S "+socket+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "replaced-alpha", "-c", cwd, "sleep", "600").CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	panePid := func() int {
		t.Helper()
		out, err := exec.Command(wrapper, "list-panes", "-t", "replaced-alpha", "-F", "#{pane_pid}").Output()
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("pane_pid %q: %v", out, err)
		}
		return n
	}
	// The runtime's own per-process record for pid: its start time is the one
	// the real `ps` reports, written in UTC as the runtime writes it.
	writeProcessRecord := func(pid int, conv string) {
		t.Helper()
		out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatalf("ps for pid %d: %v", pid, err)
		}
		started, err := time.ParseInLocation(psStartTimeLayout, strings.TrimSpace(string(out)), time.Local)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": conv, "cwd": cwd,
			"procStart": started.UTC().Format(psStartTimeLayout),
		})
		if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conversation := func(d *Driver) (known bool, id string) {
		t.Helper()
		got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range got.Items() {
			if s.ID == "replaced-alpha" {
				if s.Conversation == nil {
					t.Fatal("no conversation field")
				}
				return s.Conversation.Known, s.Conversation.ID
			}
		}
		t.Fatal("session not listed")
		return false, ""
	}

	d := New("testbox", WithBinary(wrapper), WithRecordRoot(records), WithProcessSessionsRoot(sessions))

	first := panePid()
	writeProcessRecord(first, resumedConv)
	if known, id := conversation(d); !known || id != resumedConv {
		t.Fatalf("precondition: the first process's conversation, got known=%v id=%q", known, id)
	}

	// The relaunch. The old process's record is deliberately left where it is —
	// the runtime does not clean it up on a kill — so only the pid can say it
	// is not this pane's any more.
	if out, err := exec.Command(wrapper, "respawn-pane", "-k", "-t", "replaced-alpha", "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("respawn-pane: %v (%s)", err, out)
	}
	second := panePid()
	if second == first {
		t.Fatalf("respawn-pane left pane_pid at %d; the multiplexer does not report a replaced process as a new pid", first)
	}

	// Before the new process has written its record: unknown, never the old id.
	if known, id := conversation(d); known || id != "" {
		t.Fatalf("the relaunched process has no record yet; got known=%v id=%q", known, id)
	}

	writeProcessRecord(second, replacedConv)
	if known, id := conversation(d); !known || id != replacedConv {
		t.Fatalf("the relaunched process is in %s; got known=%v id=%q", replacedConv, known, id)
	}
}
