package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// sampleReport is a fixed, fully populated report. Its JSON is the golden
// file: the shape of schema 1 as a caller sees it.
func sampleReport() Report {
	r := Report{
		Claude: Candidate{
			Path:     "/opt/candidate/claude",
			Version:  "9.9.9",
			Resolved: "/opt/candidate/versions/9.9.9",
			Sha256:   "0000000000000000000000000000000000000000000000000000000000000000",
			Arch:     "x86_64",
		},
		ColabFleet: Build{Version: "v0.0.0", Commit: "abc1234"},
		Checks: []Result{
			{ID: "F-APIERR", Pass: true, Detail: "found"},
			{ID: "F-LIMIT", Pass: false, Detail: "missing"},
			{ID: "H-RC", Error: true, Detail: "could not read the file"},
		},
		Turns:      3,
		Only:       []string{"F-LIMIT", "F-APIERR", "H-RC"},
		DurationMs: 1234,
	}
	r.Finalize()
	return r
}

// TestReportJSONShapeGolden pins the schema 1 JSON shape. A change to this
// file's output is a change to a public contract: bump Schema (or make the
// change additive) on purpose, then regenerate with UPDATE_GOLDEN=1.
func TestReportJSONShapeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleReport()); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "report-schema1.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (regenerate with UPDATE_GOLDEN=1)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("report JSON differs from %s (regenerate with UPDATE_GOLDEN=1 only if the change is deliberate)\n got:\n%s\nwant:\n%s", golden, buf.Bytes(), want)
	}
}

// TestReportRequiredCore pins the fields a caller may rely on. Removing any of
// them is a schema bump, not an edit.
func TestReportRequiredCore(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleReport()); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "claude", "checks", "pass"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("top-level %q missing", k)
		}
	}
	claude, _ := doc["claude"].(map[string]any)
	for _, k := range []string{"path", "version"} {
		if _, ok := claude[k]; !ok {
			t.Errorf("claude.%s missing", k)
		}
	}
	checks, _ := doc["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("no checks in the sample report")
	}
	for _, c := range checks {
		m, _ := c.(map[string]any)
		for _, k := range []string{"id", "pass", "detail"} {
			if _, ok := m[k]; !ok {
				t.Errorf("checks[].%s missing in %v", k, m)
			}
		}
	}
	if doc["schema"] != float64(Schema) {
		t.Errorf("schema = %v, want %d", doc["schema"], Schema)
	}
}

// TestExitCode pins the mapping: 0 pass · 1 a must check failed · 2 a must
// check errored. A proven failure outranks "could not certify".
func TestExitCode(t *testing.T) {
	must := func(pass, errored bool) Result { return Result{ID: "X-M", Gate: GateMust, Pass: pass, Error: errored} }
	warn := func(pass, errored bool) Result { return Result{ID: "X-W", Gate: GateWarn, Pass: pass, Error: errored} }
	cases := []struct {
		name   string
		checks []Result
		want   int
	}{
		{"all must pass", []Result{must(true, false), must(true, false)}, 0},
		{"a must fails", []Result{must(true, false), must(false, false)}, 1},
		{"a must errors", []Result{must(true, false), must(false, true)}, 2},
		{"failure outranks error", []Result{must(false, false), must(false, true)}, 1},
		{"a warn failing does not matter", []Result{must(true, false), warn(false, false)}, 0},
		{"a warn erroring does not matter", []Result{must(true, false), warn(false, true)}, 0},
		{"nothing judged cannot certify", nil, 2},
		{"an unrecognised gate counts as must", []Result{{ID: "X", Gate: "odd", Pass: false}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Report{Checks: tc.checks}).ExitCode(); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFinalize(t *testing.T) {
	t.Run("gate comes from the catalogue, not from the check", func(t *testing.T) {
		r := Report{Checks: []Result{{ID: "F-LIMIT", Gate: GateMust, Pass: false}}}
		r.Finalize()
		if r.Checks[0].Gate != GateWarn {
			t.Errorf("gate = %q, want the catalogue's %q", r.Checks[0].Gate, GateWarn)
		}
		if !r.Pass {
			t.Error("a failing warn check must not fail the report")
		}
	})
	t.Run("error implies not pass", func(t *testing.T) {
		r := Report{Checks: []Result{{ID: "X-M", Gate: GateMust, Pass: true, Error: true}}}
		r.Finalize()
		if r.Checks[0].Pass {
			t.Error("Pass must be forced false when Error is set")
		}
		if r.Pass {
			t.Error("report must not pass with a must error")
		}
	})
	t.Run("catalogue order, unknown ids after by id", func(t *testing.T) {
		r := Report{Checks: []Result{
			{ID: "Z-UNKNOWN", Gate: GateWarn, Pass: true},
			{ID: "H-RC", Pass: true},
			{ID: "A-UNKNOWN", Gate: GateWarn, Pass: true},
			{ID: "F-LIMIT", Pass: true},
		}}
		r.Finalize()
		var got []string
		for _, c := range r.Checks {
			got = append(got, c.ID)
		}
		want := []string{"F-LIMIT", "H-RC", "A-UNKNOWN", "Z-UNKNOWN"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})
	t.Run("a report that judged nothing does not pass", func(t *testing.T) {
		r := Report{}
		r.Finalize()
		if r.Pass {
			t.Error("empty report must not pass")
		}
		if r.Schema != Schema {
			t.Errorf("schema = %d, want %d", r.Schema, Schema)
		}
	})
	t.Run("relied-on is copied from the catalogue", func(t *testing.T) {
		r := Report{Checks: []Result{{ID: "F-LIMIT", Pass: true}}}
		r.Finalize()
		if len(r.Checks[0].ReliedOn) == 0 {
			t.Error("reliedOn empty")
		}
		// Mutating the report must not reach back into the catalogue.
		r.Checks[0].ReliedOn[0] = "changed"
		if s, _ := Lookup("F-LIMIT"); s.ReliedOn[0] == "changed" {
			t.Error("Finalize aliased the catalogue's slice")
		}
	})
}

func TestCatalogueWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Catalogue() {
		if s.ID == "" || s.Asserts == "" || len(s.ReliedOn) == 0 {
			t.Errorf("incomplete catalogue entry: %+v", s)
		}
		if s.Gate != GateMust && s.Gate != GateWarn {
			t.Errorf("%s: gate %q", s.ID, s.Gate)
		}
		if seen[s.ID] {
			t.Errorf("duplicate id %s", s.ID)
		}
		seen[s.ID] = true
	}
}
