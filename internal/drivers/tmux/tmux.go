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
//     time; it cannot know which start time the req meant. See
//     Driver.Close, which implements the strongest form available at this
//     signature and documents the window it cannot close.
//
//  2. §4.3's SupportsResume and §10's idempotency retention are different
//     properties, and the spec treats them as one concern. Sessions here
//     genuinely survive a service restart — the multiplexer owns them, not
//     this process — so SupportsResume is true. The idempotency key store
//     does not survive, because it lives in this process's memory. A req
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
	"github.com/godx-jp/colab-fleet/internal/state"
)

const (
	// DefaultRuntime is the runtime id this driver reports. It names both
	// halves deliberately: the multiplexer supplies the session substrate,
	// the CLI supplies the agent, and neither alone identifies what a
	// req is talking to.
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

	// promptClearWindow bounds the wait for an answered prompt to disappear.
	promptClearWindow   = 3 * time.Second
	promptClearInterval = 200 * time.Millisecond

	// startingWindow is how long a session with no visible interface is
	// given the benefit of the doubt. The runtime takes tens of seconds to
	// paint; beyond this, silence means something other than booting.
	startingWindow = 90 * time.Second

	// unsentAgeWorthMentioning is when a composer holding text stops looking
	// like someone typing. Below it the age is noise; above it, it is the
	// whole story.
	unsentAgeWorthMentioning = 10 * time.Minute

	// submitConfirmWindow bounds the wait for delivered text to render before
	// it is submitted. Generous on purpose: a slow render and a stuck pane
	// look identical over a short budget, and failing early strands the text.
	submitConfirmWindow   = 4 * time.Second
	submitConfirmInterval = 150 * time.Millisecond

	// promptDeliveryWindow bounds how long §2.1's initial prompt waits for
	// the runtime to become ready. Generous, because starting an agent is
	// slow and a prompt arriving late is better than one arriving into a
	// terminal that is not listening.
	promptDeliveryWindow = 90 * time.Second
	// promptPollInterval is how often readiness is checked. This is not the
	// polling §5.5 forbids: that rule is about callers learning of state
	// changes, and this is one driver waiting for a process it just started.
	promptPollInterval = 1500 * time.Millisecond

	// defaultIdempotencyRetention is how long a create key is honoured
	// (§10: "retention must outlive the caller's retry window").
	defaultIdempotencyRetention = 30 * time.Minute
)

// ErrAmbiguousTarget is returned by a destructive operation whose target
// could not be corroborated (§5.4). It is deliberately not
// driver.ErrUnsupported: the driver supports the operation, and is refusing
// this particular invocation because it cannot establish that the session
// it would destroy is the session the req meant.
// ErrAmbiguousTarget wraps the package-level sentinel so callers can match on
// either, and so the service can map it to a wire kind without importing this
// driver.
var ErrAmbiguousTarget = fmt.Errorf("tmux: refusing a destructive operation: %w", fleet.ErrAmbiguousTarget)

// ErrNotFound is returned when no session matches a ref.
var ErrNotFound = errors.New("tmux: no such session")

// execFunc runs the multiplexer binary and returns its stdout. Injected so
// tests can drive the driver without a live multiplexer.
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// CommandBuilder produces the argv the multiplexer should run for a new
// session. contextFile is a path to the caller's context, already written
// to disk — it is passed by path and must never be inlined into the
// returned argv (§5.3). An empty contextFile means the req supplied
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
	shell        string
	bareExec     bool
	run          execFunc
	dial         ctlDialer
	now          func() time.Time
	nonce        func() string

	store   *state.Store
	idemErr error

	mu sync.Mutex
	// observed is this driver's most recent sighting of each session id,
	// keyed by id. It is what Close corroborates against (§5.4) and what
	// startup reconciliation adopts into (§12).
	observed map[string]observation
	// idem is the durable idempotency table (§10). Backed by a file when a
	// state store is configured; in-memory otherwise, which is honest for a
	// throwaway instance and was the defect (D5) for a real one.
	idem      *idemStore
	retention time.Duration

	// quota is the account-level block (§2.3's QuotaBlock), remembered
	// because it outlives the screen that announced it and survives a
	// restart — a weekly limit measured on this fleet had four days to run,
	// and the service is deployed by restarting it.
	quota *fleet.QuotaBlock

	// stranded remembers, per session, text this driver delivered and could
	// not confirm — the record a resume is checked against. In memory only:
	// it describes what is sitting in a composer right now, and a composer
	// does not survive the multiplexer, let alone a service restart.
	stranded map[string]string

	// environments remembers what each created session's process received
	// (see environment.go). In memory only, for the reason stated on
	// Environment.
	environments map[string]fleet.SessionEnvironment
}

type observation struct {
	created time.Time
	cwd     string
	at      time.Time

	// status and statusSince implement §8's "`since` is the time the status
	// was first observed to hold, not the time it began".
	//
	// This is what separates a wedged pane from an operator mid-thought. A
	// sibling project measured a session holding the same unsent line for
	// fourteen hours while its supervisor's veto — "an operator has text
	// pending, do not evict" — stayed correct policy applied to a premise
	// that had stopped being true. The veto assumes a human will come back;
	// on a wedged pane none can, because typing does nothing.
	//
	// Nothing here probes the pane to find out. The discriminator that
	// project used was to type a character and see whether it appeared,
	// which is not something to do to a live session. Duration is the same
	// signal read passively: text unchanged for hours is not a sentence
	// somebody is still composing.
	status      fleet.Status
	statusSince time.Time
	// sinceRestored marks a statusSince that came from disk rather than from
	// an observation this instance made. It travels with the observation so
	// the provenance is not lost the moment the value is cached in memory.
	sinceRestored bool

	// digest fingerprints the screen this observation classified, so the
	// next one can tell "unchanged" from "changed" without keeping the pane
	// text. See classify.go's resolveAmbiguity: an unchanged screen is what
	// settles "idle or a turn that has not painted yet", which is otherwise
	// the largest source of unknown in a real fleet.
	digest string
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

// WithLoginShell sets the interpreter a created session's argv is wrapped in.
// Default: $SHELL, or a platform default when the process manager does not
// export one — which is the common case, not the exception.
func WithLoginShell(path string) Option {
	return func(d *Driver) {
		if path != "" {
			d.shell = path
		}
	}
}

// WithBareExec runs the agent directly, with no shell in front of it.
//
// This is the OLD behaviour and it is not the default, because it is what made
// a created session second-class: with no shell there is no startup file, and
// with no startup file there are no credentials, so the agent starts perfectly
// and fails at its first tool call. Available because a substrate whose agent
// needs no such environment should not pay for an interactive shell it does not
// need — but a caller reaching for it is opting out of parity, and should know
// that is what it is.
func WithBareExec() Option { return func(d *Driver) { d.bareExec = true } }

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
		retention:    defaultIdempotencyRetention,
	}
	for _, o := range opts {
		o(d)
	}
	// Constructed after options so a configured state store and retention
	// are both in effect. An unreadable table is fatal rather than silently
	// discarded: losing keys quietly is how §10's disaster arrives dressed
	// as a clean start.
	idem, err := newIdemStore(d.store, d.retention, d.now)
	if err != nil {
		d.idemErr = err
		idem, _ = newIdemStore(nil, d.retention, d.now)
	}
	d.idem = idem
	d.loadQuota()
	return d
}

