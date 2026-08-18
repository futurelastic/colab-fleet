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
	"log"
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

	// sendReceptiveWindow bounds Send's wait for the runtime to be able to
	// receive input. Deliberately SHORT: §4.4 caps a call at the driver's
	// declared deadline, so this covers the race, not a startup. Beyond it
	// Send refuses and says what to wait for.
	sendReceptiveWindow   = 2 * time.Second
	sendReceptiveInterval = 200 * time.Millisecond

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

	// strandedRetention is how long a stranded-delivery record (#11) is
	// honoured after the delivery that produced it. Same value as
	// defaultIdempotencyRetention, named separately because it answers a
	// different question — how long a stuck delivery is still worth
	// finishing on a caller's behalf, not how long a retry window is
	// honoured — and reusing the number is a deliberate consistency with
	// §10's already-argued "must outlive a service restart," not a
	// placeholder. Without an expiry, a durable record eventually matches
	// text a human typed that happens to be identical — precisely what
	// strandedMatches's exact-match rule exists to exclude.
	strandedRetention = 30 * time.Minute
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
	// not confirm — the record a resume is checked against.
	//
	// Durable when a state store is configured (#11): a composer holding
	// unsent text survives a multiplexer restart on its own, but until now
	// the driver's own memory of having put it there did not survive a
	// SERVICE restart — so resumeIfStranded, the one door out of §2.4's
	// busy-composer refusal, stopped working on exactly the deploys it
	// exists to survive. See noteStranded/strandedMatches for the
	// corroboration (§5.4: id + cwd, not id alone) and strandedRetention
	// for the lifetime a durable record needs that an in-memory one never
	// did.
	stranded map[string]strandedRecord

	// environments remembers what each created session's process received
	// (see environment.go). In memory only, for the reason stated on
	// Environment.
	environments map[string]fleet.SessionEnvironment

	// counters is this driver's self-observability registry — see
	// counters.go. Its own mutex, not d.mu: nothing about a count is
	// otherwise related to session state, and sharing a lock would only
	// make counting something contend with it for no reason.
	counters counterSet

	// conversations locates the runtime's own record of each session — see
	// conversation.go. Nil until a record root is configured, and nil is
	// what makes a listing report nothing at all rather than reporting that
	// no record was found.
	conversations *conversationStore

	// credentialPath is the runtime's own local credential store, stat'ed to
	// answer #12 (SessionState.CredentialGeneration, EventMachineAccount).
	// Empty means unconfigured — the honest default for a constructor a
	// test, a sandbox or another program can call — and every session then
	// reports the field absent rather than a guessed value. See
	// WithCredentialPath.
	//
	// Unlike quota this needs no field alongside it to remember a value
	// across reads: a file's modification time does not evaporate the way a
	// scrolled-away screen notice does, so the filesystem already holds the
	// fact and a cached copy in this struct would only be a second,
	// potentially stale one.
	credentialPath string
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

// WithRecordRoot points this driver at the runtime's own conversation record
// store, enabling the lookup that fills Session.Conversation.
//
// # Why this is off by default
//
// A driver constructed without it reports NOTHING about conversations — the
// field stays absent, meaning nobody looked — and that is the honest default
// for a constructor a test, a sandbox or another program can call. A default
// that read a real user's record store merely because a Driver was constructed
// would make an unconfigured process go looking through somebody's
// conversations, and would make every test's answer depend on the machine it
// ran on.
//
// The composition root supplies it, the same way it supplies the state store.
// An empty path disables the lookup again, explicitly.
func WithRecordRoot(path string) Option {
	return func(d *Driver) {
		if path == "" {
			d.conversations = nil
			return
		}
		d.conversations = newConversationStore(path)
	}
}

