package tmux

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// fakeMux is a stand-in for the multiplexer binary. It records every
// invocation so tests can assert on subprocess count — the property
// driver.Driver.List's doc comment calls a "correct-looking bug" if got
// wrong — and on what never reached a command line (§5.3).
// fakeMux is shared between the test goroutine and the driver's engine
// goroutine whenever a subscription is live, so every field is guarded. An
// earlier version was not, and the race detector found it the moment a test
// mutated state while a stream was running — a fault in the harness rather
// than the driver, but one that would have made every subscription test
// quietly untrustworthy.
type fakeMux struct {
	mu         sync.Mutex
	calls      [][]string
	sessions   []fakeSession
	captures   map[string]string
	failList   bool
	failCreate bool
	// buffers/pasted make the fake behave like a terminal: text delivered
	// through the paste buffer becomes visible in a later capture. Without
	// that, send() can never confirm its own delivery and the confirmation
	// path is untestable.
	buffers map[string]string
	pasted  map[string]string
	noEcho  bool // simulate a pane that never renders what was pasted
}

func (f *fakeMux) setCapture(paneID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures[paneID] = text
}

func (f *fakeMux) addSession(s fakeSession, capture string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, s)
	f.captures[s.paneID] = capture
}

func (f *fakeMux) dropLastSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.sessions[len(f.sessions)-1]
	f.sessions = f.sessions[:len(f.sessions)-1]
	delete(f.captures, last.paneID)
}

func (f *fakeMux) setFailList(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList = v
}

func (f *fakeMux) callsSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
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

// testCaller is what a service would hand a local driver. Credential is
// empty on purpose: a local driver has no peer to present it to, and a test
// that supplied one would be asserting a behaviour this driver must not have.
var testCaller = fleet.Request{Caller: fleet.Caller{Principal: "test:unit"}}

func (f *fakeMux) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if f.buffers == nil {
		f.buffers = map[string]string{}
	}
	if f.pasted == nil {
		f.pasted = map[string]string{}
	}
	switch args[0] {
	case "load-buffer":
		// One invocation carries both halves, separated by a literal ";":
		//   load-buffer -b <name> <file> ; paste-buffer -b <name> -t <pane> -d
		var content, pane string
		for i, a := range args {
			switch a {
			case "load-buffer":
				if i+3 < len(args) {
					if raw, err := os.ReadFile(args[i+3]); err == nil {
						content = string(raw)
					}
				}
			case "paste-buffer":
				for j := i; j < len(args)-1; j++ {
					if args[j] == "-t" {
						pane = args[j+1]
					}
				}
			}
		}
		if pane != "" && !f.noEcho {
			f.pasted[pane] += content
		}
		return nil, nil
	case "capture-pane":
		// Single-pane capture, as used to confirm delivery.
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-t" {
				return []byte(f.captures[args[i+1]] + "\n" + f.pasted[args[i+1]]), nil
			}
		}
		return nil, nil
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
	case "new-session":
		if f.failCreate {
			return nil, errors.New("cannot create session")
		}
		return nil, nil
	case "rename-session":
		// -t "=OLD" NEW. The "=" pin is stripped the way the real
		// multiplexer does, so a test can assert it was sent.
		//
		// No locking here: exec() already holds f.mu, and sync.Mutex is not
		// reentrant — taking it again deadlocks the whole suite, which is
		// exactly what it did.
		var from, to string
		for i := 0; i < len(args); i++ {
			if args[i] == "-t" && i+1 < len(args) {
				from = strings.TrimPrefix(args[i+1], "=")
				if i+2 < len(args) {
					to = args[i+2]
				}
			}
		}
		for i := range f.sessions {
			if f.sessions[i].name == from {
				f.sessions[i].name = to
				return nil, nil
			}
		}
		return nil, errors.New("can't find session")
	case "send-keys":
		// Model the one key whose EFFECT a test depends on: C-u clears the
		// composer. Anything else stays a no-op, as before.
		//
		// Modelling it here rather than in the test matters: clearing the pane
		// before calling Discard would have it see an empty composer and
		// return early, so the test would pass while proving nothing about the
		// keystroke.
		var pane string
		clear := false
		for i := 0; i < len(args); i++ {
			if args[i] == "-t" && i+1 < len(args) {
				pane = args[i+1]
			}
			if args[i] == "C-u" {
				clear = true
			}
		}
		if clear && pane != "" {
			delete(f.pasted, pane)
			f.captures[pane] = idleFixtureFor("cleared")
		}
		return nil, nil
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
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsSnapshot()) != 2 {
		t.Errorf("List made %d subprocess calls for %d sessions; must be constant (2), "+
			"not proportional — see driver.Driver.List's contract",
			len(f.callsSnapshot()), len(f.sessions))
	}
}

