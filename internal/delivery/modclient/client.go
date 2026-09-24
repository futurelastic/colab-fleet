package modclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// State is the client's view of whether the module can be used right now.
type State string

const (
	// StateStarting: a child is being launched or has not yet said hello and
	// passed its first health probe.
	StateStarting State = "starting"
	// StateAvailable: hello accepted and the last health probe was ok.
	StateAvailable State = "available"
	// StateUnavailable: no usable child — it exited, could not be spawned, or
	// reports itself unhealthy. The supervisor is restarting it (or, for an
	// unhealthy but connected child, probing until it recovers).
	StateUnavailable State = "unavailable"
	// StateDisabled: the module will not be used until the daemon restarts —
	// its hello was refused, or the client was stopped.
	StateDisabled State = "disabled"
)

// Deadlines are the per-request limits the client enforces on its own; the
// module is not trusted to answer in time.
type Deadlines struct {
	// Default is the limit for every operation not named below (15 s).
	Default time.Duration
	// Prepare is the limit for prepare-launch (5 s): it sits on the session
	// create path, so a slow module must cost a launch seconds, not minutes.
	Prepare time.Duration
	// AttachExtra is added to an attach's own TimeoutMs (5 s), the module-side
	// wait being the point of that call.
	AttachExtra time.Duration
}

// Config configures a Client. The zero value of every optional field selects
// the documented default.
type Config struct {
	// Name is the module's name, used in logs and Status.
	Name string
	// Path is the module executable; it is started as `<Path> serve`.
	Path string
	// Env is the COMPLETE child environment (see ChildEnv).
	Env []string
	// Launcher starts the process; nil means ExecLauncher.
	Launcher Launcher

	// HelloTimeout is how long a new child has to say hello (default 3 s).
	HelloTimeout time.Duration
	// BackoffMin/BackoffMax bound the delay before a restart (1 s, 60 s); the
	// delay doubles per consecutive short-lived child, and resets to
	// BackoffMin once a child has stayed up for BackoffResetAfter (60 s).
	BackoffMin, BackoffMax, BackoffResetAfter time.Duration
	// HealthInterval is the period of the health loop (default 30 s). A
	// negative value disables the PERIODIC loop; the immediate probe that
	// follows a missed deadline still runs.
	HealthInterval time.Duration
	// HealthRetryAfter is how soon a probe is repeated after one failed (default
	// 1 s, never longer than HealthInterval when that is set): a module that has
	// just failed a probe should be judged on the next one in seconds, not after
	// a whole interval.
	HealthRetryAfter time.Duration
	// ShutdownGrace is how long Stop waits for the child to exit after its
	// stdin is closed before killing it (default 2 s).
	ShutdownGrace time.Duration
	// Deadlines overrides the per-request limits.
	Deadlines Deadlines

	// Logf receives log lines; nil discards them. Text a module supplied is
	// always quoted (%q) and truncated before it reaches Logf.
	Logf func(format string, args ...any)
	// Count is called with a short counter name for what the client itself
	// observes: hello_ok, hello_refused, spawned, exited, restarted and
	// deadline_missed. Every other counter of the spec's list is the CALLER's,
	// because only the caller knows an attach outcome or a fallback.
	Count func(name string)
	// OnReady is called, in its own goroutine, once per generation — the first
	// time a child that said hello also passes a health probe. The argument is
	// the generation. A caller re-attaches its lanes from here; because the call
	// is asynchronous and generations can overlap, the callback must check that
	// gen is still Generation() before acting on it.
	OnReady func(gen uint64)
	// OnChange is called, in its own goroutine and coalesced, after any change
	// to what Status reports.
	OnChange func()
}

func (cfg Config) withDefaults() Config {
	if cfg.Launcher == nil {
		cfg.Launcher = ExecLauncher
	}
	def := func(p *time.Duration, v time.Duration) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&cfg.HelloTimeout, 3*time.Second)
	def(&cfg.BackoffMin, time.Second)
	def(&cfg.BackoffMax, 60*time.Second)
	def(&cfg.BackoffResetAfter, 60*time.Second)
	def(&cfg.ShutdownGrace, 2*time.Second)
	def(&cfg.HealthRetryAfter, time.Second)
	def(&cfg.Deadlines.Default, 15*time.Second)
	def(&cfg.Deadlines.Prepare, 5*time.Second)
	def(&cfg.Deadlines.AttachExtra, 5*time.Second)
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = 30 * time.Second
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	return cfg
}

// Status is a snapshot of the client. Hello and Health are copies.
type Status struct {
	Name  string
	State State
	// Suspect is set when a request missed its deadline and the immediate
	// health probe has not yet succeeded. A suspect module is not Usable.
	Suspect bool
	// Reason says why the module is not (fully) available; empty when it is.
	Reason string
	// Hello is the most recent accepted hello; it can be stale when State is
	// not StateAvailable.
	Hello *Hello
	// Health is the most recent health result of the CURRENT child.
	Health     *HealthResult
	Since      time.Time
	Restarts   int
	Generation uint64
}

