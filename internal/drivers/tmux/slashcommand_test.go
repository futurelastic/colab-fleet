package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// transcriptSession sets up a record root holding one conversation named
// after the session, so the driver resolves a transcript for it, and returns
// a function that appends one entry to it shortly after it is called.
func transcriptSession(t *testing.T) (d *Driver, f *fakeMux, ref fleet.SessionRef, appendSoon func(map[string]any)) {
	t.Helper()
	root := t.TempDir()
	const cwd, name = "/work/alpha", "alpha💬"
	dir := filepath.Join(root, recordDirFor(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(dir, "conv-1.jsonl")
	line := func(v map[string]any) string {
		v["timestamp"] = time.Now().Format(time.RFC3339Nano)
		v["sessionId"] = "conv-1"
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	if err := os.WriteFile(conv, []byte(line(map[string]any{"type": "custom-title", "customTitle": name})), 0o600); err != nil {
		t.Fatal(err)
	}
	f = twoSessions()
	d = New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now), WithRecordRoot(root))
	appendSoon = func(v map[string]any) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			fh, err := os.OpenFile(conv, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			defer fh.Close()
			_, _ = fh.WriteString(line(v))
		}()
	}
	return d, f, fleet.SessionRef{Machine: "testbox", ID: name}, appendSoon
}

// #180 M3: a slash command the runtime accepted is recorded as a
// <command-name> user entry. That confirms the send, and is never read as "a
// different turn" that turns an accepted command into a false unknown.
func TestSlashCommandSendConfirmsFromTranscript(t *testing.T) {
	d, _, ref, appendSoon := transcriptSession(t)
	appendSoon(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": "<command-name>/rename</command-name>\n            <command-message>rename</command-message>\n            <command-args>alpha-renamed</command-args>"}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, "/rename alpha-renamed", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued || !strings.Contains(got.Reason, "transcript") {
		t.Fatalf("outcome = %s (%s), want queued, confirmed by the transcript", got.Outcome, got.Reason)
	}
}

// A command's own output (<local-command-stdout>) and a bash-mode line are
// not turns: they neither confirm nor contradict a message.
func TestCommandOutputEntriesAreNotCandidates(t *testing.T) {
	for _, content := range []string{
		"<local-command-stdout>Session renamed</local-command-stdout>",
		"<bash-input>ls</bash-input>",
	} {
		b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
		if _, _, _, ok := extractTranscriptCandidate(b); ok {
			t.Errorf("%q was taken as a candidate turn", content)
		}
	}
	if !commandMatches("/rename x", "/rename x") || commandMatches("/rename x", "/rename y") ||
		commandMatches("/clear", "/rename") || !commandMatches("/rc", "/rc") || commandMatches("/rename", "rename") {
		t.Fatal("commandMatches")
	}
}

// When the transcript records a different turn and the composer emptied, the
// receipt must not claim the text is sitting in the composer.
func TestDifferentTurnReasonIsAccurate(t *testing.T) {
	d, _, ref, appendSoon := transcriptSession(t)
	appendSoon(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "a different message entirely"}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, "the message this driver sent", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %s (%s), want unknown", got.Outcome, got.Reason)
	}
	if strings.Contains(got.Reason, "sitting there unsent") || !strings.Contains(got.Reason, "whether this text arrived is unknown") {
		t.Fatalf("reason = %q", got.Reason)
	}
}