func TestListCarriesExactlyOneSourceAndRealStatuses(t *testing.T) {
	d := newTestDriver(twoSessions())
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
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
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
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
	got, err := d.Send(context.Background(), testCaller,
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
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("this substrate cannot confirm receipt, so queued is the honest outcome; got %q", got.Outcome)
	}
	// The payload must not appear in any argv (§5.3's rationale).
	for _, c := range f.callsSnapshot() {
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
	got, _ := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("dead session: want refused, got %q", got.Outcome)
	}
	got, _ = d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "nope"}, "x", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("missing session: want refused, got %q", got.Outcome)
	}
}

// §5.4: never act destructively on an id match alone.
func TestCloseRefusesAnUncorroboratedTarget(t *testing.T) {
	d := newTestDriver(twoSessions())
	_, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("closing a never-observed id must refuse (§5.4); got %v", err)
	}
}

func TestCloseRefusesWhenTheIdWasRecycled(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// Same name, different session: the exact hazard §5.4 describes.
	f.sessions[0].created = 1785699999
	_, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("a recycled id must refuse, got %v", err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("a refused close must not have killed anything")
		}
	}
}

func TestCloseProceedsWhenCorroborated(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	ack, err := d.Close(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("corroborated close should proceed: ack=%+v err=%v", ack, err)
	}
	var killed bool
	for _, c := range f.callsSnapshot() {
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

	first, err := d.Create(context.Background(), testCaller, "key-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	before := countCalls(f, "new-session")
	second, err := d.Create(context.Background(), testCaller, "key-1", spec)
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
	_, err := d.Create(context.Background(), testCaller, "", fleet.SessionSpec{Cwd: "/w"})
	if err == nil {
		t.Error("§10 makes the key required, not optional")
	}
}

// §5.3: context and prompt travel by path/buffer, never in argv.
func TestCreateKeepsPromptAndContextOutOfArgv(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	secret := "a-prompt-that-must-not-be-greppable"
	_, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
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
	for _, c := range f.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, secret) {
				t.Errorf("prompt reached a command line (§5.3): %v", c)
			}
		}
	}
	// The context path, by contrast, is exactly what SHOULD be in argv.
	var sawContextPath bool
	for _, c := range f.callsSnapshot() {
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
	_, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{
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
	if len(got.Live.Items()) != 2 {
		t.Errorf("reconciliation must surface everything found, got %d", len(got.Live.Items()))
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("§12 rule 4 is absolute: reconciliation destroys nothing")
		}
	}
}

// §5.7 for a singular read: "looked, not there" is an answer, not an error.
// Absence is an answer — that part never changed. What changed is that there
// are TWO absences, and they were being given the same one.
//
// A session this driver watched and can no longer find is dead. An id it has
// never seen is not dead: claiming so invents a history, and tells a caller
// who mistyped an id that its work has died.
func TestStateSeparatesGoneFromNeverHere(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	// Never seen: no history to report, so this is not an answer about a
	// session at all.
	_, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Fatalf("an id never observed must not be reported as dead; got err=%v", err)
	}

	// Seen, then gone: this one really is dead, and absence is the answer.
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	f.dropLastSession()
	got, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"})
	if err != nil {
		t.Fatalf("a session that was here and is gone is an answer, not an error: %v", err)
	}
	if got.Status != fleet.StatusDead {
		t.Errorf("want dead, got %q", got.Status)
	}
	if got.Confidence != fleet.ConfidenceInferred {
		t.Errorf("want inferred, got %q", got.Confidence)
	}
	// Name what ended. An id alone is recyclable (§5.4), so a caller
	// reconciling its own records needs more than the id it asked with.
	if !strings.Contains(got.Evidence, "/work/beta") {
		t.Errorf("evidence should say which session ended, got %q", got.Evidence)
	}
}