// Client supervises one module child process and multiplexes requests over its
// stdio. See the package documentation.
type Client struct {
	cfg Config

	ctx    context.Context // cancelled when Stop begins; used by internal probes
	cancel context.CancelFunc

	nextID atomic.Uint64 // request ids; NEVER reset across generations
	gen    atomic.Uint64 // mirrors generation for lock-free reads

	mu       sync.Mutex
	state    State
	suspect  bool
	reason   string
	since    time.Time
	restarts int
	hello    *Hello
	health   *HealthResult
	reserved []string
	cur      *child // the connected, hello-accepted child, or nil
	spawned  bool   // a child has been started at least once
	started  bool
	stopping bool

	stopOnce sync.Once
	stopCh   chan struct{}
	supDone  chan struct{}

	changeCh   chan struct{}
	notifyStop chan struct{}
	notifyDone chan struct{}
	cbWG       sync.WaitGroup
}

// New returns a Client that has not started anything. Call Start.
func New(cfg Config) *Client {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:        cfg,
		ctx:        ctx,
		cancel:     cancel,
		state:      StateStarting,
		reason:     "module starting",
		since:      time.Now(),
		stopCh:     make(chan struct{}),
		supDone:    make(chan struct{}),
		changeCh:   make(chan struct{}, 1),
		notifyStop: make(chan struct{}),
		notifyDone: make(chan struct{}),
	}
}

// Start launches the supervisor and returns immediately; it never blocks on the
// module. It is idempotent, and a no-op after Stop.
func (c *Client) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopping {
		return
	}
	c.started = true
	go c.notifyLoop()
	go c.supervise()
}

// Stop shuts the module down and waits for the supervisor: it closes the child's
// stdin (the module drops its connections but keeps its on-disk state), waits up
// to ShutdownGrace for it to exit, then kills its process group. Idempotent, and
// safe on a client that was never started. After Stop the state is disabled.
func (c *Client) Stop() { c.stopOnce.Do(c.stop) }

func (c *Client) stop() {
	c.mu.Lock()
	c.stopping = true
	started := c.started
	c.mu.Unlock()
	c.cancel()
	if started {
		close(c.stopCh)
		<-c.supDone
	}
	c.mu.Lock()
	if c.state != StateDisabled {
		c.setLocked(StateDisabled, "stopped")
	}
	c.suspect = false
	c.mu.Unlock()
	if started {
		close(c.notifyStop)
		<-c.notifyDone
		waitTimeout(&c.cbWG, 2*time.Second)
	}
}

// --- accessors -------------------------------------------------------------------

// Status returns a snapshot of the client.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{
		Name:       c.cfg.Name,
		State:      c.state,
		Suspect:    c.suspect,
		Reason:     c.reason,
		Since:      c.since,
		Restarts:   c.restarts,
		Generation: c.gen.Load(),
	}
	if c.hello != nil {
		h := copyHello(*c.hello)
		s.Hello = &h
	}
	if c.health != nil {
		h := copyHealth(*c.health)
		s.Health = &h
	}
	return s
}

// Usable reports whether a caller may offer this module a message right now:
// the state is available and no request has recently missed its deadline. It is
// POLICY for callers — the operations themselves are attempted whenever a child
// is connected, so a caller can still close a lane on a module it no longer
// trusts with a send.
func (c *Client) Usable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == StateAvailable && !c.suspect
}

// Generation counts successful hellos: 0 before the first, then 1, 2, ... A
// lane attached in one generation does not exist in the next.
func (c *Client) Generation() uint64 { return c.gen.Load() }

// Hello returns the most recent accepted hello, and whether there was one.
func (c *Client) Hello() (Hello, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hello == nil {
		return Hello{}, false
	}
	return copyHello(*c.hello), true
}

// LastHealth returns the most recent health result of the current child, and
// whether there was one.
func (c *Client) LastHealth() (HealthResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health == nil {
		return HealthResult{}, false
	}
	return copyHealth(*c.health), true
}

// ReservedEnvPrefixes returns the environment prefixes the module reserves, from
// the last hello or health result that carried them. They are retained after
// the child exits: the guard that keeps a caller from setting those names must
// not lapse just because the module is restarting.
func (c *Client) ReservedEnvPrefixes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reserved...)
}

func copyHello(h Hello) Hello {
	h.Ops = append([]string(nil), h.Ops...)
	h.ReservedEnvPrefixes = append([]string(nil), h.ReservedEnvPrefixes...)
	return h
}

func copyHealth(h HealthResult) HealthResult {
	h.SupportedClaude = append(json.RawMessage(nil), h.SupportedClaude...)
	h.Lanes = append(json.RawMessage(nil), h.Lanes...)
	h.Counters = append(json.RawMessage(nil), h.Counters...)
	if h.ReservedEnvPrefixes != nil {
		h.ReservedEnvPrefixes = append([]string{}, h.ReservedEnvPrefixes...)
	}
	return h
}