// WithCredentialPath points this driver at the runtime's own local
// credential store, enabling #12: a session's CredentialGeneration and the
// machine.account event both come from stat'ing this one path.
//
// Off by default for the same reason as WithRecordRoot — a driver
// constructed for a test or a sandbox must not go stat'ing a real file
// merely because it was built. The composition root supplies a real path;
// an empty one disables the feature again, explicitly, and every session
// then reports CredentialGeneration absent rather than a guessed value
// (§5.7).
func WithCredentialPath(path string) Option {
	return func(d *Driver) { d.credentialPath = path }
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
	d.loadStranded()
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
// ConfirmsDelivery is false, and for TWO reasons rather than the one
// originally written here.
//
// The first is a limit on observing an OUTCOME: the driver can see afterwards
// that the composer is empty, but "the composer is empty" does not distinguish
// "the agent received it" from "something else cleared it".
//
// The second is a limit on verifying its own ACTION, and it is the one this
// note used to omit. Send issues a submit keystroke and then returns without
// looking: the confirmation it performs happens BEFORE the submit and proves
// only that the text rendered. So the driver cannot say the submit registered
// either — not merely that it cannot see what came of it. Those are different
// claims, and collapsing them made `queued` read as stronger than it is.
//
// Respond does not have this gap: it calls promptCleared afterwards and
// downgrades to unknown. Send has no equivalent, which is why its receipt now
// names the submit as unverified rather than scoping the doubt to the agent.
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

// Counters implements driver.CounterReporter. See counters.go's doc comment
// for what accumulates here and why it stops at two names today — this
// method does not add a third; it only opens the read path #9 asked for
// onto whatever counters.go currently owns.
func (d *Driver) Counters() map[string]int64 {
	return d.counters.Snapshot()
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
		// The shape comes from classifyCaptureArgs, which is where the -e
		// rationale lives: the composer's placeholder is distinguishable from
		// typed input only by being rendered dim, and stripping colour here
		// would discard the one signal separating "nobody typed anything"
		// from "do not overwrite me".
		capArgs = append(capArgs, "display-message", "-p", mark+strconv.Itoa(i), ";")
		capArgs = append(capArgs, classifyCaptureArgs(r.paneID, d.captureLines)...)
	}
	// A pane can vanish in the gap between the listing above and this
	// capture — a session that ends while unobserved is ordinary churn
	// (reconcile.go treats it as exactly that), not a machine failure. The
	// multiplexer exits nonzero when ANY chained capture-pane target in this
	// one invocation is gone, and an earlier version of this call treated
	// that as fatal for the whole batch — discarding the screens of every
	// OTHER session in the same call, on a machine with any churn at all
	// (see #29).
	//
	// The err here is deliberately not returned. Go's Cmd.Output still hands
	// back whatever the process wrote to stdout before the failing
	// sub-command, and the association loop three lines down already
	// tolerates one pane's capture going missing — an absent entry in
	// byIndex classifies that session "unknown" rather than aborting (see
	// the marker-corruption comment above). So a nonzero exit here is
	// folded into the same tolerance: keep whatever this invocation
	// produced, and let the caller count what is missing rather than
	// throwing all of it away.
	capOut, _ := d.run(ctx, d.bin, capArgs...)
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
		//
		// This is now genuinely "no response": enumerate only returns an
		// error when the LISTING call itself failed (see enumerate's own
		// comment — a capture-side failure is folded into the per-session
		// "unknown" path below instead). Unreachable is the right word
		// exactly because nothing about this machine answered at all.
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     fleet.SourceUnreachable,
			Error:      err.Error(),
			ObservedAt: d.now(),
			// The multiplexer not answering says nothing about whether the
			// ACCOUNT is refusing work — that memory is this driver's own
			// (quotaBlock, below) and does not depend on reaching tmux at
			// all (#10). Reachability and willingness are different
			// questions; answer both, independently, even when one of them
			// just failed.
			Quota: d.quotaBlock(),
		}
		return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{src})
	}

	// A pane can have vanished between the listing and the capture (see
	// enumerate's own comment and #29): that row survives — it is still a
	// real session this machine reported — but its screen was not read, and
	// captures simply has no entry for it. Count that here, once, rather
	// than at each of the sites below that read captures: this is the one
	// place that gets to decide what a miss says about the SOURCE, as
	// opposed to what it says about the one session that missed.
	missed := 0
	for _, r := range rows {
		if _, ok := captures[r.paneID]; !ok {
			missed++
		}
	}

	d.noteSessionSet(rows)

	sessions := make([]fleet.Session, 0, len(rows))

	// Which sessions still need their conversation record located. Collected
	// here and resolved below, after the lock is dropped: that lookup reads a
	// filesystem, and holding the lock that guards every session's observed
	// state across a directory read would make one slow disk stall the whole
	// listing.
	type pendingConversation struct {
		index   int
		key     conversationKey
		cwd     string
		name    string
		started time.Time
	}
	var pending []pendingConversation

	now := d.now()
	// Read once, outside the loop, and stamp every session in this response
	// with the same value: they are all being answered as of this one
	// instant, and a machine-wide fact read once per session risks reading
	// two different generations into one snapshot if the file changes
	// mid-loop (#12).
	gen := d.credentialGeneration()
	d.mu.Lock()
	for _, r := range rows {
		text, captured := captures[r.paneID]
		young := now.Sub(r.created) < startingWindow
		raw, digest := classifyPaneRemembering(text, captured, !r.dead, young, d.memoryLocked(r.session), now)
		st, carried := d.stampSinceLocked(r.session, raw, now)
		st.CredentialGeneration = gen
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
		if d.conversations != nil {
			// Keyed on the pane rather than the session name, because a
			// rename changes the name and the title already written into the
			// record does not — see conversationKey.
			pending = append(pending, pendingConversation{
				index:   len(sessions),
				key:     conversationKey{pane: r.paneID, created: r.created},
				cwd:     r.cwd,
				name:    r.session,
				started: r.created,
			})
		}
		sessions = append(sessions, s)
	}
	obs := make(map[string]observation, len(d.observed))
	for k, v := range d.observed {
		obs[k] = v
	}
	d.mu.Unlock()
	d.noteStatuses(obs)

	// Locate each session's record in the runtime's own store. This is the
	// only source on this path that is not the runtime describing itself
	// (conversation.go says why that matters), and it is also the only one
	// that can answer "I looked and could not tell" — which is a different
	// answer from the absent field a driver with no store leaves behind.
	for _, p := range pending {
		sessions[p.index].Conversation = d.conversations.lookup(p.key, p.cwd, p.name, p.started)
	}

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

	// q is read once and reused below for the source's own Quota field
	// (#10) — the same remembered fact, at both grains, from a single
	// lock acquisition rather than two.
	q := d.quotaBlock()
	if q != nil {
		for i := range sessions {
			sessions[i].State = quotaBlockedState(sessions[i].State, q)
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
		// A scheduler asking "where can work run right now" reads this
		// field, not Items() — the account fact must be visible here even
		// against a filter that empties the item list entirely (#10).
		Quota: q,
	}
	if missed > 0 {
		// This machine answered — every session in count is real, including
		// the ones that missed a capture — so unreachable would be a lie.
		// But it did not answer IN FULL, and §5.7's rule that absence and
		// failure are different answers applies to the source's own status
		// exactly as it does to a session's: reporting SourceOK here would
		// have this read call itself complete while N of its sessions are
		// carrying "unknown" for a reason the caller cannot see without
		// this Error string. NewCollection reads this Status and turns
		// Complete() false on its own — nothing below has to remember to.
		src.Status = fleet.SourceDegraded
		src.Error = fmt.Sprintf(
			"%d of %d sessions' screens were not captured this read "+
				"(a pane vanished between listing and capture, or the capture "+
				"invocation otherwise failed for it); each reports its own "+
				"state as unknown rather than a guess",
			missed, len(rows))
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
		st.CredentialGeneration = d.credentialGeneration() // #12, same as List's per-session stamp
		d.observed[r.session] = observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			sinceRestored: carried,
		}
		d.mu.Unlock()
		// Same rewrite List applies, generalised to a one-session read (#10)
		// — see quotaBlockedState's own comment for why a session's own
		// state must not be reported as an unqualified "starting"/"idle"/
		// "unknown" while this machine's account is known to be refusing
		// work. This is the read Create's own HTTP handler makes to build
		// the state it hands back in a 201 — without this, a create
		// reported nothing and the fact was swallowed until a later poll.
		return quotaBlockedState(st, d.quotaBlock()), nil
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

	// THE RUNTIME MUST BE ABLE TO RECEIVE BEFORE ANYTHING IS DELIVERED.
	//
	// Measured: delivering to a session that has not finished starting renders
	// the text in the composer and drops the submit, two runs in three, while
	// the receipt still said "queued". Nothing here looked, because Create
	// returns as soon as the process is spawned and Send trusted that.
	//
	// The check is the composer's presence, not a delay — see receptive for
	// why that is evidence about the input path rather than about elapsed
	// time, and for what it does not prove.
	//
	// It refuses rather than waiting out a startup, and that is a deliberate
	// reading of §4.4: a runtime takes far longer to paint than this driver's
	// declared deadline allows, so a Send that blocked until a new session was
	// ready would overrun its own declaration. A refusal is also the honest
	// outcome — §2.4 exists for input that would corrupt a session, and text
	// delivered into a runtime that is not listening strands exactly that way.
	if ready, blocked := d.awaitReceptive(ctx, target.paneID); !ready {
		if blocked {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is showing a selection menu; delivered text would " +
					"drive the menu rather than be received as input (§2.4)",
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is not able to receive input yet: no composer has been " +
				"painted, so the runtime is still starting or is not listening. " +
				"Delivering now would render the text and lose the submit. Wait for " +
				"the session to report idle, then send again",
		}, nil
	}

	// Re-read the screen AFTER the readiness gate. The enumeration above was
	// taken before the wait, and acting on it here would decide "is somebody
	// typing" from a screen that is now stale by as long as the wait took.
	//
	// Through captureForClassify, which owns the escape-carrying shape this
	// decision depends on. The first version of this re-read dropped the
	// escapes and so read the composer's dim placeholder as text a human had
	// typed — refusing delivery to any idle session showing a hint, and
	// blaming an operator who did not exist.
	screenNow := newScreen(captures[target.paneID])
	if sc, ok := d.captureForClassify(ctx, target.paneID); ok {
		screenNow = sc
	}

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
		if opts.ResumeIfStranded && d.strandedMatches(ref.ID, target.cwd, text) {
			// Wake key before the newline, the same shape as the other two
			// submit sites (#21). This pane is idle BY DEFINITION — the branch
			// only runs when a composer has been sitting on an unsubmitted
			// line — so if a lone newline is ever dropped there, it is dropped
			// here.
			//
			// The measurement behind the original change is not settled: it
			// was made by reading a pane, and a composer holding only its own
			// faint placeholder produces exactly the reading "nothing
			// happened, the text is still there". What justifies this edit is
			// consistency, not that number — three submit sites, one shape,
			// and no reason for this one to differ. The cost either way is a
			// trailing space.
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "Space", "C-m"); err != nil {
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

	// The composer's marker state, read immediately before this delivery's
	// own paste — not reused from screenNow above, which was captured before
	// the pending-composer gate and can be stale by however long that check
	// took. Everything confirmLanded and confirmSubmitted attribute to THIS
	// delivery is a CHANGE relative to this snapshot; see markerCounts for
	// why the presence of a marker was never enough on its own.
	before := d.paintedMarkers(ctx, target.paneID)

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
	key, atCount, landed := d.confirmLanded(ctx, target.paneID, text, before)
	if !landed {
		// The text is in the composer and was not submitted. Say so plainly:
		// the caller must decide whether to retry or clear it, and silence
		// here is how a session ends up holding an instruction nobody knows
		// about.
		//
		// Two established facts, kept apart on purpose: no literal prefix
		// rendered, AND no single new paste marker could be pinned on this
		// delivery alone — either nothing has landed yet, or something
		// landed at the same moment as an unrelated change and this driver
		// will not guess which is ours. Either way the honest instruction is
		// the same as before.
		// Record what we left behind, so the caller has a way to finish this
		// rather than being told where the text is and left there.
		d.noteStranded(ref.ID, target.cwd, text)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: "text was delivered to the composer but did not render in time " +
				"to be confirmed landed, and no single new paste marker could be " +
				"attributed to this delivery alone; it may be sitting there unsent " +
				"— retry the same send with resumeIfStranded to submit it",
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
	// CONFIRM THE SUBMIT REGISTERED, by watching the composer empty OR by
	// watching this delivery's own attributed marker clear — see
	// confirmSubmitted for why the second path had to be added.
	//
	// Everything before this point is inference about whether a keystroke
	// would be received; this is evidence about whether it was. Without it a
	// dropped submit is indistinguishable from a delivered one — same receipt,
	// same silence — and the caller learns about it, if ever, from a session
	// that mysteriously never answers.
	//
	// Recorded as stranded so the resumeIfStranded path can finish it. That
	// path previously could not reach this class of failure at all: the record
	// was only written when the text failed to RENDER, so a submit that went
	// nowhere left nothing behind, the resume was refused for lack of a
	// record, and every later send was refused for a busy composer.
	if !d.confirmSubmitted(ctx, target.paneID, key, atCount) {
		d.noteStranded(ref.ID, target.cwd, text)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: "the text landed and was attributed to this delivery, and a submit was " +
				"issued, but this delivery's own block did not clear and the composer did " +
				"not empty — the submit did not register for it. It is sitting there " +
				"unsent; retry the same send with resumeIfStranded to submit it",
		}, nil
	}

	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeQueued,
		// Still queued, not submitted: an emptied composer says the SUBMIT
		// took, not that the agent consumed the input — and §4.3's
		// ConfirmsDelivery stays false for exactly that distinction. What the
		// confirmation buys is that this receipt no longer covers the case
		// where nothing was submitted at all.
		Reason: "text rendered in the composer and the submit registered (the composer " +
			"emptied); agent receipt is not observable on this substrate",
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
				"supply the composerDigest from a read as ?expect=<composerDigest> "+
				"(a query parameter, the same place startedAt goes — not a body field, "+
				"even though composerDigest is the name of a field IN the read response)",
			ErrAmbiguousTarget, len(pending))
	}
	if got := screenDigest(pending); got != expectDigest {
		return fleet.Ack{}, fmt.Errorf(
			"%w: the composer holds different text than the caller saw "+
				"(expected digest %s, found %s) — somebody may be typing right now",
			ErrAmbiguousTarget, expectDigest, got)
	}

	// C-u clears the line the cursor sits on. Measured on a live session
	// rather than assumed: C-a C-k and Escape were tried too, and C-u alone
	// is enough — for a single line.
	//
	// It is readline's unix-line-discard, which kills from the cursor back
	// to the start of the CURRENT line, not the whole buffer. A composer
	// holding one short line empties in one press, which is what the
	// original measurement above confirmed and all this code's tests
	// against it were exercising. A composer spanning several lines does
	// not: one press clears the line the cursor is on and leaves every
	// line above it standing, so a single un-repeated press against a
	// multi-line paste (issue #32: ~6.6 KB, roughly four visual lines) can
	// only ever get partway there.
	//
	// So this presses the same key again on every iteration the composer
	// is still non-empty — the same thing an operator clearing a stuck
	// multi-line prompt by hand would do — walking it backward one line at
	// a time until nothing is left or the window runs out. Verification
	// stays in the loop for the reason it was already there: a keypress
	// that did not register looks exactly like one that did, the same
	// reason send confirms before submitting.
	deadline := d.now().Add(promptClearWindow)
	left := pending
	for {
		if _, err := d.run(ctx, d.bin, "send-keys", "-t", live.paneID, "C-u"); err != nil {
			return fleet.Ack{}, fmt.Errorf("discard: %w", err)
		}
		if sc, ok := d.captureForClassify(ctx, live.paneID); ok {
			got, _ := composerText(sc)
			if got == "" {
				return fleet.Ack{Accepted: true}, nil
			}
			left = got
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return fleet.Ack{}, discardIncomplete(pending, left)
		}
		select {
		case <-ctx.Done():
			return fleet.Ack{}, ctx.Err()
		case <-time.After(promptClearInterval):
		}
	}
}

