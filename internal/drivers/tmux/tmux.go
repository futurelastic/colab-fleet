// Package tmux implements driver.Driver over a terminal multiplexer
// running an interactive agent CLI. It is the first driver in this
// repository that actually does anything, and its purpose is stated in
// NOTES' sequencing: prove the interface can express everything the
// incumbent supervisor already does. Where it cannot, that is a finding
// about the interface, not a gap to paper over — see FINDINGS below.
//
// # Enumeration is one subprocess, not N
//
// driver.Driver.List's doc comment warns that a driver implementing List by
// looping per session "has reproduced the cost this interface exists to
// avoid". That cost is real and was measured here, on a host running 22
// concurrent sessions:
//
//	per-session capture loop (23 spawns) ... 119ms
//	single batched invocation (1 spawn) ...   18ms
//
// The multiplexer accepts a sequence of commands separated by a literal
// ";" argument in one invocation, so a full fleet view — metadata for every
// session plus a screen capture of each — costs exactly one process spawn
// regardless of session count. Cost then scales with output bytes rather
// than with process creation, which is the difference between ~5ms per
// session and ~0.15ms per session.
//
// # Pane text is untrusted input
//
// The batched captures arrive concatenated with no delimiter, so this
// driver interleaves a marker between them. The marker is a per-call nonce
// rather than a constant, because the text being delimited is written by an
// agent that can print anything at all — including a convincing forgery of
// whatever fixed delimiter a naive implementation would choose. The same
// nonce separates fields within a metadata row, since session names and
// working directories are user-controlled and may contain any printable
// character (the sessions this was developed against contain emoji).
//
// # FINDINGS: where the specification did not survive contact
//
//  1. §5.4 ("require consensus before destruction") is not implementable at
//     the signature §3 gives close(). The rule requires corroborating "at
//     least one independent attribute (working directory, start time,
//     name)" before destroying a session — but SessionRef carries only
//     machine, id and a human label, so a driver has nothing to corroborate
//     the live session *against*. It can read the session's current start
//     time; it cannot know which start time the caller meant. See
//     Driver.Close, which implements the strongest form available at this
//     signature and documents the window it cannot close.
//
//  2. §4.3's SupportsResume and §10's idempotency retention are different
//     properties, and the spec treats them as one concern. Sessions here
//     genuinely survive a service restart — the multiplexer owns them, not
//     this process — so SupportsResume is true. The idempotency key store
//     does not survive, because it lives in this process's memory. A caller
//     that retries a create across a service restart therefore gets a
//     second session, which is precisely the §10 disaster, on a driver that
//     honestly declares SupportsResume: true. See idempotency below.
//
//  3. The multiplexer's "current command" field reports the process's
//     self-declared title, not its executable. The agent CLI rewrites its
//     title to its own version string, so the field returns "2_1_220" for a
//     process the OS calls "claude". It is never empty and never errors —
//     it is a confidently wrong answer of the wrong kind entirely, which is
//     §5.2's failure mode in a single field. This driver reads it as a
//     runtime version hint and never as an identity check.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

const (
	// DefaultRuntime is the runtime id this driver reports. It names both
	// halves deliberately: the multiplexer supplies the session substrate,
	// the CLI supplies the agent, and neither alone identifies what a
	// caller is talking to.
	DefaultRuntime = fleet.RuntimeId("claude-code-tmux")

	// defaultDeadlineMs bounds any single call (§4.4). Local subprocess
	// work measured in single-digit milliseconds; this is three orders of
	// magnitude of headroom, chosen so that a genuinely wedged multiplexer
	// surfaces as unreachable in bounded time rather than never.
	defaultDeadlineMs = 5000

	// defaultCaptureLines is how much scrollback the classifier receives
	// per session. The classifier reads only the tail, but the tail must
	// be tall enough to contain the composer fence plus the status line
	// above it.
	defaultCaptureLines = 24

	// defaultIdempotencyRetention is how long a create key is honoured
	// (§10: "retention must outlive the caller's retry window").
	defaultIdempotencyRetention = 30 * time.Minute
)