func countCalls(f *fakeMux, verb string) int {
	n := 0
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

// §5.4's real guarantee: corroborate against what the CALLER observed.
//
// alpha was created at 1785600000 in the fixture.
func expectStarted(unix int64) fleet.Request {
	ts := time.Unix(unix, 0)
	return fleet.Request{
		Caller: fleet.Caller{Principal: "test:unit"},
		Expect: fleet.Expectation{StartedAt: &ts},
	}
}

func TestCloseWithMatchingExpectationNeedsNoPriorSighting(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	// Deliberately no List first: the caller's own observation is enough,
	// which is the point — the driver's sightings are not the authority.
	ack, err := d.Close(context.Background(), expectStarted(1785600000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if err != nil || !ack.Accepted {
		t.Fatalf("a caller quoting the right start time should be able to close: ack=%+v err=%v", ack, err)
	}
	if countCalls(f, "kill-session") != 1 {
		t.Error("expected exactly one kill-session")
	}
}

// The test the whole envelope exists for. The driver's OWN sighting is
// current and would pass the weak check — but the caller is quoting a session
// that no longer exists at that id. Before Request.Expect this was
// unexpressible, and the destroy would have proceeded.
func TestStaleCallerExpectationRefusesEvenWhenTheDriversOwnSightingIsFresh(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	// Driver observes the session as it is now — weak check would pass.
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	// Caller, however, is talking about an earlier session at the same id.
	_, err := d.Close(context.Background(), expectStarted(1785500000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("a stale caller expectation must refuse; got %v", err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "kill-session" {
			t.Fatal("destroyed a session the caller did not mean — §5.4's exact failure")
		}
	}
}

// Omitting the expectation is allowed, but the caller must get the weaker
// guarantee explicitly rather than silently.
func TestCloseWithoutExpectationFallsBackAndSaysSo(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	_, err := d.Close(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("no expectation and no prior sighting must refuse; got %v", err)
	}
	if !strings.Contains(err.Error(), "no expected start time") {
		t.Errorf("refusal should name which check it applied, got: %v", err)
	}
}

// A caller cannot quote a start time it was never given, so reads must carry it.
func TestListExposesStartedAtSoCallersCanCorroborate(t *testing.T) {
	d := newTestDriver(twoSessions())
	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.StartedAt == nil {
			t.Errorf("session %q has no StartedAt; the caller has nothing to quote back "+
				"and §5.4's strong check is unreachable", s.ID)
		}
	}
}

// The submit race, from a sibling project's measurements: pasting and
// submitting back-to-back lets the submit win, the prompt is submitted EMPTY,
// and the text lands afterwards where it sits unsent forever. Counted there at
// eight stranded operator instructions in one day.
func TestSubmitOnlyAfterTheTextIsVisible(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued", got.Outcome)
	}
	// The capture must happen between the paste and the submit.
	var pasteAt, captureAt, submitAt = -1, -1, -1
	for i, c := range f.callsSnapshot() {
		switch c[0] {
		case "load-buffer":
			pasteAt = i
		case "capture-pane":
			if captureAt < 0 && pasteAt >= 0 {
				captureAt = i
			}
		case "send-keys":
			submitAt = i
		}
	}
	if pasteAt < 0 || captureAt < 0 || submitAt < 0 {
		t.Fatalf("expected paste, capture and submit; got %d/%d/%d", pasteAt, captureAt, submitAt)
	}
	if !(pasteAt < captureAt && captureAt < submitAt) {
		t.Errorf("order was paste=%d capture=%d submit=%d; the confirmation must sit "+
			"between them or the submit can win the race", pasteAt, captureAt, submitAt)
	}
}

// `Enter` was observed being silently dropped on a pane where `C-m` submitted
// immediately — same text, seconds apart. Only one of them has been seen to
// work when the other did not.
func TestSubmitUsesControlM(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "x", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			last := c[len(c)-1]
			if last == "Enter" {
				t.Error("submitted with Enter; C-m is the form measured to work where Enter did not")
			}
			if last != "C-m" {
				t.Errorf("submit key = %q, want C-m", last)
			}
		}
	}
}

// sentKeys returns the key names from a `send-keys` invocation — everything
// after the `-t <pane>` target. It is deliberately dumb: the assertion below is
// about the ORDER of the keys on the wire, and anything smarter would be
// re-implementing the driver instead of checking it.
func sentKeys(argv []string) []string {
	for i := 0; i < len(argv); i++ {
		if argv[i] == "-t" {
			return argv[i+2:]
		}
	}
	return argv[1:]
}

// A "printable key" is one that puts a character on screen — the property the
// measurement turns on. Named control keys (C-m, Enter, Escape, BSpace) are
// not printable; `Space` and the digits are.
func printableKey(k string) bool {
	if k == "Space" {
		return true
	}
	r := []rune(k)
	return len(r) == 1 && unicode.IsPrint(r[0])
}

func isNewlineKey(k string) bool { return k == "C-m" || k == "Enter" }

// The FIRST keystroke into an idle pane is swallowed when that keystroke is
// Enter — measured 6 times out of 6 on real sessions, where the same pane
// accepted a printable key and then submitted on the very next newline. A paste
// is not a keystroke, so after paste-buffer the submit is always the first
// keystroke: a lone newline strands the delivered text in the composer while
// the receipt reports success.
//
// So every submit must carry a printable wake key IMMEDIATELY before its
// newline, in the same invocation — a second call would race, and a gap would
// put the newline back in the first-keystroke slot.
//
// The trailing space is deliberate. Tidying it away with a `BSpace` before the
// newline would restore a non-printable key as the first post-idle keystroke,
// which is the untested case; this test fails that shape too, because BSpace is
// not printable.
func TestEverySubmitCarriesAPrintableWakeKeyBeforeTheNewline(t *testing.T) {
	prompted := func() *fakeMux {
		f := twoSessions()
		f.captures["%1"] = fixtureTrustPrompt
		return f
	}
	cases := []struct {
		name string
		mux  *fakeMux
		act  func(*Driver) error
	}{
		{
			// The post-paste submit: the site the defect was measured on.
			name: "send",
			mux:  twoSessions(),
			act: func(d *Driver) error {
				_, err := d.Send(context.Background(), testCaller,
					fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing",
					driver.SendOptions{Submit: true})
				return err
			},
		},
		{
			// "Accept the highlighted option" — the other path that used to
			// send a bare newline into a pane that has been idle by definition,
			// since it has been sitting on a question.
			name: "respond/accept-highlighted",
			mux:  prompted(),
			act: func(d *Driver) error {
				_, err := d.Respond(context.Background(), testCaller,
					fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{})
				return err
			},
		},
		{
			// Never affected — it leads with a printable digit — and that is
			// exactly why it corroborates the diagnosis. Pinned so a later
			// tidy-up cannot quietly remove the wake key that is already there.
			name: "respond/by-choice",
			mux:  prompted(),
			act: func(d *Driver) error {
				_, err := d.Respond(context.Background(), testCaller,
					fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.Response{Choice: 2})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(tc.mux)
			if err := tc.act(d); err != nil {
				t.Fatal(err)
			}
			submits := 0
			for _, c := range tc.mux.callsSnapshot() {
				if c[0] != "send-keys" {
					continue
				}
				keys := sentKeys(c)
				var last string
				if len(keys) > 0 {
					last = keys[len(keys)-1]
				}
				if !isNewlineKey(last) {
					continue // not a submit — Escape, C-u and friends
				}
				submits++
				if len(keys) < 2 {
					t.Fatalf("submit sent %v — a lone newline is swallowed as the first "+
						"keystroke into an idle pane (6/6), stranding the text in the composer", keys)
				}
				if wake := keys[len(keys)-2]; !printableKey(wake) {
					t.Errorf("key before the newline is %q, which is not printable; keys were %v — "+
						"only a printable key was measured to wake the pane", wake, keys)
				}
			}
			if submits == 0 {
				t.Fatal("no submit invocation was made at all; this assertion checked nothing")
			}
		})
	}
}

// A pane that never renders the delivered text must not be submitted blind,
// and the caller must be TOLD the text is sitting there — silence is how a
// session ends up holding an instruction nobody knows about.
func TestUnrenderedTextIsReportedNotSubmitted(t *testing.T) {
	f := twoSessions()
	f.noEcho = true
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "never renders", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Errorf("outcome = %q, want unknown — delivered but not confirmed", got.Outcome)
	}
	if !strings.Contains(got.Reason, "unsent") {
		t.Errorf("reason = %q; it must say the text is sitting in the composer", got.Reason)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "send-keys" {
			t.Error("submitted without confirming the text had rendered")
		}
	}
}

