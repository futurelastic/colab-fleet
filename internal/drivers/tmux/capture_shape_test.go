package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every capture that feeds the classifier must carry the escapes, and this
// pins it at the SOURCE rather than one site at a time.
//
// # Why a source scan and not only a behavioural test
//
// The behavioural tests below catch a wrong shape at the sites they exercise.
// They cannot catch the site somebody adds next month. Three separate captures
// shipped without `-e`, each written in good faith, and nothing objected —
// because the flag is easy to omit and the symptom appears somewhere else
// entirely: a composer holding only its dim placeholder reads as text a human
// typed, `Send` refuses to protect it, and an idle session becomes unreachable
// through the API while looking perfectly healthy.
//
// A grep-shaped test is unusual, and it is the right tool exactly when the
// property is "nobody writes this by hand anywhere". The driver already relies
// on one such gate elsewhere in the fleet for the same class of reason.
func TestClassifierCapturesAreBuiltInOnePlace(t *testing.T) {
	// The single definition, plus the one documented exception. Anything else
	// naming capture-pane is a new site that has not been considered.
	const shapeFn = "func classifyCaptureArgs("
	allowed := map[string]string{
		"classifyCaptureArgs": "the one definition of the shape",
		"confirmLanded":       "does not feed the classifier; strips attributes itself and wants -J",
		"paintedMarkers":      "does not feed the classifier; shares confirmLanded's -J shape so a before/after marker reading is comparable",
	}

	src := readDriverSource(t)
	if !strings.Contains(src, shapeFn) {
		t.Fatalf("%s is gone; this test is pinning a shape that no longer exists", shapeFn)
	}

	for _, fn := range functionsMentioning(src, `"capture-pane"`) {
		if _, ok := allowed[fn]; !ok {
			t.Errorf("%s builds a capture-pane invocation by hand.\n"+
				"Classifier captures must come from classifyCaptureArgs: the composer's "+
				"placeholder is distinguished from typed text by DIMNESS ALONE, so a capture "+
				"without -e makes an idle session look like it holds somebody's unsent input "+
				"— and Send then refuses to deliver to it.\n"+
				"If this capture genuinely does not feed the classifier, add it to the "+
				"allowed list here with the reason.", fn)
		}
	}
}

// The shape itself, asserted once so the reason survives a refactor that keeps
// the function but changes what it returns.
func TestClassifierCaptureShapeKeepsTheEscapes(t *testing.T) {
	args := classifyCaptureArgs("%9", 24)
	var hasE, hasP bool
	for _, a := range args {
		switch a {
		case "-e":
			hasE = true
		case "-p":
			hasP = true
		case "-J":
			t.Error("-J joins wrapped lines; the classifier parses continuation lines itself, " +
				"and the batched enumeration every status in the fleet is read through has " +
				"never used it. Adopting it here would change what the classifier sees " +
				"everywhere, on no evidence.")
		}
	}
	if !hasE {
		t.Error("no -e: the capture cannot express dimness, so the composer's placeholder " +
			"is indistinguishable from text a human typed")
	}
	if !hasP {
		t.Error("no -p: nothing is written to stdout")
	}
}

func readDriverSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}

// functionsMentioning returns the name of every function whose body contains
// needle. Line-oriented and deliberately simple: it only has to attribute a
// literal to the nearest preceding func declaration.
func functionsMentioning(src, needle string) []string {
	var out []string
	current := ""
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "func ") {
			current = funcName(line)
			continue
		}
		if strings.Contains(line, needle) && !strings.Contains(line, "//") {
			if current != "" && !contains(out, current) {
				out = append(out, current)
			}
		}
	}
	return out
}

func funcName(decl string) string {
	rest := strings.TrimPrefix(decl, "func ")
	if strings.HasPrefix(rest, "(") { // method: skip the receiver
		if i := strings.Index(rest, ")"); i >= 0 {
			rest = strings.TrimSpace(rest[i+1:])
		}
	}
	if i := strings.IndexAny(rest, "("); i >= 0 {
		return strings.TrimSpace(rest[:i])
	}
	return strings.TrimSpace(rest)
}