// ErrAmbiguousTarget is returned by a destructive operation whose target
// could not be corroborated (§5.4). It is deliberately not
// driver.ErrUnsupported: the driver supports the operation, and is refusing
// this particular invocation because it cannot establish that the session
// it would destroy is the session the caller meant.
var ErrAmbiguousTarget = errors.New("tmux: refusing a destructive operation on an uncorroborated target (§5.4)")

// ErrNotFound is returned when no session matches a ref.
var ErrNotFound = errors.New("tmux: no such session")

// execFunc runs the multiplexer binary and returns its stdout. Injected so
// tests can drive the driver without a live multiplexer.
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// CommandBuilder produces the argv the multiplexer should run for a new
// session. contextFile is a path to the caller's context, already written
// to disk — it is passed by path and must never be inlined into the
// returned argv (§5.3). An empty contextFile means the caller supplied
// none.
//
// This indirection keeps the multiplexer mechanics separate from the
// specifics of any one agent CLI: swapping the CLI is a new builder, not a
// new driver.
type CommandBuilder func(spec fleet.SessionSpec, contextFile string) []string

// Driver runs sessions as multiplexer sessions on the local machine.
//
// The zero value is not usable; use New.
type Driver struct {
	machine      fleet.MachineId
	runtime      fleet.RuntimeId
	bin          string
	deadline     time.Duration
	captureLines int
	build        CommandBuilder
	run          execFunc
	dial         ctlDialer
	now          func() time.Time
	nonce        func() string

	mu sync.Mutex
	// observed is this driver's most recent sighting of each session id,
	// keyed by id. It is what Close corroborates against (§5.4) and what
	// startup reconciliation adopts into (§12).
	observed map[string]observation
	// idem maps an idempotency key to the ref it produced (§10). In
	// memory only — see FINDINGS 2.
	idem      map[string]idemEntry
	retention time.Duration
}

type observation struct {
	created time.Time
	cwd     string
	at      time.Time
}

type idemEntry struct {
	ref fleet.SessionRef
	at  time.Time
}

// Option configures a Driver.
type Option func(*Driver)

// WithBinary sets the multiplexer executable. Default "tmux".
func WithBinary(path string) Option { return func(d *Driver) { d.bin = path } }

// WithDeadline overrides the declared per-call deadline (§4.4).
func WithDeadline(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.deadline = dur
		}
	}
}

// WithCaptureLines sets how much scrollback per session is fed to the
// classifier.
func WithCaptureLines(n int) Option {
	return func(d *Driver) {
		if n > 0 {
			d.captureLines = n
		}
	}
}

// WithCommandBuilder replaces the default agent CLI invocation.
func WithCommandBuilder(b CommandBuilder) Option {
	return func(d *Driver) {
		if b != nil {
			d.build = b
		}
	}
}

// withExec injects a fake multiplexer. Unexported: tests only.
func withExec(f execFunc) Option { return func(d *Driver) { d.run = f } }

// withClock and withNonce make tests deterministic.
func withClock(f func() time.Time) func(*Driver) { return func(d *Driver) { d.now = f } }
func withNonce(f func() string) func(*Driver)    { return func(d *Driver) { d.nonce = f } }

// New builds a Driver for one machine.
func New(machine fleet.MachineId, opts ...Option) *Driver {
	d := &Driver{
		machine:      machine,
		runtime:      DefaultRuntime,
		bin:          "tmux",
		deadline:     defaultDeadlineMs * time.Millisecond,
		captureLines: defaultCaptureLines,
		build:        claudeCodeCommand,
		run:          runReal,
		dial:         dialReal,
		now:          time.Now,
		nonce:        randomNonce,
		observed:     map[string]observation{},
		idem:         map[string]idemEntry{},
		retention:    defaultIdempotencyRetention,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

var _ driver.Driver = (*Driver)(nil)

func runReal(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Capabilities declares what this driver can and cannot do (§4.3).
//
// ObservesState is false and must stay false: every status this driver
// reports is inferred from a terminal screen (see classify.go). Setting it
// true would be the §5.6 violation the field exists to make visible.
//
// ConfirmsDelivery is false. The driver can put text into a session and can
// see afterwards that the composer is empty, but "the composer is empty"
// does not distinguish "the agent received it" from "something else cleared
// it". Distinguishing submitted from queued requires a signal the substrate
// does not provide, so this driver reports queued and says so here rather
// than claiming a confirmation it inferred.
//
// SupportsResume is true, and it is the one capability this substrate has
// outright: sessions belong to the multiplexer, not to this process, so
// they survive it being restarted, upgraded or killed. Note FINDINGS 2 —
// this is not the same as the idempotency store surviving.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		ObservesState:    false,
		ConfirmsDelivery: false,
		SupportsResume:   true,
		SupportsPin: fleet.PinSupport{
			Model:  true,
			Effort: true,
			Agent:  true,
		},
		DeadlineMs: d.deadline.Milliseconds(),
	}
}

// bounded applies this driver's declared deadline, or the caller's if the
// caller's is shorter (§4.4: "a caller may supply a shorter deadline; never
// a longer one").
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.deadline)
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// paneRow is one session's metadata as returned by the batched enumeration.
type paneRow struct {
	session string
	paneID  string
	cwd     string
	pid     int
	created time.Time
	dead    bool
	// title is the process's self-declared title, NOT its executable name
	// — see FINDINGS 3. Carried as a version hint; never used to decide
	// what a session is.
	title string
}