// WithState makes this driver remember idempotency keys across a restart
// (§10, defect D5).
func WithState(st *state.Store) Option { return func(d *Driver) { d.store = st } }

// StateError reports a failure to load durable state, if any. A caller that
// ignores it gets a working driver with an empty key table, which is the
// behaviour that made D5 a defect — so cmd surfaces it at startup.
func (d *Driver) StateError() error { return d.idemErr }

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
// A note on fleet.Caller, which every operation below now takes: this is a
// LOCAL driver, so it has no peer to present credentials to and ignores
// Caller.Credential entirely. It must still never invent a Principal it was
// not handed — the audit trail §6 requires is only worth having if nothing
// in the chain manufactures an actor.
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
		// A local driver is describing itself, so this is observed by
		// definition — there is no network between the claim and its
		// subject.
		Source: fleet.CapabilitiesObserved,
	}
}

// bounded applies this driver's declared deadline, or the caller's if the
// caller's is shorter (§4.4: "a req may supply a shorter deadline; never
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
			// -e keeps escape sequences: the composer's placeholder is
			// distinguishable from typed input only by being rendered dim,
			// and stripping colour here would discard the one signal that
			// separates "nobody typed anything" from "do not overwrite me".
			"capture-pane", "-p", "-e", "-t", r.paneID, "-S", "-"+strconv.Itoa(d.captureLines),
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
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
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

	d.noteSessionSet(rows)

	sessions := make([]fleet.Session, 0, len(rows))
	now := d.now()
	d.mu.Lock()
	for _, r := range rows {
		text, captured := captures[r.paneID]
		young := now.Sub(r.created) < startingWindow
		raw, digest := classifyPaneRemembering(text, captured, !r.dead, young, d.memoryLocked(r.session), now)
		st, carried := d.stampSinceLocked(r.session, raw, now)
		d.observed[r.session] = observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			sinceRestored: carried,
		}
		started := r.created
		s := fleet.Session{
			SessionRef: fleet.SessionRef{Machine: d.machine, ID: r.session, Name: r.session},
			StartedAt:  &started,
			Runtime:    d.runtime,
			Cwd:        fleet.AbsolutePath(r.cwd),
			Attach:     d.attachHint(r.session),
			State:      st,
		}
		if !matchesFilter(s, filter) {
			continue
		}
		sessions = append(sessions, s)
	}
	obs := make(map[string]observation, len(d.observed))
	for k, v := range d.observed {
		obs[k] = v
	}
	d.mu.Unlock()
	d.noteStatuses(obs)

	// A usage limit belongs to the ACCOUNT, not to whichever pane happened to
	// print the notice, and it outlives that notice by days. Observe it once
	// per read: any session showing it sets the block, any session actually
	// working clears it.
	var sawLimit, sawWorking bool
	var hint string
	for i := range sessions {
		switch sessions[i].State.Status {
		case fleet.StatusQuotaBlocked:
			sawLimit = true
			if q := sessions[i].State.Quota; q != nil && q.ResetHint != "" {
				hint = q.ResetHint
			}
		case fleet.StatusWorking:
			sawWorking = true
		}
	}
	d.noteQuotaBlock(sawLimit, hint, sawWorking, now)

	// Apply it. A session that reads idle on a machine whose account is
	// refusing work is not available, and idle is the status that means send
	// it work — the whole failure this exists to prevent.
	//
	// Two statuses are rewritten, and the second was left out at first.
	//
	// idle, because idle is the status that means send it work.
	//
	// unknown, because unknown is not a competing truth — it is this driver
	// saying it could not determine one (§5.7), and an account fact IS more
	// specific than that. Leaving it out had a visible cost: four sessions on
	// a blocked machine flapped unknown → quota_blocked → unknown across
	// consecutive reads, because their panes redraw a counter, so the digest
	// changed and the ambiguity that resolves to idle never settled. Eight
	// spurious state events per cycle on a fleet where nothing was happening.
	//
	// Nothing else is rewritten. working, waiting_input and unsent text each
	// carry something observed just now, and a remembered fact must not
	// overwrite an observation.
	//
	// The same corroboration works in reverse, and must. A limit notice sits
	// on the screen long after the limit lifts — nobody types into a session
	// that refused them, so nothing overwrites it — and read literally it says
	// "blocked" forever. Measured on a working machine: two sessions reporting
	// quota_blocked from notices that had already expired, while two others on
	// that same account were working.
	//
	// A session working NOW is proof the account is not refusing work, and it
	// outranks a screen that has not changed since it was refused. So the
	// notice becomes what it is — history — and the session reports what it
	// otherwise is: settled, with nothing running and no question pending.
	// This is F53's divider again, with the evidence in another pane instead
	// of a lower line.
	if sawWorking {
		for i := range sessions {
			if sessions[i].State.Status != fleet.StatusQuotaBlocked {
				continue
			}
			st := sessions[i].State
			st.Status = fleet.StatusIdle
			st.Quota = nil
			st.Evidence = "a limit notice is on screen, but another session on this account is working now, so the notice is history"
			sessions[i].State = st
		}
	}

	if q := d.quotaBlock(); q != nil {
		for i := range sessions {
			var seen string
			switch sessions[i].State.Status {
			case fleet.StatusIdle:
				seen = "the session itself looks idle"
			case fleet.StatusUnknown:
				seen = "the session's own screen was inconclusive"
			default:
				continue
			}
			st := sessions[i].State
			st.Status = fleet.StatusQuotaBlocked
			st.Quota = q
			st.Evidence = "this machine's account is refusing work; " + seen
			if q.ResetHint != "" {
				st.Evidence += " (reported reset: " + q.ResetHint + ")"
			}
			sessions[i].State = st
		}
	}

	// Every block carries a real since. The per-session path builds its
	// QuotaBlock in classify, which has no clock, so it left the zero time —
	// which serialises as year 1 and is worse than absent: a caller computing
	// "blocked for how long" gets two millennia. The status's own since is the
	// right answer, and it already survives restarts.
	for i := range sessions {
		if q := sessions[i].State.Quota; q != nil && q.Since.IsZero() {
			blocked := *q
			blocked.Since = now
			if since := sessions[i].State.Since; since != nil && !since.IsZero() {
				blocked.Since = *since
			}
			sessions[i].State.Quota = &blocked
		}
	}

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
func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
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
		now := d.now()
		text, captured := captures[r.paneID]
		d.mu.Lock()
		raw, digest := classifyPaneRemembering(text, captured, !r.dead,
			now.Sub(r.created) < startingWindow, d.memoryLocked(r.session), now)
		st, carried := d.stampSinceLocked(r.session, raw, now)
		d.observed[r.session] = observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			sinceRestored: carried,
		}
		d.mu.Unlock()
		return st, nil
	}
	// §5.7 applied to a singular read, and then applied a second time to its
	// own answer.
	//
	// "I looked and it is not there" is a real answer, not a failure to look
	// — that much was always right. What was wrong is that it was the ONLY
	// answer: every unfound id returned `dead`, including ids this machine
	// has never had.
	//
	// `dead` is a claim about history — it existed, and it ended. For a
	// mistyped id there is no such history, so the claim is manufactured, and
	// a caller gets told its session died when the truth is that no such
	// session was ever here. Those deserve opposite reactions.
	//
	// The driver's own memory settles it, and that memory already exists for
	// §8's `since` and §12's reconciliation.
	d.mu.Lock()
	prior, seen := d.observed[ref.ID]
	d.mu.Unlock()
	if !seen {
		return fleet.SessionState{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	evidence := "session was present in the multiplexer and is no longer"
	if prior.cwd != "" {
		// Name what is gone. A caller reconciling its own records needs to
		// know WHICH session ended, and an id alone is recyclable (§5.4).
		evidence += "; last seen in " + prior.cwd
	}
	return fleet.InferredState(fleet.StatusDead, evidence, nil), nil
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
func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
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

	screenNow := newScreen(captures[target.paneID])

	// A blocking menu is its own refusal, named honestly. Pasting text into a
	// selection prompt does not deliver a message — it drives the menu, which
	// is a different and worse kind of corruption than concatenation.
	if awaitingSelection(screenNow) {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is showing a selection menu; delivered text would " +
				"drive the menu rather than be received as input (§2.4)",
		}, nil
	}

	if pending, ok := composerText(screenNow); ok && pending != "" {
		// The one case where a busy composer is not somebody else's business:
		// this driver put the text there itself, could not confirm it, and
		// said so. Completing that delivery is finishing the caller's original
		// request, not starting a new one.
		//
		// Established from OUR OWN RECORD, never by reading the screen back —
		// a multi-line paste renders as a collapsed summary, so the bytes are
		// not there to compare (F49), and the messages most likely to strand
		// are exactly the long ones.
		if opts.ResumeIfStranded && d.strandedMatches(ref.ID, text) {
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "C-m"); err != nil {
				return fleet.DeliveryReceipt{}, fmt.Errorf("send: submitting stranded text: %w", err)
			}
			d.forgetStranded(ref.ID)
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeSubmitted,
				Reason:  "submitted text this driver had delivered and could not confirm earlier",
			}, nil
		}
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
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: delivering: %w", err)
	}

	if !opts.Submit {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeQueued,
			Reason:  "placed in the composer, not submitted",
		}, nil
	}

	// CONFIRM THE TEXT LANDED BEFORE SUBMITTING, and submit with C-m.
	//
	// Both halves come from a sibling project that measured this family of
	// failures over months, and both are the difference between "the message
	// arrived" and "the message was received".
	//
	// The race: pasting and submitting back-to-back lets the submit win, so
	// the prompt is submitted EMPTY and the text lands a moment later — where
	// it then sits unsent forever. That signature was counted at eight
	// stranded operator instructions in a single day, and separately at 37 of
	// 39 panes fleet-wide.
	//
	// The keystroke: `Enter` was observed being silently dropped on a pane
	// where `C-m` submitted immediately, same text, seconds apart. They are
	// the same character in principle; they are not the same in practice, and
	// only one of them has been seen to work when the other did not.
	if !d.confirmLanded(ctx, target.paneID, text) {
		// The text is in the composer and was not submitted. Say so plainly:
		// the caller must decide whether to retry or clear it, and silence
		// here is how a session ends up holding an instruction nobody knows
		// about.
		// Record what we left behind, so the caller has a way to finish this
		// rather than being told where the text is and left there.
		d.noteStranded(ref.ID, text)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: "text was delivered to the composer but did not render in time " +
				"to be submitted safely; it is sitting there unsent — retry the same " +
				"send with resumeIfStranded to submit it",
		}, nil
	}
	// The wake key: `Space` before the newline, in ONE send-keys call.
	//
	// The FIRST keystroke into an idle pane is swallowed when that keystroke is
	// Enter — measured 6 times out of 6 on real sessions. A printable key in
	// the same position is not swallowed, and once it has landed the pane is no
	// longer idle, so the newline that follows it submits.
	//
	// A paste is not a keystroke. So after paste-buffer the submit is ALWAYS
	// the first keystroke, which means a lone newline here hits the failing
	// case on every delivery into a pane that has gone idle — most of them.
	// The confirmation above proves the text RENDERED; it does not make the
	// submit land, and the two failures look identical from outside: a receipt
	// that says submitted and a composer still holding the line.
	//
	// Both keys go in one invocation because they are both key names — no `-l`
	// — and because a second call would reintroduce a race between them.
	//
	// The trailing space is accepted as harmless: a submitted line one space
	// longer changes nothing downstream. Do NOT tidy it with a `BSpace` before
	// the newline — that puts a non-printable key back in the first-keystroke
	// slot, which is precisely the untested case.
	if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "Space", "C-m"); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: submitting: %w", err)
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
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
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
// machine, id and a human label. There is no field in which a req can
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
//     reaches the driver. A req that listed sessions, went away, came
//     back after a recycle and called close gets no protection from this
//     check, because the driver's own sighting may have been refreshed in
//     the meantime.
//
// Closing the second window needs SessionRef to carry a corroborating
// attribute — a start time the req observed — so that close can compare
// against the caller's belief rather than the driver's. That is a change to
// the specification, not to this file; it is recorded in the package doc's
// FINDINGS and in the spec's §5.4 (open defect D2, §14).
//
// A ref this driver has never seen is refused outright rather than
// destroyed on an id match, which is the literal thing §5.4 forbids.
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.Ack{}, ErrNotFound
	}

	// The strong guarantee: compare against what the CALLER observed.
	//
	// This is the window that matters. A driver comparing against its own
	// last sighting only proves nothing changed since the driver looked,
	// which says nothing about the interval the caller has been away — and
	// across a network that interval contains a round trip at minimum.
	if want := req.Expect.StartedAt; want != nil {
		if !live.created.Equal(*want) {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
				ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		return d.killCorroborated(ctx, ref)
	}

	// The weak guarantee, applied only when the caller supplied nothing to
	// corroborate against — and named as weak in any refusal, so nobody
	// mistakes it for the rule above. It closes the window between this
	// driver's own sighting and now, which is better than an id match and
	// less than §5.4 asks for.
	d.mu.Lock()
	prior, seen := d.observed[ref.ID]
	d.mu.Unlock()
	if !seen {
		return fleet.Ack{}, fmt.Errorf(
			"%w: caller supplied no expected start time, and this driver has no prior "+
				"observation of id %q either; nothing corroborates the target",
			ErrAmbiguousTarget, ref.ID)
	}
	if !live.created.Equal(prior.created) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q was recycled since this driver last observed it (weak check: "+
				"the caller supplied no expected start time)",
			ErrAmbiguousTarget, ref.ID)
	}
	if live.cwd != prior.cwd {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q now has working directory %q, not %q (weak check)",
			ErrAmbiguousTarget, ref.ID, live.cwd, prior.cwd)
	}
	return d.killCorroborated(ctx, ref)
}

