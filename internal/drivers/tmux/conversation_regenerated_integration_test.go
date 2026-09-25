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

// TestLiveConversationFollowsANewConversationStartedInTheSameProcess is #203's
// own reproduction against a REAL multiplexer and the real `ps`.
//
// The process in the pane is never touched: same pane, same pid, same start
// time. Only the runtime's per-process record is rewritten in place, as `/clear`
// rewrites it — and the next listing must name the new conversation.
//
// What a fake cannot state, and this establishes: the start time the memo holds
// comes from the real `ps` (local time), the record's comes from its own text
// (UTC), and the check compares the two instants. A zone mistake there would not
// fail the fake tests, which render both from the same value; it would make the
// check never fire on a machine whose zone is not UTC, or fire on nothing.
//
// Runs against a PRIVATE server (own socket), for the reason
// capture_chunk_integration_test.go gives.
func TestLiveConversationFollowsANewConversationStartedInTheSameProcess(t *testing.T) {
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

	socket := filepath.Join("/tmp", "fl-cleared-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })
	wrapper := filepath.Join(t.TempDir(), "mux")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+bin+" -S "+socket+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "new-session", "-d", "-s", "cleared-alpha", "-c", cwd, "sleep", "600").CombinedOutput(); err != nil {
		t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	out, err := exec.Command(wrapper, "list-panes", "-t", "cleared-alpha", "-F", "#{pane_pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("pane_pid %q: %v", out, err)
	}

	// The record the runtime keeps for this process: its start time is the one the
	// real `ps` reports, written in UTC as the runtime writes it.
	psOut, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps for pid %d: %v", pid, err)
	}
	started, err := time.ParseInLocation(psStartTimeLayout, strings.TrimSpace(string(psOut)), time.Local)
	if err != nil {
		t.Fatal(err)
	}
	writeProcessRecord := func(conv string) {
		t.Helper()
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
			if s.ID == "cleared-alpha" {
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

	writeProcessRecord(resumedConv)
	for i := 0; i < 2; i++ {
		if known, id := conversation(d); !known || id != resumedConv {
			t.Fatalf("listing %d: the process's conversation, got known=%v id=%q", i, known, id)
		}
	}

	// `/clear`: the same process, a record that now names another conversation.
	writeProcessRecord(clearedConv)
	for i := 0; i < 3; i++ {
		if known, id := conversation(d); !known || id != clearedConv {
			t.Fatalf("listing %d after the clear: the process is in %s; got known=%v id=%q", i, clearedConv, known, id)
		}
	}
}