// §8: "`since` is the time the status was first observed to hold, not the time
// it began." Holding it steady across reads is what makes duration meaningful.
func TestSinceHoldsWhileTheStatusDoesAndResetsWhenItChanges(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))

	first, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Since == nil {
		t.Fatal("no since recorded")
	}
	t0 := *first.Since

	clock = clock.Add(2 * time.Hour)
	again, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Since.Equal(t0) {
		t.Errorf("since moved to %v while the status was unchanged; duration is only "+
			"meaningful if the clock does not restart on every read", *again.Since)
	}

	// A real change restarts it.
	f.setCapture("%1", fixtureWorking)
	clock = clock.Add(time.Minute)
	changed, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Since.Equal(t0) {
		t.Error("since survived a status change; it marks when THIS status began")
	}
}

// The discriminator a sibling project could only get by typing into the pane —
// available passively as duration. Text unchanged for hours is not a sentence
// somebody is still composing.
func TestLongHeldUnsentInputSaysHowLong(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))

	// beta holds unsent input in the fixture.
	if _, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(14 * time.Hour)
	got, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != fleet.StatusWaitingInput {
		t.Fatalf("status = %q", got.Status)
	}
	if !strings.Contains(got.Evidence, "unchanged for") {
		t.Errorf("evidence = %q; a human reading this needs the age, which is the "+
			"whole difference between 'someone is typing' and 'nobody is coming back'", got.Evidence)
	}
	if age := clock.Sub(*got.Since); age < 13*time.Hour {
		t.Errorf("since implies %v; want the full holding period", age)
	}
}

