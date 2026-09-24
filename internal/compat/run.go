package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Verdict is what a check's evaluator returns.
type Verdict struct {
	Pass bool
	// Err means the evidence needed to judge was missing: the check could not
	// run. It is not a behavioural failure and is reported apart from one.
	Err    bool
	Detail string
}

// Passed, Failed and Errored build the three kinds of Verdict.
func Passed(detail string) Verdict  { return Verdict{Pass: true, Detail: detail} }
func Failed(detail string) Verdict  { return Verdict{Detail: detail} }
func Errored(detail string) Verdict { return Verdict{Err: true, Detail: detail} }

// Probe drives a shared session into a named state and stores what it saw.
//
// A probe is the expensive half of "drive once, judge many": one booted
// session, one screen state or one send serves every check that reads it.
// A probe returns an error only when the ENVIRONMENT failed (a multiplexer
// error, a timeout, an isolation breach). A behavioural outcome — a refused
// send, no composer on screen — is evidence to record, not an error, so the
// check that reads it can report a failure instead of "could not run".
type Probe struct {
	ID string
	// Stage orders probes: lower runs first. Within a stage, declaration
	// order.
	Stage int
	// Needs names probes that must have completed first.
	Needs []string
	// Budget bounds the probe; the run's overall deadline bounds it too and
	// the shorter wins. Zero means "the overall deadline only".
	Budget time.Duration
	Run    func(ctx context.Context) error
}

// Check judges what probes recorded. It never touches the multiplexer.
type Check struct {
	// ID must be in the catalogue.
	ID string
	// Probes names every probe whose evidence Eval reads.
	Probes []string
	Eval   func() Verdict
}

// Suite is a set of probes and the checks that read them.
type Suite struct {
	Probes []Probe
	Checks []Check
}

// RunOptions tunes Run.
type RunOptions struct {
	// Only restricts the run to these check IDs; the probes they need are
	// pulled in automatically. Empty means every check in the suite.
	Only []string
	// Log receives one line per probe and per skipped probe. Nil discards.
	Log io.Writer
}