// Discard clears unsent composer text without submitting it (§3).
//
// # Why this verb had to exist
//
// `send` refuses to append to a composer that already holds something (§2.4),
// which is right — appending corrupts somebody's line. But that refusal left a
// caller with nowhere to go: the only operations that touch a busy composer
// were "submit it" and "destroy the session holding it", and neither is safe
// for text the caller did not write.
//
// It arrived from real use twice over. A fleet was found holding operator text
// unsent for hours, and separately a supervisor's own keepalive stranded lines
// it never meant to send. Both needed removal, and removal did not exist.
//
// # It destroys typing, so it corroborates like a destroy
//
// expectDigest is what the caller last saw in ComposerDigest. A mismatch means
// the composer changed since — most likely a human typing this second — and
// deleting then would destroy something nobody has looked at. Refused, with
// the same sentinel `close` uses for the same reason.
//
// Discarding blind (no digest) is refused outright rather than treated as
// permission. "I do not know what is there, remove it" is exactly the request
// this operation must not honour.
//
// # An empty composer is a success
//
// A caller that timed out and retried must not be told it failed for having
// worked the first time. Nothing is destroyed by clearing nothing.
func (d *Driver) Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !live.created.Equal(*want) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	pending, _ := composerText(newScreen(captures[live.paneID]))
	if pending == "" {
		// Already clear — including the case where what looked like text was
		// the dim placeholder, which is not text at all and never was.
		return fleet.Ack{Accepted: true}, nil
	}
	if expectDigest == "" {
		return fleet.Ack{}, fmt.Errorf(
			"%w: refusing to discard %d characters the caller has not seen; "+
				"supply the composerDigest from a read",
			ErrAmbiguousTarget, len(pending))
	}
	if got := screenDigest(pending); got != expectDigest {
		return fleet.Ack{}, fmt.Errorf(
			"%w: the composer holds different text than the caller saw "+
				"(expected digest %s, found %s) — somebody may be typing right now",
			ErrAmbiguousTarget, expectDigest, got)
	}

	// C-u clears the line. Measured on a live session rather than assumed:
	// C-a C-k and Escape were tried too, and C-u alone is enough.
	if _, err := d.run(ctx, d.bin, "send-keys", "-t", live.paneID, "C-u"); err != nil {
		return fleet.Ack{}, fmt.Errorf("discard: %w", err)
	}

	// Verify, because a keypress that did not register looks exactly like one
	// that did — the same reason send confirms before submitting.
	deadline := d.now().Add(promptClearWindow)
	for {
		out, err := d.run(ctx, d.bin, "capture-pane", "-p", "-e", "-J", "-t", live.paneID)
		if err == nil {
			if left, _ := composerText(newScreen(string(out))); left == "" {
				return fleet.Ack{Accepted: true}, nil
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return fleet.Ack{}, errors.New(
				"discard: the clear keystroke did not empty the composer; text is still there")
		}
		select {
		case <-ctx.Done():
			return fleet.Ack{}, ctx.Err()
		case <-time.After(promptClearInterval):
		}
	}
}