// --- small helpers ---------------------------------------------------------------

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

func (c *Client) count(name string) {
	if c.cfg.Count != nil {
		c.cfg.Count(name)
	}
}

// setLocked records a state and reason (c.mu held).
func (c *Client) setLocked(state State, reason string) {
	if c.state == state && c.reason == reason {
		return
	}
	if c.state != state {
		c.since = time.Now()
	}
	c.state, c.reason = state, reason
	c.changed()
}

// changed asks the notifier for an OnChange call; it never blocks.
func (c *Client) changed() {
	select {
	case c.changeCh <- struct{}{}:
	default:
	}
}

func (c *Client) notifyLoop() {
	defer close(c.notifyDone)
	fire := func() {
		if c.cfg.OnChange != nil {
			c.cfg.OnChange()
		}
	}
	for {
		select {
		case <-c.changeCh:
			fire()
		case <-c.notifyStop:
			select {
			case <-c.changeCh:
				fire()
			default:
			}
			return
		}
	}
}

func (c *Client) fireReadyLocked(gen uint64) {
	if c.cfg.OnReady == nil {
		return
	}
	c.cbWG.Add(1)
	go func() {
		defer c.cbWG.Done()
		c.cfg.OnReady(gen)
	}()
}

// waitTimeout waits for wg for at most d and reports whether it finished.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// waitChan waits for ch to close for at most d.
func waitChan(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// nextBackoff returns the delay before the next restart and the delay to use
// after that. A child that stayed up for BackoffResetAfter earns a fresh start
// at BackoffMin; anything shorter keeps doubling toward BackoffMax, so a module
// that crashes on every launch is retried ever more slowly instead of in a loop.
func nextBackoff(cur, uptime time.Duration, cfg Config) (wait, next time.Duration) {
	wait = cur
	if uptime >= cfg.BackoffResetAfter || wait < cfg.BackoffMin {
		wait = cfg.BackoffMin
	}
	if wait > cfg.BackoffMax {
		wait = cfg.BackoffMax
	}
	next = wait * 2
	if next > cfg.BackoffMax {
		next = cfg.BackoffMax
	}
	return wait, next
}

// --- supervisor ------------------------------------------------------------------

type endKind int

const (
	endExited  endKind = iota // the child ended (or never started): restart after backoff
	endRefused                // hello refused: disabled until the daemon restarts
	endStopped                // Stop was called
)

// supervise owns the child's whole life cycle: launch, hello, run, reap, back
// off, repeat. It is the only goroutine that starts or waits for a child.
func (c *Client) supervise() {
	defer close(c.supDone)
	wait := c.cfg.BackoffMin
	for {
		began := time.Now()
		if end := c.runChild(); end != endExited {
			return
		}
		var delay time.Duration
		delay, wait = nextBackoff(wait, time.Since(began), c.cfg)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-c.stopCh:
			t.Stop()
			return
		}
	}
}

// helloLine is the first line a child wrote.
type helloLine struct {
	line     []byte
	oversize bool
}

func (c *Client) runChild() endKind {
	select {
	case <-c.stopCh:
		return endStopped
	default:
	}
	proc, err := c.cfg.Launcher(c.ctx, c.cfg.Path, []string{"serve"}, c.cfg.Env)
	if err != nil {
		c.logf("delivery module %q: could not start: %v", c.cfg.Name, err)
		c.mu.Lock()
		c.cur = nil
		c.setLocked(StateUnavailable, "spawn failed: "+clean(err.Error(), 200))
		c.mu.Unlock()
		return endExited
	}
	c.count("spawned")
	c.mu.Lock()
	restart := c.spawned
	if restart {
		c.restarts++
	}
	c.spawned = true
	c.setLocked(StateStarting, "waiting for hello")
	c.mu.Unlock()
	if restart {
		c.count("restarted")
	}

	ch := newChild(c, proc)
	ch.start()

	// Hello: the first line, within HelloTimeout.
	var hl helloLine
	timer := time.NewTimer(c.cfg.HelloTimeout)
	defer timer.Stop()
	select {
	case hl = <-ch.helloCh:
	case <-timer.C:
		return c.refuse(ch, fmt.Sprintf("no hello line within %s", c.cfg.HelloTimeout))
	case <-ch.broken:
		select {
		case hl = <-ch.helloCh: // the line arrived just before the pipe closed
		default:
			return c.ended(ch, "child ended before saying hello")
		}
	case <-ch.exited:
		select {
		case hl = <-ch.helloCh:
		default:
			return c.ended(ch, "child exited before saying hello: "+exitText(ch.waitErr))
		}
	case <-c.stopCh:
		c.shutdown(ch)
		c.reap(ch)
		return endStopped
	}
	if hl.oversize {
		return c.refuse(ch, "hello line exceeds the line limit")
	}
	h, err := ParseHello(hl.line)
	if err != nil {
		var ref *HelloRefusal
		if errors.As(err, &ref) {
			return c.refuse(ch, ref.Reason)
		}
		return c.refuse(ch, err.Error())
	}

	c.mu.Lock()
	gen := c.gen.Add(1)
	ch.gen.Store(gen)
	ch.helloed = true
	c.cur = ch
	hh := copyHello(h)
	c.hello = &hh
	c.reserved = append([]string(nil), h.ReservedEnvPrefixes...)
	c.health = nil
	c.suspect = false
	c.setLocked(StateStarting, "awaiting first health probe")
	c.changed()
	c.mu.Unlock()
	c.count("hello_ok")

	ch.wg.Add(1)
	go ch.prober()

	select {
	case <-ch.exited:
		return c.detachEnded(ch, "child exited: "+exitText(ch.waitErr))
	case <-ch.broken:
		ch.killUnlessExited()
		return c.detachEnded(ch, ch.brokenWhy)
	case <-c.stopCh:
		c.shutdown(ch)
		c.mu.Lock()
		if c.cur == ch {
			c.cur = nil
		}
		c.mu.Unlock()
		c.reap(ch)
		return endStopped
	}
}

