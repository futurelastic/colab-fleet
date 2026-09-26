package trustseed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The external-imports question (colab-fleet #211) is answered by the same
// mechanism as the folder-trust one, so these tests pin what is NEW about it:
// three keys per project instead of one, judged independently, counted apart
// from the trust key, and bound by the same scope guards and race rule.

// projectEntry reads one project's fields out of the state file, or fails the
// test when the project is not there at all.
func projectEntry(t *testing.T, statePath, dir string) map[string]any {
	t.Helper()
	top := readState(t, statePath)
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(top["projects"], &projects); err != nil {
		t.Fatal(err)
	}
	raw, ok := projects[dir]
	if !ok {
		t.Fatalf("no project entry for %s", dir)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

// hasProject reports whether the state file carries an entry for dir at all.
func hasProject(t *testing.T, statePath, dir string) bool {
	t.Helper()
	top := readState(t, statePath)
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(top["projects"], &projects); err != nil {
		t.Fatal(err)
	}
	_, ok := projects[dir]
	return ok
}

func wantAllThreeTrue(t *testing.T, entry map[string]any, where string) {
	t.Helper()
	for _, k := range []string{trustKey, importsApprovedKey, importsShownKey} {
		if entry[k] != true {
			t.Errorf("%s: %s = %v, want true", where, k, entry[k])
		}
	}
}

func TestSeedAllSeedsTheExternalImportsKeysBesideTheTrustKey(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "acme", "widgets")
	mkRepo(t, repo)
	worktree := filepath.Join(root, "acme", "widgets-worktrees", "feature-x")
	mkWorktree(t, worktree)

	s := New(statePath, home, []string{root})
	result, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 2 || result.ImportsGranted != 2 {
		t.Fatalf("Granted = %d, ImportsGranted = %d, want 2 and 2", result.Granted, result.ImportsGranted)
	}
	for _, dir := range []string{repo, worktree} {
		wantAllThreeTrue(t, projectEntry(t, statePath, dir), dir)
	}

	// The two keys are counted APART, so a caller can tell which is being
	// written (#211 point 4).
	c := s.Counters()
	if c[CounterGranted] != 2 {
		t.Errorf("%s = %d, want 2", CounterGranted, c[CounterGranted])
	}
	if c[CounterImportsGranted] != 2 {
		t.Errorf("%s = %d, want 2", CounterImportsGranted, c[CounterImportsGranted])
	}

	// A project the pass had no reason to touch gained nothing: the seed is a
	// standing policy over the roots, not over whatever the file already held.
	known := projectEntry(t, statePath, "/already/known")
	if _, ok := known[importsApprovedKey]; ok {
		t.Errorf("a project outside every root was given %s", importsApprovedKey)
	}
}

// The runtime writes the imports keys itself, as false, before they are
// answered — measured on a real state file. A project that is already trusted
// must still be answered on the second question; the old skip-the-whole-entry
// shortcut would have left it asking forever.
func TestImportsKeysAreSeededOnAProjectTheRuntimeAlreadyTrusts(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)
	writeState(t, statePath, map[string]any{
		"projects": map[string]any{
			repo: map[string]any{
				trustKey:           true,
				importsApprovedKey: false,
				importsShownKey:    false,
				"history":          []string{"a prior command"},
				"allowedTools":     []string{},
			},
		},
	})

	s := New(statePath, home, []string{root})
	result, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 0 {
		t.Errorf("Granted = %d, want 0 — the trust key was already true and is not rewritten", result.Granted)
	}
	if result.ImportsGranted != 1 {
		t.Errorf("ImportsGranted = %d, want 1", result.ImportsGranted)
	}

	entry := projectEntry(t, statePath, repo)
	wantAllThreeTrue(t, entry, repo)
	hist, _ := entry["history"].([]any)
	if len(hist) != 1 || hist[0] != "a prior command" {
		t.Errorf("the entry's own history was disturbed: %v", entry["history"])
	}
	if _, ok := entry["allowedTools"]; !ok {
		t.Error("the entry's own allowedTools key was dropped")
	}
}

// Add-only, key by key: a key already true is left exactly as it is, and a pass
// that finds all three true writes nothing at all.
func TestAKeyAlreadyTrueIsNeverRewrittenAndAFullySeededFileIsUntouched(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)
	// The approval is true, its companion is not (an interrupted write, or a
	// runtime that never set it). Only the missing one may be added, and the
	// approval must not be counted as a grant — this package did not set it.
	writeState(t, statePath, map[string]any{
		"projects": map[string]any{
			repo: map[string]any{trustKey: true, importsApprovedKey: true, "extra": "kept"},
		},
	})

	s := New(statePath, home, []string{root})
	result, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 0 || result.ImportsGranted != 0 {
		t.Errorf("Granted = %d, ImportsGranted = %d, want 0 and 0 — the counted keys were already true",
			result.Granted, result.ImportsGranted)
	}
	entry := projectEntry(t, statePath, repo)
	wantAllThreeTrue(t, entry, repo)
	if entry["extra"] != "kept" {
		t.Errorf("an unrelated field of the entry was lost: %v", entry)
	}

	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedAll(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a pass over a fully seeded file rewrote it; nothing was dirty")
	}
}

