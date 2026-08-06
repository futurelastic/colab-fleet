package tmux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// Event subscription over the multiplexer's control mode.
//
// # What the substrate actually offers, measured
//
// Control mode is a real push channel — no polling, notifications arrive
// sub-second. But its scoping is not what a naive reading assumes, and the
// difference decides the whole design. Probed directly:
//
//	%output for the ATTACHED session ........................ delivered
//	%output for a sibling session .......................... NOT delivered
//	%subscription-changed (refresh-client -B, %pane) ....... attached only
//	%subscription-changed targeting a sibling's pane by id . NOT delivered
//	%sessions-changed / %unlinked-window-add / -close ...... DELIVERED (global)
//
// So content events are per-attachment and lifecycle events are global.
// That asymmetry is the entire architecture:
//
//   - ONE always-on control client yields every session appearing and
//     disappearing, fleet-wide, regardless of which session it is attached
//     to.
//   - Content changes need a client attached to the session in question, so
//     those are opened on demand — when a req subscribes — and reaped on
//     unsubscribe. Cost is O(subscribers), typically one or two, rather than
//     O(sessions).
//
// The rejected alternative was a client per session at startup. It works and
// it is simpler, but it restores exactly the per-session process cost the
// batched enumeration exists to avoid, and it buys fidelity nobody asked
// for: %output is a raw byte firehose, and this driver does not want bytes.
//
// Verified separately, because it would have been disqualifying: attaching a
// control client does NOT renegotiate the session's size. A session created
// at 200x50 measured 200x50 before attach, during attach, and after detach.
// Subscribing to somebody's live session does not reflow their terminal.
//
// # Push-triggered pull
//
// Notifications are used only as CHANGE TRIGGERS. When one arrives, this
// code re-runs the ordinary batched enumeration and diffs the result. It
// never parses %output's payload to infer what happened.
//
// That is deliberate, and it is not the polling §5.5 forbids: reads are
// edge-triggered by the substrate, never timer-triggered. Nothing wakes up
// on an interval, and an idle fleet costs nothing. What it buys is that
// exactly one code path decides what a session's state is — the same
// classifier List and State use. A second, stream-only interpretation of
// pane bytes would be a second source of truth about status, free to drift
// from the first, and the two would disagree only under load, which is when
// anybody would care.
//
// # FINDING: Event carries fields a driver cannot fill
//
// fleet.Event has Cursor and Epoch, which §7.3 defines as assigned by "each
// service instance" — a driver has no access to either, and two drivers
// under one service must not be minting competing cursor sequences. This
// driver therefore leaves both zero and the service is expected to stamp
// them on the way out. Recorded because the type's shape implies drivers
// fill it in, and any driver that tried would be wrong in a way that only
// shows up as a subscriber silently missing a resync.

// lifecycleKey is the conns map key for the lifecycle client. It is not a
// valid session name, so it can never collide with one.
const lifecycleKey = "\x00lifecycle"

const (
	// coalesceWindow batches a burst of notifications into one enumeration.
	// An agent producing output emits many %output notifications per
	// second; the state it is in changes far more slowly. Without this,
	// a chatty session would drive one full enumeration per output chunk.
	coalesceWindow = 150 * time.Millisecond
)

// ctlNote is one parsed control-mode notification: the leading %name and
// its whitespace-separated arguments. The payload after the arguments is
// deliberately discarded — see "push-triggered pull" above.
type ctlNote struct {
	Name string
	Args []string
}

// ctlConn is one live control-mode client.
type ctlConn interface {
	Notes() <-chan ctlNote
	Close() error
}

// maxContentClients bounds how many per-session control clients ONE
// subscription may open.
//
// The number is chosen against the multiplexer server's descriptor budget,
// not against this process's: a server holding ~80 descriptors idle was
// pushed past its limit by 62 of these, and the failure surfaced as every
// new client being refused — including a human's terminal. 16 leaves an
// order of magnitude of headroom for several concurrent subscribers plus
// everything else on the machine.
const maxContentClients = 16

// ctlDialer opens a control-mode client attached to one session. Injected
// so tests can drive subscription logic without spawning a multiplexer.
type ctlDialer func(ctx context.Context, bin, session string) (ctlConn, error)

// withCtlDialer injects a fake control-mode transport. Unexported: tests only.
func withCtlDialer(f ctlDialer) Option { return func(d *Driver) { d.dial = f } }

