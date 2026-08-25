package tmux

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// fakePS stands in for the OS process table `processStartedAt` queries via
// `ps -o lstart= -p <pid>` — the same kind of test double fakeMux already is
// for the multiplexer, kept separate on purpose (see Driver.psRun's doc
// comment in tmux.go: a fake built for one exec seam must not silently
// answer for the other).
type fakePS struct {
	lines map[int]string // pid -> the exact line the real ps would print
	fail  bool           // true models the binary itself failing to run
}

func (f *fakePS) exec(_ context.Context, _ string, args ...string) ([]byte, error) {
	if f.fail {
		return nil, errors.New("fakePS: ps binary failed to run")
	}
	pid := -1
	for i := 0; i < len(args); i++ {
		if args[i] == "-p" && i+1 < len(args) {
			pid, _ = strconv.Atoi(args[i+1])
		}
	}
	line, ok := f.lines[pid]
	if !ok {
		// The real ps prints nothing on stdout and exits non-zero when the
		// pid does not exist — modelled here as runReal's own caller
		// (exec.Cmd.Output) would report it: an error, no bytes.
		return nil, errors.New("fakePS: ps: No matching processes were found")
	}
	return []byte(line + "\n"), nil
}

func (f *fakePS) set(pid int, startedAt time.Time) {
	if f.lines == nil {
		f.lines = map[int]string{}
	}
	f.lines[pid] = startedAt.Format(psStartTimeLayout)
}

func (f *fakePS) remove(pid int) {
	delete(f.lines, pid)
}

func newProcessIdentityTestDriver(mux *fakeMux, ps *fakePS) *Driver {
	return New("testbox",
		withExec(mux.exec),
		withPSExec(ps.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
}

// oneLiveSession builds the smallest fixture ResolveProcessIdentity needs: a
// single non-dead pane whose reported pid is genuinely running, per ps.
func oneLiveSession(pid int, startedAt time.Time) (*fakeMux, *fakePS) {
	mux := &fakeMux{
		sessions: []fakeSession{
			{name: "alpha", paneID: "%1", cwd: "/work/alpha", pid: pid, created: 1785600000},
		},
		captures: map[string]string{"%1": idleFixtureFor("alpha")},
	}
	ps := &fakePS{}
	ps.set(pid, startedAt)
	return mux, ps
}

func TestResolveProcessIdentity_LiveSessionResolves(t *testing.T) {
	startedAt := time.Date(2026, 8, 26, 10, 15, 23, 0, time.Local)
	mux, ps := oneLiveSession(4242, startedAt)
	d := newProcessIdentityTestDriver(mux, ps)

	got, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if err != nil {
		t.Fatalf("ResolveProcessIdentity: %v", err)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
	if !got.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, startedAt)
	}
}

func TestResolveProcessIdentity_NoSuchSessionRefuses(t *testing.T) {
	mux, ps := oneLiveSession(4242, time.Now())
	d := newProcessIdentityTestDriver(mux, ps)

	_, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "does-not-exist"})
	if !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
}

func TestResolveProcessIdentity_DeadPaneRefuses(t *testing.T) {
	mux, ps := oneLiveSession(4242, time.Now())
	mux.sessions[0].dead = true
	d := newProcessIdentityTestDriver(mux, ps)

	_, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
}

