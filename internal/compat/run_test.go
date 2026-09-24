package compat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorder collects the order probes ran in.
type recorder struct{ ran []string }

func (r *recorder) probe(id string, stage int, needs ...string) Probe {
	return Probe{ID: id, Stage: stage, Needs: needs, Run: func(context.Context) error {
		r.ran = append(r.ran, id)
		return nil
	}}
}

func pass(d string) func() Verdict { return func() Verdict { return Passed(d) } }

func byID(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

func TestRunOnlyPullsInNeededProbes(t *testing.T) {
	var rec recorder
	s := Suite{
		Probes: []Probe{rec.probe("p1", 1), rec.probe("p2", 1), rec.probe("p3", 1)},
		Checks: []Check{
			{ID: "F-LIMIT", Probes: []string{"p1"}, Eval: pass("a")},
			{ID: "F-APIERR", Probes: []string{"p2"}, Eval: pass("b")},
			{ID: "H-RC", Probes: []string{"p3"}, Eval: pass("c")},
		},
	}
	got, err := Run(context.Background(), s, RunOptions{Only: []string{"F-APIERR"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.ran, []string{"p2"}) {
		t.Errorf("probes run = %v, want only p2", rec.ran)
	}
	if len(got) != 1 || got[0].ID != "F-APIERR" {
		t.Errorf("results = %+v, want only F-APIERR", got)
	}
}

func TestRunNeedsRunFirstAndStagesOrder(t *testing.T) {
	var rec recorder
	s := Suite{
		Probes: []Probe{rec.probe("late", 5), rec.probe("mid", 3, "early"), rec.probe("early", 1)},
	}
	// Validate rejects a need that is declared after its dependant within the
	// same or a later stage only when it would really run after it; here
	// "early" has a lower stage, so it is fine even though declared later.
	s.Checks = []Check{{ID: "F-LIMIT", Probes: []string{"late", "mid"}, Eval: pass("x")}}
	if _, err := Run(context.Background(), s, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"early", "mid", "late"}; !reflect.DeepEqual(rec.ran, want) {
		t.Errorf("order = %v, want %v", rec.ran, want)
	}
}

func TestRunProbeErrorMakesReadersErrorNotFail(t *testing.T) {
	boom := errors.New("multiplexer went away")
	var ran []string
	s := Suite{
		Probes: []Probe{
			{ID: "p1", Stage: 1, Run: func(context.Context) error { ran = append(ran, "p1"); return boom }},
			{ID: "p2", Stage: 2, Needs: []string{"p1"}, Run: func(context.Context) error { ran = append(ran, "p2"); return nil }},
			{ID: "p3", Stage: 2, Run: func(context.Context) error { ran = append(ran, "p3"); return nil }},
		},
		Checks: []Check{
			{ID: "F-LIMIT", Probes: []string{"p1"}, Eval: pass("unreachable")},
			{ID: "F-APIERR", Probes: []string{"p2"}, Eval: pass("unreachable")},
			{ID: "H-RC", Probes: []string{"p3"}, Eval: pass("independent probe still runs")},
		},
	}
	got, err := Run(context.Background(), s, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := byID(got)
	if !r["F-LIMIT"].Error || r["F-LIMIT"].Pass || !strings.Contains(r["F-LIMIT"].Detail, "multiplexer went away") {
		t.Errorf("F-LIMIT = %+v, want error carrying the probe's error", r["F-LIMIT"])
	}
	if !r["F-APIERR"].Error || !strings.Contains(r["F-APIERR"].Detail, "prerequisite probe p2 did not complete") {
		t.Errorf("F-APIERR = %+v, want error about the skipped probe", r["F-APIERR"])
	}
	if !r["H-RC"].Pass {
		t.Errorf("H-RC = %+v, an unrelated probe must still run", r["H-RC"])
	}
	if reflect.DeepEqual(ran, []string{"p1", "p2", "p3"}) {
		t.Errorf("p2 ran although its prerequisite failed: %v", ran)
	}
}

func TestRunRecoversPanics(t *testing.T) {
	s := Suite{
		Probes: []Probe{{ID: "p", Stage: 1, Run: func(context.Context) error { panic("kaboom") }}},
		Checks: []Check{
			{ID: "F-LIMIT", Probes: []string{"p"}, Eval: pass("no")},
			{ID: "F-APIERR", Eval: func() Verdict { panic("eval blew up") }},
		},
	}
	got, err := Run(context.Background(), s, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := byID(got)
	if !r["F-LIMIT"].Error || !strings.Contains(r["F-LIMIT"].Detail, "panicked") {
		t.Errorf("probe panic: %+v", r["F-LIMIT"])
	}
	if !r["F-APIERR"].Error || !strings.Contains(r["F-APIERR"].Detail, "panicked") {
		t.Errorf("check panic: %+v", r["F-APIERR"])
	}
}

func TestRunCancelSkipsRemainingProbes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran []string
	s := Suite{
		Probes: []Probe{
			{ID: "p1", Stage: 1, Run: func(context.Context) error { ran = append(ran, "p1"); cancel(); return nil }},
			{ID: "p2", Stage: 2, Run: func(context.Context) error { ran = append(ran, "p2"); return nil }},
		},
		Checks: []Check{
			{ID: "F-LIMIT", Probes: []string{"p1"}, Eval: pass("done before the signal")},
			{ID: "F-APIERR", Probes: []string{"p2"}, Eval: pass("never ran")},
		},
	}
	got, _ := Run(ctx, s, RunOptions{})
	if !reflect.DeepEqual(ran, []string{"p1"}) {
		t.Errorf("ran = %v, want only p1", ran)
	}
	r := byID(got)
	if !r["F-LIMIT"].Pass {
		t.Errorf("F-LIMIT = %+v, its evidence was complete", r["F-LIMIT"])
	}
	if !r["F-APIERR"].Error || !strings.Contains(r["F-APIERR"].Detail, "cancelled") {
		t.Errorf("F-APIERR = %+v, want an error naming the cancellation", r["F-APIERR"])
	}
}

func TestRunBudgetBoundsProbe(t *testing.T) {
	s := Suite{
		Probes: []Probe{{ID: "slow", Stage: 1, Budget: 30 * time.Millisecond, Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
		Checks: []Check{{ID: "F-LIMIT", Probes: []string{"slow"}, Eval: pass("no")}},
	}
	start := time.Now()
	got, _ := Run(context.Background(), s, RunOptions{})
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe was not bounded by its budget")
	}
	if !got[0].Error || !strings.Contains(got[0].Detail, "deadline") {
		t.Errorf("got %+v, want an error naming the deadline", got[0])
	}
}

func TestRunOverallDeadlineBoundsProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	s := Suite{
		Probes: []Probe{{ID: "slow", Stage: 1, Budget: time.Hour, Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
		Checks: []Check{{ID: "F-LIMIT", Probes: []string{"slow"}, Eval: pass("no")}},
	}
	start := time.Now()
	got, _ := Run(ctx, s, RunOptions{})
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe outlived the overall deadline")
	}
	if !got[0].Error {
		t.Errorf("got %+v, want an error", got[0])
	}
}

func TestRunVerdictsAndReliedOn(t *testing.T) {
	s := Suite{Checks: []Check{
		{ID: "F-LIMIT", Eval: func() Verdict { return Failed("saw the wrong thing") }},
		{ID: "F-APIERR", Eval: func() Verdict { return Errored("no evidence") }},
		{ID: "H-RC", Eval: pass("fine")},
	}}
	got, err := Run(context.Background(), s, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := byID(got)
	if r["F-LIMIT"].Pass || r["F-LIMIT"].Error {
		t.Errorf("F-LIMIT = %+v, want a behavioural failure", r["F-LIMIT"])
	}
	if r["F-APIERR"].Pass || !r["F-APIERR"].Error {
		t.Errorf("F-APIERR = %+v, want error", r["F-APIERR"])
	}
	if !r["H-RC"].Pass {
		t.Errorf("H-RC = %+v, want pass", r["H-RC"])
	}
	for id, res := range r {
		if !strings.Contains(res.Detail, "relied on by") {
			t.Errorf("%s detail %q lacks the relied-on-by suffix", id, res.Detail)
		}
		if len(res.ReliedOn) == 0 {
			t.Errorf("%s: reliedOn empty", id)
		}
	}
}

func TestRunOnlyNamesACheckThisBuildDoesNotImplement(t *testing.T) {
	s := Suite{Checks: []Check{{ID: "F-LIMIT", Eval: pass("x")}}}
	if _, err := Run(context.Background(), s, RunOptions{Only: []string{"H-RC"}}); err == nil {
		t.Error("--only naming a catalogued but unimplemented check must be refused, not silently run nothing")
	}
	if _, err := Run(context.Background(), s, RunOptions{Only: []string{"NOPE"}}); err == nil {
		t.Error("--only naming an unknown check must be refused")
	}
}

func TestResolveOnly(t *testing.T) {
	got, err := ResolveOnly([]string{" F-LIMIT ", "H-RC", "F-LIMIT", ""})
	if err != nil || !reflect.DeepEqual(got, []string{"F-LIMIT", "H-RC"}) {
		t.Errorf("got %v, %v; want the two ids once each", got, err)
	}
	if _, err := ResolveOnly([]string{"F-LIMIT", "F-LIMT"}); err == nil {
		t.Error("a typo must be a usage error")
	}
	if got, err := ResolveOnly(nil); err != nil || got != nil {
		t.Errorf("no --only = %v, %v; want nil, nil", got, err)
	}
	if _, err := ResolveOnly([]string{"", " "}); err == nil {
		t.Error("an --only that names nothing must be refused")
	}
}

func TestSuiteValidate(t *testing.T) {
	ok := func(context.Context) error { return nil }
	cases := []struct {
		name string
		s    Suite
		want string
	}{
		{"unknown need", Suite{Probes: []Probe{{ID: "a", Needs: []string{"zz"}, Run: ok}}}, "unknown probe"},
		{"need runs later", Suite{Probes: []Probe{{ID: "a", Stage: 1, Needs: []string{"b"}, Run: ok}, {ID: "b", Stage: 2, Run: ok}}}, "after it"},
		{"need declared later in the same stage", Suite{Probes: []Probe{{ID: "a", Needs: []string{"b"}, Run: ok}, {ID: "b", Run: ok}}}, "after it"},
		{"duplicate probe", Suite{Probes: []Probe{{ID: "a", Run: ok}, {ID: "a", Run: ok}}}, "duplicate probe"},
		{"check not in catalogue", Suite{Checks: []Check{{ID: "NOT-A-CHECK", Eval: pass("")}}}, "not in the catalogue"},
		{"duplicate check", Suite{Checks: []Check{{ID: "F-LIMIT", Eval: pass("")}, {ID: "F-LIMIT", Eval: pass("")}}}, "duplicate check"},
		{"check reads unknown probe", Suite{Checks: []Check{{ID: "F-LIMIT", Probes: []string{"nope"}, Eval: pass("")}}}, "unknown probe"},
		{"check without Eval", Suite{Checks: []Check{{ID: "F-LIMIT"}}}, "no Eval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := (Suite{}).Validate(); err != nil {
		t.Errorf("an empty suite is valid: %v", err)
	}
}