// On a create the seed runs for the session's own directory. The runtime's
// project is the enclosing repository root when there is one, otherwise the
// directory itself — so a directory in no repository gets its own entry, and a
// subdirectory of a repository seeds the repository's, not its own.
func TestSeedPathSeedsTheProjectTheRuntimeWouldKeyTheAnswerUnder(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "repo")
	mkRepo(t, repo)
	sub := filepath.Join(repo, "pkg", "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(root, "not-a-repo")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{root})
	if err := s.SeedPath(sub); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, repo), "the repository root")
	if hasProject(t, statePath, sub) {
		t.Error("a subdirectory of a repository got an entry of its own; the runtime keys the answer under the root")
	}

	if err := s.SeedPath(loose); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, loose), "a directory in no repository")

	c := s.Counters()
	if c[CounterGranted] != 2 || c[CounterImportsGranted] != 2 {
		t.Errorf("counters = trust %d, imports %d, want 2 and 2", c[CounterGranted], c[CounterImportsGranted])
	}
}

// The scope guards bind the new keys exactly as they bind the old one: a
// directory outside every root, the home directory and the filesystem root are
// refused, and a refusal writes nothing under either key.
func TestTheScopeGuardsRefuseTheImportsKeysToo(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(home, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "Elsewhere", "repo")
	mkRepo(t, outside)

	s := New(statePath, home, []string{root})
	for _, dir := range []string{outside, home, string(filepath.Separator)} {
		if err := s.SeedPath(dir); err == nil {
			t.Errorf("SeedPath(%s) was not refused", dir)
		}
	}
	if s.Counters()[CounterRefused] != 3 {
		t.Errorf("CounterRefused = %d, want 3", s.Counters()[CounterRefused])
	}
	if c := s.Counters(); c[CounterGranted] != 0 || c[CounterImportsGranted] != 0 {
		t.Errorf("a refusal was counted as a grant: %v", c)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a refused seed still changed the state file")
	}
}

// A project already trusted, its imports not yet answered, and the runtime
// rewrites the file mid-pass. The abandoned write must leave the concurrent
// writer's content intact and count nothing — and the next pass converges.
func TestAnImportsOnlyWriteThatLosesTheRaceCommitsNothing(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)
	writeState(t, statePath, map[string]any{
		"projects": map[string]any{repo: map[string]any{trustKey: true}},
	})
	t0 := time.Now().Add(-time.Hour)
	if err := os.Chtimes(statePath, t0, t0); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{root})
	external := map[string]any{
		"projects": map[string]any{
			repo:                          map[string]any{trustKey: true},
			"/from/the/concurrent/writer": map[string]any{trustKey: true},
		},
	}
	raced := false
	s.afterRead = func() {
		if raced {
			return
		}
		raced = true
		raw, _ := json.Marshal(external)
		if err := os.WriteFile(statePath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(statePath, now, now); err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.SeedAll()
	if err != nil {
		t.Fatalf("a lost race must not surface as an error: %v", err)
	}
	if !result.LostRace || result.ImportsGranted != 0 || result.Granted != 0 {
		t.Fatalf("result = %+v, want a lost race that granted nothing", result)
	}
	if c := s.Counters(); c[CounterImportsGranted] != 0 || c[CounterLostRace] != 1 {
		t.Errorf("counters = %v, want imports 0 and lost_race 1", c)
	}
	if got := projectEntry(t, statePath, repo); got[importsApprovedKey] != nil {
		t.Errorf("the abandoned write still landed: %v", got)
	}
	if !hasProject(t, statePath, "/from/the/concurrent/writer") {
		t.Error("the concurrent writer's own entry was clobbered")
	}

	s.afterRead = nil
	again, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if again.LostRace || again.ImportsGranted != 1 {
		t.Fatalf("converging pass = %+v, want ImportsGranted 1", again)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, repo), repo)
}