// A brief hold is noise, not a story.
func TestRecentUnsentInputDoesNotShoutAboutAge(t *testing.T) {
	f := twoSessions()
	clock := time.Unix(1785760000, 0)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return clock }))
	if _, err := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(30 * time.Second)
	got, _ := d.State(context.Background(), testCaller, fleet.SessionRef{ID: "beta"})
	if strings.Contains(got.Evidence, "unchanged for") {
		t.Errorf("evidence = %q; thirty seconds is somebody typing", got.Evidence)
	}
}

// The abstraction is only complete if a client never has to know what the
// substrate is — including for the one job a supervisor's users touch
// directly, which is putting a terminal in front of a session.
func TestListCarriesAnAttachHint(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var h *fleet.AttachHint
	for _, s := range col.Items() {
		if s.ID == "alpha💬" {
			h = s.Attach
		}
	}
	if h == nil {
		t.Fatal("no attach hint; a client would have to know this is a multiplexer")
	}
	if h.Target != "alpha💬" {
		t.Errorf("target = %q, want the session's own handle", h.Target)
	}
	if len(h.Command) == 0 || len(h.ReadOnly) == 0 {
		t.Fatal("both a take-over and a watch form are needed; a client that cannot tell them apart offers the dangerous one")
	}
	// Argv, not a shell string: this id contains an emoji, and a caller
	// interpolating it into a shell is a quoting bug waiting to happen.
	last := h.Command[len(h.Command)-1]
	if last != "alpha💬" {
		t.Errorf("command should end with the verbatim id, got %q", last)
	}
	if h.Command[0] == "tmux" {
		t.Error("hint must carry the resolved binary path; a bare name fails in the non-interactive shell a supervisor runs it from")
	}
	if !h.Shared {
		t.Error("this substrate allows concurrent viewers; saying otherwise makes a supervisor warn about eviction that cannot happen")
	}
}

