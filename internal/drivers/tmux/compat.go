package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/godx-jp/colab-fleet/internal/compat"
)

// colab-fleet #183: `colab-fleetd compat`.
//
// # What this is
//
// A check of a candidate build of the agent runtime against the assumptions
// this driver makes about it. The driver does not call an API — it reads a
// terminal UI and a few files the runtime writes, none of which is a published
// contract — so a new runtime release can change any of it, and the first sign
// is a fleet that has quietly stopped delivering or classifying. This asks
// before the fleet takes the build.
//
// # Why it lives in this package
//
// Almost everything a check needs is unexported here: the classifier, the
// composer reader, the transcript and process-record readers, the command
// builder. Exporting ~25 of them for one caller would permanently widen the
// driver's API and invite other code to depend on classifier internals. So the
// checks sit beside the code they exercise and this file exports exactly two
// names: CompatOptions and RunCompat. The report contract itself — types,
// catalogue, runner, exit codes — is in internal/compat, which imports nothing
// from this repository.
//
// The command only reports. It never installs, pins, switches or promotes a
// build; that decision belongs to whoever reads the report.

// CompatOptions configures RunCompat.
type CompatOptions struct {
	// Claude is the candidate binary: an absolute path. It is launched by that
	// path — resolved through symlinks first — and never looked up on PATH,
	// because a login shell's PATH would silently put the installed build back
	// in front of the candidate.
	Claude string
	// Only restricts the run to these check IDs (already validated by the
	// caller, or refused here). Empty runs every check.
	Only []string
	// PackDir, when set, receives the raw evidence each check produced.
	PackDir string
	// Build identifies the colab-fleet code running the checks.
	Build compat.Build
	// Log receives progress lines. Nil discards.
	Log io.Writer
	// Getenv reads the environment; nil means os.Getenv. Injectable so a test
	// cannot be influenced by the machine it runs on.
	Getenv func(string) string
}

// RunCompat checks the candidate and returns the schema 1 report.
//
// The error is for an invocation that cannot be run at all — the path is not
// an absolute path to an executable file, or --only names a check this build
// has no implementation for. Those produce no report. Every other problem,
// including a candidate that cannot be identified or an isolated multiplexer
// that cannot start, is a REPORT whose checks are error:true, because "could
// not run" must reach the caller as something it can retry, not as a
// disappearing act.
//
// A non-nil error may accompany a non-zero report: a --pack that could not be
// written is reported as an error, but never hides the verdict it belongs to.
func RunCompat(ctx context.Context, o CompatOptions) (compat.Report, error) {
	started := time.Now()
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	only, err := compat.ResolveOnly(o.Only)
	if err != nil {
		return compat.Report{}, err
	}
	resolved, err := resolveCandidate(o.Claude)
	if err != nil {
		return compat.Report{}, err
	}

	h := &compatHarness{getenv: getenv, log: o.Log}
	suite := newCompatSuite(h)
	// Validate the suite and --only before any expensive work, so a typo costs
	// nothing.
	if err := suite.Validate(); err != nil {
		return compat.Report{}, fmt.Errorf("internal: the compat suite is inconsistent: %w", err)
	}
	if _, err := compat.ErroredAll(suite, only, ""); err != nil {
		return compat.Report{}, err
	}

	var pack *compatPack
	if o.PackDir != "" {
		if pack, err = newCompatPack(o.PackDir); err != nil {
			return compat.Report{}, err
		}
	}

	rep := compat.Report{ColabFleet: o.Build, Only: only}
	var results []compat.Result
	cand, ierr := describeCandidate(ctx, o.Claude, resolved, getenv)
	rep.Claude = cand
	if ierr != nil {
		// Nothing can be judged about a build that cannot even say what it is.
		results, err = compat.ErroredAll(suite, only, "could not identify the candidate: "+ierr.Error())
	} else {
		h.cand = cand
		results, err = compat.Run(ctx, suite, compat.RunOptions{Only: only, Log: o.Log})
	}
	if err != nil {
		return compat.Report{}, err
	}
	rep.Checks = results
	rep.Turns = h.turns
	rep.DurationMs = time.Since(started).Milliseconds()
	rep.Finalize()

	if pack != nil {
		if perr := pack.writeReport(rep); perr != nil {
			return rep, fmt.Errorf("--pack: %w", perr)
		}
	}
	return rep, nil
}

// compatHarness holds what one run learns. Probes write into it; check
// evaluators read from it. Nothing in it is shared between runs.
type compatHarness struct {
	cand   compat.Candidate
	getenv func(string) string
	log    io.Writer

	// turns counts model turns spent. Every model prompt goes through one
	// helper that increments it, so the report's number is the real one.
	turns int

	// markers holds which static marker strings the candidate contains.
	markers map[string]bool
}

func (h *compatHarness) logf(format string, a ...any) {
	if h.log != nil {
		fmt.Fprintf(h.log, format+"\n", a...)
	}
}

// newCompatSuite assembles every probe and check this build implements. A step
// that adds checks adds its probes and checks here, together with their
// catalogue rows and their rows in docs/compat.md.
func newCompatSuite(h *compatHarness) compat.Suite {
	var s compat.Suite
	h.addStatic(&s)
	return s
}

// compatUsagef builds the error for an invocation that cannot be run. It is
// a plain error on purpose: the caller prints it and exits 2, and no other
// package needs to tell it apart from a runtime failure.
func compatUsagef(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// trimLine returns the first non-empty line of s, trimmed.
func trimLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
