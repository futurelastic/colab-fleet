package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// This file is the replay harness §8 of issue #8 asked for: every case
// under testdata/corpus/ is a real (redacted) or faithfully reconstructed
// incident, scored against classifyPaneRemembering exactly as tmux.go's
// polling loop would call it, rather than against the one screen that
// motivated whichever heuristic is newest. See testdata/corpus/README.md
// for the redaction design this exists to verify, not merely exercise.

// corpusObservation is one capture in a case's sequence — a single screen
// for a directly-classifiable case, or a step in an ambiguity-resolution
// sequence for one that needs a second look to settle.
type corpusObservation struct {
	// AfterSeconds is this observation's time, relative to the case's
	// first observation. classifyPaneRemembering needs a real clock to
	// judge staleness (spinnerPaintGrace), so a sequence states its own
	// timeline rather than relying on wall-clock time when the test runs.
	AfterSeconds int `json:"afterSeconds"`
	// Young mirrors classifyAgedDetail's own "still plausibly starting"
	// input. Almost always false; stated per-observation because a case
	// about a fresh spawn needs it true only for its first look.
	Young bool `json:"young"`
	// Want is the fleet.Status this observation must classify to.
	Want string `json:"want"`
	// WantControlChannel is the fleet.ControlChannelState this observation
	// must report, or "none" to assert that it reports nothing at all.
	//
	// Optional, and absent means "this case does not speak to it" rather than
	// "there is none" — the corpus should not start asserting a field for
	// every case that never cared about it. It is here because the property
	// #48 records cannot be expressed by Want alone: those 37 sessions
	// classified to a perfectly correct `idle`, and the whole defect was the
	// second fact nothing carried.
	WantControlChannel string `json:"wantControlChannel,omitempty"`
}

// corpusCase is one testdata/corpus/<name>/case.json.
type corpusCase struct {
	Name string `json:"name"`
	// For is the oracle: why this case exists and what property it
	// pins down. Required — a fixture whose expected classification
	// nobody can explain is a fixture nobody can safely change, which is
	// exactly the failure this corpus exists to stop repeating.
	For string `json:"for"`
	// Source names the issue or finding this case was carried forward
	// from, so a reader can go find the incident rather than trust the
	// paraphrase.
	Source string `json:"source"`
	// RedactedFrom records provenance: "real" for a capture taken and
	// redacted at curation time, or a note on how a historical case was
	// reconstructed when no raw capture was ever kept (this driver only
	// ever persisted a screenDigest, never the text — see
	// resolveAmbiguity's comment on screenDigest).
	RedactedFrom string `json:"redactedFrom"`
	// Alive is the process-liveness input classifyPaneRemembering needs;
	// every case here is about screen-reading, so this is almost always
	// true.
	Alive bool `json:"alive"`
	// SetProperty is optional. When present, it states a property of a
	// SET of panes (not this one alone) that this case's screen is only
	// evidence FOR — see fleet-recovery-simultaneous-ambiguity/case.json
	// for the one case that needs it, and testdata/corpus/README.md,
	// "What this corpus cannot express", for why that property cannot
	// itself be asserted here.
	SetProperty  string              `json:"setProperty,omitempty"`
	Observations []corpusObservation `json:"observations"`
}

// corpusCaseDirs lists every case directory under testdata/corpus, sorted,
// so both tests below iterate the same set and a new case is picked up by
// neither test needing to be told about it by name.
func corpusCaseDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", "corpus"))
	if err != nil {
		t.Fatalf("reading testdata/corpus: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("testdata/corpus has no cases — the replay harness has nothing to replay")
	}
	return names
}

func loadCorpusCase(t *testing.T, name string) (corpusCase, string) {
	t.Helper()
	dir := filepath.Join("testdata", "corpus", name)
	raw, err := os.ReadFile(filepath.Join(dir, "screen.txt"))
	if err != nil {
		t.Fatalf("%s: reading screen.txt: %v", name, err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatalf("%s: reading case.json: %v", name, err)
	}
	var c corpusCase
	if err := json.Unmarshal(meta, &c); err != nil {
		t.Fatalf("%s: parsing case.json: %v", name, err)
	}
	if c.For == "" {
		t.Fatalf("%s: case.json has no \"for\" — every corpus case must record the property it pins down, not just its expected answer", name)
	}
	if len(c.Observations) == 0 {
		t.Fatalf("%s: case.json has no observations", name)
	}
	return c, string(raw)
}

// TestCorpusReplaysToItsStatedState is the replay entry point onto
// classifyPaneRemembering — the same function tmux.go's polling loop calls
// — threading paneMemory across a case's observations exactly as a real
// poll cycle would, so an ambiguity-resolution case exercises the second
// look and not just the first.
func TestCorpusReplaysToItsStatedState(t *testing.T) {
	for _, name := range corpusCaseDirs(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			c, raw := loadCorpusCase(t, name)

			t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var prior paneMemory
			for i, obs := range c.Observations {
				now := t0.Add(time.Duration(obs.AfterSeconds) * time.Second)
				got, digest := classifyPaneRemembering(raw, true, c.Alive, obs.Young, prior, now)
				if obs.WantControlChannel != "" {
					switch {
					case obs.WantControlChannel == "none":
						if got.ControlChannel != nil {
							t.Errorf("observation %d (t+%ds): control channel = %q, want none",
								i, obs.AfterSeconds, got.ControlChannel.State)
						}
					case got.ControlChannel == nil:
						t.Errorf("observation %d (t+%ds): control channel absent, want %q",
							i, obs.AfterSeconds, obs.WantControlChannel)
					case string(got.ControlChannel.State) != obs.WantControlChannel:
						t.Errorf("observation %d (t+%ds): control channel = %q, want %q",
							i, obs.AfterSeconds, got.ControlChannel.State, obs.WantControlChannel)
					}
				}
				want := fleet.Status(obs.Want)
				if got.Status != want {
					t.Errorf("observation %d (t+%ds): status = %q, want %q (evidence: %s)",
						i, obs.AfterSeconds, got.Status, want, got.Evidence)
				}
				prior = paneMemory{known: true, digest: digest, at: now}
			}
		})
	}
}

// TestCorpusIsFullyRedacted is how discarding is VERIFIED rather than
// trusted. It re-runs RedactCapture over every committed screen.txt and
// requires the result to be byte-identical to what is on disk: a screen
// this function would still change on a second pass is a screen that was
// not fully redacted the first time, whoever wrote it.
//
// This is deliberately a build-time gate, not a curation-time courtesy.
// Redaction that is applied once by hand and never re-checked is
// redaction that fails the fifth time — the corpus's own reason for
// existing, aimed back at itself.
func TestCorpusIsFullyRedacted(t *testing.T) {
	for _, name := range corpusCaseDirs(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "corpus", name, "screen.txt")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading screen.txt: %v", err)
			}
			redacted := RedactCapture(string(raw))
			if redacted != string(raw) {
				t.Errorf("%s is not a fixed point of RedactCapture — it still carries "+
					"something the redactor would remove on a second pass.\n"+
					"re-redacted form:\n%s", path, redacted)
			}
		})
	}
}