// A multi-line paste is not echoed: the runtime collapses it into a summary,
// so the bytes just delivered appear nowhere on screen. Matching the text then
// fails forever and every long message is reported stranded — delivered,
// honestly refused, and left sitting in the composer. Measured the first time a
// long message went to a live session.
func TestCollapsedPasteCountsAsDelivered(t *testing.T) {
	const rule = "────────────────────"
	cases := []struct {
		name    string
		painted string
		want    bool
	}{
		{"the observed form", rule + "\n❯ [Pasted text #1 +8 lines]\n" + rule, true},
		{"reworded, still counting lines", rule + "\n❯ [attached 12 lines]\n" + rule, true},
		{"a bracketed thing that is not a paste", rule + "\n❯ see [the docs] first\n" + rule, false},
		{"an ordinary typed line", rule + "\n❯ merge it\n" + rule, false},
		{"empty composer", rule + "\n❯ \n" + rule, false},
		{"a bracket somewhere else entirely", "transcript [4 lines] here\n" + rule + "\n❯ \n" + rule, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerHoldsCollapsedPaste(tc.painted); got != tc.want {
				t.Errorf("composerHoldsCollapsedPaste = %v, want %v", got, tc.want)
			}
		})
	}
}

// Rename changes the very thing callers address a session by, so it is
// corroborated exactly as Close is — and its failure is quieter than Close's:
// renaming the wrong session succeeds silently and leaves two sessions
// misnamed rather than raising anything.
func TestRenameCorroboratesLikeClose(t *testing.T) {
	ctx := context.Background()

	t.Run("with the caller's expected start time", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		started := time.Unix(1785600000, 0)
		req := testCaller
		req.Expect.StartedAt = &started

		if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		col, _ := d.List(ctx, testCaller, driver.ListFilter{})
		var names []string
		for _, s := range col.Items() {
			names = append(names, s.ID)
		}
		if !slicesContains(names, "renamed💬") || slicesContains(names, "alpha💬") {
			t.Errorf("after rename the fleet reads %v", names)
		}
	})

	t.Run("a stale expectation is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		wrong := time.Unix(1700000000, 0)
		req := testCaller
		req.Expect.StartedAt = &wrong

		_, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("want an ambiguous-target refusal, got %v", err)
		}
	})

	t.Run("nothing to corroborate against is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f) // never listed, so the driver has no sighting
		_, err := d.Rename(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("want a refusal when nothing corroborates, got %v", err)
		}
	})

	t.Run("a name already in use is refused", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		started := time.Unix(1785600000, 0)
		req := testCaller
		req.Expect.StartedAt = &started

		_, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "beta")
		if err == nil {
			t.Error("renaming onto a live session's name must be refused, not left to the multiplexer")
		}
	})

	t.Run("an unknown id is not found", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		_, err := d.Rename(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"}, "whatever")
		if !errors.Is(err, fleet.ErrNoSuchSession) {
			t.Errorf("want no-such-session, got %v", err)
		}
	})
}

// The driver's memory must move with the name, or the renamed session looks
// brand new: `since` resets and §12 reports it as newly adopted.
func TestRenameCarriesTheDriversMemory(t *testing.T) {
	ctx := context.Background()
	f := twoSessions()
	d := newTestDriver(f)
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	started := time.Unix(1785600000, 0)
	req := testCaller
	req.Expect.StartedAt = &started
	if _, err := d.Rename(ctx, req, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "renamed💬"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	d.mu.Lock()
	_, oldGone := d.observed["alpha💬"]
	_, newKept := d.observed["renamed💬"]
	d.mu.Unlock()
	if oldGone {
		t.Error("the old id is still remembered")
	}
	if !newKept {
		t.Error("the driver forgot the session it just renamed — since would reset and §12 would call it adopted")
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Discard destroys somebody's typing, so every refusal here is the feature.
func TestDiscardRefusesWhatItCannotCorroborate(t *testing.T) {
	ctx := context.Background()

	t.Run("blind discard is refused", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent
		d := newTestDriver(f)
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("no digest should be refused, not treated as permission; got %v", err)
		}
	})

	t.Run("a stale digest is refused", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent
		d := newTestDriver(f)
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"}, "not-the-digest")
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Errorf("a changed composer must refuse — somebody may be typing; got %v", err)
		}
	})

	t.Run("an empty composer succeeds, so a retry is safe", func(t *testing.T) {
		f := twoSessions() // alpha's composer is empty
		d := newTestDriver(f)
		if _, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "anything"); err != nil {
			t.Errorf("clearing nothing destroys nothing and must not fail: %v", err)
		}
	})

	t.Run("an unknown session is not found", func(t *testing.T) {
		d := newTestDriver(twoSessions())
		_, err := d.Discard(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "ghost"}, "x")
		if !errors.Is(err, fleet.ErrNoSuchSession) {
			t.Errorf("want no-such-session, got %v", err)
		}
	})
}

