package compat

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteJSON writes the report as indented JSON with a trailing newline.
func WriteJSON(w io.Writer, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// WriteText writes the human form: one line per check, then the verdict.
// It states plainly when the run was partial, because a partial pass is not a
// certification.
func WriteText(w io.Writer, r Report) {
	fmt.Fprintf(w, "candidate  %s  version %s\n", r.Claude.Path, r.Claude.Version)
	if r.Claude.Sha256 != "" {
		fmt.Fprintf(w, "           sha256 %s  arch %s\n", r.Claude.Sha256, r.Claude.Arch)
	}
	fmt.Fprintf(w, "checked by colab-fleet %s (%s)\n\n", r.ColabFleet.Version, r.ColabFleet.Commit)
	for _, c := range r.Checks {
		state := "pass"
		switch {
		case c.Error:
			state = "ERROR"
		case !c.Pass && isMust(c.Gate):
			state = "FAIL"
		case !c.Pass:
			state = "warn"
		}
		fmt.Fprintf(w, "%-5s %-4s %-12s %s\n", state, c.Gate, c.ID, c.Detail)
	}
	verdict := "PASS"
	if !r.Pass {
		verdict = "NOT CERTIFIED"
	}
	fmt.Fprintf(w, "\n%s — %d model turn(s), %d ms\n", verdict, r.Turns, r.DurationMs)
	if len(r.Only) > 0 {
		fmt.Fprintf(w, "partial run (--only %v): not a certification\n", r.Only)
	}
}