// discardIncomplete reports a clear that ran out of time without ever
// emptying the composer, and says which of two situations that is — because
// "still not empty" covers two outcomes a caller must treat oppositely, and
// nothing distinguished them before this existed.
//
// # The two outcomes, and why they cannot share a message
//
// Unchanged means the keystroke never registered at all: the composer reads
// exactly what the caller already corroborated (before, the digest-verified
// pending text). Nothing was destroyed, so retrying Discard with the same
// digest is exactly as safe as the first attempt was.
//
// Changed-but-nonempty means it registered PARTIALLY: some of the text is
// gone, none of it cleanly, and the composer now holds neither what the
// caller saw nor nothing — the worst of the three possible outcomes,
// because a caller told only "not cleared" cannot tell it apart from either
// of the other two. Retrying blind is actively dangerous here: the digest
// this failure leaves behind no longer matches what the caller read, so a
// retry against the old digest will be refused as stale (§5.4) anyway, and
// a caller that worked around that by re-reading and retrying without
// looking at what it now read could clear further into a corrupted message
// instead of stopping to ask a human.
//
// # Why both still map to conflict, not invalid
//
// Both are wrapped in ErrAmbiguousTarget, which the service maps to 409
// (conflict), not 400 (invalid) — see writeDriverError. The request was
// well formed; what failed is that the driver could not carry it out, which
// is not a caller mistake to fix by resending the same bytes. §5.4's kind
// already exists for "well-formed request, state the driver cannot
// corroborate" for the PRE-condition (the digest disagreeing before
// acting); this is the identical shape of problem at the POST-condition
// (the result disagreeing after acting), so it reuses the same kind rather
// than inventing a new one the wire's closed error-kind set does not have
// a slot for.
//
// # Why this is a message, not a field
//
// WaitingReason exists as a structured field because waiting_input started
// meaning two things that demand OPPOSITE automated handling — a prompt
// wants an answer, unsent input must not be sent to. This does not: in
// both cases here the correct caller action is identical — do not retry
// blind, re-read the composer before deciding anything — so a discriminator
// nothing would ever branch on would be dead weight. It is also not this
// file's convention: every other ErrAmbiguousTarget case in this driver
// (Close, Rename, and the two above) differentiates by message text alone,
// and a caller of this API already gets the general instruction for
// conflict — re-read and decide — from api-http.md §2.
func discardIncomplete(before, after string) error {
	if after == before {
		return fmt.Errorf(
			"%w: discard: the clear keystroke did not register; the composer is "+
				"unchanged from what was read, so retrying with the same digest is safe",
			ErrAmbiguousTarget)
	}
	return fmt.Errorf(
		"%w: discard: the clear keystroke ran but did not finish; the composer now "+
			"holds neither the original text nor nothing — it is damaged, not merely "+
			"unclear, so re-read it before doing anything else rather than retrying blind",
		ErrAmbiguousTarget)
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
	// #11: a destroyed session's composer is gone with it, so any stranded
	// record for this id has nothing left to resume into. Forgetting it here
	// is the proactive half — strandedMatches's cwd check and
	// strandedRetention are the backstop for every path that isn't an
	// explicit Close, but there is no reason to leave this one waiting out
	// its window when Close already knows it is dead.
	d.forgetStranded(ref.ID)
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
//
// # This never refuses because the account is refusing work (#10)
//
// Deliberately absent: a check against quotaBlock before starting the
// multiplexer session. A create must not be silently refused because the
// account behind the machine is refusing work — the caller is told and
// decides, joining `discard`-without-digest or `respond`-without-prompt
// would report as a well-formed create that "cannot succeed", when in fact
// the session it names is perfectly real and will exist to try again the
// moment the account does not refuse.
//
// The report itself does not happen here, and does not need to: the caller
// learns it from the state this call's own SessionRef reads back as, which
// is quotaBlockedState applied to whatever State (or a subsequent List)
// returns for it. That is deliberate, not an oversight — a session created
// while blocked starts out `starting`, which quotaBlockedState treats as
// silence about the account rather than evidence against it, the same as
// `idle` and `unknown` already were. See quotaBlockedState for the shared
// rule and Driver.State for where it is applied to the read Create's own
// HTTP handler makes to build a 201 response.
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
	if err := validateEnv(spec.Env); err != nil {
		return fleet.SessionRef{}, fmt.Errorf("create: %w", err)
	}
	// Refuse rather than start a session missing what the caller asked for. The
	// bare-exec shape has no shell to apply an environment file in, so a create
	// carrying variables cannot be honoured there — and a session that comes up
	// without the identity its supervisor gave it looks perfectly healthy and
	// fails later, somewhere else, which is the failure mode this whole area
	// keeps producing.
	if len(spec.Env) > 0 && d.bareExec {
		return fleet.SessionRef{}, errors.New(
			"create: this driver is configured without the login-shell wrap, so it has " +
				"no out-of-band channel for env; refusing rather than starting a session without it")
	}
	if spec.PermissionMode != "" && spec.PermissionMode != fleet.PermissionModeBypass {
		return fleet.SessionRef{}, fmt.Errorf(
			"create: unknown permissionMode %q (this runtime has one: %q)",
			spec.PermissionMode, fleet.PermissionModeBypass)
	}
	if spec.Resume != "" && !safeArgvValue(spec.Resume) {
		return fleet.SessionRef{}, fmt.Errorf(
			"create: resume %q would be read as a flag by the agent, not as a conversation id",
			spec.Resume)
	}
	for _, k := range spec.Consents {
		if _, ok := consentableKinds[k]; !ok {
			return fleet.SessionRef{}, fmt.Errorf(
				"create: %q is not a consentable question — see the driver's note on "+
					"why some boot questions have no safe affirmative option", k)
		}
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
	envPath, err := d.stageEnv(spec.Env)
	if err != nil {
		_ = d.idem.release(key)
		return fleet.SessionRef{}, fmt.Errorf("create: staging env: %w", err)
	}
	recordPath := ""
	if !d.bareExec {
		recordPath = d.envRecordPath()
		argv = loginWrap(d.loginShell(), recordPath, envPath, argv)
	}

	args := append([]string{
		"new-session", "-d", "-s", name, "-c", string(spec.Cwd), "--",
	}, argv...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		// The create demonstrably failed, so the reservation describes
		// nothing. Releasing it keeps a retry from being answered with a
		// session that was never started.
		_ = d.idem.release(key)
		// And the staged file is now certain to have no reader. It may hold a
		// credential, so it goes now rather than at the sweep below.
		if envPath != "" {
			_ = os.Remove(envPath)
		}
		return fleet.SessionRef{}, fmt.Errorf("create: %w", err)
	}
	if envPath != "" {
		// The wrapper unlinks it the moment it has read it. This is the case
		// where it never does — the shell died, the agent binary was missing —
		// and a file of values must not outlive the session it was staged for.
		go d.sweepStagedEnv(envPath)
	}

	ref := fleet.SessionRef{Machine: d.machine, ID: name, Name: name}
	if err := d.idem.complete(key, ref); err != nil {
		return fleet.SessionRef{}, fmt.Errorf("create: recording result: %w", err)
	}

	if recordPath != "" {
		go d.captureEnvironment(name, recordPath)
	}
	if spec.Prompt != "" || spec.TrustCwd {
		go d.settleNewSession(req, ref, built)
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
	// A pin is data from a caller and lands in this argv beside real flags. A
	// value beginning with "-" is therefore not a value at all — the agent CLI
	// reads it as another flag, and a create grant becomes "run the agent with
	// arguments of my choosing". Nothing rejected these before; safeArgvValue
	// is applied to every caller-supplied element from here down.
	if spec.Resume != "" && safeArgvValue(string(spec.Resume)) {
		argv = append(argv, "--resume", spec.Resume)
	}
	// Nil means "whatever a first-class session gets", which on this
	// substrate is enabled — an unaware caller must not silently receive the
	// second-class shape. Only an explicit false opts out.
	if spec.RemoteControl == nil || *spec.RemoteControl {
		if spec.Name != "" {
			argv = append(argv, "--remote-control", spec.Name, "-n", spec.Name)
		}
	}
	if spec.Agent != "" && safeArgvValue(string(spec.Agent)) {
		argv = append(argv, "--agent", string(spec.Agent))
	}
	if spec.Model != "" && safeArgvValue(spec.Model) {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" && safeArgvValue(spec.Effort) {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.PermissionMode == fleet.PermissionModeBypass {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if contextFile != "" {
		argv = append(argv, "--append-system-prompt-file", contextFile)
	}
	return argv
}

// safeArgvValue reports whether a caller-supplied value may be passed as an
// argv element.
//
// It answers one question: can this be mistaken for a flag? A leading "-" is
// the whole hazard — the agent CLI would read it as an option rather than as
// the value of the option before it, so a `model` of "--dangerously-skip-
// permissions" starts a session nobody asked for. The rest of the character set
// is left alone on purpose: these values never traverse a shell (the argv is
// exec'd directly, and the login wrap binds it as positional parameters), so
// quoting hazards do not arise and a stricter filter would only reject
// legitimate names this driver has no business vetting.
//
// Empty is not safe either: an empty element would silently pair the flag with
// whatever follows it.
func safeArgvValue(v string) bool {
	return v != "" && !strings.HasPrefix(v, "-")
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

// settleNewSession carries a freshly created session from "the process is
// spawned" to "it is doing the work it was created for": it waits for the
// runtime to be ready and then sends §2.1's initial prompt, and — only when the
// caller asked for it — answers the folder-trust question the runtime puts in
// front of that.
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
//
// # A blocking question is waited THROUGH, not given up on
//
// This loop used to return the moment it saw any prompt, on the reasoning that
// answering one is a decision it does not hold. That reasoning is still right,
// and returning was still wrong: the two are separate acts. A human who answers
// the trust question ten seconds later gets a session that is ready, willing,
// and holding no work — because the instruction it was created with was
// discarded while the modal was up, and nothing anywhere records that it
// existed. Measured on a live fleet: a session parked on that question for two
// days, and the work it was spawned for nowhere.
//
// So a prompt this routine may not answer is now a reason to keep waiting for
// the rest of the window, not a reason to stop.
func (d *Driver) settleNewSession(req fleet.Request, ref fleet.SessionRef, spec fleet.SessionSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), promptDeliveryWindow)
	defer cancel()

	// One consent, spent once — per kind, because a session can meet more than
	// one boot question on the way up. A question re-read on the next poll,
	// because the keypress has not repainted yet, must not be answered twice:
	// the second digit lands in whatever screen replaced it.
	answered := map[fleet.PromptKind]bool{}
	for {
		if ctx.Err() != nil {
			return
		}
		ready, blocking := d.promptReadiness(ctx, ref.ID)
		// An unclassified screen may still be one this driver can identify —
		// not from what it says, but from what this driver did to produce it.
		// See acceptanceScreen.
		if blocking != nil && blocking.Kind == "" && acceptanceScreen(spec, blocking) {
			blocking.Kind = fleet.PromptBypassAcceptance
		}
		switch {
		case blocking != nil && spec.ConsentsTo(blocking.Kind) && !answered[blocking.Kind]:
			// The caller described this session in the create request — its
			// working directory, its permission mode — and consented, in the
			// same request, to the runtime's boot question about what it
			// described. The decision is the caller's; this is only its
			// execution, which is the line prompt.go draws when it says a
			// service that decided what to answer would have become a
			// supervisor.
			if choice, ok := affirmativeOption(blocking); ok {
				answered[blocking.Kind] = true
				_, _ = d.Respond(ctx, req, ref, fleet.Response{
					Choice: choice, Nonce: blocking.Nonce,
				})
			}
		case ready:
			if spec.Prompt != "" {
				d.deliverInitialPrompt(ctx, req, ref, spec.Prompt)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(promptPollInterval):
		}
	}
}

// deliverInitialPrompt sends §2.1's initial prompt and, unlike the single
// discarded call this replaced, does something with what Send tells it.
//
// # What this receipt could never do before
//
// Create already returned 201 by the time this runs, so nobody is waiting on
// this specific call the way an ordinary caller waits on Send's HTTP
// response. That is what made the old `_, _ = d.Send(...)` different from
// every other place this driver ignores nothing: it was not indifference to
// a receipt, it was the one receipt with no possible reader. #44 measured
// what that cost — 3 of 5 multi-line initial prompts stranded in a single
// run, all recovered by a human who happened to read the session's own
// state and call the resume path, none of the six ever told without being
// asked.
//
// # One retry, inside the window, through the path already proven correct
//
// #44's own measurement is why this is a retry and not a redesign: every
// strand in that run cleared on the very next attempt, seconds later, with
// nothing different but time — consistent with a busy machine's repaint
// racing the submit keystroke, worse as load grows, not a defect in the
// delivery logic. And a retry already has somewhere honest to go: Send's
// first attempt calls noteStranded on any unconfirmed outcome, exactly as it
// does for a caller-initiated send, so a second Send with ResumeIfStranded
// walks the same recovery path a human used by hand — the identical
// mechanism, not a parallel one built for this call site.
//
// A retry that silently succeeds would hide the one number #44 says matters:
// how often this needs to happen at all. counterInitialPromptRetried is
// incremented on every retry regardless of its outcome, so a machine
// clearing every strand on retry still shows up in the count — that rate is
// the load signal, not something to launder away by only counting failures.
//
// # What still reaches #11 unchanged, and what does not
//
// noteStranded's record lives in d.stranded, which #11 already tracks as
// in-memory and restart-fragile. This function adds no new writer to that
// map — both the first attempt and the retry go through the same Send this
// driver's other callers already use, so this call site is not a third
// writer of anything. What it DOES do is call Send up to twice where the
// discarded version called it once, so a machine under exactly the load
// pattern #44 measured now writes that entry twice as often on the losing
// side of the race before either clearing it (resumeIfStranded's own
// forgetStranded) or giving up. That is a real cost of retrying and is
// recorded here rather than left for #11 to discover on its own.
//
// # Why the second failure is a log line and a counter, not a new event kind
//
// events.go's EventKind is `api-http.md §4`'s closed, normative set, and this
// driver has no channel into the hub outside the subscription engine's own
// poll-diff loop (internal/service/events.go). Inventing a delivery-specific
// event here would mean a spec change and a new cross-package wire this
// function has no business owning. It is also not the only way a
// subscriber learns: the composer is, physically, still holding the text,
// so the very next classify of this pane reports `waiting_input` with
// `WaitingOn: unsent-input` — the same read #44 measured working 6 times out
// of 6 for detection — and the subscription engine already emits
// `session.state` the moment that status differs from what a live
// subscriber last saw, with no code added here. What was missing was never
// the detection path; it was that Create's caller has no reason to be
// looking. counterInitialPromptStranded and the log line exist for the
// caller who is not subscribed and never will be — an operator asking
// afterwards "did this happen, how often" — which is exactly the shape #9
// describes wanting and not having.
func (d *Driver) deliverInitialPrompt(ctx context.Context, req fleet.Request, ref fleet.SessionRef, prompt string) {
	receipt, err := d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true})
	if err != nil || receipt.Outcome != fleet.OutcomeUnknown {
		// Delivered and confirmed, refused outright (not this call's
		// problem to retry into), or a transport-level error Send itself
		// already wraps and named — none of those are the race this retry
		// exists for.
		return
	}

	d.counters.incr(counterInitialPromptRetried)
	retry, err := d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err == nil && retry.Outcome == fleet.OutcomeSubmitted {
		return
	}

	// Still sitting there after the one retry the measured pattern earns it.
	// The text and the record of it are exactly where an ordinary stranded
	// send leaves them (composer, d.stranded) — nothing here is lost, only
	// unannounced, which this closes.
	d.counters.incr(counterInitialPromptStranded)
	log.Printf("tmux: initial prompt still unsent after one retry session=%s machine=%s",
		ref.ID, d.machine)
}