// A project the runtime stored as a bare null is not a shape this package
// invented, and there is nothing in it to preserve. It must be filled in, not
// crash the daemon that runs this pass on a timer.
func TestANullProjectEntryIsFilledInNotPanickedOn(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)
	writeState(t, statePath, map[string]any{
		"projects": map[string]any{repo: nil},
	})

	s := New(statePath, home, []string{root})
	if _, err := s.SeedAll(); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, repo), repo)
}

// The two path variants a symlinked root produces are two project keys, and the
// runtime may look the answer up under either. Both must carry all three keys.
func TestBothPathVariantsOfASymlinkedRootGetTheImportsKeys(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	real := filepath.Join(home, "real-workspace")
	repo := filepath.Join(real, "one")
	mkRepo(t, repo)
	link := filepath.Join(home, "linked-workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Through SeedPath, the create-time entry point: WalkDir does not descend
	// through a symlinked root, so a full pass would find nothing here — a limit
	// this test does not exercise and the trust key has always had.
	s := New(statePath, home, []string{link})
	if err := s.SeedPath(filepath.Join(link, "one")); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, filepath.Join(link, "one")), "the path as configured")
	wantAllThreeTrue(t, projectEntry(t, statePath, repo), "the resolved path")
}

// NewTrustOnly is the compat harness's way to make a directory the runtime
// trusts but has not been told it may import from (colab-fleet #212). It has to
// write exactly the trust key — not the imports pair, and not the counter that
// says the imports question was answered — while keeping every guard the
// standing seeder has, because the file it writes is the runtime's own.
func TestNewTrustOnlyAnswersTheTrustQuestionAndLeavesTheImportsOneStanding(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "scratch", "i")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewTrustOnly(statePath, home, []string{root})
	if err := s.SeedPath(root); err != nil {
		t.Fatal(err)
	}
	entry := projectEntry(t, statePath, root)
	if entry[trustKey] != true {
		t.Errorf("%s = %v, want true", trustKey, entry[trustKey])
	}
	for _, k := range []string{importsApprovedKey, importsShownKey} {
		if v, ok := entry[k]; ok {
			t.Errorf("%s = %v: a trust-only seed must leave the imports question standing", k, v)
		}
	}
	c := s.Counters()
	if c[CounterGranted] != 1 || c[CounterImportsGranted] != 0 {
		t.Errorf("counters = %v, want granted 1 and imports_granted 0", c)
	}

	// A second, standing seeder over the same directory then completes the
	// answer: the two are one mechanism, not two, and a trust-only entry is an
	// entry the standing seeder still upgrades rather than skips.
	full := New(statePath, home, []string{root})
	if err := full.SeedPath(root); err != nil {
		t.Fatal(err)
	}
	wantAllThreeTrue(t, projectEntry(t, statePath, root), root)
}

// The guards are the standing seeder's, whichever keys it writes: outside every
// configured root it refuses and writes nothing.
func TestNewTrustOnlyKeepsTheScopeGuards(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	root := filepath.Join(home, "scratch", "i")
	outside := filepath.Join(home, "scratch", "u")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := NewTrustOnly(statePath, home, []string{root})
	if err := s.SeedPath(outside); err == nil {
		t.Error("SeedPath outside every configured root succeeded")
	}
	if hasProject(t, statePath, outside) {
		t.Errorf("a refused directory was written: %s", outside)
	}
	if s.Counters()[CounterRefused] != 1 {
		t.Errorf("counters = %v, want one refusal", s.Counters())
	}
}
