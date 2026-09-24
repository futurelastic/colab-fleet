package tmux

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/compat"
)

// fakeCandidate writes an executable stand-in for the runtime: a script that
// answers --version and whose body carries the given text, which is what the
// static checks scan. No real runtime, no network, no tokens.
func fakeCandidate(t *testing.T, version, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "candidate")
	script := "#!/bin/sh\n# " + body + "\ncase \"$1\" in --version) echo \"" + version + " (Synthetic Runtime)\" ;; esac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The temp dir may sit behind a symlink; tests compare against the
	// resolved form.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func allMarkersText() string {
	return strings.Join(staticMarkerList(), " ")
}

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// The static marker vocabulary is only worth having if it is exactly what the
// classifier matches. This runs every marker through the function that relies
// on it: change the classifier's wording and this fails until the vocabulary
// follows.
func TestStaticMarkersAreTiedToTheClassifier(t *testing.T) {
	tied := map[string]func() bool{
		// usageLimit
		"hit your":       func() bool { _, ok := usageLimit(newScreen("You've hit your session limit\n")); return ok },
		"/usage-credits": func() bool { _, ok := usageLimit(newScreen("run /usage-credits to finish\n")); return ok },
		// resetHintIn
		"resets ":          func() bool { h, ok := resetHintIn("limit · resets 9pm"); return ok && h == "9pm" },
		"available again ": func() bool { h, ok := resetHintIn("available again at 9pm"); return ok && h == "at 9pm" },
		// lastTurnFailed / retryableWords
		"api error": func() bool { _, ok := lastTurnFailed(newScreen("API Error: 529 overloaded\n")); return ok },
		"try again ": func() bool {
			te, ok := lastTurnFailed(newScreen("API Error: 529, please try again later\n"))
			return ok && te.Retryable
		},
		"temporary": func() bool {
			te, ok := lastTurnFailed(newScreen("API Error: temporary failure\n"))
			return ok && te.Retryable
		},
		// controlStateIn
		"/rc active": func() bool {
			st, ok := controlStateIn("/rc active")
			return ok && st == fleet.ControlChannelActive
		},
		"/rc failed": func() bool {
			st, ok := controlStateIn("/rc failed")
			return ok && st == fleet.ControlChannelFailed
		},
		"/rc reconnecting": func() bool {
			st, ok := controlStateIn("/rc reconnecting")
			return ok && st == fleet.ControlChannelReconnecting
		},
		"/rc connecting": func() bool {
			st, ok := controlStateIn("/rc connecting")
			return ok && st == fleet.ControlChannelConnecting
		},
	}
	for _, m := range staticMarkerList() {
		fn, ok := tied[m]
		if !ok {
			t.Errorf("marker %q is required by a check but tied to no classifier function here", m)
			continue
		}
		if m != strings.ToLower(m) {
			t.Errorf("marker %q must be lower-case: the scan folds case", m)
		}
		if !fn() {
			t.Errorf("marker %q: the classifier function that relies on it no longer accepts this wording", m)
		}
	}
	for m := range tied {
		found := false
		for _, x := range staticMarkerList() {
			if x == m {
				found = true
			}
		}
		if !found {
			t.Errorf("tied marker %q is in no check's vocabulary", m)
		}
	}
}

func TestStaticMarkerListSortedUnique(t *testing.T) {
	l := staticMarkerList()
	if !sort.StringsAreSorted(l) {
		t.Errorf("not sorted: %v", l)
	}
	seen := map[string]bool{}
	for _, m := range l {
		if seen[m] {
			t.Errorf("duplicate %q", m)
		}
		seen[m] = true
	}
}

func TestScanMarkers(t *testing.T) {
	dir := t.TempDir()
	// 9 MiB of filler with a marker that straddles the first 4 MiB chunk
	// boundary, an upper-case one in the middle, and one at the very end.
	const chunk = 4 << 20
	data := bytes.Repeat([]byte{'x'}, 9<<20)
	copy(data[chunk-3:], "hit your")
	copy(data[6<<20:], "/RC Active")
	copy(data[len(data)-len("api error"):], "api error")
	p := filepath.Join(dir, "big")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scanMarkers(context.Background(), p, []string{"hit your", "/rc active", "api error", "never there"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"hit your", "/rc active", "api error"} {
		if !got[m] {
			t.Errorf("%q not found (boundary, case-fold or end-of-file handling is wrong)", m)
		}
	}
	if got["never there"] {
		t.Error("a marker that is absent was reported present")
	}
}

func TestScanMarkersHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := fakeCandidate(t, "1.0.0", "x")
	if _, err := scanMarkers(ctx, p, []string{"x"}); err == nil {
		t.Error("a cancelled scan must return an error, not a verdict")
	}
}