// The happy path: the digest the caller read is the digest that gets cleared,
// and the clear is VERIFIED rather than assumed — a keystroke that did not
// register looks exactly like one that did.
func TestDiscardClearsWhatTheCallerSaw(t *testing.T) {
	f := twoSessions()
	f.captures["%2"] = fixtureUnsent
	d := newTestDriver(f)

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, s := range col.Items() {
		if s.ID == "beta" {
			digest = s.State.ComposerDigest
		}
	}
	if digest == "" {
		t.Fatal("a session holding unsent text published no composerDigest, so a caller cannot discard it safely")
	}

	if _, err := d.Discard(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, digest); err != nil {
		t.Fatalf("discard with the digest the caller read: %v", err)
	}

	var sawClear bool
	for _, call := range f.callsSnapshot() {
		if len(call) >= 4 && call[0] == "send-keys" && call[len(call)-1] == "C-u" {
			sawClear = true
		}
	}
	if !sawClear {
		t.Error("no clear keystroke was sent")
	}
}

// #3: send's own safety refusal used to be a dead end. It delivers, cannot
// confirm, and says the text is sitting there — after which a second send is
// refused by the very rule that protects the text, and nothing else submits.
func TestResumeSubmitsOnlyWhatThisDriverStranded(t *testing.T) {
	ctx := context.Background()

	t.Run("resume completes our own stranded delivery", func(t *testing.T) {
		f := twoSessions()
		f.noEcho = true // the pane never renders the paste, so confirm times out
		d := newTestDriver(f)

		r1, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the long message", driver.SendOptions{Submit: true})
		if err != nil {
			t.Fatal(err)
		}
		if r1.Outcome != fleet.OutcomeUnknown {
			t.Fatalf("expected an unconfirmed delivery, got %s", r1.Outcome)
		}

		// The text is now visibly in the composer.
		f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ the long message\n"+rule+"\n")
		f.noEcho = false

		r2, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the long message", driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if err != nil {
			t.Fatal(err)
		}
		if r2.Outcome != fleet.OutcomeSubmitted {
			t.Errorf("resume outcome = %s (%s), want submitted", r2.Outcome, r2.Reason)
		}
	})

	t.Run("resume never submits text we did not place", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = fixtureUnsent // a human's typing, nothing to do with us
		d := newTestDriver(f)

		r, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "beta"},
			"something else entirely", driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != fleet.OutcomeRefused {
			t.Errorf("outcome = %s, want refused — this driver never delivered that text", r.Outcome)
		}
	})

	t.Run("resume with different text is refused", func(t *testing.T) {
		f := twoSessions()
		f.noEcho = true
		d := newTestDriver(f)
		if _, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"the original", driver.SendOptions{Submit: true}); err != nil {
			t.Fatal(err)
		}
		f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ the original\n"+rule+"\n")
		f.noEcho = false

		r, err := d.Send(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
			"NOT the original", driver.SendOptions{Submit: true, ResumeIfStranded: true})
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != fleet.OutcomeRefused {
			t.Errorf("outcome = %s, want refused — resume finishes one delivery, it does not start another", r.Outcome)
		}
	})
}

