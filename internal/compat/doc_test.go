package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// docPath is the document the report contract is published in. These tests
// read it the way a person would: a table between two markers is the claim,
// and the code is checked against it in both directions.
const docPath = "../../docs/compat.md"

// requiredCore is what a caller may rely on. Removing a field from this list
// is a schema bump, not an edit — and the doc has to agree.
var requiredCore = []string{
	"schema", "claude", "claude.path", "claude.version",
	"checks", "checks[].id", "checks[].pass", "checks[].detail",
	"pass",
}

// docTable returns the rows (cells trimmed, backticks removed) of the table
// between <!-- compat:<name>:begin --> and <!-- compat:<name>:end -->.
func docTable(t *testing.T, name string) [][]string {
	t.Helper()
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	begin, end := "<!-- compat:"+name+":begin -->", "<!-- compat:"+name+":end -->"
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("%s: markers %q … %q not found", docPath, begin, end)
	}
	var rows [][]string
	for n, line := range strings.Split(doc[i+len(begin):j], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if n <= 2 && (strings.Contains(line, "---")) {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for k := range cells {
			cells[k] = strings.Trim(strings.TrimSpace(cells[k]), "`")
		}
		rows = append(rows, cells)
	}
	if len(rows) < 2 {
		t.Fatalf("table %q has no rows", name)
	}
	return rows[1:] // the header
}

// TestCatalogueMatchesDoc holds the catalogue table in the doc to the
// catalogue in the code, in order, on every column. A check added without its
// row, or a row describing a check that no longer exists, fails the build.
func TestCatalogueMatchesDoc(t *testing.T) {
	rows := docTable(t, "catalogue")
	cat := Catalogue()
	if len(rows) != len(cat) {
		t.Errorf("the doc lists %d checks, the catalogue has %d", len(rows), len(cat))
	}
	for i, s := range cat {
		if i >= len(rows) {
			t.Errorf("%s is in the catalogue but not in %s", s.ID, docPath)
			continue
		}
		r := rows[i]
		if len(r) != 4 {
			t.Errorf("row %d has %d cells, want 4: %v", i, len(r), r)
			continue
		}
		if r[0] != s.ID {
			t.Errorf("row %d is %q, the catalogue has %q there (same order, please)", i, r[0], s.ID)
			continue
		}
		if r[1] != string(s.Gate) {
			t.Errorf("%s: doc says gate %q, code says %q", s.ID, r[1], s.Gate)
		}
		if r[2] != s.Asserts {
			t.Errorf("%s: the doc's description differs from the code's\n doc:  %s\n code: %s", s.ID, r[2], s.Asserts)
		}
		var docRelied []string
		for _, x := range strings.Split(r[3], ",") {
			if x = strings.Trim(strings.TrimSpace(x), "`"); x != "" {
				docRelied = append(docRelied, x)
			}
		}
		want := append([]string(nil), s.ReliedOn...)
		sort.Strings(want)
		sort.Strings(docRelied)
		if !reflect.DeepEqual(docRelied, want) {
			t.Errorf("%s: doc relied-on %v, code %v", s.ID, docRelied, want)
		}
	}
	for _, r := range rows[len(cat):] {
		t.Errorf("the doc lists %q, which is not in the catalogue", r[0])
	}
}

// fieldRows walks a struct type and returns (path, json type) for every JSON
// field, the way a reader of the JSON would name them.
func fieldRows(typ reflect.Type, prefix string) map[string]string {
	out := map[string]string{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		switch f.Type.Kind() {
		case reflect.String:
			out[path] = "string"
		case reflect.Int, reflect.Int64:
			out[path] = "integer"
		case reflect.Bool:
			out[path] = "boolean"
		case reflect.Struct:
			out[path] = "object"
			for k, v := range fieldRows(f.Type, path+".") {
				out[k] = v
			}
		case reflect.Slice:
			out[path] = "array"
			if f.Type.Elem().Kind() == reflect.Struct {
				for k, v := range fieldRows(f.Type.Elem(), path+"[].") {
					out[k] = v
				}
			}
		default:
			out[path] = "?" + f.Type.Kind().String()
		}
	}
	return out
}