// realCtlConn attaches a control-mode client as a subprocess.
type realCtlConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	notes chan ctlNote
	once  sync.Once
}

func dialReal(ctx context.Context, bin, session string) (ctlConn, error) {
	// -C is control mode. Attaching is what scopes content notifications to
	// this session; there is no unattached form that sees them.
	cmd := exec.Command(bin, "-C", "attach-session", "-t", session)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stdin must stay open for the lifetime of the client: closing it is
	// how the client is told to detach, so it doubles as the shutdown
	// signal in Close.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &realCtlConn{cmd: cmd, stdin: stdin, notes: make(chan ctlNote, 64)}
	go func() {
		defer close(c.notes)
		sc := bufio.NewScanner(stdout)
		// Control-mode lines can be long: %output carries a pane's entire
		// output chunk octal-escaped on one line.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			note, ok := parseNote(sc.Text())
			if !ok {
				continue
			}
			select {
			case c.notes <- note:
			default:
				// A full buffer means the consumer is behind. Dropping is
				// correct here rather than blocking: these are triggers,
				// not data, and a dropped trigger is covered by the next
				// one — every notification causes the same full
				// re-enumeration. Blocking would stall the multiplexer's
				// client instead.
			}
		}
	}()
	return c, nil
}

func (c *realCtlConn) Notes() <-chan ctlNote { return c.notes }

func (c *realCtlConn) Close() error {
	c.once.Do(func() {
		// Closing stdin asks the client to detach cleanly; killing is the
		// fallback if it does not.
		_ = c.stdin.Close()
		done := make(chan struct{})
		go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	})
	return nil
}

// parseNote splits a control-mode line into its notification name and
// arguments. Lines that are not notifications (command output, %begin/%end
// bodies) are rejected.
func parseNote(line string) (ctlNote, bool) {
	if !strings.HasPrefix(line, "%") {
		return ctlNote{}, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ctlNote{}, false
	}
	name := strings.TrimPrefix(fields[0], "%")
	if name == "" {
		return ctlNote{}, false
	}
	return ctlNote{Name: name, Args: fields[1:]}, true
}

// isLifecycleNote reports whether a notification means the set of sessions
// may have changed. These arrive on ANY attached client, fleet-wide.
func isLifecycleNote(name string) bool {
	switch name {
	case "sessions-changed", "unlinked-window-add", "unlinked-window-close",
		"window-add", "window-close", "session-closed", "exit":
		return true
	}
	return false
}

// isContentNote reports whether a notification means a session's screen may
// have changed. Scoped to the client's attached session.
func isContentNote(name string) bool {
	switch name {
	case "output", "subscription-changed", "layout-change", "window-renamed":
		return true
	}
	return false
}

// eventStream implements driver.EventStream over control-mode clients.
type eventStream struct {
	d       *Driver
	req     fleet.Request
	filter  driver.SubscribeFilter
	cancel  context.CancelFunc
	out     chan fleet.Event
	errc    chan error
	trigger chan struct{}

	// quota tracking, owned by run() alone — no lock.
	quotaKnown bool
	quotaBlock *fleet.QuotaBlock

	mu      sync.Mutex
	conns   map[string]ctlConn // session id -> content client
	closed  bool
	closeCh chan struct{}
}