// Rename changes a session's id (§3).
//
// # The id IS the name on this substrate
//
// A multiplexer session's name is its handle: it is what an operator sees in
// their status bar, what a picker lists, and what every command targets. So a
// rename here is not cosmetic relabelling — it changes the very thing callers
// address the session by, which is why the service emits an event and why this
// corroborates before acting.
//
// # Corroborated exactly like Close, and for a sharper reason
//
// Close destroys the wrong session if the id was recycled. Rename does
// something subtler and arguably worse: it succeeds, silently, on a session
// the caller never meant — and leaves BOTH sessions misnamed, the target
// wearing a name that belongs to another piece of work. §5.4's rule is the
// same and the failure is quieter, so the check is the same.
func (d *Driver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	to = strings.TrimSpace(to)
	if to == "" {
		return fleet.Ack{}, errors.New("rename: new name is empty")
	}
	if to == ref.ID {
		// Not an error: the caller asked for a state that already holds.
		return fleet.Ack{Accepted: true}, nil
	}

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
		}
		// Refuse a collision rather than letting the multiplexer decide. Two
		// sessions cannot share a name, and the failure mode of finding out
		// afterwards is an operator who believes a rename happened.
		if rows[i].session == to {
			return fleet.Ack{}, fmt.Errorf("rename: %q is already in use on this machine", to)
		}
	}
	if live == nil {
		return fleet.Ack{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}

	if want := req.Expect.StartedAt; want != nil {
		if !live.created.Equal(*want) {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
				ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	} else {
		// The weak guarantee, named as weak — same shape as Close.
		d.mu.Lock()
		prior, seen := d.observed[ref.ID]
		d.mu.Unlock()
		if !seen {
			return fleet.Ack{}, fmt.Errorf(
				"%w: caller supplied no expected start time, and this driver has no prior "+
					"observation of id %q either; nothing corroborates the target",
				ErrAmbiguousTarget, ref.ID)
		}
		if !live.created.Equal(prior.created) || live.cwd != prior.cwd {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q was recycled since this driver last observed it (weak check)",
				ErrAmbiguousTarget, ref.ID)
		}
	}

	// "=" pins an exact name. Without it the multiplexer resolves prefixes and
	// patterns, and would happily rename a DIFFERENT session whose name merely
	// starts with this one — which on a fleet full of `<repo>-<issue>` names is
	// not a hypothetical.
	if _, err := d.run(ctx, d.bin, "rename-session", "-t", "="+ref.ID, to); err != nil {
		return fleet.Ack{}, fmt.Errorf("rename: %w", err)
	}

	// Carry the driver's own memory across, or the renamed session looks
	// brand new: its `since` would reset and §12 would call it adopted.
	d.mu.Lock()
	if prior, ok := d.observed[ref.ID]; ok {
		d.observed[to] = prior
		delete(d.observed, ref.ID)
	}
	d.mu.Unlock()

	return fleet.Ack{Accepted: true}, nil
}