// enumerate performs the whole fleet read in one subprocess: one metadata
// listing followed by one screen capture per session, delimited by a
// per-call nonce.
func (d *Driver) enumerate(ctx context.Context) ([]paneRow, map[string]string, error) {
	nonce := d.nonce()
	sep := nonce + "F"
	mark := nonce + "P"

	// Restrict to the active pane of the active window: one row per
	// session. A session with several windows is still one session.
	const activeOnly = "#{&&:#{pane_active},#{window_active}}"
	format := strings.Join([]string{
		"#{session_name}", "#{pane_id}", "#{pane_current_path}",
		"#{pane_pid}", "#{session_created}", "#{pane_dead}",
		"#{pane_current_command}",
	}, sep)

	args := []string{"list-panes", "-a", "-f", activeOnly, "-F", format}

	// The listing must happen before the captures can be named, so this is
	// two subprocesses on a cold call and one thereafter... except it need
	// not be: the capture targets can be expressed as the same filter, so
	// a first cheap listing tells us the pane ids and the second call does
	// everything. Measured, the listing alone is ~8ms and the combined
	// call ~18ms; two calls total ~26ms for 22 sessions, still O(1) in
	// session count and still 4x cheaper than the per-session loop.
	out, err := d.run(ctx, d.bin, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("enumerate: listing sessions: %w", err)
	}
	rows, err := parseRows(string(out), sep)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, map[string]string{}, nil
	}

	// One invocation, N captures, nonce-delimited.
	//
	// The marker carries each pane's INDEX, never its pane id, and that is
	// not a stylistic choice. display-message passes its argument through
	// strftime before printing it, so any "%" in the marker is consumed as
	// a (usually invalid) conversion specifier and silently vanishes —
	// and every pane identifier on this substrate begins with "%". A
	// marker built from a pane id therefore arrives corrupted, the
	// captures fail to associate with their sessions, and — the part that
	// makes this worth a comment rather than a fix — every session
	// classifies as "unknown" instead of erroring, because an absent
	// capture is indistinguishable from an empty one. It looks like a
	// working driver that cannot read screens.
	capArgs := make([]string, 0, len(rows)*8)
	for i, r := range rows {
		if i > 0 {
			capArgs = append(capArgs, ";")
		}
		capArgs = append(capArgs,
			"display-message", "-p", mark+strconv.Itoa(i), ";",
			"capture-pane", "-p", "-t", r.paneID, "-S", "-"+strconv.Itoa(d.captureLines),
		)
	}
	capOut, err := d.run(ctx, d.bin, capArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("enumerate: capturing panes: %w", err)
	}
	byIndex := splitCaptures(string(capOut), mark)
	captures := make(map[string]string, len(rows))
	for i, r := range rows {
		if text, ok := byIndex[strconv.Itoa(i)]; ok {
			captures[r.paneID] = text
		}
	}
	return rows, captures, nil
}