// consentableKinds maps each boot question a caller may consent to onto the
// words that identify its affirmative option.
//
// # Why the resume chooser is absent, and must stay absent
//
// It is the obvious fourth entry and it has no safe answer. The other two ask a
// yes/no about something the caller DESCRIBED in its own create request — this
// directory, this permission mode — so "the option that agrees" is a fact about
// the screen. The resume chooser asks WHICH conversation to continue, and its
// options are summaries of somebody's prior sessions. Nothing in the option text
// identifies the one the caller named; a consent here would be a coin flip
// dressed as an agreement, and losing it resumes a stranger's work.
//
// A caller that wants it answered reads `state.prompt` and answers by index
// through `respond`, which is exactly the split prompt.go describes: this
// service says what is being asked, a supervisor decides what to answer.
//
// # PromptSettingsTrust is absent too, for a third reason
//
// Its affirmative agrees to an ADMINISTRATOR's managed-policy payload, not to
// anything the caller described in its own request — see prompt.go's doc
// comment on the kind. A consent here would let a session-creating caller
// accept a policy change on behalf of an operator who never saw it, which
// this layer is not in a position to speak for. Absence from this map is
// enough: the loop in Create refuses any Consents entry with no map key, the
// same mechanism that refuses PromptResumeChooser above.
var consentableKinds = map[fleet.PromptKind][]string{
	// "Yes, I trust this folder"
	fleet.PromptFolderTrust: {"trust", "folder"},
	// PromptBypassAcceptance is consentable but has NO entry here, because its
	// affirmative option is the generic "Yes, I accept" and this table is
	// applied to whatever screen happens to be on the pane. A needle that loose
	// would accept some future dialog nobody has seen. It is resolved by
	// provenance instead — see acceptanceScreen.
	fleet.PromptBypassAcceptance: nil,
}

