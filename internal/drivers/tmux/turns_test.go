package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Every fixture here is SYNTHETIC: the entry shapes were read off the runtime's
// own writer, the words were made up for this file. A real record is somebody's
// conversation and does not belong in a public repository's history.

func tsAt(i int) string { return fmt.Sprintf("2026-10-04T10:%02d:%02d.000Z", i/60, i%60) }

func jsonLine(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assistant(t *testing.T, i int, blocks ...map[string]any) string {
	return jsonLine(t, map[string]any{
		"type": "assistant", "timestamp": tsAt(i), "isSidechain": false,
		"message": map[string]any{"role": "assistant", "model": "model-x", "content": blocks},
	})
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func writeTurnsRecord(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conv-1.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func textsOf(p fleet.TurnsPage) []string {
	out := make([]string, 0, len(p.Turns))
	for _, tr := range p.Turns {
		out = append(out, tr.Text)
	}
	return out
}

// The ruling's own test: a record holding all three kinds — agent text, tool
// output, inbound message text — returns the first and none of the others.
func TestReadTurns_ReturnsAssistantTextOnly(t *testing.T) {
	const (
		toolOut   = "TOOL-OUTPUT-MARKER"
		fileBody  = "FILE-CONTENT-MARKER"
		humanIn   = "HUMAN-MESSAGE-MARKER"
		peerIn    = "PEER-SESSION-MARKER"
		hookOut   = "HOOK-OUTPUT-MARKER"
		thought   = "REASONING-MARKER"
		toolInput = "TOOL-INPUT-MARKER"
		subagent  = "SUBAGENT-MARKER"
		apiErr    = "API-ERROR-NOTICE-MARKER"
		stringy   = "PLAIN-STRING-CONTENT-MARKER"
		meta      = "META-MARKER"
	)
	p := writeTurnsRecord(t,
		jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(1), "message": map[string]any{"role": "user", "content": humanIn}}),
		jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(2), "origin": map[string]any{"kind": "peer"},
			"message": map[string]any{"role": "user", "content": []any{textBlock(peerIn)}}}),
		assistant(t, 3, map[string]any{"type": "thinking", "thinking": thought}),
		assistant(t, 4, textBlock("first thing the agent said")),
		assistant(t, 5, map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"path": toolInput}}),
		jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(6), "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": toolOut + " " + fileBody}}}}),
		jsonLine(t, map[string]any{"type": "system", "subtype": "hook", "timestamp": tsAt(7), "content": hookOut}),
		jsonLine(t, map[string]any{"type": "attachment", "timestamp": tsAt(8), "attachment": map[string]any{"text": hookOut}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(9), "isSidechain": true,
			"message": map[string]any{"role": "assistant", "model": "model-x", "content": []any{textBlock(subagent)}}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(10), "isApiErrorMessage": true,
			"message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []any{textBlock(apiErr)}}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(11), "isMeta": true,
			"message": map[string]any{"role": "assistant", "model": "model-x", "content": []any{textBlock(meta)}}}),
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": tsAt(12),
			"message": map[string]any{"role": "assistant", "model": "model-x", "content": stringy}}),
		`{"type":"assistant", this line is not JSON`,
		assistant(t, 13, textBlock("second thing"), textBlock("and a second block of it")),
	)

	page, err := readTurns(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := textsOf(page)
	want := []string{"first thing the agent said", "second thing\n\nand a second block of it"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("turns = %q, want %q", got, want)
	}
	wire, _ := json.Marshal(page)
	for _, leaked := range []string{toolOut, fileBody, humanIn, peerIn, hookOut, thought, toolInput, subagent, apiErr, stringy, meta} {
		if strings.Contains(string(wire), leaked) {
			t.Errorf("response carries %q, which is not assistant text:\n%s", leaked, wire)
		}
	}
	if page.Turns[0].At.Format("2006-01-02T15:04:05Z") != "2026-10-04T10:00:04Z" {
		t.Errorf("at = %v", page.Turns[0].At)
	}
}