func TestResolveCandidate(t *testing.T) {
	good := fakeCandidate(t, "1.0.0", "x")
	if got, err := resolveCandidate(good); err != nil || got != good {
		t.Errorf("resolveCandidate(%q) = %q, %v", good, got, err)
	}
	// A symlink resolves to its target, so a link swapped later cannot change
	// the binary under test.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCandidate(link); err != nil || got != good {
		t.Errorf("symlink resolved to %q, %v; want %q", got, err, good)
	}
	notExec := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty":          "",
		"relative":       "claude",
		"dot-relative":   "./claude",
		"missing":        filepath.Join(t.TempDir(), "nope"),
		"a directory":    t.TempDir(),
		"not executable": notExec,
	} {
		if _, err := resolveCandidate(in); err == nil {
			t.Errorf("%s (%q): want a usage error", name, in)
		}
	}
}

func TestParseVersionLine(t *testing.T) {
	cases := map[string]string{
		"2.1.281 (Claude Code)\n":      "2.1.281",
		"\n  9.9.9 (Synthetic)  \nx\n": "9.9.9",
		"1.2.3":                        "1.2.3",
		"":                             "",
		"\n\n":                         "",
	}
	for in, want := range cases {
		if got := parseVersionLine(in); got != want {
			t.Errorf("parseVersionLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The candidate's environment is the whole of it. A variable the caller
// happened to have set must never reach the runtime under test.
func TestCompatBaseEnvIsMinimal(t *testing.T) {
	got := compatBaseEnv(env("HOME", "/h", "USER", "u", "SHELL", "/bin/zsh", "SECRET_TOKEN", "leak", "CLAUDE_CODE_SESSION_KIND", "bg", "TMUX", "/x,1,0"))
	joined := strings.Join(got, "\n")
	for _, must := range []string{"HOME=/h", "USER=u", "LOGNAME=u", "SHELL=/bin/zsh", "TERM=xterm-256color", "DISABLE_UPDATES=1", "DISABLE_AUTOUPDATER=1", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"} {
		if !strings.Contains(joined, must) {
			t.Errorf("environment lacks %s\n%s", must, joined)
		}
	}
	for _, mustNot := range []string{"SECRET_TOKEN", "CLAUDE_CODE_SESSION_KIND", "TMUX="} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("environment leaks %s\n%s", mustNot, joined)
		}
	}
}

func TestCandidateArchOfARealExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	got := candidateArch(exe)
	want := map[string]string{"amd64": "x86_64", "arm64": "arm64", "386": "i386", "arm": "arm"}[runtime.GOARCH]
	if want != "" && got != want {
		t.Errorf("candidateArch(test binary) = %q, want %q", got, want)
	}
	if got := candidateArch(fakeCandidate(t, "1.0.0", "x")); got != "unknown" {
		t.Errorf("a script's arch = %q, want unknown", got)
	}
}

func TestDescribeCandidate(t *testing.T) {
	p := fakeCandidate(t, "9.9.9", "x")
	c, err := describeCandidate(context.Background(), p, p, env("HOME", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "9.9.9" || c.Path != p || c.Resolved != p || len(c.Sha256) != 64 || c.Arch == "" {
		t.Errorf("candidate = %+v", c)
	}
	// A candidate that cannot say what it is still reports what was learned.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err = describeCandidate(context.Background(), broken, broken, env("HOME", t.TempDir()))
	if err == nil {
		t.Fatal("a candidate whose --version fails must not be identified")
	}
	if c.Sha256 == "" {
		t.Error("the digest should survive a failed --version")
	}
}

// End to end without a runtime: the static checks pass on a candidate that
// carries the wording, fail (as warnings) on one that does not, and every
// detail names the driver code that relies on it.
func TestRunCompatStaticChecks(t *testing.T) {
	ctx := context.Background()
	opts := func(claude string, only ...string) CompatOptions {
		return CompatOptions{Claude: claude, Only: only, Getenv: env("HOME", t.TempDir()), Build: compat.Build{Version: "v0", Commit: "abc"}}
	}

	rep, err := RunCompat(ctx, opts(fakeCandidate(t, "9.9.9", allMarkersText()), "F-LIMIT", "F-APIERR", "H-RC"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != compat.Schema || rep.Claude.Version != "9.9.9" || rep.ColabFleet.Commit != "abc" || len(rep.Only) != 3 {
		t.Errorf("report header = %+v", rep)
	}
	if !rep.Pass || rep.ExitCode() != 0 || len(rep.Checks) != 3 {
		t.Fatalf("report = %+v, want 3 passing checks", rep)
	}
	for _, c := range rep.Checks {
		if !c.Pass || !strings.Contains(c.Detail, "relied on by") {
			t.Errorf("%s = %+v", c.ID, c)
		}
	}

	rep, err = RunCompat(ctx, opts(fakeCandidate(t, "9.9.9", "nothing relevant here"), "F-LIMIT"))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Checks[0]
	if c.Pass || c.Error || !strings.Contains(c.Detail, `"hit your"`) {
		t.Errorf("F-LIMIT on a candidate without the wording = %+v, want a behavioural failure naming what is missing", c)
	}
	// A warn check failing never changes the verdict.
	if !rep.Pass || rep.ExitCode() != 0 {
		t.Errorf("a failing warn check changed the verdict: %+v", rep)
	}
}

func TestRunCompatRefusesBadInvocations(t *testing.T) {
	ctx := context.Background()
	good := fakeCandidate(t, "1.0.0", "x")
	base := func() CompatOptions { return CompatOptions{Claude: good, Getenv: env("HOME", t.TempDir())} }
	cases := map[string]func(o *CompatOptions){
		"relative path":            func(o *CompatOptions) { o.Claude = "claude" },
		"missing path":             func(o *CompatOptions) { o.Claude = filepath.Join(t.TempDir(), "nope") },
		"unknown --only":           func(o *CompatOptions) { o.Only = []string{"NOT-A-CHECK"} },
		"a typo in --only":         func(o *CompatOptions) { o.Only = []string{"F-LIMT"} },
		"--only naming nothing":    func(o *CompatOptions) { o.Only = []string{" ", ""} },
		"--only for an unbuilt id": func(o *CompatOptions) { o.Only = []string{"D1"} },
	}
	for name, mutate := range cases {
		o := base()
		mutate(&o)
		if rep, err := RunCompat(ctx, o); err == nil {
			t.Errorf("%s: want an error, got report %+v", name, rep)
		} else if rep.Schema != 0 {
			t.Errorf("%s: a bad invocation must produce no report", name)
		}
	}
}

// A candidate that cannot say what it is is a report of errors — retryable —
// not a disappearing act and not a verdict.
func TestRunCompatUnidentifiableCandidateIsAllErrors(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := RunCompat(context.Background(), CompatOptions{Claude: broken, Getenv: env("HOME", t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("no checks in the report")
	}
	for _, c := range rep.Checks {
		if !c.Error || c.Pass || !strings.Contains(c.Detail, "could not identify the candidate") {
			t.Errorf("%s = %+v, want error", c.ID, c)
		}
	}
}

func TestRunCompatWritesThePack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pack")
	rep, err := RunCompat(context.Background(), CompatOptions{
		Claude: fakeCandidate(t, "9.9.9", allMarkersText()), PackDir: dir, Only: []string{"H-RC"}, Getenv: env("HOME", t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got compat.Report
	if err := json.Unmarshal(b, &got); err != nil || got.Claude.Version != rep.Claude.Version || len(got.Checks) != 1 {
		t.Errorf("pack report = %+v, %v", got, err)
	}
	if fi, _ := os.Stat(dir); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("pack dir mode = %v, want 0700", fi)
	}
	// Reusing a directory that already has contents is refused: two runs'
	// evidence in one pack describes no candidate.
	if _, err := RunCompat(context.Background(), CompatOptions{Claude: fakeCandidate(t, "9.9.9", "x"), PackDir: dir, Getenv: env("HOME", t.TempDir())}); err == nil {
		t.Error("a non-empty --pack directory must be refused")
	}
}

// Every catalogue entry names driver code that depends on the behaviour it
// asserts. If that code is renamed or removed, the entry points at nothing and
// a failing report would tell its reader to look somewhere that no longer
// exists — so each "<file>#<identifier>" is resolved against the source.
func TestReliedOnEntriesResolveToSource(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, spec := range compat.Catalogue() {
		for _, entry := range spec.ReliedOn {
			file, ident, ok := strings.Cut(entry, "#")
			if !ok || file == "" || ident == "" {
				t.Errorf("%s: %q is not <file>#<identifier>", spec.ID, entry)
				continue
			}
			if !topLevelIdentifiers(t, filepath.Join(root, file))[ident] {
				t.Errorf("%s: %s has no top-level %q — the code this check protects was renamed or removed; update the catalogue", spec.ID, file, ident)
			}
		}
	}
}

// topLevelIdentifiers lists the package-level names a Go file declares:
// functions (not methods), types, variables and constants.
func topLevelIdentifiers(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				out[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					out[sp.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

// The suite implements exactly the catalogue: a catalogued check with no
// evaluator would be silently absent from every report, and an evaluator with no
// catalogue row could not be gated or documented.
func TestCompatSuiteCoversTheCatalogue(t *testing.T) {
	suite := newCompatSuite(&compatHarness{})
	if err := suite.Validate(); err != nil {
		t.Fatalf("the suite is inconsistent: %v", err)
	}
	have := map[string]bool{}
	for _, c := range suite.Checks {
		have[c.ID] = true
	}
	for _, s := range compat.Catalogue() {
		if !have[s.ID] {
			t.Errorf("%s is in the catalogue and has no check in the suite", s.ID)
		}
		delete(have, s.ID)
	}
	for id := range have {
		t.Errorf("the suite has a check %q that is not in the catalogue", id)
	}
}