func (d *Driver) killCorroborated(ctx context.Context, ref fleet.SessionRef) (fleet.Ack, error) {
	if _, err := d.run(ctx, d.bin, "kill-session", "-t", ref.ID); err != nil {
		return fleet.Ack{}, fmt.Errorf("close: %w", err)
	}
	d.mu.Lock()
	delete(d.observed, ref.ID)
	d.mu.Unlock()
	return fleet.Ack{Accepted: true}, nil
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
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.SessionRef, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.SessionRef{}, errors.New("create: idempotency key is required (§10)")
	}
	// A completed key returns what it produced; a pending one means this
	// driver was interrupted mid-create and must find out what happened
	// before doing anything (§10, see idempotency.go).
	if ref, rec, found := d.idem.lookup(key); found {
		if rec.Phase == idemComplete {
			return ref, nil
		}
		if adopted, ok := d.resolvePending(ctx, key, rec); ok {
			return adopted, nil
		}
		// Nothing was started, or nothing survives. Safe to proceed.
		_ = d.idem.release(key)
	}

	// The name is resolved BEFORE the argv is built, and this ordering is the
	// seam the whole creation contract hangs on.
	//
	// The resolved string is what the multiplexer session is called, what the
	// remote-control binding is keyed on, and what the agent calls itself. A
	// builder handed the REQUESTED name would bind remote control to a name
	// the session does not have — which fails exactly the way this whole area
	// fails: silently, later, and somewhere else.
	requested := spec.Name
	if requested == "" {
		requested = "fleet-" + d.nonce()
	}
	name, ok := d.resolveName(ctx, requested, spec.Marker)
	if !ok {
		return fleet.SessionRef{}, fmt.Errorf(
			"create: could not derive a free session name from %q; either it sanitizes "+
				"to nothing, or too many sessions already carry it", requested)
	}
	if spec.Cwd == "" {
		return fleet.SessionRef{}, errors.New("create: cwd is required")
	}

	contextFile := string(spec.ContextRef)
	if contextFile != "" && !filepath.IsAbs(contextFile) {
		return fleet.SessionRef{}, fmt.Errorf("create: contextRef must be absolute, got %q", contextFile)
	}

	// Intent first. A crash between here and the session starting leaves a
	// pending record, which the next attempt resolves by looking rather
	// than guessing.
	if err := d.idem.reserve(key, name, string(spec.Cwd)); err != nil {
		return fleet.SessionRef{}, fmt.Errorf("create: recording intent: %w", err)
	}

	// The builder sees the RESOLVED name, not the requested one — see above.
	built := spec
	built.Name = name
	argv := d.build(built, contextFile)

	// Wrap the agent in a login+interactive shell so it inherits the same
	// environment a launcher-created session does, and stage a record of what
	// it actually ended up with. See environment.go for why interactive is not
	// optional and why the record carries no values.
	recordPath := ""
	if !d.bareExec {
		recordPath = d.envRecordPath()
		argv = loginWrap(d.loginShell(), recordPath, argv)
	}

	args := append([]string{
		"new-session", "-d", "-s", name, "-c", string(spec.Cwd), "--",
	}, argv...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		// The create demonstrably failed, so the reservation describes
		// nothing. Releasing it keeps a retry from being answered with a
		// session that was never started.
		_ = d.idem.release(key)
		return fleet.SessionRef{}, fmt.Errorf("create: %w", err)
	}

	ref := fleet.SessionRef{Machine: d.machine, ID: name, Name: name}
	if err := d.idem.complete(key, ref); err != nil {
		return fleet.SessionRef{}, fmt.Errorf("create: recording result: %w", err)
	}

	if recordPath != "" {
		go d.captureEnvironment(name, recordPath)
	}
	if spec.Prompt != "" {
		go d.deliverInitialPrompt(req, ref, spec.Prompt)
	}
	return ref, nil
}