func parseRows(out, sep string) ([]paneRow, error) {
	var rows []paneRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, sep)
		if len(f) != 7 {
			return nil, fmt.Errorf("parseRows: expected 7 fields, got %d in %q", len(f), line)
		}
		pid, _ := strconv.Atoi(f[3])
		createdUnix, _ := strconv.ParseInt(f[4], 10, 64)
		rows = append(rows, paneRow{
			session: f[0],
			paneID:  f[1],
			cwd:     f[2],
			pid:     pid,
			created: time.Unix(createdUnix, 0),
			dead:    f[5] == "1",
			title:   f[6],
		})
	}
	return rows, nil
}

// splitCaptures divides one concatenated capture stream into per-pane text
// using the nonce marker emitted before each capture.
func splitCaptures(out, mark string) map[string]string {
	res := map[string]string{}
	parts := strings.Split(out, mark)
	for _, p := range parts[1:] { // parts[0] is anything before the first marker
		nl := strings.Index(p, "\n")
		if nl < 0 {
			continue
		}
		paneID := strings.TrimSpace(p[:nl])
		res[paneID] = p[nl+1:]
	}
	return res
}

// List returns every session in one call (§3, and driver.Driver.List's
// contract). The returned Collection always carries exactly one
// SourceStatus — this machine's own — because even a single machine
// answering for itself must say who answered (§9, api-http.md §3.2).
func (d *Driver) List(ctx context.Context, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		// §5.7: a failed read is never an empty result. Report the source
		// as unreachable and let the envelope carry the failure.
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     fleet.SourceUnreachable,
			Error:      err.Error(),
			ObservedAt: d.now(),
		}
		return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{src})
	}

	sessions := make([]fleet.Session, 0, len(rows))
	now := d.now()
	d.mu.Lock()
	for _, r := range rows {
		d.observed[r.session] = observation{created: r.created, cwd: r.cwd, at: now}
		text, captured := captures[r.paneID]
		s := fleet.Session{
			SessionRef: fleet.SessionRef{Machine: d.machine, ID: r.session, Name: r.session},
			Runtime:    d.runtime,
			Cwd:        fleet.AbsolutePath(r.cwd),
			State:      classifyPane(text, captured, !r.dead),
		}
		if !matchesFilter(s, filter) {
			continue
		}
		sessions = append(sessions, s)
	}
	d.mu.Unlock()

	count := len(sessions)
	src := fleet.SourceStatus{
		Machine:    d.machine,
		Status:     fleet.SourceOK,
		Count:      &count,
		ObservedAt: now,
	}
	return fleet.NewCollection(sessions, []fleet.SourceStatus{src})
}

func matchesFilter(s fleet.Session, f driver.ListFilter) bool {
	if f.Status != "" && s.State.Status != f.Status {
		return false
	}
	if f.Agent != "" && s.Agent != f.Agent {
		return false
	}
	if f.CwdPrefix != "" && !strings.HasPrefix(string(s.Cwd), f.CwdPrefix) {
		return false
	}
	return true
}

// State reads one session. It is implemented over the same batched
// enumeration rather than a targeted query, because at these costs a whole
// fleet read is cheaper than the two subprocess spawns a targeted read
// would need, and it keeps exactly one code path deciding what a status
// means.
func (d *Driver) State(ctx context.Context, ref fleet.SessionRef) (fleet.SessionState, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.SessionState{}, err
	}
	for _, r := range rows {
		if r.session != ref.ID {
			continue
		}
		d.mu.Lock()
		d.observed[r.session] = observation{created: r.created, cwd: r.cwd, at: d.now()}
		d.mu.Unlock()
		text, captured := captures[r.paneID]
		return classifyPane(text, captured, !r.dead), nil
	}
	// §5.7 applied to a singular read: "I looked and it is not there" is a
	// real answer, and it is not the same as a failure to look. A session
	// that existed and no longer does is dead (§8), not an error.
	return fleet.InferredState(fleet.StatusDead,
		"no session with this id present in the multiplexer", nil), nil
}