// Subscribe opens a live event stream (§3, §5.5).
//
// Sessions matching the filter at subscribe time get a content client each;
// sessions that appear later and match are picked up as the lifecycle client
// reports them. Both kinds of notification are triggers for the same
// enumerate-and-diff.
func (d *Driver) Subscribe(ctx context.Context, req fleet.Request, filter driver.SubscribeFilter) (driver.EventStream, error) {
	// The lifecycle client must attach to *some* session, because there is
	// no unattached control-mode form that receives notifications. With no
	// sessions on the machine there is nothing to attach to — and also
	// nothing to report, so this is not as circular as it looks: the first
	// session's creation is missed, and every subsequent change is seen.
	// Documented rather than papered over; closing it would need a
	// keep-alive session this driver has no business creating.
	listCtx, cancelList := d.bounded(ctx)
	rows, _, err := d.enumerate(listCtx)
	cancelList()
	if err != nil {
		return nil, fmt.Errorf("subscribe: enumerating: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("subscribe: no sessions to attach a control client to; "+
			"control mode has no unattached form (%w)", driver.ErrUnsupported)
	}

	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &eventStream{
		d:       d,
		req:     req,
		filter:  filter,
		cancel:  cancel,
		out:     make(chan fleet.Event, 64),
		errc:    make(chan error, 1),
		conns:   map[string]ctlConn{},
		closeCh: make(chan struct{}),
		trigger: make(chan struct{}, 1),
	}

	trigger := s.trigger

	// The lifecycle client: attached to an arbitrary session, listened to
	// for fleet-wide session appearance and disappearance.
	life, err := d.dial(streamCtx, d.bin, rows[0].session)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("subscribe: opening lifecycle client: %w", err)
	}
	go s.superviseLifecycle(streamCtx, trigger, life)

	// Content clients for the sessions this subscription cares about — and
	// only those. This is where filter granularity turns into cost: one
	// connection per watched session, so a caller that named two sessions
	// opens two, not one per session on the machine.
	//
	// Bounded, because the cost is not paid in this process. Each client is a
	// connection to a multiplexer server that other tools — launchers,
	// supervisors, a human's terminal — also depend on, and that server has a
	// file-descriptor budget shared by all of them. An unbounded subscription
	// therefore does not degrade itself, it degrades the MACHINE: measured
	// during an incident where one forgotten subscriber held 62 clients on a
	// 69-session host, exhausted the server's descriptors, and left every new
	// attach failing with "server exited unexpectedly" — while every session
	// was in fact alive and healthy.
	//
	// Capping is safe because of how this driver uses notifications. They are
	// triggers, never data: ANY notification causes a full enumerate-and-diff
	// across every session (see "push-triggered pull"). So watching a subset
	// still detects changes everywhere; what degrades is latency on sessions
	// that are quiet while the watched ones are also quiet — and lifecycle
	// events, which are global, are unaffected either way.
	//
	// A caller that names sessions gets exactly those, up to the cap, because
	// naming is a statement about what matters.
	opened := 0
	for _, r := range rows {
		if !filter.Matches(r.session, r.cwd) {
			continue
		}
		if opened >= maxContentClients {
			// Said out loud rather than silently truncated: a subscriber
			// that believes it has per-session triggers for everything and
			// does not is exactly the "confident report on evidence the
			// reporter manufactured" this project keeps meeting.
			log.Printf("tmux: subscription watching %d sessions, capped at %d content clients; "+
				"changes are still detected fleet-wide (notifications are triggers, not data), "+
				"latency may rise for unwatched sessions",
				len(rows), maxContentClients)
			break
		}
		conn, err := d.dial(streamCtx, d.bin, r.session)
		if err != nil {
			continue // a session that vanished between enumerate and dial
		}
		s.mu.Lock()
		s.conns[r.session] = conn
		s.mu.Unlock()
		opened++
		go pump(conn.Notes(), trigger, isContentNote, streamCtx)
	}
	s.mu.Lock()
	s.conns[lifecycleKey] = life
	s.mu.Unlock()

	// Seed the baseline BEFORE returning, not inside the engine goroutine.
	//
	// If the engine took its own first reading, everything that happened
	// between Subscribe returning and that reading would be absorbed into
	// the baseline and never reported — a subscriber would hold a stream it
	// believes is complete, with a silent hole at the front of it. §7.3
	// draws exactly this line: "announced gaps are recoverable; silent gaps
	// are not." A gap here cannot even be announced, because nothing knows
	// it happened.
	//
	// Seeding synchronously makes the guarantee stateable: every change
	// after Subscribe returns is either delivered or is a bug.
	seed := map[string]fleet.Session{}
	if base, err := d.List(ctx, req, driver.ListFilter{CwdPrefix: filter.CwdPrefix}); err == nil {
		for _, sess := range base.Items() {
			if !filter.Matches(sess.ID, string(sess.Cwd)) {
				continue
			}
			seed[sess.ID] = sess
		}
	}

	go s.run(streamCtx, trigger, seed)
	return s, nil
}