// affirmativeOption picks the option that AGREES, by index.
//
// It reads the option text and nothing else, for the reason classifyPromptKind
// states at length: options are strings the runtime emits, while the question
// is written by the agent and is therefore injectable. And it does not fall
// back to the highlighted option — prompt.go's own example is two boot prompts
// with the same shape whose safe answer sits at different indices:
//
//	❯ 1. Yes, I trust this folder        ❯ 1. No, exit
//	  2. No, continue without these        2. Yes, I accept
//
// Exactly one option may match. Zero means this is not the screen we were told
// about; two means the wording has changed under us (a "No, I don't trust this
// folder" matches the same needles), and in both cases the honest move is to
// answer nothing and leave a question on screen for a human — the same direction
// §5.6 sends every other unreadable case.
// acceptanceScreen reports whether an unclassified boot screen is the
// permission-mode acceptance one, using evidence the screen cannot supply.
//
// # Identify it by what we did, not by what it says
//
// The classifier cannot name this screen: its identifying words are in the
// question, its options are the generic "Yes, I accept" / "No, exit", and
// reading questions is the thing option-matching exists to avoid. Loosening the
// classifier to match "accept" would put this kind on screens nobody has seen,
// in a package whose kinds are used by clients to decide what to auto-answer.
//
// But this driver is not a bystander here. It PASSED the flag that raises this
// screen, moments ago, to this session. That is provenance, it is unavailable
// to the classifier, and it is far stronger evidence than any wording: an agent
// can print any sentence it likes into its own prompt, and cannot cause the
// driver to have started it in a mode it was never asked for.
//
// Three conditions, all required:
//
//   - the caller asked for the mode whose acceptance screen this is;
//   - the caller consented to this kind in the same request;
//   - the screen is a two-option accept/decline pair, matched on OPTION text
//     only, which is the material the invariant does allow.
//
// The last one is what keeps a coincidence from being read as agreement. It
// deliberately does not identify the screen on its own — only in the presence
// of the first two, which no agent-authored prompt can manufacture.
func acceptanceScreen(spec fleet.SessionSpec, p *fleet.SessionPrompt) bool {
	if spec.PermissionMode != fleet.PermissionModeBypass {
		return false
	}
	if !spec.ConsentsTo(fleet.PromptBypassAcceptance) {
		return false
	}
	if p == nil || len(p.Options) != 2 {
		return false
	}
	accepts, declines := 0, 0
	for _, o := range p.Options {
		lower := strings.ToLower(o)
		if strings.Contains(lower, "accept") {
			accepts++
		}
		if strings.Contains(lower, "exit") || strings.Contains(lower, "no,") {
			declines++
		}
	}
	return accepts == 1 && declines == 1
}

