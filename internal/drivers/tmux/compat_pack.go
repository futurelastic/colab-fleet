package tmux

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/godx-jp/colab-fleet/internal/compat"
)

// compatPack is the directory `--pack` writes: the raw evidence each check
// produced, so a tool outside this repository can replay its own classifiers
// over the same states without driving the candidate again. What those tools
// are is none of this repository's business.
//
// It is local data, created 0700. Captures that reach it pass through
// RedactCapture first (see the step that adds them); nothing here is meant to
// leave the machine unread.
type compatPack struct{ dir string }

// newCompatPack creates (or reuses) the directory. It refuses one that already
// holds anything else: mixing a run's evidence into a directory that has other
// contents is how a pack ends up describing two different candidates.
func newCompatPack(dir string) (*compatPack, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, compatUsagef("--pack %q: %v", dir, err)
	}
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return nil, compatUsagef("--pack %q already has contents; give a new or empty directory", dir)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, compatUsagef("--pack %q: %v", dir, err)
	}
	return &compatPack{dir: abs}, nil
}

// writeReport stores the finished report alongside the evidence.
func (p *compatPack) writeReport(r compat.Report) error {
	f, err := os.OpenFile(filepath.Join(p.dir, "report.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing the report into the pack: %w", err)
	}
	defer f.Close()
	return compat.WriteJSON(f, r)
}