// superviseLifecycle keeps a lifecycle client alive across the death of
// whichever session it happens to be attached to.
//
// This exists because of an asymmetry that is easy to miss: the lifecycle
// client is attached to a session the driver does not own and did not
// create. Control mode has no unattached form, so *some* arbitrary session
// has to host the connection — and when that session exits, the client exits
// with it.
//
// Without supervision the failure is silent and total. The client's
// notification channel closes, the pump returns, and the stream keeps
// running while receiving nothing. Every subscriber then sees a healthy,
// open, permanently empty stream. That is the worst failure shape available
// to this design, because a stream delivering nothing is indistinguishable
// from a fleet in which nothing is happening — the observer cannot tell
// "quiet" from "deaf", which is §5.7's confusion wearing a different hat.
//
// So: when the host session dies, re-attach to another one, and say so.
// If nothing can be attached to, report the source degraded rather than
// going quiet. A subscriber that is told it is degraded can refetch; a
// subscriber that is told nothing cannot.
func (s *eventStream) superviseLifecycle(ctx context.Context, trigger chan<- struct{}, first ctlConn) {
	conn := first
	for {
		// Drain this client until it dies or the stream is closed.
		pump(conn.Notes(), trigger, isLifecycleNote, ctx)
		if ctx.Err() != nil {
			return
		}

		// The host session went away. Everything about the fleet is now
		// unobserved until a new client is attached, so say so before
		// trying.
		s.emit(ctx, fleet.Event{
			Machine: s.d.machine,
			Kind:    fleet.EventSourceStatus,
			Payload: fleet.SourceStatus{
				Machine:    s.d.machine,
				Status:     fleet.SourceDegraded,
				Error:      "lifecycle control client lost its host session; re-attaching",
				ObservedAt: s.d.now(),
			},
		})
		// A trigger too: the set of sessions demonstrably just changed.
		select {
		case trigger <- struct{}{}:
		default:
		}

		next, err := s.reattachLifecycle(ctx)
		if err != nil {
			return
		}
		conn = next
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[lifecycleKey] = conn
		s.mu.Unlock()
	}
}

// reattachLifecycle finds any surviving session and attaches to it, backing
// off while none exists. It gives up only when the stream is closed.
func (s *eventStream) reattachLifecycle(ctx context.Context) (ctlConn, error) {
	backoff := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		listCtx, cancel := s.d.bounded(ctx)
		rows, _, err := s.d.enumerate(listCtx)
		cancel()
		if err == nil {
			for _, r := range rows {
				if conn, derr := s.d.dial(ctx, s.d.bin, r.session); derr == nil {
					return conn, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// pump forwards interesting notifications to the trigger channel, coalescing
// by virtue of the channel holding at most one pending trigger.
func pump(notes <-chan ctlNote, trigger chan<- struct{}, want func(string) bool, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-notes:
			if !ok {
				return
			}
			if !want(n.Name) {
				continue
			}
			select {
			case trigger <- struct{}{}:
			default: // one pending trigger is as good as ten
			}
		}
	}
}

// run is the stream's engine: wait for a trigger, let a burst settle,
// enumerate once, diff, emit.
func (s *eventStream) run(ctx context.Context, trigger <-chan struct{}, known map[string]fleet.Session) {
	defer close(s.out)

	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}

		// Let a burst settle. An agent mid-turn emits many notifications
		// per second; its state changes far more slowly.
		select {
		case <-ctx.Done():
			return
		case <-time.After(coalesceWindow):
		}
		drain(trigger)

		cur, err := s.d.List(ctx, s.req, driver.ListFilter{CwdPrefix: s.filter.CwdPrefix})
		if err != nil {
			continue
		}
		if !cur.Complete() {
			// §5.7 on the event stream: a read that did not fully succeed
			// must not be diffed as though it had, or every unreadable
			// session emits a spurious "closed".
			for _, src := range cur.Sources() {
				if src.Status != fleet.SourceOK {
					s.emit(ctx, fleet.Event{
						Machine: s.d.machine,
						Kind:    fleet.EventSourceStatus,
						Payload: src,
					})
				}
			}
			continue
		}

		// The account's own state, before the per-session diff. A supervisor
		// that acts on this stops dispatching; a supervisor that waits for
		// the session diff learns the same thing one stalled session at a
		// time, which is how the fact was learned 48 times before.
		//
		// The first pass announces a block that is already in force — a
		// subscriber connecting mid-outage must not have to wait for a
		// transition that already happened — but says nothing when there is
		// none, because "not blocked" is the unremarkable case and an event
		// for it on every new subscription is noise.
		if q := s.d.quotaBlock(); (q != nil) != (s.quotaBlock != nil) || !s.quotaKnown {
			if q != nil || s.quotaKnown {
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventMachineQuota,
					Payload: fleet.MachineQuotaPayload{
						Machine: s.d.machine,
						Blocked: q != nil,
						Quota:   q,
					},
				})
			}
			s.quotaKnown = true
			s.quotaBlock = q
		}

		seen := map[string]bool{}
		for _, sess := range cur.Items() {
			// Attachments are filtered, but lifecycle notifications are
			// fleet-wide, so the diff sees everything. Narrow here too or a
			// subscription that named one session would still be told about
			// every session appearing anywhere.
			if !s.filter.Matches(sess.ID, string(sess.Cwd)) {
				continue
			}
			seen[sess.ID] = true
			prev, had := known[sess.ID]
			switch {
			case !had:
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventSessionCreated,
					Payload: sess,
				})
				s.attachContent(ctx, sess)
			case prev.State.Status != sess.State.Status:
				s.emit(ctx, fleet.Event{
					Machine: s.d.machine,
					Kind:    fleet.EventSessionState,
					Payload: fleet.SessionStatePayload{Ref: sess.SessionRef, State: sess.State},
				})
			}
			known[sess.ID] = sess
		}
		for id, prev := range known {
			if seen[id] {
				continue
			}
			s.emit(ctx, fleet.Event{
				Machine: s.d.machine,
				Kind:    fleet.EventSessionClosed,
				Payload: fleet.SessionStatePayload{
					Ref: prev.SessionRef,
					State: fleet.InferredState(fleet.StatusDead,
						"session no longer present in the multiplexer", nil),
				},
			})
			delete(known, id)
			s.detachContent(id)
		}
	}
}

