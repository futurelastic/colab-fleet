package trustseed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeState writes a minimal runtime state file with one unrelated
// top-level field and one unrelated project entry, so every test can assert
// neither was touched.
func writeState(t *testing.T, path string, extra map[string]any) {
	t.Helper()
	doc := map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "someone@example.com"},
		"projects": map[string]any{
			"/already/known": map[string]any{
				"hasTrustDialogAccepted": true,
				"history":                []string{"a prior command"},
			},
		},
	}
	for k, v := range extra {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tempHome returns a symlink-resolved temp directory. t.TempDir() on macOS
// commonly sits under /tmp or /var, both of which are themselves symlinks
// (to /private/tmp, /private/var) — every path built under an unresolved
// home would then carry a DIFFERENT resolved-symlink form, and lookupKeys
// would count that as a second, legitimately distinct key. That behaviour
// is correct (see lookupKeys); resolving up front here keeps these tests
// about the add-only/race logic, not about how many key variants one path
// happens to have.
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func readState(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	return top
}

func mkRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// mkWorktree makes dir a worktree root the way git does: a .git FILE, not a
// directory, per the issue's "a worktree root is a repository root for this
// purpose".
func mkWorktree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeedAllGrantsUnderConfiguredRootAndLeavesEverythingElseAlone(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "acme", "widgets")
	mkRepo(t, repo)
	worktree := filepath.Join(root, "acme", "widgets-worktrees", "feature-x")
	mkWorktree(t, worktree)

	s := New(statePath, home, []string{root})
	if !s.Enabled() {
		t.Fatal("expected Enabled with a valid root configured")
	}

	result, err := s.SeedAll()
	if err != nil {
		t.Fatalf("SeedAll: %v", err)
	}
	if result.Islands != 2 {
		t.Fatalf("Islands = %d, want 2 (repo + worktree)", result.Islands)
	}
	if result.Granted != 2 {
		t.Fatalf("Granted = %d, want 2", result.Granted)
	}
	if len(result.RootsMissing) != 0 {
		t.Fatalf("RootsMissing = %v, want none", result.RootsMissing)
	}

	top := readState(t, statePath)

	// The unrelated top-level field survived untouched.
	var oauth map[string]any
	if err := json.Unmarshal(top["oauthAccount"], &oauth); err != nil {
		t.Fatal(err)
	}
	if oauth["emailAddress"] != "someone@example.com" {
		t.Errorf("oauthAccount was disturbed: %v", oauth)
	}

	var projects map[string]json.RawMessage
	if err := json.Unmarshal(top["projects"], &projects); err != nil {
		t.Fatal(err)
	}

	// The pre-existing, unrelated project entry kept its own fields.
	var known struct {
		HasTrustDialogAccepted bool     `json:"hasTrustDialogAccepted"`
		History                []string `json:"history"`
	}
	if err := json.Unmarshal(projects["/already/known"], &known); err != nil {
		t.Fatal(err)
	}
	if !known.HasTrustDialogAccepted || len(known.History) != 1 || known.History[0] != "a prior command" {
		t.Errorf("pre-existing project entry was rewritten: %+v", known)
	}

	// Both new islands were granted.
	for _, dir := range []string{repo, worktree} {
		raw, ok := projects[dir]
		if !ok {
			t.Fatalf("no project entry for %s", dir)
		}
		var got struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !got.HasTrustDialogAccepted {
			t.Errorf("%s: hasTrustDialogAccepted not set", dir)
		}
	}

	counters := s.Counters()
	if counters[CounterGranted] != 2 {
		t.Errorf("CounterGranted = %d, want 2", counters[CounterGranted])
	}
}

func TestSeedAllIsANoOpOnceAlreadySet(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)

	s := New(statePath, home, []string{root})
	if _, err := s.SeedAll(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 0 {
		t.Errorf("second pass Granted = %d, want 0 (already set)", result.Granted)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a no-op pass rewrote the state file; ensureKeys must skip the write entirely when nothing is dirty")
	}
}

func TestSeedAllReportsAMissingRootWithoutFailingTheOthers(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	present := filepath.Join(home, "workspace")
	repo := filepath.Join(present, "one")
	mkRepo(t, repo)
	missing := filepath.Join(home, "DoesNotExist")

	s := New(statePath, home, []string{present, missing})
	result, err := s.SeedAll()
	if err != nil {
		t.Fatalf("a missing root must not fail the whole pass: %v", err)
	}
	if len(result.RootsMissing) != 1 || result.RootsMissing[0] != missing {
		t.Errorf("RootsMissing = %v, want [%s]", result.RootsMissing, missing)
	}
	if result.Granted != 1 {
		t.Errorf("Granted = %d, want 1 (the present root's island)", result.Granted)
	}
	if s.Counters()[CounterRootMissing] == 0 {
		t.Error("CounterRootMissing was not incremented")
	}
}

func TestSeedPathRefusesOutsideConfiguredRoot(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "Elsewhere", "repo")
	mkRepo(t, outside)

	s := New(statePath, home, []string{root})
	if err := s.SeedPath(outside); err == nil {
		t.Fatal("expected a refusal for a path outside every configured root")
	}
	if s.Counters()[CounterRefused] != 1 {
		t.Errorf("CounterRefused = %d, want 1", s.Counters()[CounterRefused])
	}

	top := readState(t, statePath)
	var projects map[string]json.RawMessage
	_ = json.Unmarshal(top["projects"], &projects)
	if _, ok := projects[outside]; ok {
		t.Error("a refused path must never be written")
	}
}