// TestResolveProcessIdentity_ZeroPIDRefusesAndCounts is the "multiplexer
// reported no usable pid" branch — an unparsable #{pane_pid} decodes to 0
// (parseRows' strconv.Atoi failure mode), and #116 forbids treating that as
// "pid zero" rather than "no pid".
func TestResolveProcessIdentity_ZeroPIDRefusesAndCounts(t *testing.T) {
	mux, ps := oneLiveSession(0, time.Now())
	d := newProcessIdentityTestDriver(mux, ps)

	_, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
	if got := d.Counters()[counterProcessIdentityUnresolved]; got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

// TestResolveProcessIdentity_PSMissesRefusesAndCounts is the race #116's own
// report is about: the multiplexer reports a pid, but by the time this
// driver asks the OS, that process is already gone.
func TestResolveProcessIdentity_PSMissesRefusesAndCounts(t *testing.T) {
	mux, ps := oneLiveSession(4242, time.Now())
	ps.remove(4242) // the OS no longer has this pid
	d := newProcessIdentityTestDriver(mux, ps)

	_, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
	if got := d.Counters()[counterProcessIdentityUnresolved]; got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

func TestVerifyProcessIdentity_SameProcessStillRunningSucceeds(t *testing.T) {
	startedAt := time.Date(2026, 8, 26, 10, 15, 23, 0, time.Local)
	mux, ps := oneLiveSession(4242, startedAt)
	d := newProcessIdentityTestDriver(mux, ps)

	got, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if err != nil {
		t.Fatalf("ResolveProcessIdentity: %v", err)
	}
	if err := d.VerifyProcessIdentity(context.Background(), got); err != nil {
		t.Errorf("VerifyProcessIdentity on an unchanged process: %v", err)
	}
}

// TestVerifyProcessIdentity_RecycledPIDRefuses is #116's central scenario:
// the same numeric pid, a DIFFERENT process. VerifyProcessIdentity's whole
// reason to exist is catching this.
func TestVerifyProcessIdentity_RecycledPIDRefuses(t *testing.T) {
	startedAt := time.Date(2026, 8, 26, 10, 15, 23, 0, time.Local)
	mux, ps := oneLiveSession(4242, startedAt)
	d := newProcessIdentityTestDriver(mux, ps)

	want, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if err != nil {
		t.Fatalf("ResolveProcessIdentity: %v", err)
	}

	// The original process exits; the kernel hands pid 4242 to something new.
	ps.set(4242, startedAt.Add(5*time.Minute))

	if err := d.VerifyProcessIdentity(context.Background(), want); !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
}

func TestVerifyProcessIdentity_ProcessGoneRefuses(t *testing.T) {
	startedAt := time.Date(2026, 8, 26, 10, 15, 23, 0, time.Local)
	mux, ps := oneLiveSession(4242, startedAt)
	d := newProcessIdentityTestDriver(mux, ps)

	want, err := d.ResolveProcessIdentity(context.Background(), fleet.SessionRef{ID: "alpha"})
	if err != nil {
		t.Fatalf("ResolveProcessIdentity: %v", err)
	}

	ps.remove(4242)

	if err := d.VerifyProcessIdentity(context.Background(), want); !errors.Is(err, ErrProcessIdentityUnresolved) {
		t.Fatalf("err = %v, want ErrProcessIdentityUnresolved", err)
	}
}

func TestProcessIdentityCoverage_EverySessionResolves(t *testing.T) {
	mux := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Unix(1785600000, 0))
	ps.set(200, time.Unix(1785600001, 0))
	d := newProcessIdentityTestDriver(mux, ps)

	total, unresolved, err := d.ProcessIdentityCoverage(context.Background())
	if err != nil {
		t.Fatalf("ProcessIdentityCoverage: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

// TestProcessIdentityCoverage_ReportsAGap is #116's fourth requirement
// directly: a session this driver created but cannot resolve a process
// identity for must be NAMED, not merged silently into a passing total.
func TestProcessIdentityCoverage_ReportsAGap(t *testing.T) {
	mux := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Unix(1785600000, 0))
	// pid 200 ("beta") has no ps entry: unresolved.
	d := newProcessIdentityTestDriver(mux, ps)

	total, unresolved, err := d.ProcessIdentityCoverage(context.Background())
	if err != nil {
		t.Fatalf("ProcessIdentityCoverage: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(unresolved) != 1 || unresolved[0] != "beta" {
		t.Errorf("unresolved = %v, want [beta]", unresolved)
	}
	if got := d.Counters()[counterProcessIdentityUnresolved]; got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

// TestProcessIdentityCoverage_SkipsDeadPanes: a dead pane has no live
// process to resolve BY DESIGN — that is not the coverage gap #116's report
// is about, and counting it as one would make the signal noisy on every
// machine with ordinary churn.
func TestProcessIdentityCoverage_SkipsDeadPanes(t *testing.T) {
	mux := twoSessions()
	mux.sessions[1].dead = true // beta
	ps := &fakePS{}
	ps.set(100, time.Unix(1785600000, 0))
	d := newProcessIdentityTestDriver(mux, ps)

	total, unresolved, err := d.ProcessIdentityCoverage(context.Background())
	if err != nil {
		t.Fatalf("ProcessIdentityCoverage: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 (dead pane excluded)", total)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}
