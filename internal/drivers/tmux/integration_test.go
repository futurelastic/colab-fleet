package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// This file exercises the driver against a real multiplexer holding real
// sessions. It is opt-in (FLEET_TMUX_INTEGRATION=1) because it depends on
// the machine it runs on, and CI has no sessions to look at.
//
// PRE-EXISTING SESSIONS ARE NEVER MUTATED, and that is a safety property
// rather than a scoping convenience: the sessions on a developer's machine
// are somebody's live work. Nothing here sends to, interrupts, or closes a
// session it did not create. The one test that needs a session to act on
// creates its own and destroys it.
//
// The single exception is deliberate and was measured before being accepted:
// a live subscription's lifecycle control client attaches to whichever
// session it finds, because control mode has no unattached form. Attaching
// does not renegotiate session size (verified: 200x50 before, during and
// after), and the client is detached when the stream closes.

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

// Live subscription against a real control-mode client.
//
// This asserts the architecture's central claim: that ONE control client,
// attached to some arbitrary session, learns about sessions appearing and
// disappearing anywhere on the machine. The subscription is filtered to a
// directory only this test uses, so no pre-existing session gets a content
// client — the only thing that can deliver this event is the global
// lifecycle channel.
//
// An earlier version of this test asserted on a state change instead, and
// was wrong twice over: the probe session runs a plain shell rather than the
// agent TUI, so its classified status is unknown before AND after any output
// (correctly — no composer, nothing to read), and the temp directory needed
// symlink resolution before it could match a filter at all. Both failures
// produced "no event", which is also what a genuinely broken subscription
// produces. Recorded because it is the same trap as the marker bug: absence
// of an event is a weak signal, and a test built on one is a test that
// passes for the wrong reason just as easily as it fails for it.
func TestLiveSubscribeSeesASessionAppearAndDisappear(t *testing.T) {
	requireLiveMux(t)

	dir, err := os.MkdirTemp("", "fleet-live-sub-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// The multiplexer reports a pane's resolved path; on some systems the
	// temp root is a symlink, and an unresolved prefix silently matches
	// nothing.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	d := New("local")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := d.Subscribe(ctx, driver.SubscribeFilter{CwdPrefix: dir})
	if err != nil {
		t.Fatalf("Subscribe against a live multiplexer failed: %v", err)
	}
	defer stream.Close()

	name := "fleet-livetest-" + randomNonce()
	if err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir,
		"bash", "--noprofile", "--norc").Run(); err != nil {
		t.Skipf("could not create a probe session: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = exec.Command("tmux", "kill-session", "-t", name).Run()
		}
	}()

	ev := awaitKind(t, ctx, stream, fleet.EventSessionCreated, 15*time.Second)
	sess, ok := ev.Payload.(fleet.Session)
	if !ok || sess.ID != name {
		t.Fatalf("created event carried %+v, want session %q", ev.Payload, name)
	}
	t.Logf("global lifecycle client saw a new session: %q", sess.ID)

	// And going away again.
	if err := exec.Command("tmux", "kill-session", "-t", name).Run(); err != nil {
		t.Fatal(err)
	}
	killed = true

	ev = awaitKind(t, ctx, stream, fleet.EventSessionClosed, 15*time.Second)
	p, ok := ev.Payload.(fleet.SessionStatePayload)
	if !ok || p.Ref.ID != name {
		t.Fatalf("closed event carried %+v, want session %q", ev.Payload, name)
	}
	if p.State.Status != fleet.StatusDead {
		t.Errorf("closed session reported %q, want dead", p.State.Status)
	}
	t.Logf("and saw it go: %q -> %s", p.Ref.ID, p.State.Status)
}

// awaitKind reads events until one of the wanted kind arrives. Other kinds
// are legitimate traffic (a source.status from an unrelated hiccup, a state
// change on another matching session) and must not fail the test.
func awaitKind(t *testing.T, ctx context.Context, s driver.EventStream,
	want fleet.EventKind, within time.Duration) fleet.Event {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		ev, err := s.Next(deadline)
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		if ev.Kind == want {
			return ev
		}
		t.Logf("(skipping %q while waiting for %q)", ev.Kind, want)
	}
}