// ResolveOnly validates an --only list against the catalogue. An ID the
// catalogue does not know is a usage error: silently running nothing for a
// typo is the failure class this command exists to prevent.
func ResolveOnly(ids []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if _, ok := Lookup(id); !ok {
			return nil, fmt.Errorf("unknown check id %q", id)
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 && len(ids) > 0 {
		return nil, errors.New("--only names no check")
	}
	return out, nil
}

// Validate checks that a suite is well formed. Run calls it, and a test
// calls it on the real suite.
func (s Suite) Validate() error {
	probes := map[string]int{}
	for i, p := range s.Probes {
		if p.ID == "" || p.Run == nil {
			return fmt.Errorf("probe #%d: needs an ID and a Run", i)
		}
		if _, dup := probes[p.ID]; dup {
			return fmt.Errorf("duplicate probe %q", p.ID)
		}
		probes[p.ID] = i
	}
	for _, p := range s.Probes {
		for _, n := range p.Needs {
			j, ok := probes[n]
			if !ok {
				return fmt.Errorf("probe %q needs unknown probe %q", p.ID, n)
			}
			if s.Probes[j].Stage > p.Stage || (s.Probes[j].Stage == p.Stage && j > probes[p.ID]) {
				return fmt.Errorf("probe %q needs %q, which would run after it", p.ID, n)
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Checks {
		if _, ok := Lookup(c.ID); !ok {
			return fmt.Errorf("check %q is not in the catalogue", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate check %q", c.ID)
		}
		seen[c.ID] = true
		if c.Eval == nil {
			return fmt.Errorf("check %q has no Eval", c.ID)
		}
		for _, n := range c.Probes {
			if _, ok := probes[n]; !ok {
				return fmt.Errorf("check %q reads unknown probe %q", c.ID, n)
			}
		}
	}
	return nil
}

// outcome is what happened to one probe.
type outcome struct {
	ran  bool
	err  error  // environment failure while running
	skip string // why it did not run, when it did not
}

func (o outcome) ok() bool { return o.ran && o.err == nil }

func (o outcome) why() string {
	switch {
	case o.err != nil:
		return o.err.Error()
	case o.skip != "":
		return o.skip
	}
	return "did not run"
}

// Run executes the suite and returns one Result per selected check, in suite
// order (Report.Finalize puts them in catalogue order). Probes run one at a
// time, in stage order. Every result carries the "relied on by" suffix.
//
// Cancelling ctx (a signal, or the overall deadline) skips the probes that
// have not started; each affected check then reports error with the reason.
// A probe that panics is recovered and counted as an environment error.
func Run(ctx context.Context, s Suite, o RunOptions) ([]Result, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	selected, err := selectChecks(s, o.Only)
	if err != nil {
		return nil, err
	}

	byID := map[string]Probe{}
	for _, p := range s.Probes {
		byID[p.ID] = p
	}
	need := map[string]bool{}
	var pull func(id string)
	pull = func(id string) {
		if need[id] {
			return
		}
		need[id] = true
		for _, n := range byID[id].Needs {
			pull(n)
		}
	}
	for _, c := range selected {
		for _, id := range c.Probes {
			pull(id)
		}
	}
	var order []Probe
	for _, p := range s.Probes {
		if need[p.ID] {
			order = append(order, p)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Stage < order[j].Stage })

	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	done := map[string]outcome{}
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			done[p.ID] = outcome{skip: "run cancelled before it started: " + cause(ctx)}
			logf("skip  %s: %s", p.ID, done[p.ID].skip)
			continue
		}
		var blocked string
		for _, n := range p.Needs {
			if !done[n].ok() {
				blocked = fmt.Sprintf("prerequisite probe %s did not complete", n)
				break
			}
		}
		if blocked != "" {
			done[p.ID] = outcome{skip: blocked}
			logf("skip  %s: %s", p.ID, blocked)
			continue
		}
		start := time.Now()
		err := runProbe(ctx, p)
		done[p.ID] = outcome{ran: true, err: err}
		if err != nil {
			logf("error %s (%s): %v", p.ID, time.Since(start).Round(time.Millisecond), err)
		} else {
			logf("ok    %s (%s)", p.ID, time.Since(start).Round(time.Millisecond))
		}
	}

	var results []Result
	for _, c := range selected {
		results = append(results, evaluate(c, done))
	}
	return results, nil
}

// selectChecks returns the suite's checks that --only asks for (all of them
// when only is empty). An ID the catalogue does not know, or one it knows but
// this build has no implementation for, is an error: silently running nothing
// for a typo is the failure class this command exists to prevent.
func selectChecks(s Suite, only []string) ([]Check, error) {
	want := map[string]bool{}
	for _, id := range only {
		if _, ok := Lookup(id); !ok {
			return nil, fmt.Errorf("unknown check id %q", id)
		}
		want[id] = true
	}
	var selected []Check
	have := map[string]bool{}
	for _, c := range s.Checks {
		have[c.ID] = true
		if len(want) == 0 || want[c.ID] {
			selected = append(selected, c)
		}
	}
	for id := range want {
		if !have[id] {
			return nil, fmt.Errorf("check %q is in the catalogue but has no implementation in this build", id)
		}
	}
	return selected, nil
}

// ErroredAll returns an error Result for every check the run would have
// selected, all carrying the same reason. It is for the case where nothing can
// be judged at all (the candidate cannot be identified, the isolated
// multiplexer cannot start): each check then reports "could not run" — never a
// pass and never a failure — so a caller retries instead of rejecting.
func ErroredAll(s Suite, only []string, why string) ([]Result, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	selected, err := selectChecks(s, only)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, c := range selected {
		spec, _ := Lookup(c.ID)
		out = append(out, Result{
			ID: c.ID, Gate: spec.Gate, Error: true,
			Detail:   withReliedOn(why, spec.ReliedOn),
			ReliedOn: spec.ReliedOn,
		})
	}
	return out, nil
}

// cause reports why ctx ended, distinguishing the deadline from a signal.
func cause(ctx context.Context) string {
	if c := context.Cause(ctx); c != nil {
		return c.Error()
	}
	return ctx.Err().Error()
}

// runProbe runs one probe under min(its own budget, the time left before the
// run's deadline), converting a panic into an error.
func runProbe(ctx context.Context, p Probe) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("probe %s panicked: %v", p.ID, r)
		}
	}()
	budget := p.Budget
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		if left <= 0 {
			return errors.New("the run's overall deadline had already passed")
		}
		if budget == 0 || left < budget {
			budget = left
		}
	}
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	return p.Run(ctx)
}

// evaluate turns a check plus the probe outcomes into a Result. A check whose
// probe did not complete is an error, never a pass or a failure: there is no
// evidence either way.
func evaluate(c Check, done map[string]outcome) (res Result) {
	spec, _ := Lookup(c.ID)
	res = Result{ID: c.ID, Gate: spec.Gate}
	defer func() {
		res.Detail = withReliedOn(res.Detail, spec.ReliedOn)
		res.ReliedOn = spec.ReliedOn
	}()
	for _, id := range c.Probes {
		if o := done[id]; !o.ok() {
			res.Error = true
			res.Detail = fmt.Sprintf("prerequisite probe %s did not complete: %s", id, o.why())
			return res
		}
	}
	var v Verdict
	func() {
		defer func() {
			if r := recover(); r != nil {
				v = Errored(fmt.Sprintf("the check panicked: %v", r))
			}
		}()
		v = c.Eval()
	}()
	res.Detail = v.Detail
	switch {
	case v.Err:
		res.Error = true
	default:
		res.Pass = v.Pass
	}
	return res
}

// withReliedOn appends the driver code the check protects, so a failing
// report says what would break, not only what was seen.
func withReliedOn(detail string, reliedOn []string) string {
	if len(reliedOn) == 0 {
		return detail
	}
	if detail == "" {
		return "relied on by: " + strings.Join(reliedOn, ", ")
	}
	return detail + " · relied on by: " + strings.Join(reliedOn, ", ")
}