// A usage limit belongs to the account and lasts days; the notice is on screen
// for seconds. Measured on a live fleet: 51 sessions, the notice visible in 2
// panes, 0 reported blocked, 48 reporting idle — the status that means "send it
// work" — while the account had four days left to run.
func TestQuotaBlockOutlivesTheNoticeThatAnnouncedIt(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	// beta shows the limit; alpha looks perfectly idle, as most panes do.
	f.captures["%2"] = "transcript\nYou've hit your weekly limit · resets Aug 10 at 12am\n" +
		rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status != fleet.StatusQuotaBlocked {
			t.Errorf("%s reported %s; every session on a refusing account is blocked",
				s.ID, s.State.Status)
		}
	}

	// The notice scrolls away, which is what actually happened.
	f.setCapture("%2", idleFixtureFor("beta"))
	col, err = d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status != fleet.StatusQuotaBlocked {
			t.Errorf("%s went back to %s once the notice scrolled away — the block is remembered, not read",
				s.ID, s.State.Status)
		}
		if s.State.Quota == nil || s.State.Quota.ResetHint == "" {
			t.Errorf("%s lost the reset hint; a caller should not scrape it from prose", s.ID)
		}
	}
}

// One working session is proof the account works. Nothing else clears the
// block — least of all the scraped reset time, which is prose the runtime may
// reword and which a supervisor next door already parsed into garbage.
func TestOneWorkingSessionClearsTheQuotaBlock(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%2"] = "transcript\nYou've hit your weekly limit\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	// The account recovers: a session starts working again.
	f.setCapture("%1", "transcript\n✻ Brewing… (3s · ↓ 1.2k tokens)\n"+rule+"\n❯ \n"+rule+"\n")
	f.setCapture("%2", idleFixtureFor("beta"))
	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status == fleet.StatusQuotaBlocked {
			t.Errorf("%s still blocked after a session was observed working", s.ID)
		}
	}
}

// A notice outlives the limit it announced: nobody types into a session that
// refused them, so the screen never changes. Read literally it says "blocked"
// forever — measured on a working machine as two sessions blocked by expired
// notices while two others on that account worked.
func TestWorkingSessionOverrulesAnExpiredNoticeOnAnotherScreen(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "transcript\n✻ Brewing… (3s · ↓ 1.2k tokens)\n" + rule + "\n❯ \n" + rule + "\n"
	f.captures["%2"] = "  ⎿  You've hit your session limit · resets 3:40pm (Asia/Saigon)\n" +
		"     /upgrade to increase your usage limit.\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var sawWorking bool
	for _, s := range col.Items() {
		switch s.State.Status {
		case fleet.StatusWorking:
			sawWorking = true
		case fleet.StatusQuotaBlocked:
			t.Errorf("%s reported blocked while another session on the account works", s.ID)
		}
		if s.State.Quota != nil {
			t.Errorf("%s: an overruled notice left a quota block behind", s.ID)
		}
	}
	if !sawWorking {
		t.Fatal("fixture never produced a working session; the test proves nothing")
	}
}

// A block always carries a real since. The per-session path builds it in
// classify, which has no clock; the zero time serialises as year 1, so a
// caller asking "blocked for how long" got two millennia.
func TestQuotaBlockCarriesARealSince(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	notice := "  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)\n" + rule + "\n❯ \n" + rule + "\n"
	f.captures["%1"] = notice
	f.captures["%2"] = notice
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		q := s.State.Quota
		if q == nil {
			t.Fatalf("%s: no quota block on a blocked session", s.ID)
		}
		if q.Since.IsZero() {
			t.Errorf("%s: since is the zero time, which claims a date in year 1", s.ID)
		}
	}
}

// unknown is not a competing truth — it is this driver saying it could not
// determine one, and an account fact is more specific than that. Left out at
// first, which showed up as four sessions flapping between unknown and
// quota_blocked across consecutive reads, on panes that redraw a counter.
func TestAccountBlockCoversSessionsItCouldNotClassify(t *testing.T) {
	ctx := context.Background()
	const rule = "────────────────────"
	f := twoSessions()
	f.captures["%1"] = "transcript\nYou've hit your weekly limit · resets Aug 10\n" + rule + "\n❯ \n" + rule + "\n"
	// A pane with no spinner and an empty composer, seen once: the driver has
	// nothing to settle it against, so it classifies unknown.
	f.captures["%2"] = "transcript\n" + rule + "\n❯ \n" + rule + "\n"
	d := newTestDriver(f)

	col, err := d.List(ctx, testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range col.Items() {
		if s.State.Status == fleet.StatusUnknown {
			t.Errorf("%s left unknown while the account is refusing work — "+
				"unknown is the absence of a truth, not a better one", s.ID)
		}
	}
}