func drain(c <-chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// attachContent opens a content client for a session that appeared after
// the subscription started. Best effort: without it, the session's
// lifecycle is still tracked, only its state changes are not pushed.
func (s *eventStream) attachContent(ctx context.Context, sess fleet.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, ok := s.conns[sess.ID]; ok {
		return
	}
	if !s.filter.Matches(sess.ID, string(sess.Cwd)) {
		return
	}
	// The same bound as Subscribe's initial pass, and this is the path that
	// actually matters over time: a subscription held for hours meets every
	// session the machine ever creates. Capping only the initial pass would
	// bound the wrong thing — the fleet at t=0 rather than the fleet's
	// accumulated history — and the leak would simply arrive more slowly.
	//
	// conns includes the lifecycle client, hence the +1.
	if len(s.conns) >= maxContentClients+1 {
		return
	}
	conn, err := s.d.dial(ctx, s.d.bin, sess.ID)
	if err != nil {
		return
	}
	s.conns[sess.ID] = conn
	// Content notifications share the same trigger path; this stream's
	// engine is already draining it.
	go pump(conn.Notes(), s.triggerFor(), isContentNote, ctx)
}

// triggerFor exists so attachContent can feed the same coalescing channel
// the engine reads. It is set once in Subscribe.
func (s *eventStream) triggerFor() chan<- struct{} { return s.trigger }

func (s *eventStream) detachContent(id string) {
	s.mu.Lock()
	conn, ok := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	if ok {
		_ = conn.Close()
	}
}

func (s *eventStream) emit(ctx context.Context, ev fleet.Event) {
	// Cursor and Epoch are deliberately left zero — §7.3 assigns them per
	// service instance, and a driver has neither. See this file's FINDING.
	select {
	case s.out <- ev:
	case <-ctx.Done():
	}
}

// Next blocks until an event is available, ctx is cancelled, or the stream
// ends (§3, §5.5: a req is never expected to poll).
func (s *eventStream) Next(ctx context.Context) (fleet.Event, error) {
	select {
	case <-ctx.Done():
		return fleet.Event{}, ctx.Err()
	case ev, ok := <-s.out:
		if !ok {
			return fleet.Event{}, io.EOF
		}
		return ev, nil
	}
}

// Close reaps every control client this subscription opened. A subscription
// that leaked clients would accumulate one attached process per subscribe
// call, which is the failure mode the demand-driven design exists to avoid.
func (s *eventStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]ctlConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[string]ctlConn{}
	s.mu.Unlock()

	s.cancel()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}
