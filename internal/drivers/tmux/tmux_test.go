package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// fakeMux is a stand-in for the multiplexer binary. It records every
// invocation so tests can assert on subprocess count — the property
// driver.Driver.List's doc comment calls a "correct-looking bug" if got
// wrong — and on what never reached a command line (§5.3).
type fakeMux struct {
	calls    [][]string
	sessions []fakeSession
	captures map[string]string
	failList bool
}

type fakeSession struct {
	name    string
	paneID  string
	cwd     string
	pid     int
	created int64
	dead    bool
	title   string
}

const testNonce = "0badc0de"

func (f *fakeMux) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "list-panes":
		if f.failList {
			return nil, errors.New("multiplexer server not running")
		}
		sep := testNonce + "F"
		var b strings.Builder
		for _, s := range f.sessions {
			dead := "0"
			if s.dead {
				dead = "1"
			}
			b.WriteString(strings.Join([]string{
				s.name, s.paneID, s.cwd, itoa(s.pid), itoa64(s.created), dead, s.title,
			}, sep))
			b.WriteString("\n")
		}
		return []byte(b.String()), nil
	case "display-message":
		// The batched capture call: emit marker + capture for each pane.
		mark := testNonce + "P"
		var b strings.Builder
		for i, s := range f.sessions {
			b.WriteString(mark + intToStr(i) + "\n")
			b.WriteString(f.captures[s.paneID])
			b.WriteString("\n")
		}
		return []byte(b.String()), nil
	default:
		return nil, nil
	}
}

func itoa(i int) string     { return strings.TrimSpace(strings.Join([]string{intToStr(i)}, "")) }
func itoa64(i int64) string { return intToStr(int(i)) }
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func newTestDriver(f *fakeMux) *Driver {
	return New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
}

func idleFixtureFor(label string) string {
	return "  transcript line for " + label + "\n✻ Brewed for 1m 0s\n" + rule + "\n❯\n" + rule + "\n  ⏵⏵ auto mode on"
}

func twoSessions() *fakeMux {
	return &fakeMux{
		sessions: []fakeSession{
			{name: "alpha💬", paneID: "%1", cwd: "/work/alpha", pid: 100, created: 1785600000, title: "2_1_220"},
			{name: "beta", paneID: "%2", cwd: "/work/beta", pid: 200, created: 1785600001, title: "2_1_220"},
		},
		captures: map[string]string{
			"%1": idleFixtureFor("alpha"),
			"%2": fixtureUnsent,
		},
	}
}

// The headline cost property: enumerating N sessions costs a constant
// number of subprocess spawns, not one per session.
func TestListCostsConstantSpawns(t *testing.T) {
	f := twoSessions()
	// Grow to a size where a per-session implementation would be obvious.
	for i := 0; i < 40; i++ {
		id := "%" + intToStr(100+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "s" + intToStr(i), paneID: id, cwd: "/w", pid: 1000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("s" + intToStr(i))
	}
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Errorf("List made %d subprocess calls for %d sessions; must be constant (2), "+
			"not proportional — see driver.Driver.List's contract",
			len(f.calls), len(f.sessions))
	}
}

func TestListCarriesExactlyOneSourceAndRealStatuses(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.List(context.Background(), driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources()) != 1 {
		t.Fatalf("a local answer carries exactly one SourceStatus (§9), got %d", len(got.Sources()))
	}
	if got.Sources()[0].Status != fleet.SourceOK || !got.Complete() {
		t.Errorf("healthy read should be ok+complete, got %v complete=%v",
			got.Sources()[0].Status, got.Complete())
	}
	if len(got.Items()) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got.Items()))
	}
	byID := map[string]fleet.Session{}
	for _, s := range got.Items() {
		byID[s.ID] = s
	}
	if byID["alpha💬"].State.Status != fleet.StatusIdle {
		t.Errorf("alpha: want idle, got %q", byID["alpha💬"].State.Status)
	}
	if byID["beta"].State.Status != fleet.StatusWaitingInput {
		t.Errorf("beta (unsent composer): want waiting_input, got %q", byID["beta"].State.Status)
	}
	if byID["alpha💬"].Cwd != "/work/alpha" {
		t.Errorf("emoji session name broke field parsing: cwd = %q", byID["alpha💬"].Cwd)
	}
}

// §5.7: a failed read must never render as an empty result.
func TestListFailureIsNotAnEmptyList(t *testing.T) {
	f := twoSessions()
	f.failList = true
	d := newTestDriver(f)
	got, err := d.List(context.Background(), driver.ListFilter{})
	if err != nil {
		t.Fatalf("a source failure belongs in the envelope, not in err: %v", err)
	}
	if got.Complete() {
		t.Error("a failed read must not report complete")
	}
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Errorf("want one unreachable source, got %+v", got.Sources())
	}
	if got.Sources()[0].Error == "" {
		t.Error("an unreachable source must carry why (§9)")
	}
}

// §2.4's refusal, which the README listed as "prose that has never run".
func TestSendRefusesWhenComposerHoldsUnsentInput(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.Send(context.Background(),
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("a refusal is a domain outcome, not an error: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("want refused, got %q", got.Outcome)
	}
	if got.Reason == "" {
		t.Error("a refusal must explain itself (§2.4)")
	}
}

func TestSendDeliversWhenComposerIsEmpty(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("this substrate cannot confirm receipt, so queued is the honest outcome; got %q", got.Outcome)
	}
	// The payload must not appear in any argv (§5.3's rationale).
	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, "hello") {
				t.Errorf("payload reached a command line: %v", c)
			}
		}
	}
}

