package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// #180 L5: the runtime's per-process record supplies a session id that
// becomes part of a path. A non-UUID id is refused, and no id can resolve a
// path outside the record root.
func TestProcessSessionRecordSessionIDCannotEscapeTheRecordRoot(t *testing.T) {
	dir := t.TempDir()
	d := &Driver{processSessionsRoot: dir}
	if err := os.WriteFile(filepath.Join(dir, "100.json"),
		[]byte(`{"pid":100,"sessionId":"../../outside","cwd":"/work/a","procStart":"Mon Jan  2 15:04:05 2026"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.readProcessSessionRecord(100); ok {
		t.Fatal("a session id shaped like a path was accepted")
	}
	root := t.TempDir()
	store := &conversationStore{root: root}
	p := store.recordPath("/work/a", "../../../outside")
	if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("recordPath resolved %q, outside the record root %q", p, root)
	}
	if !uuidShaped("0a0b0c0d-0000-4000-8000-00000000abc1") || uuidShaped("conv-1") || uuidShaped("../../0000-4000-8000-00000000abc1") {
		t.Fatal("uuidShaped")
	}
}

// #180 L7: arrow keys are for dialogs. On an idle, empty composer an arrow
// drives the runtime itself (Left opens its agent view), so it is refused.
func TestKeysRefusesArrowsOnAnEmptyComposer(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")
	for _, key := range []fleet.KeyName{fleet.KeyLeft, fleet.KeyUp, fleet.KeyDown, fleet.KeyRight} {
		got, err := d.Keys(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, key, want)
		if err != nil {
			t.Fatalf("Keys(%s): %v", key, err)
		}
		if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "arrow") {
			t.Fatalf("Keys(%s) = %s (%s), want refused", key, got.Outcome, got.Reason)
		}
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			t.Fatalf("a key reached an idle composer: %v", c)
		}
	}
}