// claudeCodeCommand is the default CommandBuilder.
//
// Note what is absent: the prompt. It is delivered after the session is up
// (see Create) precisely so it stays out of this argv.
//
// # Why the remote-control flags are here by default
//
// They were missing, and their absence was invisible at creation: the session
// started, listed, read and drove perfectly, and was simply unreachable from
// any remote client. That is most of the value of creating one remotely in the
// first place — a session you must be sitting at the machine to use is a
// session you could have created by sitting at the machine.
//
// The binding is keyed on spec.Name, which Create has already resolved to the
// canonical string. The session name, the remote-control binding and the
// agent's own name are therefore the SAME string from birth, which is what
// makes a session findable by the one identifier every surface shows.
func claudeCodeCommand(spec fleet.SessionSpec, contextFile string) []string {
	argv := []string{"claude"}
	// Nil means "whatever a first-class session gets", which on this
	// substrate is enabled — an unaware caller must not silently receive the
	// second-class shape. Only an explicit false opts out.
	if spec.RemoteControl == nil || *spec.RemoteControl {
		if spec.Name != "" {
			argv = append(argv, "--remote-control", spec.Name, "-n", spec.Name)
		}
	}
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

// Respond answers a prompt the session is blocked on (§3).
//
// # It refuses when nothing is being asked
//
// A keypress delivered to a session that is not at a prompt lands in whatever
// that session was doing. Unlike a message — which at worst appears in a
// composer where a human can see and delete it — a stray keypress is consumed
// invisibly, and "Enter" against a composer holding a half-typed thought
// submits it.
//
// So this checks for a prompt first and refuses otherwise, which is §2.4's
// reasoning applied to control rather than to text.
//
// # Why this operation had to exist
//
// Three real prompts in one session made the case: a folder-trust question on
// every newly created session, a resume-from-summary question on a session
// being reattached, and a menu inside a running conversation. None could be
// answered through send(), because send() is built to guarantee it never
// produces a keystroke. A supervisor could start an agent and then not get
// past its first question — which is how a fleet loses a session to a dialog
// nobody can reach.
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
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
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "no session with this id"}, nil
	}
	if target.dead {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "session process has exited"}, nil
	}

	before := parsePrompt(newScreen(captures[target.paneID]))
	if before == nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is not waiting on a prompt; a keypress would be " +
				"consumed by whatever it is doing instead",
		}, nil
	}

	// A stale nonce means the caller is answering a question that is no
	// longer on screen. Submitting by index anyway would answer a DIFFERENT
	// question — and the two boot prompts on this substrate put the safe
	// option at different indices, so that is not a theoretical harm.
	if resp.Nonce != "" && resp.Nonce != before.Nonce {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the prompt changed since it was read; answering by index now " +
				"would answer a different question",
		}, nil
	}
	if resp.Choice > len(before.Options) {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "no such option on this prompt",
		}, nil
	}

	// C-m rather than Enter — see confirmLanded for the measurement behind
	// this. A prompt that swallows the keypress leaves the session blocked,
	// which is the failure this operation exists to end.
	//
	// And `Space` before it, for the same reason as the submit in send(): the
	// first keystroke into an idle pane is swallowed when it is Enter, measured
	// 6 of 6. "Accept the highlighted option" is the one branch here that would
	// otherwise send a lone newline into exactly that slot — the `Choice > 0`
	// branch below already leads with a printable digit and never showed the
	// fault, which is itself corroboration. Escape is left alone: what was
	// measured is the Enter case, and guessing past the measurement is how the
	// wrong key ends up shipped.
	keys := []string{"Space", "C-m"}
	switch {
	case resp.Cancel:
		keys = []string{"Escape"}
	case resp.Choice > 0:
		keys = []string{strconv.Itoa(resp.Choice), "C-m"}
	}
	args := []string{"send-keys", "-t", target.paneID}
	args = append(args, keys...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}

	answered := "accepted the highlighted option"
	switch {
	case resp.Cancel:
		answered = "cancelled the prompt"
	case resp.Choice > 0:
		answered = "chose option " + strconv.Itoa(resp.Choice)
	}
	if resp.Choice > 0 && resp.Choice <= len(before.Options) {
		answered += " (" + before.Options[resp.Choice-1] + ")"
	} else if before.Selected > 0 && before.Selected <= len(before.Options) {
		answered += " (" + before.Options[before.Selected-1] + ")"
	}
	if resp.Nonce == "" {
		answered += "; answered without a nonce, so nothing verified the prompt " +
			"had not changed since it was read"
	}

	// Confirm the prompt actually went away. A keypress a prompt swallows
	// leaves the session exactly as stuck as before, and reporting success
	// there is how a supervisor concludes it has cleared something it has
	// not.
	if d.promptCleared(ctx, target.paneID, before.Nonce) {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: answered}, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason:  answered + "; the prompt is still on screen, so the keypress may not have registered",
	}, nil
}

