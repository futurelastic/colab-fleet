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

// --- #187: the runtime's own local_command entries confirm a slash command ---
//
// The fixtures below have the SHAPE measured on real transcripts (runtime builds
// 2.1.273 through 2.1.281) and synthetic values: a real transcript is somebody's
// conversation. See localCommandLine for what was measured.

// localCommandEcho is the first entry a runtime-run command writes.
func localCommandEcho(name, args string) map[string]any {
	return map[string]any{
		"type": "system", "subtype": "local_command", "level": "info", "isMeta": false,
		"content": "<command-name>/" + name + "</command-name>\n            <command-message>" + name +
			"</command-message>\n            <command-args>" + args + "</command-args>",
	}
}

// localCommandResult is the entry written right after it: the command's output,
// with the command itself named structurally and WITHOUT its slash.
func localCommandResult(name, args string) map[string]any {
	return map[string]any{
		"type": "system", "subtype": "local_command", "level": "info", "isMeta": false,
		"commandRun": map[string]any{"command": name, "args": args},
		"content":    "<local-command-stdout>synthetic output</local-command-stdout>",
	}
}

// localCommandReminder is what the runtime writes after a command: a meta user
// entry that is not a turn.
func localCommandReminder() map[string]any {
	return map[string]any{"type": "user", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "<system-reminder>synthetic</system-reminder>"}}
}

func TestLocalCommandEntriesAreCommandCandidates(t *testing.T) {
	for name, tc := range map[string]struct {
		entry map[string]any
		want  string
	}{
		"echo with args":            {localCommandEcho("rename", "alpha-renamed"), "/rename alpha-renamed"},
		"echo without args":         {localCommandEcho("model", ""), "/model"},
		"result with args":          {localCommandResult("rename", "alpha-renamed"), "/rename alpha-renamed"},
		"result without args":       {localCommandResult("remote-control", ""), "/remote-control"},
		"result whose name is a /x": {localCommandResult("/rename", "alpha"), "/rename alpha"},
	} {
		b, _ := json.Marshal(tc.entry)
		kind, text, _, ok := extractTranscriptCandidate(b)
		if !ok || kind != "command" || text != tc.want {
			t.Errorf("%s: got (%q, %q, ok=%v), want (\"command\", %q, true)", name, kind, text, ok, tc.want)
		}
	}
}

// Nothing else the runtime writes as a system entry, and no local_command entry
// that names no command, may be taken for one.
func TestSystemEntriesThatNameNoCommandAreNotCandidates(t *testing.T) {
	for name, entry := range map[string]map[string]any{
		"another subtype":                {"type": "system", "subtype": "turn_duration", "content": "<command-name>/rename</command-name>"},
		"no subtype":                     {"type": "system", "content": "<command-name>/rename</command-name>"},
		"output only, no commandRun":     {"type": "system", "subtype": "local_command", "content": "<local-command-stdout>hi</local-command-stdout>"},
		"empty commandRun name":          {"type": "system", "subtype": "local_command", "commandRun": map[string]any{"command": "", "args": "x"}},
		"commandRun that is not object":  {"type": "system", "subtype": "local_command", "commandRun": "rename"},
		"echo naming no slash command":   {"type": "system", "subtype": "local_command", "content": "<command-name>rename</command-name>"},
		"echo with an unclosed name tag": {"type": "system", "subtype": "local_command", "content": "<command-name>/rename"},
		"meta entry":                     {"type": "system", "subtype": "local_command", "isMeta": true, "commandRun": map[string]any{"command": "rename"}},
		"sidechain entry":                {"type": "system", "subtype": "local_command", "isSidechain": true, "commandRun": map[string]any{"command": "rename"}},
	} {
		b, _ := json.Marshal(entry)
		if kind, text, _, ok := extractTranscriptCandidate(b); ok {
			t.Errorf("%s: taken as a candidate (%q, %q)", name, kind, text)
		}
	}
}

