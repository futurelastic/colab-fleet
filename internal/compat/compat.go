// Package compat is the contract of `colab-fleetd compat` (colab-fleet #183):
// the versioned report the command prints, the catalogue of check IDs behind
// it, and the generic runner that turns "drive a candidate binary into some
// states" plus "judge what was seen" into that report.
//
// # What the command answers
//
// One question: does THIS candidate build of the agent runtime still behave
// the way this service assumes? The service does not call an API. It reads a
// terminal UI and a few files the runtime writes, and none of that is a
// published contract, so a new runtime release can change any of it and the
// first sign is a fleet that has quietly stopped delivering or classifying.
// The command checks a build before the fleet takes it. It only reports:
// installing, pinning and promoting a build are somebody else's decision.
//
// # Why this package imports nothing from the repository
//
// The report is a public, versioned contract that callers read as JSON. The
// checks themselves have to live next to the driver code they exercise (most
// of what they call is unexported), which is a very large package; keeping
// the contract here means it can be read, tested and reviewed on its own, and
// the driver package never has to grow an exported surface for one caller.
//
// # Versioning rule
//
// Any change to the shape bumps Schema. Adding a field does not. A check ID
// is stable once shipped: renaming or removing one is a schema bump, and the
// shipped-IDs test exists so that it cannot happen by accident. See
// docs/compat.md for the rule in full.
package compat

import (
	"sort"
)

// Schema is the report's schema number. It appears in every report as
// "schema". See the package comment for when it changes.
const Schema = 1

// Gate says what a failing check means for the verdict.
type Gate string

const (
	// GateMust is a behaviour the driver needs in order to deliver, confirm,
	// classify a dialog or identify a session. A failure here rejects the
	// candidate.
	GateMust Gate = "must"
	// GateWarn is cosmetic, or a path that is not live yet. It is reported
	// and never changes the verdict.
	GateWarn Gate = "warn"
)

// isMust treats an unrecognised gate as must: a check that somehow lost its
// gate must not be able to pass a candidate by being ignored.
func isMust(g Gate) bool { return g != GateWarn }

// Candidate identifies the binary that was checked.
type Candidate struct {
	// Path is the path the caller supplied.
	Path string `json:"path"`
	// Version is what the candidate itself reports for --version.
	Version string `json:"version"`
	// Resolved is the symlink-resolved path the candidate was actually
	// launched by, so a link swapped mid-run cannot change the binary under
	// test.
	Resolved string `json:"resolved,omitempty"`
	// Sha256 is the hex digest of the file at Resolved.
	Sha256 string `json:"sha256,omitempty"`
	// Arch is the machine architecture named in the binary's own header.
	Arch string `json:"arch,omitempty"`
}

// Build identifies the colab-fleet code that ran the checks.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Modified reports a binary built from a tree with uncommitted changes,
	// which has no meaningful identity.
	Modified bool `json:"modified,omitempty"`
}

// Result is one check's outcome.
//
// Pass and Error are deliberately separate. Error means the check could not
// run: the environment failed, or a timeout left no evidence either way. It
// is reported apart from a behavioural failure so a caller can retry an error
// and reject a failure. Error implies !Pass.
type Result struct {
	ID     string `json:"id"`
	Gate   Gate   `json:"gate"`
	Pass   bool   `json:"pass"`
	Error  bool   `json:"error"`
	Detail string `json:"detail"`
	// ReliedOn names the driver code that depends on the behaviour this
	// check asserts. Additive.
	ReliedOn []string `json:"reliedOn,omitempty"`
}

// Report is the schema 1 document.
//
// The required core, which a caller may rely on, is schema, claude.path,
// claude.version, checks[].id, checks[].pass, checks[].detail and pass.
// Everything else is additive.
type Report struct {
	Schema     int       `json:"schema"`
	Claude     Candidate `json:"claude"`
	ColabFleet Build     `json:"colabFleet"`
	Checks     []Result  `json:"checks"`
	// Turns is the number of model turns the run spent. Synthetic,
	// nonce-tagged prompts only.
	Turns int `json:"turns"`
	// Pass is true when every must check passed with no must error.
	Pass bool `json:"pass"`
	// Only is present when the run was restricted with --only. Such a report
	// is a partial run, not a certification.
	Only       []string `json:"only,omitempty"`
	DurationMs int64    `json:"durationMs"`
}

// Finalize makes a Report internally consistent: it stamps the schema,
// takes every check's gate and relied-on list from the catalogue (a check
// cannot choose its own gate), forces Error to imply !Pass, puts the checks in
// catalogue order, and computes Pass.
//
// Pass is false for a report with no checks at all: a run that judged nothing
// has certified nothing.
func (r *Report) Finalize() {
	r.Schema = Schema
	for i := range r.Checks {
		c := &r.Checks[i]
		if s, ok := Lookup(c.ID); ok {
			c.Gate = s.Gate
			c.ReliedOn = append([]string(nil), s.ReliedOn...)
		}
		if c.Error {
			c.Pass = false
		}
	}
	order := map[string]int{}
	for i, s := range catalogue {
		order[s.ID] = i
	}
	sort.SliceStable(r.Checks, func(i, j int) bool {
		a, aok := order[r.Checks[i].ID]
		b, bok := order[r.Checks[j].ID]
		switch {
		case aok && bok:
			return a < b
		case aok != bok:
			return aok
		default:
			return r.Checks[i].ID < r.Checks[j].ID
		}
	})
	r.Pass = len(r.Checks) > 0
	for _, c := range r.Checks {
		if isMust(c.Gate) && (!c.Pass || c.Error) {
			r.Pass = false
		}
	}
}

// ExitCode maps a finalized report to the process exit code:
//
//	0  every must check passed
//	1  a must check failed
//	2  could not certify: a must check errored (or nothing was judged)
//
// A proven failure outranks "could not certify" when both are present, because
// retrying cannot clear it. A bad invocation also exits 2 but produces no
// report at all, so it is not decided here.
func (r Report) ExitCode() int {
	failed, errored := false, false
	for _, c := range r.Checks {
		if !isMust(c.Gate) {
			continue
		}
		switch {
		case c.Error:
			errored = true
		case !c.Pass:
			failed = true
		}
	}
	switch {
	case failed:
		return 1
	case errored || len(r.Checks) == 0:
		return 2
	}
	return 0
}