// refuse disables the module for the rest of the daemon's life: the same
// binary will say the same thing at the next launch, so retrying is only noise.
func (c *Client) refuse(ch *child, reason string) endKind {
	c.logf("delivery module %q: hello refused (%s); module disabled until the daemon restarts", c.cfg.Name, reason)
	c.count("hello_refused")
	ch.proc.Kill()
	c.reap(ch)
	c.mu.Lock()
	c.cur = nil
	c.suspect = false
	c.setLocked(StateDisabled, "hello refused: "+reason)
	c.mu.Unlock()
	return endRefused
}

// ended handles a child that ended before a hello was accepted: it is a crash,
// not a refusal, so the supervisor restarts it with backoff.
func (c *Client) ended(ch *child, reason string) endKind {
	ch.proc.Kill()
	c.reap(ch)
	c.count("exited")
	c.logf("delivery module %q: %s", c.cfg.Name, reason)
	c.mu.Lock()
	c.setLocked(StateUnavailable, reason)
	c.mu.Unlock()
	return endExited
}

// detachEnded handles the end of a child that had been accepted.
func (c *Client) detachEnded(ch *child, reason string) endKind {
	c.mu.Lock()
	if c.cur == ch {
		c.cur = nil
	}
	c.suspect = false
	c.setLocked(StateUnavailable, reason)
	c.mu.Unlock()
	c.reap(ch)
	c.count("exited")
	c.logf("delivery module %q: %s; restarting", c.cfg.Name, reason)
	return endExited
}

func exitText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return clean(err.Error(), 120)
}

// shutdown is the graceful path: close stdin, give the child ShutdownGrace to
// exit on its own, then kill its process group.
func (c *Client) shutdown(ch *child) {
	ch.closeStdin()
	if !waitChan(ch.exited, c.cfg.ShutdownGrace) {
		c.logf("delivery module %q: did not exit within %s of stdin closing; killing it", c.cfg.Name, c.cfg.ShutdownGrace)
		ch.proc.Kill()
	}
}

// reap finishes a child that has been told to die (or already has): it waits for
// the process, lets the reader drain what is already in the pipe, fails every
// request still pending on that child, and waits for the child's goroutines. It
// never touches the client's state.
func (c *Client) reap(ch *child) {
	if !waitChan(ch.exited, 5*time.Second) {
		c.logf("delivery module %q: process did not report an exit; abandoning it", c.cfg.Name)
	}
	waitChan(ch.readerDone, 3*time.Second)
	ch.failPending("the module process ended")
	close(ch.dead)
	ch.closeStdin()
	waitTimeout(&ch.wg, 3*time.Second)
}

// --- health ----------------------------------------------------------------------