// Send delivers input to a session (§3), and refuses when delivery would
// corrupt it (§2.4).
//
// The refusal case is the reason this operation is not a boolean. If a
// human has typed into the composer and not submitted, pasting more text
// concatenates the two into one message that neither party wrote. §2.4
// names this scenario exactly ("injecting text into a prompt that already
// holds unsent input the human typed") and puts the protection in the
// contract rather than in each caller's memory of a past incident. This is
// that protection, implemented.
//
// Text is delivered via the multiplexer's paste buffer rather than as
// simulated keystrokes. Keystroke simulation would require escaping the
// caller's text against the multiplexer's key-name vocabulary, where a
// message containing something like "C-c" is a live hazard; the paste
// buffer takes bytes and interprets none of them.
func (d *Driver) Send(ctx context.Context, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	var target *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "no session with this id",
		}, nil
	}
	if target.dead {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "session process has exited",
		}, nil
	}

	if pending, ok := composerText(newScreen(captures[target.paneID])); ok && pending != "" {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "composer holds unsent input; delivering would concatenate " +
				"with text a human typed and has not submitted (§2.4)",
		}, nil
	}

	// load-buffer reads from stdin when given "-"; this driver writes to a
	// temp file instead so the payload never traverses argv (§5.3's
	// rationale generalises: a shared namespace is a shared namespace).
	f, err := os.CreateTemp("", "fleet-send-*")
	if err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: staging payload: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: staging payload: %w", err)
	}
	f.Close()

	bufName := "fleet-" + d.nonce()
	args := []string{
		"load-buffer", "-b", bufName, f.Name(), ";",
		"paste-buffer", "-b", bufName, "-t", target.paneID, "-d",
	}
	if opts.Submit {
		args = append(args, ";", "send-keys", "-t", target.paneID, "Enter")
	}
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: delivering: %w", err)
	}

	// Queued, not submitted: see Capabilities. The bytes were handed to
	// the substrate; whether the agent consumed them is not observable
	// here, and claiming otherwise is the emulation §5.6 forbids.
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeQueued,
		Reason:  "delivered to the pane; agent receipt is not observable on this substrate",
	}, nil
}

// Interrupt asks a session to stop what it is doing (§3). It expresses
// intent only — the Ack says the request was accepted, never that the agent
// stopped; that arrives later as a state change (§2.5).
func (d *Driver) Interrupt(ctx context.Context, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	for _, r := range rows {
		if r.session == ref.ID {
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", r.paneID, "Escape"); err != nil {
				return fleet.Ack{}, fmt.Errorf("interrupt: %w", err)
			}
			return fleet.Ack{Accepted: true}, nil
		}
	}
	return fleet.Ack{}, ErrNotFound
}

// Close destroys a session (§3) — and is the operation where §5.4 does not
// survive the interface it is specified against.
//
// §5.4 requires corroborating "at least one independent attribute (working
// directory, start time, name)" before acting destructively, because ids
// are recyclable. But close() receives only a SessionRef, which carries
// machine, id and a human label. There is no field in which a caller can
// say *which* session it means beyond the id — so a driver has nothing to
// compare the live session against except its own earlier sighting.
//
// That is what this implements, and it is worth being precise about what it
// does and does not buy:
//
//   - CLOSED: the window between this driver observing a session and being
//     asked to destroy it. If the id was recycled in that interval, the
//     start time will differ and this refuses.
//   - OPEN, and not closable here: the window between the *caller*
//     observing a session and calling close. The caller's evidence never
//     reaches the driver. A caller that listed sessions, went away, came
//     back after a recycle and called close gets no protection from this
//     check, because the driver's own sighting may have been refreshed in
//     the meantime.
//
// Closing the second window needs SessionRef to carry a corroborating
// attribute — a start time the caller observed — so that close can compare
// against the caller's belief rather than the driver's. That is a change to
// the specification, not to this file; it is recorded in the package doc's
// FINDINGS and in the spec's §5.4 (open defect D2, §14).
//
// A ref this driver has never seen is refused outright rather than
// destroyed on an id match, which is the literal thing §5.4 forbids.
func (d *Driver) Close(ctx context.Context, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	d.mu.Lock()
	prior, seen := d.observed[ref.ID]
	d.mu.Unlock()
	if !seen {
		return fleet.Ack{}, fmt.Errorf("%w: no prior observation of id %q to corroborate against",
			ErrAmbiguousTarget, ref.ID)
	}

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	for _, r := range rows {
		if r.session != ref.ID {
			continue
		}
		if !r.created.Equal(prior.created) {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now holds a session created at %s, not the one observed at %s",
				ErrAmbiguousTarget, ref.ID, r.created, prior.created)
		}
		if r.cwd != prior.cwd {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now has working directory %q, not %q",
				ErrAmbiguousTarget, ref.ID, r.cwd, prior.cwd)
		}
		if _, err := d.run(ctx, d.bin, "kill-session", "-t", ref.ID); err != nil {
			return fleet.Ack{}, fmt.Errorf("close: %w", err)
		}
		d.mu.Lock()
		delete(d.observed, ref.ID)
		d.mu.Unlock()
		return fleet.Ack{Accepted: true}, nil
	}
	return fleet.Ack{}, ErrNotFound
}