// TestReportFieldsMatchDoc compares every field of the Report type — found by
// reflection, so a new field cannot be missed — against the field table:
// same paths, same types, and the required core exactly as declared above.
func TestReportFieldsMatchDoc(t *testing.T) {
	code := fieldRows(reflect.TypeOf(Report{}), "")
	docTypes := map[string]string{}
	docRequired := map[string]bool{}
	for _, r := range docTable(t, "fields") {
		if len(r) != 3 {
			t.Fatalf("field row %v: want 3 cells", r)
		}
		docTypes[r[0]] = r[1]
		switch r[2] {
		case "required":
			docRequired[r[0]] = true
		case "additive":
		default:
			t.Errorf("%s: the third column is %q, want required or additive", r[0], r[2])
		}
	}
	for path, typ := range code {
		d, ok := docTypes[path]
		if !ok {
			t.Errorf("the Report type has %q (%s) and %s does not document it", path, typ, docPath)
			continue
		}
		if d != typ {
			t.Errorf("%s: the doc says %s, the type is %s", path, d, typ)
		}
	}
	for path := range docTypes {
		if _, ok := code[path]; !ok {
			t.Errorf("the doc documents %q, which the Report type does not have", path)
		}
	}
	want := map[string]bool{}
	for _, p := range requiredCore {
		want[p] = true
	}
	for p := range want {
		if !docRequired[p] {
			t.Errorf("%s belongs to the required core and the doc does not mark it required", p)
		}
	}
	for p := range docRequired {
		if !want[p] {
			t.Errorf("the doc marks %s required, but it is not in the required core", p)
		}
	}
}

// TestDocExampleParses decodes the JSON example in the doc strictly: an
// example that names a field the type lacks, or lacks a required one, is a
// lie in the one place people copy from.
func TestDocExampleParses(t *testing.T) {
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "```json\n")
	if i < 0 {
		t.Fatal("no ```json example in the doc")
	}
	rest := doc[i+len("```json\n"):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatal("unterminated example")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(rest[:j])))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("the doc's example does not decode into the Report type: %v", err)
	}
	if r.Schema != Schema || r.Claude.Path == "" || r.Claude.Version == "" || len(r.Checks) == 0 {
		t.Errorf("the example is missing required fields: %+v", r)
	}
	for _, c := range r.Checks {
		if _, ok := Lookup(c.ID); !ok {
			t.Errorf("the example shows check %q, which is not in the catalogue", c.ID)
		}
		if c.ID == "" || c.Detail == "" {
			t.Errorf("example check %+v lacks id or detail", c)
		}
	}
}

// TestShippedIDsStable makes renaming or removing a check ID a deliberate
// act. The list is the contract: a listed ID that vanished from the
// catalogue is a removal or rename (a schema bump); a catalogue ID that is not
// listed is a new check that has not been recorded; a header that disagrees with
// Schema is a bump nobody recorded.
func TestShippedIDsStable(t *testing.T) {
	b, err := os.ReadFile("testdata/shipped-ids.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 || lines[0] != "schema "+itoa(Schema) {
		t.Fatalf("the first line of shipped-ids.txt must be %q, got %q", "schema "+itoa(Schema), lines[0])
	}
	listed := map[string]bool{}
	for _, id := range lines[1:] {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if listed[id] {
			t.Errorf("duplicate %q in shipped-ids.txt", id)
		}
		listed[id] = true
		if _, ok := Lookup(id); !ok {
			t.Errorf("%q was shipped and is no longer in the catalogue: removing or renaming a check ID is a schema bump — bump Schema, then edit this file deliberately", id)
		}
	}
	for _, s := range Catalogue() {
		if !listed[s.ID] {
			t.Errorf("%q is in the catalogue but not in shipped-ids.txt: record every check you add", s.ID)
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