// probe sends one internal health request and applies the result to the state
// machine:
//
//   - ok:true clears the strikes and Suspect, records the result, and makes the
//     module available; OnReady fires the first time that happens for this
//     generation and never again for the same one.
//   - ok:false makes the module unavailable at once ("health not ok"). The child
//     is kept: it says it is alive, and a later ok:true restores it.
//   - an error is a strike; the second consecutive strike makes the module
//     unavailable, and if both were deadline misses the child is killed so the
//     restart path runs — a module that cannot answer health twice running is
//     not going to answer a send.
func (c *Client) probe(ch *child) {
	var hr HealthResult
	raw, err := c.do(c.ctx, ch, OpHealth, HealthArgs{}, c.cfg.Deadlines.Default, true)
	if err == nil {
		err = decodeResult(OpHealth, raw, &hr)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if ch != c.cur || c.stopping {
		return
	}
	switch {
	case err != nil:
		ch.strikes++
		if errors.Is(err, ErrDeadline) {
			ch.deadlineStrikes++
		} else {
			ch.deadlineStrikes = 0
		}
		switch {
		case ch.strikes >= 2:
			c.setLocked(StateUnavailable, "health probe failed twice: "+clean(err.Error(), 160))
		case c.state == StateStarting:
			c.setLocked(StateUnavailable, "first health probe failed: "+clean(err.Error(), 160))
		}
		if ch.deadlineStrikes >= 2 {
			ch.markBroken("health probe missed its deadline twice")
		}
	case !hr.OK:
		ch.strikes++
		ch.deadlineStrikes = 0
		c.storeHealthLocked(hr)
		c.setLocked(StateUnavailable, "health not ok")
	default:
		ch.strikes, ch.deadlineStrikes = 0, 0
		c.storeHealthLocked(hr)
		if c.suspect {
			c.suspect = false
			c.changed()
		}
		c.setLocked(StateAvailable, "")
		if !ch.readyFired {
			ch.readyFired = true
			c.fireReadyLocked(ch.gen.Load())
		}
	}
}

// storeHealthLocked records a health result and refreshes what a hello also
// carries: the version, and the reserved prefixes when the result names them
// (already validated; an absent field leaves the last known set alone).
func (c *Client) storeHealthLocked(hr HealthResult) {
	old := c.health
	h := copyHealth(hr)
	c.health = &h
	if hr.ReservedEnvPrefixes != nil {
		c.reserved = append([]string(nil), hr.ReservedEnvPrefixes...)
		if c.hello != nil {
			c.hello.ReservedEnvPrefixes = append([]string(nil), hr.ReservedEnvPrefixes...)
		}
	}
	if c.hello != nil && hr.Version != "" {
		c.hello.Version = hr.Version
	}
	if old == nil || old.OK != hr.OK || old.PeerCheck != hr.PeerCheck || old.Version != hr.Version || old.Platform != hr.Platform {
		c.changed()
	}
}

// deadlineMissed records that a request outlived its deadline: it counts, marks
// the module suspect (so Usable turns false at once) and — unless the request
// was itself a health probe — asks the prober for an immediate probe. A
// successful probe clears the suspicion; two failed ones escalate.
func (c *Client) deadlineMissed(ch *child, internal bool) {
	c.count("deadline_missed")
	c.mu.Lock()
	current := ch == c.cur && !c.stopping
	if current && !c.suspect {
		c.suspect = true
		c.changed()
	}
	c.mu.Unlock()
	if current && !internal {
		select {
		case ch.probeNow <- struct{}{}:
		default:
		}
	}
}

// --- requests --------------------------------------------------------------------

type outcome struct {
	env envelope
	err error
}

type envelope struct {
	ID     json.RawMessage `json:"id"`
	OK     *bool           `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
}

type wireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Frame states: where a request is on its way into the pipe. They decide
// ErrNotSent versus ErrLost.
const (
	csQueued  int32 = iota // in the writer's queue, no byte written
	csWriting              // the writer is inside Write: some bytes may have gone
	csSent                 // written
	csDropped              // never written (skipped, abandoned, or failed with n == 0)
)

type call struct {
	id    string
	frame []byte
	state atomic.Int32
	out   chan outcome // cap 1; the first completion wins
}

func (cl *call) finish(o outcome) {
	select {
	case cl.out <- o:
	default:
	}
}

// never reports whether no byte of the frame can have reached the pipe.
func (cl *call) never() bool {
	return cl.state.CompareAndSwap(csQueued, csDropped) || cl.state.Load() == csDropped
}

// connected returns the child requests may go to, or nil.
func (c *Client) connected() *child {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping || c.cur == nil || !c.cur.helloed {
		return nil
	}
	return c.cur
}

func (c *Client) unavailableErr(op string) error {
	c.mu.Lock()
	st, why := c.state, c.reason
	c.mu.Unlock()
	return notSent(op, nil, "module %s (%s)", st, why)
}

// do sends one request to ch and waits for its answer, its deadline dl, or the
// caller's ctx — whichever comes first.
//
// Nothing here holds a lock across the wait: requests are matched by id, so a
// slow answer to one never delays another. Errors are classified for the
// caller: a *Error is the module's own ok:false answer; otherwise the error
// wraps ErrNotSent (nothing reached the module) or ErrLost (something did and
// no usable answer came back), plus ErrDeadline when the module, not the
// caller's ctx, ran out of time.
func (c *Client) do(ctx context.Context, ch *child, op string, args any, dl time.Duration, internal bool) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, notSent(op, err, "caller's context is already done")
	}
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	body, err := json.Marshal(struct {
		ID   string `json:"id"`
		Op   string `json:"op"`
		Args any    `json:"args"`
	}{id, op, args})
	if err != nil {
		return nil, notSent(op, err, "request could not be encoded")
	}
	if len(body) > MaxLineBytes {
		// The module would refuse it as too-large; say so without sending
		// megabytes down the pipe. This is the module's own code, produced
		// here, and nothing was sent.
		return nil, &Error{Code: "too-large", Message: "request exceeds the frame limit"}
	}
	cl := &call{id: id, frame: append(body, '\n'), out: make(chan outcome, 1)}
	if !ch.register(cl) {
		return nil, notSent(op, nil, "the module process is gone")
	}
	defer ch.unregister(id)

	dctx, cancel := context.WithTimeout(ctx, dl)
	defer cancel()

	select {
	case ch.writeCh <- cl:
	case <-dctx.Done():
		return nil, c.abandon(ctx, dctx, ch, cl, op, internal)
	case <-ch.dead:
	}

	select {
	case o := <-cl.out:
		return c.answer(op, o)
	case <-dctx.Done():
		select {
		case o := <-cl.out: // the answer and the deadline raced; the answer wins
			return c.answer(op, o)
		default:
		}
		return nil, c.abandon(ctx, dctx, ch, cl, op, internal)
	}
}

// answer turns a completed call into (result, error).
func (c *Client) answer(op string, o outcome) (json.RawMessage, error) {
	if o.err != nil {
		return nil, fmt.Errorf("modclient: %s: %w", op, o.err)
	}
	if o.env.OK == nil {
		return nil, badResponse(op, "response has no ok field")
	}
	if !*o.env.OK {
		e := &Error{Code: "internal", Retryable: false}
		if w := o.env.Error; w != nil {
			e.Retryable = w.Retryable
			e.Message = clean(w.Message, maxTextLen)
			if code := clean(w.Code, 64); code != "" {
				e.Code = code
			}
		}
		return nil, e
	}
	// A result is always an object. Anything else — absent, null, a scalar — is
	// unusable, and must never decode to a zero value that a caller could read
	// as "nothing was written".
	if res := bytes.TrimSpace(o.env.Result); len(res) == 0 || res[0] != '{' {
		return nil, badResponse(op, "response result is not an object")
	}
	return o.env.Result, nil
}

// abandon gives up on a call whose wait ended without an answer, and classifies
// why.
func (c *Client) abandon(ctx, dctx context.Context, ch *child, cl *call, op string, internal bool) error {
	unsent := cl.never()
	missed := ctx.Err() == nil && errors.Is(dctx.Err(), context.DeadlineExceeded)
	cause := ctx.Err()
	if missed {
		cause = ErrDeadline
		c.deadlineMissed(ch, internal)
	}
	if cause == nil {
		cause = dctx.Err()
	}
	if unsent {
		return notSent(op, cause, "request was still queued")
	}
	return fmt.Errorf("modclient: %s: %w (%w): request was written, outcome unknown", op, ErrLost, cause)
}

// decodeResult decodes a result object into out and runs its check.
func decodeResult[R any](op string, raw json.RawMessage, out *R) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return badResponse(op, "result: "+err.Error())
	}
	if ck, ok := any(out).(interface{ check() error }); ok {
		if err := ck.check(); err != nil {
			return badResponse(op, err.Error())
		}
	}
	return nil
}

func roundTrip[R any](c *Client, ctx context.Context, op string, args any, dl time.Duration) (R, error) {
	var zero, out R
	ch := c.connected()
	if ch == nil {
		return zero, c.unavailableErr(op)
	}
	raw, err := c.do(ctx, ch, op, args, dl, false)
	if err != nil {
		return zero, err
	}
	if err := decodeResult(op, raw, &out); err != nil {
		return zero, err
	}
	return out, nil
}

const maxWaitMs = 60 * 60 * 1000

func msDuration(ms int) time.Duration {
	if ms < 0 {
		ms = 0
	}
	if ms > maxWaitMs {
		ms = maxWaitMs
	}
	return time.Duration(ms) * time.Millisecond
}

// waitDeadline is the limit for a call whose module-side wait is waitMs: at
// least Default, and at least the wait plus AttachExtra, so an operation that is
// SUPPOSED to wait is not reported as a deadline miss for waiting.
func (c *Client) waitDeadline(waitMs int) time.Duration {
	d := c.cfg.Deadlines.Default
	if w := msDuration(waitMs) + c.cfg.Deadlines.AttachExtra; waitMs > 0 && w > d {
		d = w
	}
	return d
}

// PrepareLaunch asks the module for the environment (and lane key) of a session
// about to be launched. Deadline: Deadlines.Prepare.
func (c *Client) PrepareLaunch(ctx context.Context, a PrepareLaunchArgs) (PrepareLaunchResult, error) {
	return roundTrip[PrepareLaunchResult](c, ctx, OpPrepareLaunch, a, c.cfg.Deadlines.Prepare)
}

// Attach opens or re-opens the module's channel for a lane; it is idempotent.
// Deadline: TimeoutMs + Deadlines.AttachExtra, or Deadlines.Default when
// TimeoutMs is zero. A not-live probe is a RESULT (Live false), not an error.
func (c *Client) Attach(ctx context.Context, a AttachArgs) (AttachResult, error) {
	dl := c.cfg.Deadlines.Default
	if a.TimeoutMs > 0 {
		dl = msDuration(a.TimeoutMs) + c.cfg.Deadlines.AttachExtra
	}
	return roundTrip[AttachResult](c, ctx, OpAttach, a, dl)
}

// Send delivers one message. A result with Written true is NOT delivery; use
// the verdict of Confirm (or the Confirm carried in the result). Deadline:
// Deadlines.Default, extended to cover ConfirmWaitMs.
func (c *Client) Send(ctx context.Context, a SendArgs) (SendResult, error) {
	return roundTrip[SendResult](c, ctx, OpSend, a, c.waitDeadline(a.ConfirmWaitMs))
}

// Confirm asks whether a send landed. Deadline: Deadlines.Default, extended to
// cover WaitMs.
func (c *Client) Confirm(ctx context.Context, a ConfirmArgs) (ConfirmResult, error) {
	return roundTrip[ConfirmResult](c, ctx, OpConfirm, a, c.waitDeadline(a.WaitMs))
}

// CloseLane tears a lane down; it is idempotent. Deadline: Deadlines.Default.
func (c *Client) CloseLane(ctx context.Context, a CloseArgs) (CloseResult, error) {
	return roundTrip[CloseResult](c, ctx, OpClose, a, c.cfg.Deadlines.Default)
}

// Health asks the module for its health, optionally with one lane's detail. A
// module-wide answer (no LaneKey) is recorded for LastHealth, with the reserved
// prefixes it carries; a per-lane answer is only returned. Neither moves the
// state machine: only the internal loop's probes do, so a caller's query cannot
// flip the module unavailable by itself.
func (c *Client) Health(ctx context.Context, a HealthArgs) (HealthResult, error) {
	hr, err := roundTrip[HealthResult](c, ctx, OpHealth, a, c.cfg.Deadlines.Default)
	if err != nil {
		return HealthResult{}, err
	}
	if a.LaneKey == "" {
		c.mu.Lock()
		if c.cur != nil && !c.stopping {
			c.storeHealthLocked(hr)
		}
		c.mu.Unlock()
	}
	return hr, nil
}

// --- per-child machinery -------------------------------------------------------

// maxStderrLines bounds how many stderr lines of one child reach the log; a
// module that chatters must not be able to flood it.
const (
	maxStderrLines = 200
	stderrKeep     = 512
)

// child is one incarnation of the module process. Everything that belongs to a
// single process — its pending requests, its goroutines, its health strikes —
// lives here, so nothing of a dead generation can leak into the next.
type child struct {
	c    *Client
	proc Proc
	gen  atomic.Uint64 // set when the hello is accepted

	writeCh    chan *call
	dead       chan struct{} // closed once pending calls are failed; stops writer and prober
	broken     chan struct{} // closed when the connection is unusable
	brokenOnce sync.Once
	brokenWhy  string
	exited     chan struct{} // closed when proc.Wait returned
	waitErr    error
	helloCh    chan helloLine
	readerDone chan struct{}
	probeNow   chan struct{}
	wg         sync.WaitGroup

	mu      sync.Mutex
	pending map[string]*call
	closed  bool

	// Guarded by c.mu.
	helloed         bool
	readyFired      bool
	strikes         int
	deadlineStrikes int

	closeOnce sync.Once
}

func newChild(c *Client, proc Proc) *child {
	return &child{
		c:          c,
		proc:       proc,
		writeCh:    make(chan *call, 64),
		dead:       make(chan struct{}),
		broken:     make(chan struct{}),
		exited:     make(chan struct{}),
		helloCh:    make(chan helloLine, 1),
		readerDone: make(chan struct{}),
		probeNow:   make(chan struct{}, 1),
		pending:    map[string]*call{},
	}
}

// start launches the goroutines every child has: a waiter, one reader, one
// writer and one stderr logger.
func (ch *child) start() {
	ch.wg.Add(4)
	go ch.waiter()
	go ch.reader()
	go ch.writer()
	go ch.stderrLoop()
}

func (ch *child) markBroken(why string) {
	ch.brokenOnce.Do(func() {
		ch.brokenWhy = why
		close(ch.broken)
	})
}

// killUnlessExited kills the child if it has not already reported an exit.
func (ch *child) killUnlessExited() {
	select {
	case <-ch.exited:
	default:
		ch.proc.Kill()
	}
}

func (ch *child) closeStdin() {
	ch.closeOnce.Do(func() {
		if w := ch.proc.Stdin(); w != nil {
			_ = w.Close()
		}
	})
}

func (ch *child) waiter() {
	defer ch.wg.Done()
	ch.waitErr = ch.proc.Wait()
	close(ch.exited)
}

func (ch *child) register(cl *call) bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return false
	}
	ch.pending[cl.id] = cl
	return true
}

func (ch *child) unregister(id string) {
	ch.mu.Lock()
	delete(ch.pending, id)
	ch.mu.Unlock()
}

// failPending completes every request still waiting on this child. A request no
// byte of which was written is ErrNotSent; anything else is ErrLost. After it
// returns, register refuses new requests, so none can be stranded.
func (ch *child) failPending(why string) {
	ch.mu.Lock()
	ch.closed = true
	pend := ch.pending
	ch.pending = map[string]*call{}
	ch.mu.Unlock()
	for _, cl := range pend {
		if cl.never() {
			cl.finish(outcome{err: fmt.Errorf("%w: %s", ErrNotSent, why)})
		} else {
			cl.finish(outcome{err: fmt.Errorf("%w: %s", ErrLost, why)})
		}
	}
}

// writer is the ONE goroutine that writes to the child's stdin, so frames never
// interleave. A request whose caller has already given up is skipped — never
// written — which is what makes "queued, then cancelled" safely ErrNotSent.
func (ch *child) writer() {
	defer ch.wg.Done()
	w := ch.proc.Stdin()
	for {
		select {
		case <-ch.dead:
			return
		case cl := <-ch.writeCh:
			if !cl.state.CompareAndSwap(csQueued, csWriting) {
				continue // abandoned while queued
			}
			n, err := w.Write(cl.frame)
			if err != nil {
				if n > 0 {
					cl.state.Store(csSent) // a partial frame: the module may act on it
				} else {
					cl.state.Store(csDropped)
					cl.finish(outcome{err: fmt.Errorf("%w: the pipe to the module is broken: %v", ErrNotSent, err)})
				}
				ch.markBroken("write to the module failed: " + clean(err.Error(), 100))
				continue
			}
			cl.state.Store(csSent)
		}
	}
}

// reader is the ONE goroutine that reads the child's stdout. The first line is
// the hello and goes to the supervisor; every later line is a response.
func (ch *child) reader() {
	defer ch.wg.Done()
	defer close(ch.readerDone)
	br := bufio.NewReaderSize(ch.proc.Stdout(), 64<<10)
	first := true
	oversizeLogged, junkLogged := false, false
	for {
		line, oversize, err := ReadLine(br)
		if err != nil {
			ch.markBroken("the module closed its output")
			return
		}
		if first {
			first = false
			ch.helloCh <- helloLine{line: line, oversize: oversize}
			continue
		}
		if oversize {
			if !oversizeLogged {
				oversizeLogged = true
				ch.c.logf("delivery module %q: dropped a response line over %d bytes", ch.c.cfg.Name, MaxLineBytes)
			}
			continue
		}
		if !ch.dispatch(line) && !junkLogged {
			junkLogged = true
			ch.c.logf("delivery module %q: ignoring a line that is not a response: %q", ch.c.cfg.Name, clean(string(line), 120))
		}
	}
}

// dispatch matches a response line to its pending request by id and reports
// whether the line was a well-formed response envelope. A response whose
// generation is not the current one is dropped: ids never repeat across
// generations, so this is a second wall, not the only one.
func (ch *child) dispatch(line []byte) bool {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return false
	}
	id := idText(env.ID)
	if id == "" {
		return false
	}
	if g := ch.gen.Load(); g != 0 && g != ch.c.gen.Load() {
		return true
	}
	ch.mu.Lock()
	cl := ch.pending[id]
	delete(ch.pending, id)
	ch.mu.Unlock()
	if cl != nil {
		cl.finish(outcome{env: env})
	}
	return true
}

// idText normalises a response id: a JSON string, or a bare number some modules
// echo back. Anything else is no id.
func idText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	return n.String()
}

// stderrLoop logs the child's stderr, line by line, quoted and truncated. It
// never parses it: stderr is the one channel a module may write anything to.
func (ch *child) stderrLoop() {
	defer ch.wg.Done()
	r := ch.proc.Stderr()
	if r == nil {
		return
	}
	br := bufio.NewReaderSize(r, 4096)
	n := 0
	for {
		line, truncated, err := readTruncated(br, stderrKeep)
		if len(line) > 0 {
			n++
			switch {
			case n <= maxStderrLines && truncated:
				ch.c.logf("delivery module %q stderr: %q (truncated)", ch.c.cfg.Name, string(line))
			case n <= maxStderrLines:
				ch.c.logf("delivery module %q stderr: %q", ch.c.cfg.Name, string(line))
			case n == maxStderrLines+1:
				ch.c.logf("delivery module %q: further stderr lines are not logged", ch.c.cfg.Name)
			}
		}
		if err != nil {
			return
		}
	}
}

// prober runs this child's health probes: one immediately (the first is what
// makes the module available), then every HealthInterval, sooner after a
// failure (HealthRetryAfter), and on demand after a missed deadline.
func (ch *child) prober() {
	defer ch.wg.Done()
	for {
		ch.c.probe(ch)
		wait := ch.c.cfg.HealthInterval
		if ch.c.failing(ch) {
			if retry := ch.c.cfg.HealthRetryAfter; wait <= 0 || retry < wait {
				wait = retry
			}
		}
		var tick <-chan time.Time
		var timer *time.Timer
		if wait > 0 {
			timer = time.NewTimer(wait)
			tick = timer.C
		}
		select {
		case <-ch.dead:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-tick:
		case <-ch.probeNow:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// failing reports whether the last probe of ch failed.
func (c *Client) failing(ch *child) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ch.strikes > 0
}