func TestReadTurns_TailTakesTheLastLimitInOrder(t *testing.T) {
	var lines []string
	for i := 1; i <= 8; i++ {
		lines = append(lines, assistant(t, i, textBlock(fmt.Sprintf("turn %d", i))))
	}
	page, err := readTurns(writeTurnsRecord(t, lines...), "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(page), ","); got != "turn 6,turn 7,turn 8" {
		t.Fatalf("turns = %s", got)
	}
}

// The cursor walks the record forward with nothing repeated and nothing skipped,
// and what the agent writes AFTER the cursor is exactly what the next read gets.
func TestReadTurns_CursorResumesAfterWhatWasReturned(t *testing.T) {
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, assistant(t, i, textBlock(fmt.Sprintf("turn %d", i))))
		lines = append(lines, jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(i), "message": map[string]any{"role": "user", "content": "noise"}}))
	}
	p := writeTurnsRecord(t, lines...)

	tail, err := readTurns(p, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(tail), ","); got != "turn 4,turn 5" {
		t.Fatalf("tail = %s", got)
	}
	caughtUp, err := readTurns(p, tail.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(caughtUp.Turns) != 0 || caughtUp.Next != tail.Next {
		t.Fatalf("caught-up read = %+v, want no turns and the same cursor", caughtUp)
	}

	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, assistant(t, 6, textBlock("turn 6")))
	f.Close()
	more, err := readTurns(p, tail.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(more), ","); got != "turn 6" {
		t.Fatalf("after append = %s", got)
	}

	// Paging from the very start, two at a time, sees every turn exactly once.
	start := turnsCursor("conv-1", 0)
	var all []string
	for cur, guard := start, 0; guard < 10; guard++ {
		pg, err := readTurns(p, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, textsOf(pg)...)
		if len(pg.Turns) == 0 {
			break
		}
		cur = pg.Next
	}
	if got := strings.Join(all, ","); got != "turn 1,turn 2,turn 3,turn 4,turn 5,turn 6" {
		t.Fatalf("paged = %s", got)
	}
}

// A line the runtime is still writing is left alone and delivered whole later.
func TestReadTurns_PartialTrailingLineIsNotConsumed(t *testing.T) {
	first := assistant(t, 1, textBlock("whole"))
	second := assistant(t, 2, textBlock("later"))
	p := filepath.Join(t.TempDir(), "conv-1.jsonl")
	os.WriteFile(p, []byte(first+"\n"+second[:len(second)/2]), 0o600)

	page, err := readTurns(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(page), ","); got != "whole" {
		t.Fatalf("while half-written = %s", got)
	}
	os.WriteFile(p, []byte(first+"\n"+second+"\n"), 0o600)
	next, err := readTurns(p, page.Next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(next), ","); got != "later" {
		t.Fatalf("after it completed = %s", got)
	}
}

func TestReadTurns_StaleCursorIsConflictNotAnotherFilesPage(t *testing.T) {
	p := writeTurnsRecord(t, assistant(t, 1, textBlock("one")))

	_, err := readTurns(p, turnsCursor("some-other-conversation", 0), 10)
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("cursor from another conversation: err = %v, want ErrAmbiguousTarget", err)
	}
	_, err = readTurns(p, turnsCursor("conv-1", 1<<20), 10)
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("cursor past the end: err = %v, want ErrAmbiguousTarget", err)
	}
	for _, bad := range []string{"!!not base64!!", "bm90LWEtY3Vyc29y"} {
		if _, err := readTurns(p, bad, 10); err == nil || errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("garbage cursor %q: err = %v, want a plain invalid error", bad, err)
		}
	}
}