func TestSendRefusesDeadAndMissingSessions(t *testing.T) {
	f := twoSessions()
	f.sessions[0].dead = true
	d := newTestDriver(f)
	got, _ := d.Send(context.Background(),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("dead session: want refused, got %q", got.Outcome)
	}
	got, _ = d.Send(context.Background(),
		fleet.SessionRef{Machine: "testbox", ID: "nope"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("missing session: want refused, got %q", got.Outcome)
	}
}

// §5.4: never act destructively on an id match alone.
func TestCloseRefusesAnUncorroboratedTarget(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Close(context.Background(), fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("closing a never-observed id must refuse (§5.4); got %v", err)
	}
}

func TestCloseRefusesWhenTheIdWasRecycled(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// Same name, different session: the exact hazard §5.4 describes.
	f.sessions[0].created = 1785699999
	_, err := d.Close(context.Background(), fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("a recycled id must refuse, got %v", err)
	}
	for _, c := range f.calls {
		if c[0] == "kill-session" {
			t.Fatal("a refused close must not have killed anything")
		}
	}
}

func TestCloseProceedsWhenCorroborated(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	ack, err := d.Close(context.Background(), fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("corroborated close should proceed: ack=%+v err=%v", ack, err)
	}
	var killed bool
	for _, c := range f.calls {
		if c[0] == "kill-session" {
			killed = true
		}
	}
	if !killed {
		t.Error("expected a kill-session invocation")
	}
}

// §10: a repeat key returns the existing ref rather than a second session.
func TestCreateIsIdempotentPerKey(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	spec := fleet.SessionSpec{Machine: "testbox", Cwd: "/work/new", Name: "gamma"}

	first, err := d.Create(context.Background(), "key-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	before := countCalls(f, "new-session")
	second, err := d.Create(context.Background(), "key-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("same key must return the same ref: %+v vs %+v", first, second)
	}
	if countCalls(f, "new-session") != before {
		t.Error("a repeat key must not start a second session (§10)")
	}
}

func TestCreateRequiresAnIdempotencyKey(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Create(context.Background(), "", fleet.SessionSpec{Cwd: "/w"})
	if err == nil {
		t.Error("§10 makes the key required, not optional")
	}
}

// §5.3: context and prompt travel by path/buffer, never in argv.
func TestCreateKeepsPromptAndContextOutOfArgv(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	secret := "a-prompt-that-must-not-be-greppable"
	_, err := d.Create(context.Background(), "k", fleet.SessionSpec{
		Machine:    "testbox",
		Cwd:        "/work/new",
		Name:       "gamma",
		Prompt:     secret,
		ContextRef: "/tmp/ctx.txt",
		Agent:      "may",
		Model:      "opus",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Errorf("prompt reached a command line (§5.3): %v", c)
			}
		}
	}
	// The context path, by contrast, is exactly what SHOULD be in argv.
	var sawContextPath bool
	for _, c := range f.calls {
		for _, a := range c {
			if a == "/tmp/ctx.txt" {
				sawContextPath = true
			}
		}
	}
	if !sawContextPath {
		t.Error("contextRef should be passed by path (§5.3)")
	}
}

func TestCreateRejectsRelativeContextRef(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Create(context.Background(), "k", fleet.SessionSpec{
		Cwd: "/w", ContextRef: "relative/path.txt",
	})
	if err == nil {
		t.Error("contextRef is an AbsolutePath; a relative one must be rejected")
	}
}

// §4.3/§5.6: the capability declaration must not overstate the substrate.
func TestCapabilitiesAreHonest(t *testing.T) {
	d := newTestDriver(twoSessions())
	c := d.Capabilities()
	if c.ObservesState {
		t.Error("every status here is screen-inferred; ObservesState must be false (§5.6)")
	}
	if c.ConfirmsDelivery {
		t.Error("receipt is not observable on this substrate; ConfirmsDelivery must be false")
	}
	if !c.SupportsResume {
		t.Error("multiplexer sessions outlive this process; SupportsResume is true")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("§4.4 requires a positive deadline: %v", err)
	}
}

// §12: reconciliation adopts and never destroys.
func TestReconcileAdoptsAndDestroysNothing(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items()) != 2 {
		t.Errorf("reconciliation must surface everything found, got %d", len(got.Items()))
	}
	for _, c := range f.calls {
		if c[0] == "kill-session" {
			t.Fatal("§12 rule 4 is absolute: reconciliation destroys nothing")
		}
	}
}

// §5.7 for a singular read: "looked, not there" is an answer, not an error.
func TestStateOfAMissingSessionIsDeadNotAnError(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.State(context.Background(), fleet.SessionRef{Machine: "testbox", ID: "ghost"})
	if err != nil {
		t.Fatalf("absence is an answer: %v", err)
	}
	if got.Status != fleet.StatusDead {
		t.Errorf("want dead, got %q", got.Status)
	}
	if got.Confidence != fleet.ConfidenceInferred {
		t.Errorf("want inferred, got %q", got.Confidence)
	}
}

func countCalls(f *fakeMux, verb string) int {
	n := 0
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}