func affirmativeOption(p *fleet.SessionPrompt) (int, bool) {
	if p == nil {
		return 0, false
	}
	needles, ok := consentableKinds[p.Kind]
	if !ok {
		return 0, false
	}
	// A consentable kind with no needles is one identified by provenance rather
	// than by wording (see acceptanceScreen). Its affirmative is the option that
	// accepts — the same options-only material, applied to a screen we already
	// have independent grounds to believe we are looking at.
	if needles == nil {
		needles = []string{"accept"}
	}
	found := 0
	for i, o := range p.Options {
		lower := strings.ToLower(o)
		// No "does it start with no" refinement. That is guessing at wording
		// in order to keep answering a screen we have just been told we can no
		// longer read — the ambiguity below is the answer, not an obstacle.
		all := true
		for _, n := range needles {
			if !strings.Contains(lower, n) {
				all = false
				break
			}
		}
		if all {
			if found != 0 {
				return 0, false
			}
			found = i + 1
		}
	}
	return found, found != 0
}

// promptReadiness reports whether the session can receive input, and the prompt
// it is blocked on instead when it cannot.
//
// The prompt is returned rather than a bare "blocked" boolean because the two
// callers of that fact need different things from it: one only has to keep
// waiting, and one has to decide whether this is the question its caller
// consented to answer. A boolean can only carry the first.
func (d *Driver) promptReadiness(ctx context.Context, id string) (ready bool, blocking *fleet.SessionPrompt) {
	callCtx, cancel := d.bounded(ctx)
	defer cancel()
	rows, captures, err := d.enumerate(callCtx)
	if err != nil {
		return false, nil
	}
	for _, r := range rows {
		if r.session != id {
			continue
		}
		sc := newScreen(captures[r.paneID])
		if p := parsePrompt(sc); p != nil {
			p.Kind = classifyPromptKind(p)
			return false, p
		}
		text, found := composerText(sc)
		// Ready means the composer exists and is empty: the interface has
		// painted, and nothing is already sitting in it.
		return found && text == "", nil
	}
	return false, nil
}