// writeTranscript writes entries as a transcript file and returns its path and
// the offset a send would have recorded before pressing Enter: after the first
// `before` entries.
func writeTranscript(t *testing.T, before int, entries ...map[string]any) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	var b strings.Builder
	var offset int64
	for i, e := range entries {
		if i == before {
			offset = int64(b.Len())
		}
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	if before >= len(entries) {
		offset = int64(b.Len())
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, offset
}

func TestTailScanConfirmsACommandFromLocalCommandEntries(t *testing.T) {
	for name, entries := range map[string][]map[string]any{
		"the echo alone":         {localCommandEcho("rename", "alpha-renamed")},
		"the result alone":       {localCommandResult("rename", "alpha-renamed")},
		"the runtime's order":    {localCommandEcho("rename", "alpha-renamed"), localCommandResult("rename", "alpha-renamed"), localCommandReminder()},
		"after an unrelated run": {localCommandEcho("context", ""), localCommandResult("context", ""), localCommandEcho("rename", "alpha-renamed")},
	} {
		path, offset := writeTranscript(t, 0, entries...)
		got, err := transcriptTailScan(path, offset, "/rename alpha-renamed")
		if err != nil || got != transcriptScanMatched {
			t.Errorf("%s: scan = %v (err %v), want matched", name, got, err)
		}
	}
}

// A command entry that names a different command, or different arguments, is
// not evidence about this send — and must read as silence, not as "a different
// turn": a different turn stops the screen fallback, and a runtime that ran some
// other command in the same moment is not a reason to give up on this one.
func TestTailScanIgnoresALocalCommandThatIsNotTheOneSent(t *testing.T) {
	for name, entries := range map[string][]map[string]any{
		"another command":     {localCommandEcho("context", ""), localCommandResult("context", ""), localCommandReminder()},
		"other arguments":     {localCommandEcho("rename", "someone-else"), localCommandResult("rename", "someone-else")},
		"output-only entries": {{"type": "system", "subtype": "local_command", "content": "<local-command-stdout>x</local-command-stdout>"}},
	} {
		path, offset := writeTranscript(t, 0, entries...)
		got, err := transcriptTailScan(path, offset, "/rename alpha-renamed")
		if err != nil || got != transcriptScanSilent {
			t.Errorf("%s: scan = %v (err %v), want silent", name, got, err)
		}
	}
}

// A command run before the send's own offset is never read: an identical
// earlier /rename must not confirm this one.
func TestTailScanDoesNotReadALocalCommandBeforeTheOffset(t *testing.T) {
	path, offset := writeTranscript(t, 2,
		localCommandEcho("rename", "alpha-renamed"), localCommandResult("rename", "alpha-renamed"))
	if got, err := transcriptTailScan(path, offset, "/rename alpha-renamed"); err != nil || got != transcriptScanSilent {
		t.Fatalf("scan = %v (err %v), want silent", got, err)
	}
}

// The measured case of #187, end to end through Send: /rename against a runtime
// that wrote local_command entries and no <command-name> user entry. Confirmed by
// the transcript — the counters, not the wording of the reason, say which signal
// decided it: the screen fallback's own reason also contains the word
// "transcript".
func TestSlashCommandSendConfirmsFromLocalCommandEntries(t *testing.T) {
	for name, entry := range map[string]map[string]any{
		"echo":   localCommandEcho("rename", "alpha-renamed"),
		"result": localCommandResult("rename", "alpha-renamed"),
	} {
		t.Run(name, func(t *testing.T) {
			d, _, ref, appendSoon := transcriptSession(t)
			appendSoon(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			got, err := d.Send(ctx, testCaller, ref, "/rename alpha-renamed", driver.SendOptions{Submit: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeQueued {
				t.Fatalf("outcome = %s (%s), want queued", got.Outcome, got.Reason)
			}
			c := d.counters.Snapshot()
			if c[counterSubmitConfirmedByTranscript] != 1 || c[counterSubmitConfirmedByScreenAfterSilentTranscript] != 0 {
				t.Fatalf("by_transcript=%d by_screen_after_silent_transcript=%d, want 1 and 0 — the "+
					"transcript should have decided this, not the screen after the window (reason: %s)",
					c[counterSubmitConfirmedByTranscript], c[counterSubmitConfirmedByScreenAfterSilentTranscript], got.Reason)
			}
		})
	}
}