func TestSeedPathRefusesHomeAndFilesystemRoot(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)

	root := filepath.Join(home, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(statePath, home, []string{root})

	if err := s.SeedPath(home); err == nil {
		t.Error("expected a refusal for the home directory itself")
	}
	if err := s.SeedPath(string(filepath.Separator)); err == nil {
		t.Error("expected a refusal for the filesystem root")
	}
	if got := s.Counters()[CounterRefused]; got != 2 {
		t.Errorf("CounterRefused = %d, want 2", got)
	}
}

// TestEnsureKeysAbandonsOnConcurrentWrite is the case the whole package is
// designed around: the runtime rewrites its state file wholesale while this
// package is mid-pass. A write computed from a read that is no longer
// current must never be committed — it would silently discard whatever the
// concurrent writer just wrote — so this asserts the abandoned attempt
// leaves the concurrent write's content byte-for-byte intact, that it is
// reported as a lost race rather than an error, and that the NEXT pass
// (once nothing is racing it) converges and grants the key.
func TestEnsureKeysAbandonsOnConcurrentWrite(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")
	writeState(t, statePath, nil)
	t0 := time.Now().Add(-time.Hour)
	if err := os.Chtimes(statePath, t0, t0); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(home, "workspace")
	repo := filepath.Join(root, "one")
	mkRepo(t, repo)

	s := New(statePath, home, []string{root})

	// Simulate the runtime rewriting the whole file — a brand new project
	// entry this pass never saw — in the window between this pass's read
	// and its write.
	var raced bool
	externalWrite := func() map[string]any {
		return map[string]any{
			"oauthAccount": map[string]any{"emailAddress": "rotated@example.com"},
			"projects": map[string]any{
				"/from/the/concurrent/writer": map[string]any{"hasTrustDialogAccepted": true},
			},
		}
	}
	s.afterRead = func() {
		if raced {
			return
		}
		raced = true
		doc := externalWrite()
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		// Force a distinct mtime regardless of filesystem timestamp
		// resolution, so the CAS check is guaranteed to see a difference.
		t1 := time.Now()
		if err := os.Chtimes(statePath, t1, t1); err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.SeedAll()
	if err != nil {
		t.Fatalf("a lost race must not surface as an error: %v", err)
	}
	if !result.LostRace {
		t.Fatal("expected LostRace on the raced pass")
	}
	if result.Granted != 0 {
		t.Errorf("Granted = %d on a raced pass, want 0 — nothing may be committed", result.Granted)
	}

	// The concurrent writer's content must be exactly what it wrote — not
	// clobbered, not merged, not partially overwritten.
	gotRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := json.Marshal(externalWrite())
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(gotRaw, &got)
	_ = json.Unmarshal(wantRaw, &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("the raced write clobbered the concurrent writer's content:\n got  %s\n want %s", gotJSON, wantJSON)
	}

	if s.Counters()[CounterLostRace] != 1 {
		t.Errorf("CounterLostRace = %d, want 1", s.Counters()[CounterLostRace])
	}

	// The next pass, nothing racing it, converges: both the concurrent
	// writer's own project entry AND this pass's island are present.
	s.afterRead = nil
	result2, err := s.SeedAll()
	if err != nil {
		t.Fatal(err)
	}
	if result2.LostRace {
		t.Fatal("the second, unraced pass must not lose the race again")
	}
	if result2.Granted != 1 {
		t.Fatalf("Granted = %d on the converging pass, want 1", result2.Granted)
	}

	top := readState(t, statePath)
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(top["projects"], &projects); err != nil {
		t.Fatal(err)
	}
	if _, ok := projects["/from/the/concurrent/writer"]; !ok {
		t.Error("the concurrent writer's own project entry was lost by the converging pass")
	}
	var repoEntry struct {
		HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
	}
	if err := json.Unmarshal(projects[repo], &repoEntry); err != nil {
		t.Fatal(err)
	}
	if !repoEntry.HasTrustDialogAccepted {
		t.Error("the converging pass did not grant the island it was retrying for")
	}
}

func TestNewDropsInvalidRootsRatherThanReportingThemPerPass(t *testing.T) {
	home := tempHome(t)
	statePath := filepath.Join(home, ".claude.json")

	s := New(statePath, home, []string{
		"relative/path",            // not absolute
		"",                         // empty
		home,                       // equals home
		string(filepath.Separator), // filesystem root
	})
	if s.Enabled() {
		t.Fatal("expected Enabled false: every configured root was invalid")
	}
}

func TestSeederIsANoOpWhenNotConfigured(t *testing.T) {
	var nilSeeder *Seeder
	if nilSeeder.Enabled() {
		t.Fatal("nil Seeder must report Enabled() false")
	}
	if err := nilSeeder.SeedPath("/anything"); err != nil {
		t.Errorf("nil Seeder.SeedPath must be a silent no-op, got %v", err)
	}
	if result, err := nilSeeder.SeedAll(); err != nil || result.Islands != 0 || result.Granted != 0 {
		t.Errorf("nil Seeder.SeedAll must be a silent no-op, got %+v, %v", result, err)
	}
	if nilSeeder.Counters() != nil {
		t.Errorf("nil Seeder.Counters() = %v, want nil", nilSeeder.Counters())
	}
}