// deliverInitialPrompt waits for the runtime to be ready, then sends §2.1's
// initial prompt.
//
// # Why this cannot happen inside Create
//
// §2.1 says a spec may carry an initial prompt, and §4.4 says every call is
// bounded by the driver's declared deadline. On this substrate those two do
// not fit: the runtime takes far longer to paint its interface than any sane
// per-call deadline, so a Create that waited would violate its own
// declaration, and a Create that did not wait delivered into a terminal that
// was not listening.
//
// The second is what happened, and it is worse than it sounds. The paste
// landed but the submit keystroke was swallowed during startup, leaving the
// prompt sitting UNSENT in the composer — indistinguishable from a human's
// half-typed message, so every later send to that session was refused in order
// to protect text the session had put there itself. Create manufactured
// exactly the stuck session this driver exists to avoid.
//
// So delivery happens after Create returns, bounded, and only once the
// interface is ready to receive. Failure is not silent: the prompt is simply
// absent, and the session's state says what it is doing instead.
func (d *Driver) deliverInitialPrompt(req fleet.Request, ref fleet.SessionRef, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), promptDeliveryWindow)
	defer cancel()

	for {
		if ctx.Err() != nil {
			return
		}
		ready, blocked := d.promptReadiness(ctx, ref.ID)
		if blocked {
			// A prompt is waiting for a human — commonly the trust question
			// a newly created session asks about its working directory.
			// Answering it is a decision (§6 grants it separately), and a
			// driver that clicked through it would be consenting to
			// something nobody asked it to consent to.
			return
		}
		if ready {
			_, _ = d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(promptPollInterval):
		}
	}
}

// promptReadiness reports whether the session can receive input, and whether
// it is instead blocked on a prompt.
func (d *Driver) promptReadiness(ctx context.Context, id string) (ready, blocked bool) {
	callCtx, cancel := d.bounded(ctx)
	defer cancel()
	rows, captures, err := d.enumerate(callCtx)
	if err != nil {
		return false, false
	}
	for _, r := range rows {
		if r.session != id {
			continue
		}
		sc := newScreen(captures[r.paneID])
		if _, b := selectionPrompt(sc); b {
			return false, true
		}
		text, found := composerText(sc)
		// Ready means the composer exists and is empty: the interface has
		// painted, and nothing is already sitting in it.
		return found && text == "", false
	}
	return false, false
}

// confirmLanded waits until the composer actually shows the delivered text.
//
// A render can lag a busy TUI by longer than a naive fixed pause, and giving
// up too early is not free: the text is already in the box, so a premature
// failure strands an instruction rather than losing it. The loop therefore
// keeps reading while there is budget, rather than sampling a fixed number of
// times — a sibling project measured a legitimate delivery failing a ~360 ms
// verification budget while the text was, seconds later, fully and correctly
// rendered.
//
// Matching is on a prefix of the delivered text: the composer wraps long
// input across lines and may decorate it, so requiring an exact whole-string
// match would fail on precisely the long instructions that matter most.
func (d *Driver) confirmLanded(ctx context.Context, paneID, text string) bool {
	needle := strings.TrimSpace(text)
	if needle == "" {
		return true
	}
	// A prefix, because the composer wraps and the tail may be off-screen.
	if len(needle) > 24 {
		needle = needle[:24]
	}
	// ...except when the runtime does not echo the text at all.
	//
	// A MULTI-LINE paste is collapsed into a summary — "[Pasted text #1 +8
	// lines]" — so the bytes just delivered appear nowhere on screen. Matching
	// the text then fails forever, and every multi-line message is reported
	// stranded: delivered, honestly refused, and sitting in the composer.
	//
	// Measured the first time a long message was sent to a live session, and
	// it would have been every long message after it.
	//
	// The collapsed form is still positive evidence of landing — it says the
	// composer accepted a paste — so it is accepted as such. It is matched
	// structurally (a bracketed marker on the composer line naming a count of
	// lines) rather than by its wording, because the wording is exactly the
	// kind of prose that changes underneath a matcher.
	deadline := d.now().Add(submitConfirmWindow)
	for {
		out, err := d.run(ctx, d.bin, "capture-pane", "-p", "-J", "-t", paneID, "-S", "-6")
		if err == nil {
			painted := stripSGR(string(out))
			if strings.Contains(painted, needle) || composerHoldsCollapsedPaste(painted) {
				return true
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(submitConfirmInterval):
		}
	}
}

// composerHoldsCollapsedPaste reports whether the composer line shows the
// runtime's summary of a pasted block rather than the pasted text itself.
//
// Structural rather than worded: a composer line carrying a bracketed marker
// that mentions a line count. The runtime is free to reword "Pasted text"; it
// is not free to stop saying how many lines it swallowed, because that is the
// only thing the summary is FOR.
func composerHoldsCollapsedPaste(painted string) bool {
	for _, line := range strings.Split(painted, "\n") {
		if !strings.Contains(line, composerRuneMarker) {
			continue
		}
		rest := line[strings.Index(line, composerRuneMarker):]
		open := strings.Index(rest, "[")
		if open < 0 {
			continue
		}
		close := strings.Index(rest[open:], "]")
		if close < 0 {
			continue
		}
		inside := strings.ToLower(rest[open : open+close])
		if strings.Contains(inside, "line") && strings.ContainsAny(inside, "0123456789") {
			return true
		}
	}
	return false
}

// noteQuotaBlock remembers that this machine's account is refusing work, and
// forgets it the moment something proves otherwise.
//
// Called with what the current read saw: whether any session showed a limit
// notice, and whether any session was observed working. One working session is
// proof the account works, and is the only thing that clears the block — a
// reset time is scraped prose and must not be trusted to expire it.
func (d *Driver) noteQuotaBlock(sawLimit bool, hint string, sawWorking bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case sawWorking:
		if d.quota != nil {
			d.quota = nil
			d.saveQuotaLocked()
		}
	case sawLimit && d.quota == nil:
		d.quota = &fleet.QuotaBlock{Since: now, ResetHint: hint}
		d.saveQuotaLocked()
	case sawLimit && hint != "" && d.quota.ResetHint == "":
		// A later notice may carry a reset time the first one did not.
		d.quota.ResetHint = hint
		d.saveQuotaLocked()
	}
}

// quotaBlock reports the remembered account block, if any.
func (d *Driver) quotaBlock() *fleet.QuotaBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quota == nil {
		return nil
	}
	q := *d.quota
	return &q
}

func (d *Driver) saveQuotaLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save("quota", d.quota)
}

func (d *Driver) loadQuota() {
	if d.store == nil {
		return
	}
	var q fleet.QuotaBlock
	if found, err := d.store.Load("quota", &q); err == nil && found && !q.Since.IsZero() {
		d.quota = &q
	}
}