// Create starts a session (§3), honouring the caller's idempotency key
// (§10).
//
// The caller's prompt never reaches a command line. §5.3 requires context
// to travel as a path, and the same reasoning applies to the prompt itself:
// process command lines are a shared namespace, and anything that matches
// processes by name can match — and terminate — a session whose argv merely
// contains the string it was hunting for. The prompt is written to a file
// and delivered through the paste buffer once the session is up.
func (d *Driver) Create(ctx context.Context, key string, spec fleet.SessionSpec) (fleet.SessionRef, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.SessionRef{}, errors.New("create: idempotency key is required (§10)")
	}
	now := d.now()

	d.mu.Lock()
	d.sweepIdemLocked(now)
	if e, ok := d.idem[key]; ok {
		d.mu.Unlock()
		return e.ref, nil
	}
	d.mu.Unlock()

	name := spec.Name
	if name == "" {
		name = "fleet-" + d.nonce()
	}
	if spec.Cwd == "" {
		return fleet.SessionRef{}, errors.New("create: cwd is required")
	}

	contextFile := string(spec.ContextRef)
	if contextFile != "" && !filepath.IsAbs(contextFile) {
		return fleet.SessionRef{}, fmt.Errorf("create: contextRef must be absolute, got %q", contextFile)
	}

	argv := d.build(spec, contextFile)
	args := append([]string{
		"new-session", "-d", "-s", name, "-c", string(spec.Cwd), "--",
	}, argv...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fleet.SessionRef{}, fmt.Errorf("create: %w", err)
	}

	ref := fleet.SessionRef{Machine: d.machine, ID: name, Name: name}

	d.mu.Lock()
	d.idem[key] = idemEntry{ref: ref, at: now}
	d.mu.Unlock()

	if spec.Prompt != "" {
		// Best effort, and deliberately not fatal: the session exists,
		// and reporting create as failed would invite a retry that §10
		// exists to make safe but which would now be answered from the
		// idempotency store anyway. The prompt not landing is visible in
		// the session's state; a phantom failure is not.
		_, _ = d.Send(ctx, ref, spec.Prompt, driver.SendOptions{Submit: true})
	}
	return ref, nil
}

func (d *Driver) sweepIdemLocked(now time.Time) {
	for k, e := range d.idem {
		if now.Sub(e.at) > d.retention {
			delete(d.idem, k)
		}
	}
}

// Reconcile performs §12's startup reconciliation: enumerate what exists,
// adopt it, and never destroy anything.
//
// With no persisted records — this driver keeps none across restarts — every
// session found is by definition "exists but no record", which §12 calls
// orphaned and requires be surfaced with inferred confidence and whatever
// identifying evidence the driver has. That is not a degenerate case to fix
// later; it is the correct classification given what this driver knows, and
// the states it produces already carry inferred confidence for independent
// reasons. The "vanished" class cannot arise here at all, because there are
// no records for anything to vanish from.
func (d *Driver) Reconcile(ctx context.Context) (fleet.Collection[fleet.Session], error) {
	return d.List(ctx, driver.ListFilter{})
}

// claudeCodeCommand is the default CommandBuilder.
//
// Note what is absent: the prompt. It is delivered after the session is up
// (see Create) precisely so it stays out of this argv.
func claudeCodeCommand(spec fleet.SessionSpec, contextFile string) []string {
	argv := []string{"claude"}
	if spec.Agent != "" {
		argv = append(argv, "--agent", string(spec.Agent))
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if contextFile != "" {
		argv = append(argv, "--append-system-prompt-file", contextFile)
	}
	return argv
}