// confirmLanded waits until the composer actually shows the delivered text,
// and reports WHICH collapsed-paste block — if any — belongs to this
// delivery specifically.
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
//
// `before` is the composer's marker state captured immediately prior to this
// delivery's own paste (see Send). A marker that was already there is
// somebody else's — an earlier stranding this driver or a human left behind
// — and it must never count as evidence for a delivery it has nothing to do
// with. Any marker satisfied the check this replaced, which is exactly the
// defect being fixed: a send into a composer already holding residue
// confirmed against that residue, and the submit check that followed it
// could then never pass, because the residue this delivery never touched
// never leaves. Measured on a live session: one ~30-line instruction
// produced two consecutive false negatives while the agent had received
// exactly one clean copy and was acting on it, the composer meanwhile
// holding two stacked placeholders.
func (d *Driver) confirmLanded(ctx context.Context, paneID, text string, before map[pasteKey]int) (pasteKey, int, bool) {
	needle := strings.TrimSpace(text)
	if needle == "" {
		return pasteKey{}, 0, true
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
	// composer accepted a paste — so it is accepted as such, but ONLY the
	// marker `gained` attributes to this delivery: the one whose count rose
	// relative to `before`. Two markers rising at once means something other
	// than this delivery also wrote to the composer in the same window, and
	// `gained` fails to no attribution rather than guessing between them —
	// this loop keeps polling on that ambiguity exactly as it would on
	// silence, because more evidence may yet resolve it before the deadline.
	deadline := d.now().Add(submitConfirmWindow)
	for {
		// NOT classifyCaptureArgs, and deliberately so: this is the one
		// capture in the driver that does not feed the classifier. It does a
		// substring match against text this driver just pasted, strips the
		// attributes itself, and wants -J so a wrapped paste matches as one
		// line. Attributes would be discarded a line below regardless, so the
		// escape-carrying shape would buy nothing here. `before` was captured
		// with this same shape, in paintedMarkers, so the two readings are
		// comparable.
		out, err := d.run(ctx, d.bin, "capture-pane", "-p", "-J", "-t", paneID, "-S", "-6")
		if err == nil {
			painted := stripEscapes(string(out))
			if strings.Contains(painted, needle) {
				return pasteKey{}, 0, true
			}
			after := markerCounts(painted)
			if key, ok := gained(before, after); ok {
				return key, after[key], true
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return pasteKey{}, 0, false
		}
		select {
		case <-ctx.Done():
			return pasteKey{}, 0, false
		case <-time.After(submitConfirmInterval):
		}
	}
}

// pasteKey identifies one collapsed-paste summary well enough to tell it from
// the one before it.
//
// Index is the runtime's own counter ("#10"), which is monotonic within a
// session and is therefore the strong identifier. It is optional: a runtime
// that prints only a line count still gets attribution, because a SECOND block
// of the same size is a second entry under the same key and the counting below
// notices the difference. What is not optional is the line count — it is the
// only thing the summary exists to say.
type pasteKey struct{ index, lines int }

// markerCounts is the composer's paste state: how many collapsed blocks it
// shows, of which shapes.
//
// # Why a count of shapes rather than "is a marker present"
//
// A marker says "this composer holds a pasted block". It does NOT say the block
// is the one just delivered, and both confirmations used to read it as if it
// did. Any earlier stranded paste satisfies the same test, so a send into a
// composer with residue confirmed against somebody else's leftovers, and the
// submit check — which watched for the composer to EMPTY — could then never
// pass, because the residue never leaves.
//
// The compounding is what made it urgent. The receipt's own advice on failure
// is "retry with resumeIfStranded", the retry pastes again, the composer is
// less empty than before, and the next receipt is wrong for the same reason:
// the state degrades because the caller did what the receipt told it to.
// Measured on a live session, one ~30-line instruction produced two consecutive
// false negatives while the agent had received exactly one clean copy and was
// acting on it, with the composer holding two placeholders.
//
// So the unit of evidence is the CHANGE in this map across a delivery, never
// the presence of a marker.
func markerCounts(painted string) map[pasteKey]int {
	out := map[pasteKey]int{}
	for _, line := range strings.Split(painted, "\n") {
		if !strings.Contains(line, composerRuneMarker) {
			continue
		}
		rest := line[strings.Index(line, composerRuneMarker):]
		for {
			open := strings.Index(rest, "[")
			if open < 0 {
				break
			}
			shut := strings.Index(rest[open:], "]")
			if shut < 0 {
				break
			}
			inside := rest[open+1 : open+shut]
			rest = rest[open+shut:]
			lower := strings.ToLower(inside)
			if !strings.Contains(lower, "line") || !strings.ContainsAny(lower, "0123456789") {
				continue
			}
			out[pasteKey{index: numberAfter(inside, '#'), lines: numberAfter(inside, '+')}]++
		}
	}
	return out
}

// numberAfter reads the run of digits following a marker rune, or 0 when the
// rune is absent. A runtime that stops printing "#" loses the strong
// identifier and keeps the weak one; it does not lose attribution entirely.
func numberAfter(s string, marker byte) int {
	i := strings.IndexByte(s, marker)
	if i < 0 {
		return 0
	}
	n, digits := 0, 0
	for j := i + 1; j < len(s) && s[j] >= '0' && s[j] <= '9'; j++ {
		n = n*10 + int(s[j]-'0')
		digits++
		if digits > 6 {
			return 0 // not a count; this pane is attacker-influenced text
		}
	}
	if digits == 0 {
		return 0
	}
	return n
}

// gained reports the key whose count went UP between two readings — the block
// this delivery is responsible for.
//
// Ambiguity fails to "no attribution" rather than to a guess: if two keys grew,
// something other than this delivery also wrote to the composer, and claiming
// either would be the same unfounded confidence this whole change removes.
func gained(before, after map[pasteKey]int) (pasteKey, bool) {
	var found pasteKey
	hits := 0
	for k, n := range after {
		if n > before[k] {
			found = k
			hits++
		}
	}
	return found, hits == 1
}

// composerHoldsCollapsedPaste reports whether the composer line shows the
// runtime's summary of a pasted block rather than the pasted text itself.
//
// Kept for the readers that legitimately want "is there any pasted block here"
// — the §2.4 refusal cares about that, because residue is still text a send
// would concatenate with. Confirmation of a DELIVERY must not use it; that is
// what markerCounts and gained are for.
func composerHoldsCollapsedPaste(painted string) bool {
	return len(markerCounts(painted)) > 0
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

// credentialGeneration reads the local credential store's own modification
// time — the "generation" identifier #12 needs — or nil when unconfigured
// or the stat fails.
//
// One stat call, every read. No lock and no field on Driver remembers a
// value across calls, unlike quotaBlock: a file's mtime does not evaporate
// the way a scrolled-away screen notice does, so this driver's own memory
// would only be a second, potentially stale copy of what one syscall
// already answers fresh. The finding this exists to surface was itself made
// this way, from outside the process entirely — this is that same cheap
// signal, already within reach, not a new detector.
//
// It answers "which generation is in force right now", never "is a session
// still bound to it" — see SessionState.CredentialGeneration for the
// distinction this return value must not blur.
func (d *Driver) credentialGeneration() *fleet.Timestamp {
	if d.credentialPath == "" {
		return nil
	}
	info, err := os.Stat(d.credentialPath)
	if err != nil {
		return nil
	}
	t := info.ModTime()
	return &t
}

// quotaBlockedState rewrites st to quota_blocked when q is non-nil and st's
// own status is silence about the account rather than evidence about it —
// generalising List's per-session rewrite so State (a single-session read)
// applies the identical rule (#10).
//
// Three statuses qualify, and the third is the one List never needed to
// consider on its own. idle and unknown are exactly List's original two —
// see its own comment for why. starting is idle's condition at the earliest
// possible moment: a session this driver just spawned, for an account
// already known to be refusing work, paints nothing yet — and "starting" is
// what a caller reads for it. That caller is very often the one who just
// called Create, reading the state embedded in its 201 response (built from
// this exact function, by way of Driver.State) — the response #10 exists to
// keep honest. Swallowing the account fact behind "starting" until the next
// poll notices "idle" would report an unqualified success, which is exactly
// what #10 says a create must not do.
//
// Nothing else is rewritten, unchanged from List: working, waiting_input and
// unsent text each carry something OBSERVED just now, and a remembered fact
// must not overwrite an observation.
func quotaBlockedState(st fleet.SessionState, q *fleet.QuotaBlock) fleet.SessionState {
	if q == nil {
		return st
	}
	var seen string
	switch st.Status {
	case fleet.StatusIdle:
		seen = "the session itself looks idle"
	case fleet.StatusUnknown:
		seen = "the session's own screen was inconclusive"
	case fleet.StatusStarting:
		seen = "the session had not yet painted anything of its own"
	default:
		return st
	}
	st.Status = fleet.StatusQuotaBlocked
	st.Quota = q
	st.Evidence = "this machine's account is refusing work; " + seen
	if q.ResetHint != "" {
		st.Evidence += " (reported reset: " + q.ResetHint + ")"
	}
	return st
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

// strandedRecord is what noteStranded persists: the text this driver
// delivered and could not confirm, and enough beside it (§5.4) to tell a
// live session from one that merely recycled the same id.
type strandedRecord struct {
	Text string    `json:"text"`
	Cwd  string    `json:"cwd"`
	At   time.Time `json:"at"`
}

// strandedFile is the durable document, one entry per session with a
// delivery still unfinished. Its own file (state.go: one JSON document per
// concern), never folded into idempotency.json — a create key and a
// delivery-in-progress are different concerns with different shapes.
type strandedFile struct {
	Records map[string]strandedRecord `json:"records"`
}

const strandedFileName = "stranded"

// noteStranded records text this driver delivered and could not confirm.
//
// The record is what a resume corroborates against. It is deliberately OUR
// account of what we did, not a reading of the screen: the screen cannot show
// a long message back (F49), and composer contents are not evidence anyone
// meant to send them.
//
// cwd travels with it (#11): once this record survives a restart, an id
// match alone is the exact thing §5.4 forbids trusting for a
// resuming-or-destructive operation, and this is both.
func (d *Driver) noteStranded(id, cwd, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stranded == nil {
		d.stranded = map[string]strandedRecord{}
	}
	d.stranded[id] = strandedRecord{Text: text, Cwd: cwd, At: d.now()}
	d.saveStrandedLocked()
}

// strandedMatches reports whether text is exactly what this driver left in
// that session's composer, in the session the record was made for.
//
// Exact text, not prefix: "resume the delivery I made" is a different
// request from "submit something that starts the same way", and only the
// first is the one the caller is owed. cwd is required too (§5.4) — a
// durable record can outlive the session it describes, and an id is
// recyclable; matching on id and text alone would resume into whatever
// unrelated session later reused that name. A record older than
// strandedRetention is treated as absent, the same reasoning
// idemStore.sweepLocked applies to an expired idempotency key: kept
// forever, it stops being evidence about anything in particular.
func (d *Driver) strandedMatches(id, cwd, text string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepStrandedLocked()
	prior, ok := d.stranded[id]
	return ok && prior.Text == text && prior.Cwd == cwd
}

func (d *Driver) forgetStranded(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.stranded, id)
	d.saveStrandedLocked()
}

// sweepStrandedLocked drops records older than strandedRetention. Caller
// holds d.mu. Cheap and unconditional: the map is at most one entry per
// session with a delivery genuinely in flight, never a growing log.
func (d *Driver) sweepStrandedLocked() {
	if len(d.stranded) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.stranded {
		if now.Sub(rec.At) > strandedRetention {
			delete(d.stranded, id)
		}
	}
}

func (d *Driver) saveStrandedLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(strandedFileName, strandedFile{Records: d.stranded})
}

// loadStranded restores stranded-delivery records at startup, sweeping
// anything already past strandedRetention — the same "sweep on load" shape
// idemStore uses, so a record that expired while the service was down does
// not get a fresh window just for having survived to be read.
func (d *Driver) loadStranded() {
	if d.store == nil {
		return
	}
	var f strandedFile
	found, err := d.store.Load(strandedFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stranded = f.Records
	d.sweepStrandedLocked()
	d.saveStrandedLocked()
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
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			if now := parsePrompt(sc); now == nil || now.Nonce != was {
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

// receptive reports whether a pane's runtime is in a state where a keystroke
// will be received, by looking for the COMPOSER.
//
// # Why the composer is the signal, and not a timer
//
// The composer is the runtime's input widget. It is painted by the same
// component that reads keys, and it cannot appear on screen before that
// component is running — so its presence is evidence about the input path
// itself rather than about elapsed time. A clock proves nothing: the same
// wall-clock delay is comfortable on an idle machine and far too short on a
// loaded one, after a cold boot, or when the startup files a created session
// now reads are slow. That is the same class of assumption the login-shell
// work already had to stop making.
//
// # What it does NOT prove, said plainly
//
// A painted composer is strong evidence, not proof, that a keystroke will be
// consumed: the widget could in principle be drawn a moment before the input
// loop attaches. This is why it is only half the mechanism. The other half is
// confirmSubmitted, which checks AFTERWARDS whether the submit actually took —
// positive evidence rather than inference. The gate removes the common case;
// the confirmation makes the remaining case honest instead of silent.
//
// blocked reports a selection menu, which is receptive to keys but not to
// TEXT: pasting into one drives the menu instead of delivering a message.
func (d *Driver) receptive(ctx context.Context, paneID string) (ready, blocked bool) {
	sc, ok := d.captureForClassify(ctx, paneID)
	if !ok {
		// Fail closed: an unreadable pane is not a receptive one. Returning
		// "ready" here on the theory that the capture is probably fine is how
		// this defect is reintroduced.
		return false, false
	}
	if _, b := selectionPrompt(sc); b {
		return false, true
	}
	_, found := composerText(sc)
	return found, false
}

// awaitReceptive waits, within the call's own budget, for the runtime to be
// able to receive input.
//
// It does NOT wait for startup. §4.4 bounds every call by the driver's
// declared deadline, and a runtime takes far longer to paint than that
// deadline allows — so a Send that blocked until a freshly created session was
// ready would be a driver overrunning its own declaration. What this covers is
// the short race, and beyond that it reports not-ready so the caller is told
// rather than silently stranded.
//
// settleNewSession keeps its own, much longer readiness loop for the one
// case where waiting out a full startup is the point.
func (d *Driver) awaitReceptive(ctx context.Context, paneID string) (ready, blocked bool) {
	deadline := d.now().Add(sendReceptiveWindow)
	for {
		ready, blocked = d.receptive(ctx, paneID)
		if ready || blocked {
			return ready, blocked
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return false, false
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(sendReceptiveInterval):
		}
	}
}

// confirmSubmitted reports whether the submit registered, attributed to the
// delivery confirmLanded identified — `key` is its marker, `atCount` the
// count `confirmLanded` observed for that marker at the moment it confirmed
// landing.
//
// This is the fail-closed half. Everything before it is inference about
// whether the runtime would accept a keystroke; this is evidence about whether
// it did.
//
// Two independent kinds of evidence, because residue changes what "the
// composer emptied" can mean:
//
//   - the composer is fully empty. Unambiguous, and it works whether or not
//     this delivery ever produced a marker of its own — a wholly dim
//     composer counts as empty too, because that is the runtime's own
//     placeholder hint drawn into an EMPTY box, and reading it as leftover
//     text is the mistake that produced a round of false evidence on this
//     repo's tracker.
//   - the attributed key's count has fallen below `atCount`. Evidence that
//     THIS block left, even while some other block — somebody else's
//     residue, present before this delivery ever started — still occupies
//     the composer line and keeps it from ever reading empty. This is the
//     branch that did not exist before: the composer was watched for
//     emptying as the ONLY signal, so a submit that plainly registered still
//     reported unknown whenever residue happened to share the line. Measured
//     on a live session, twice consecutively, while the agent had already
//     received and was acting on the text.
//
// `atCount == 0` means confirmLanded attributed no marker at all — the
// literal single-line case — and the second branch can never fire for it:
// counts do not go negative. That degrades to exactly the old "composer
// emptied" check, which was always right for that case: a literal delivery
// leaves nothing else on the composer line to be confused with.
func (d *Driver) confirmSubmitted(ctx context.Context, paneID string, key pasteKey, atCount int) bool {
	deadline := d.now().Add(submitConfirmWindow)
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			if text, found := composerText(sc); found && text == "" {
				return true
			}
		}
		if atCount > 0 && d.paintedMarkers(ctx, paneID)[key] < atCount {
			return true
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

// paintedMarkers captures a pane in the shape confirmLanded's own capture
// uses — `-J`, so a marker that wraps at the runtime's column width still
// joins onto one line — and returns its collapsed-paste marker counts.
//
// Not captureForClassify's shape: that one deliberately leaves continuation
// lines unjoined, because the classifier parses wrapping itself. Marker
// attribution needs the opposite — a bracket split across two physical lines
// by a mid-word wrap is a bracket markerCounts cannot close, and it would
// silently drop that block from the count rather than fail loudly. Sharing
// this shape with confirmLanded is what keeps a "before" and an "after"
// reading comparable at all.
func (d *Driver) paintedMarkers(ctx context.Context, paneID string) map[pasteKey]int {
	out, err := d.run(ctx, d.bin, "capture-pane", "-p", "-J", "-t", paneID, "-S", "-6")
	if err != nil {
		return map[pasteKey]int{}
	}
	return markerCounts(stripEscapes(string(out)))
}

// captureForClassify reads a pane in THE shape the classifier requires.
//
// # Why this exists as a function rather than as a convention
//
// Every screen the classifier parses must carry escape sequences. `newScreen`
// keeps a `raw` copy of each line for one reason, stated on the type: the
// composer's placeholder is distinguished from text a human typed by DIMNESS
// ALONE. There is no wording difference to fall back on — the hint is ordinary
// prose — so a capture without `-e` cannot express the difference at all, and
// `composerText` reports the placeholder as unsent input.
//
// That is not a cosmetic error. A composer believed to hold unsent input makes
// `Send` refuse, so an idle session showing a hint becomes UNREACHABLE through
// the API, and the refusal blames an operator who does not exist. Three
// separate sites shipped with the flag missing, each added in good faith,
// because the flag is easy to omit and nothing objected.
//
// So the shape is owned here instead of being repeated. Callers ask for "a
// screen for classification" and cannot express a wrong one.
//
// # The shape, and why these flags
//
//	-p  write to stdout rather than a buffer.
//	-e  KEEP escape sequences. The whole point; see above.
//	-S  start N lines back, so the classifier sees the tail it reasons about.
//
// `-J` is deliberately ABSENT. It joins wrapped lines, and the classifier
// already handles continuation lines itself — a long message wrapping below
// the prompt is expected and parsed. More to the point, this is the shape the
// batched enumeration has always used, which is the path every status in this
// fleet has been read through; adopting a different one here would change what
// the classifier sees everywhere, on no evidence.
// classifyCaptureArgs is THE argv shape, and the only place it is written.
//
// Separate from captureForClassify because the batched enumeration cannot call
// that helper: it packs many captures into ONE subprocess invocation, which is
// the constant-spawn property its own doc comment calls load-bearing. Sharing
// the argv rather than the function is what keeps the two paths from drifting,
// which is the drift this whole issue is about.
func classifyCaptureArgs(paneID string, lines int) []string {
	return []string{"capture-pane", "-p", "-e", "-t", paneID, "-S", "-" + strconv.Itoa(lines)}
}

func (d *Driver) captureForClassify(ctx context.Context, paneID string) (screen, bool) {
	out, err := d.run(ctx, d.bin, classifyCaptureArgs(paneID, d.captureLines)...)
	if err != nil {
		// Fail closed, and let the caller decide what that means. An
		// unreadable pane is not an empty one.
		return screen{}, false
	}
	return newScreen(string(out)), true
}