// noteStranded records text this driver delivered and could not confirm.
//
// The record is what a resume corroborates against. It is deliberately OUR
// account of what we did, not a reading of the screen: the screen cannot show
// a long message back (F49), and composer contents are not evidence anyone
// meant to send them.
func (d *Driver) noteStranded(id, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stranded == nil {
		d.stranded = map[string]string{}
	}
	d.stranded[id] = text
}

// strandedMatches reports whether text is exactly what this driver left in
// that session's composer.
//
// Exact, not prefix: "resume the delivery I made" is a different request from
// "submit something that starts the same way", and only the first is the one
// the caller is owed.
func (d *Driver) strandedMatches(id, text string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	prior, ok := d.stranded[id]
	return ok && prior == text
}

func (d *Driver) forgetStranded(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.stranded, id)
}

// stampSinceLocked fills §2.3's Since: when this status was FIRST observed to
// hold, not when it began. Caller holds d.mu.
//
// For a session holding unsent input the evidence also gains the age, because
// that is the number a human needs and the one that distinguishes "somebody is
// typing" from "nobody is ever coming back". A caller reading `since` can
// compute it; a caller reading a log line cannot.
// attachHint describes how a person gets a terminal onto this session (§2.8).
//
// The binary path is this machine's, resolved the same way every other
// invocation resolves it — which matters, because the reason FLEET_TMUX_BIN
// exists is that a non-interactive shell on these machines does not have the
// multiplexer on PATH. A hint containing a bare "tmux" would work when tested
// interactively and fail exactly where a supervisor would run it.
//
// No remote form is produced. This driver knows the machine it runs on; it
// does not know how a caller reaches that machine, and inventing an ssh line
// would be this service asserting a network topology it cannot see (§7.2).
func (d *Driver) attachHint(session string) *fleet.AttachHint {
	bin := d.attachBin()
	return &fleet.AttachHint{
		Kind:   "multiplexer",
		Target: session,
		// -t takes the name verbatim; ids here routinely contain emoji and
		// spaces, which is why this is argv and not a command string.
		Command: []string{bin, "attach-session", "-t", session},
		// -r attaches read-only: the viewer sees the session and cannot type
		// into it. This is the one a supervisor should offer for "watch",
		// because the read-write attachment shares a real keyboard with
		// whatever the agent is doing.
		ReadOnly: []string{bin, "attach-session", "-r", "-t", session},
		// The multiplexer permits many concurrent clients on one session, so
		// attaching never evicts anyone.
		Shared: true,
	}
}

// attachBin resolves the multiplexer to an absolute path for the hint.
//
// The driver itself can run a bare "tmux" because whatever PATH it inherited
// resolved it. A hint is executed somewhere else entirely — possibly by a
// supervisor's non-interactive shell, which on these machines gets a bare PATH
// that does not include the package manager's prefix. Handing out a name that
// works here and not there would produce a hint that fails only in production,
// which is the same trap FLEET_TMUX_BIN exists for.
//
// Falls back to the configured value when resolution fails: a name is a worse
// answer than a path, and still better than nothing.
func (d *Driver) attachBin() string {
	if filepath.IsAbs(d.bin) {
		return d.bin
	}
	if resolved, err := exec.LookPath(d.bin); err == nil {
		return resolved
	}
	return d.bin
}

// memoryLocked returns what the driver remembers of a pane's last screen.
// Caller holds d.mu.
func (d *Driver) memoryLocked(id string) paneMemory {
	prior, ok := d.observed[id]
	if !ok || prior.digest == "" {
		return paneMemory{}
	}
	return paneMemory{known: true, digest: prior.digest, at: prior.at}
}

// Returns the state and whether its `since` was carried from a previous
// instance — the caller must store that on the observation, or the provenance
// is lost the moment the value is cached and every later read presents a
// second-hand age as one this instance measured.
func (d *Driver) stampSinceLocked(id string, st fleet.SessionState, now time.Time) (fleet.SessionState, bool) {
	since := now
	restored := false
	if prior, ok := d.observed[id]; ok && prior.status == st.Status && !prior.statusSince.IsZero() {
		since = prior.statusSince
		restored = prior.sinceRestored
	} else if rec, ok := d.persistedRecord(id); ok &&
		rec.Status == string(st.Status) && !rec.StatusSince.IsZero() {
		// No in-memory sighting, but the same status was recorded before this
		// instance started. Carrying it is the difference between reporting a
		// 14-hour stall and reporting the service's own uptime.
		since = rec.StatusSince
		restored = true
	}
	st.Since = &since

	if st.Status == fleet.StatusWaitingInput && strings.Contains(st.Evidence, "unsent input") {
		if age := now.Sub(since); age > unsentAgeWorthMentioning {
			st.Evidence += "; unchanged for " + age.Round(time.Minute).String()
		}
	}
	// Say where the number came from. §5.2 forbids presenting inference as
	// observation, and an age this instance did not measure is exactly that —
	// the value is worth keeping, the provenance is not optional.
	if restored {
		st.Evidence += " (age carried from before this service restarted)"
	}
	return st, restored
}

// persistedRecord reads one session's durable record. Caller holds d.mu.
func (d *Driver) persistedRecord(id string) (sessionRecord, bool) {
	if d.store == nil {
		return sessionRecord{}, false
	}
	rec, ok := d.loadRecords()[id]
	return rec, ok
}

// promptCleared waits briefly for the answered prompt to leave the screen.
//
// "Still the same prompt" is the only outcome that means the keypress did not
// register; a different prompt counts as cleared, because the session moved on
// and the caller's answer had its effect.
func (d *Driver) promptCleared(ctx context.Context, paneID, was string) bool {
	deadline := d.now().Add(promptClearWindow)
	for {
		out, err := d.run(ctx, d.bin, "capture-pane", "-p", "-e", "-t", paneID, "-S", "-"+strconv.Itoa(d.captureLines))
		if err == nil {
			if now := parsePrompt(newScreen(string(out))); now == nil || now.Nonce != was {
				return true
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(promptClearInterval):
		}
	}
}