func TestReadTurns_BoundsAndHonesty(t *testing.T) {
	huge := strings.Repeat("é", fleet.MaxTurnTextBytes) // 2 bytes each: forces a mid-rune cut point
	big := jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(1), "message": map[string]any{
		"role": "user", "content": strings.Repeat("x", recordLineLimit+10)}}) // longer than a line may be
	p := writeTurnsRecord(t, big, assistant(t, 2, textBlock(huge)), assistant(t, 3, textBlock("after the monster")))

	page, err := readTurns(p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (an over-long line is skipped, not fatal)", len(page.Turns))
	}
	if !page.Turns[0].Truncated || len(page.Turns[0].Text) > fleet.MaxTurnTextBytes {
		t.Errorf("long turn: truncated=%v len=%d", page.Turns[0].Truncated, len(page.Turns[0].Text))
	}
	if !strings.HasSuffix(page.Turns[0].Text, "é") {
		t.Error("truncation split a character")
	}
	if page.Turns[1].Truncated {
		t.Error("a short turn is marked truncated")
	}

	empty, err := readTurns(writeTurnsRecord(t, `{"type":"mode"}`), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Turns == nil || len(empty.Turns) != 0 || empty.Next == "" {
		t.Errorf("a record with no agent text = %+v, want an empty non-nil list and a cursor", empty)
	}
	if b, _ := json.Marshal(empty); !strings.Contains(string(b), `"turns":[]`) {
		t.Errorf("wire = %s, want turns:[] not null", b)
	}

	if _, err := readTurns(filepath.Join(t.TempDir(), "absent.jsonl"), "", 5); !errors.Is(err, fleet.ErrNoTurnRecord) {
		t.Errorf("missing record: err = %v, want ErrNoTurnRecord", err)
	}
}

// The tail widens past its first window when the last turns are buried under
// bulk the answer must not include.
func TestReadTurns_TailWidensPastBulk(t *testing.T) {
	bulk := jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(1), "message": map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": strings.Repeat("z", 200<<10)}}}})
	p := writeTurnsRecord(t,
		assistant(t, 1, textBlock("old")), bulk, bulk, bulk, assistant(t, 2, textBlock("new")), bulk, bulk)
	page, err := readTurns(p, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(page), ","); got != "old,new" {
		t.Fatalf("turns = %s, want old,new", got)
	}
}

// The driver method end to end over the fake multiplexer: the session is found
// by id, corroborated against startedAt, its record located the way sends locate
// it, and the cursor it hands back resumes after what it returned.
func TestDriverTurns_ResolvesTheSessionsOwnRecord(t *testing.T) {
	d, _, ref, _, conv := titleSyncSession(t)
	ctx := t.Context()
	appendLine(t, conv, assistant(t, 1, textBlock("hello from alpha")))
	appendLine(t, conv, jsonLine(t, map[string]any{"type": "user", "timestamp": tsAt(2), "message": map[string]any{"role": "user", "content": "a human line"}}))

	page, err := d.Turns(ctx, fleet.Request{}, ref, fleet.TurnsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(page), ","); got != "hello from alpha" {
		t.Fatalf("turns = %s", got)
	}

	appendLine(t, conv, assistant(t, 3, textBlock("and later")))
	later, err := d.Turns(ctx, fleet.Request{}, ref, fleet.TurnsQuery{Since: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textsOf(later), ","); got != "and later" {
		t.Fatalf("after the cursor = %s", got)
	}

	// Corroborated: the right start time reads; a recycled id is refused before
	// anything is read.
	if _, err := d.Turns(ctx, withStartedAt(fleet.Request{}, startedAtFixture()), ref, fleet.TurnsQuery{}); err != nil {
		t.Errorf("matching startedAt: %v", err)
	}
	_, err = d.Turns(ctx, withStartedAt(fleet.Request{}, startedAtFixture().Add(time.Hour)), ref, fleet.TurnsQuery{})
	if !errors.Is(err, fleet.ErrAmbiguousTarget) {
		t.Errorf("recycled id: err = %v, want ErrAmbiguousTarget", err)
	}

	_, err = d.Turns(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "nobody"}, fleet.TurnsQuery{})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("unknown session: err = %v, want ErrNoSuchSession", err)
	}
	// "beta" exists but no record names it: the absence of a source, said so.
	_, err = d.Turns(ctx, fleet.Request{}, fleet.SessionRef{Machine: "testbox", ID: "beta"}, fleet.TurnsQuery{})
	if !errors.Is(err, fleet.ErrNoTurnRecord) {
		t.Errorf("session with no record: err = %v, want ErrNoTurnRecord", err)
	}
	if _, err := d.Turns(ctx, fleet.Request{}, ref, fleet.TurnsQuery{Limit: fleet.MaxTurnsLimit + 1}); err == nil {
		t.Error("an over-cap limit was accepted")
	}
}
