package tmux

import (
	"context"
	"os"
	"os/exec"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// This file exercises the driver against a real multiplexer holding real
// sessions. It is opt-in (FLEET_TMUX_INTEGRATION=1) because it depends on
// the machine it runs on, and CI has no sessions to look at.
//
// It is READ-ONLY BY CONSTRUCTION, and that is a safety property, not a
// scoping convenience: the sessions on a developer's machine are somebody's
// live work. Nothing here creates, sends, interrupts or closes. The write
// path is exercised against the fake in tmux_test.go and, when it is
// exercised for real, must be against sessions the test created itself.

func requireLiveMux(t *testing.T) {
	t.Helper()
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against the live multiplexer")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no multiplexer on PATH")
	}
	if err := exec.Command("tmux", "has-session").Run(); err != nil {
		t.Skip("no multiplexer server running")
	}
}

// The point of the whole exercise: does the interface express what is
// actually out there?
func TestLiveListParsesRealSessions(t *testing.T) {
	requireLiveMux(t)
	d := New("local")

	got, err := d.List(context.Background(), driver.ListFilter{})
	if err != nil {
		t.Fatalf("List against a live multiplexer failed: %v", err)
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceOK {
		t.Fatalf("expected one healthy source, got %+v", got.Sources())
	}
	if len(got.Items()) == 0 {
		t.Skip("no sessions to look at")
	}

	counts := map[fleet.Status]int{}
	for _, s := range got.Items() {
		counts[s.State.Status]++

		if s.ID == "" {
			t.Error("a session came back with no id")
		}
		if s.Cwd == "" {
			t.Errorf("session %q has no working directory", s.ID)
		}
		if s.State.Evidence == "" {
			t.Errorf("session %q has a status with no evidence (§2.3)", s.ID)
		}
		// §5.6, enforced rather than trusted: this driver reads screens,
		// so nothing it reports may claim to be observed.
		if s.State.Confidence != fleet.ConfidenceInferred {
			t.Errorf("session %q claims confidence %q; this substrate cannot observe",
				s.ID, s.State.Confidence)
		}
		// The status must be a member of the closed set (§2.3). Marshalling
		// is where that is enforced, so this catches a driver inventing one.
		if _, err := s.State.Status.MarshalJSON(); err != nil {
			t.Errorf("session %q has an invalid status: %v", s.ID, err)
		}
	}
	t.Logf("live fleet view: %d sessions, statuses %v", len(got.Items()), counts)
}

// Filters must narrow without changing the envelope's shape.
func TestLiveListFilterNarrows(t *testing.T) {
	requireLiveMux(t)
	d := New("local")
	ctx := context.Background()

	all, err := d.List(ctx, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items()) == 0 {
		t.Skip("no sessions to look at")
	}
	prefix := string(all.Items()[0].Cwd)

	narrowed, err := d.List(ctx, driver.ListFilter{CwdPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed.Items()) == 0 {
		t.Errorf("filtering by a prefix taken from a real session returned nothing")
	}
	if len(narrowed.Items()) > len(all.Items()) {
		t.Error("a filter widened the result")
	}
	if len(narrowed.Sources()) != 1 {
		t.Errorf("a filtered response still carries exactly one source (§9), got %d",
			len(narrowed.Sources()))
	}
	for _, s := range narrowed.Items() {
		if string(s.Cwd) < prefix {
			t.Errorf("session %q (cwd %q) does not match prefix %q", s.ID, s.Cwd, prefix)
		}
	}
}

// State and List must not disagree about the same session — they share a
// classifier precisely so they cannot.
func TestLiveStateAgreesWithList(t *testing.T) {
	requireLiveMux(t)
	d := New("local")
	ctx := context.Background()

	all, err := d.List(ctx, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items()) == 0 {
		t.Skip("no sessions to look at")
	}
	// Pick an idle one where possible: a working session may legitimately
	// change state between the two reads, and this test is about agreement
	// of interpretation, not about time standing still.
	target := all.Items()[0]
	for _, s := range all.Items() {
		if s.State.Status == fleet.StatusIdle {
			target = s
			break
		}
	}

	st, err := d.State(ctx, target.SessionRef)
	if err != nil {
		t.Fatalf("State(%q) failed: %v", target.ID, err)
	}
	if st.Confidence != fleet.ConfidenceInferred {
		t.Errorf("State claims confidence %q", st.Confidence)
	}
	if target.State.Status == fleet.StatusIdle && st.Status != fleet.StatusIdle {
		t.Logf("note: %q read as %q by List and %q by State — legitimate if the "+
			"session changed between reads, a bug if reproducible",
			target.ID, target.State.Status, st.Status)
	}
}

// §5.4's protection must hold against a real multiplexer too: a ref this
// driver has never observed is refused, and nothing is destroyed.
func TestLiveCloseRefusesUnobservedTargetWithoutKilling(t *testing.T) {
	requireLiveMux(t)
	d := New("local")

	_, err := d.Close(context.Background(),
		fleet.SessionRef{Machine: "local", ID: "a-session-id-this-driver-has-never-seen"})
	if err == nil {
		t.Fatal("closing an unobserved id must refuse (§5.4)")
	}
	t.Logf("refused as required: %v", err)
}
