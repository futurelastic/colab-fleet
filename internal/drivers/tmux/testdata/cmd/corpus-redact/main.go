// Command corpus-redact is the one sanctioned way a real pane capture
// enters this repository's corpus (internal/drivers/tmux/testdata/corpus).
//
// It never writes a case.json — that file's "for" field is the human
// judgement this tool cannot supply (why the case matters, what it is
// evidence for) — but it does everything mechanical: reads a raw capture
// from a scratch file OUTSIDE this repo, redacts it with the same
// tmux.RedactCapture function CI re-checks at commit time, writes
// testdata/corpus/<name>/screen.txt, and prints a case.json STUB with the
// fields a human still has to fill in.
//
// # Why a tool, and not "redact it by hand before committing"
//
// A human editing a real capture by eye is the failure mode this corpus
// exists to prevent one level up: redact.go's own package comment states it
// plainly — a redactor that has to be told about every way content can
// appear will eventually miss one, in a repository that cannot un-publish a
// commit. Running the SAME function the CI gate re-checks, at curation
// time, means the file that lands in git is already a fixed point of
// redaction before a human ever looks at it — the human's job becomes
// reviewing the manifest, not performing the redaction.
//
// Usage:
//
//	go run ./internal/drivers/tmux/testdata/cmd/corpus-redact <raw-capture-file> <case-name>
//
// The raw capture file is read and otherwise untouched — never copied
// anywhere inside this repository, never committed. Keep it in a scratch
// location outside the working tree (or under a `*.rawcapture` name, which
// .gitignore excludes on every checkout as a second line of defence).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	tmux "github.com/godx-jp/colab-fleet/internal/drivers/tmux"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: corpus-redact <raw-capture-file> <case-name>")
		os.Exit(2)
	}
	rawPath, name := os.Args[1], os.Args[2]

	raw, err := os.ReadFile(rawPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus-redact: reading %s: %v\n", rawPath, err)
		os.Exit(1)
	}

	redacted := tmux.RedactCapture(string(raw))

	dir := filepath.Join("internal", "drivers", "tmux", "testdata", "corpus", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-redact: %v\n", err)
		os.Exit(1)
	}
	screenPath := filepath.Join(dir, "screen.txt")
	if err := os.WriteFile(screenPath, []byte(redacted), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-redact: writing %s: %v\n", screenPath, err)
		os.Exit(1)
	}

	casePath := filepath.Join(dir, "case.json")
	if _, err := os.Stat(casePath); err == nil {
		fmt.Fprintf(os.Stderr, "corpus-redact: wrote %s; %s already exists, leaving it alone\n", screenPath, casePath)
		return
	}
	stub := fmt.Sprintf(`{
  "name": %q,
  "for": "TODO — why does this case exist, and what property does it pin down? A case nobody can explain is a case nobody can safely change.",
  "source": "TODO — issue or finding number",
  "redactedFrom": "real capture, redacted by corpus-redact from %s",
  "alive": true,
  "observations": [
    { "afterSeconds": 0, "young": false, "want": "TODO" }
  ]
}
`, name, filepath.Base(rawPath))
	if err := os.WriteFile(casePath, []byte(stub), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-redact: writing %s: %v\n", casePath, err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s\nwrote %s (fill in \"for\", \"source\" and every \"want\" before committing)\n"+
		"review the redacted screen below before running `git add` — this tool discards by default, "+
		"but a human still has to confirm the manifest is right:\n\n%s\n",
		screenPath, casePath, redacted)
}
