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
	"github.com/godx-jp/colab-fleet/internal/trustseed"
)

const (
	// DefaultRuntime is the runtime id this driver reports. It names both
	// halves deliberately: the multiplexer supplies the session substrate,
	// the CLI supplies the agent, and neither alone identifies what a
	// req is talking to.
	DefaultRuntime = fleet.RuntimeId("claude-code-tmux")

	// defaultDeadlineMs bounds any single call (§4.4). Local subprocess
	// work measured in single-digit milliseconds; the original 5000 was
	// three orders of magnitude of headroom on that basis, chosen so that a
	// genuinely wedged multiplexer surfaces as unreachable in bounded time
	// rather than never.
	//
	// Raised to 30000 for colab-fleet#129: Discard's clear loop is now
	// content-derived (see clearPressMargin, maxClearPresses below) rather
	// than clock-bound, and a composer sized like #129's own field case
	// (~80 rows) legitimately needs on the order of maxClearPresses presses
	// at promptClearInterval apart — arithmetic that no longer fits under
	// the original bound at all, regardless of what "wedged" means. §4.4
	// ties the declared deadline to the whole driver, not to one verb, so
	// this is the one number every operation's "wedged" detection now
	// shares; a subprocess that is actually hung is still caught, just with
	// more slack than before, which is the correct trade once one verb's
	// legitimate worst case is this much larger than the rest.
	defaultDeadlineMs = 30000

	// defaultCaptureLines is how much scrollback the classifier receives
	// per session. The classifier reads only the tail, but the tail must
	// be tall enough to contain the composer fence plus the status line
	// above it.
	defaultCaptureLines = 24

	// promptClearWindow bounds the wait for an answered prompt to disappear.
	promptClearWindow   = 3 * time.Second
	promptClearInterval = 200 * time.Millisecond

	// stallPresses bounds how many CONSECUTIVE identical captures Discard's
	// clear loop tolerates once it has already seen the composer move at
	// least once (#87). Before any movement, "unchanged" is not yet
	// evidence of anything — a pane that has simply not redrawn — so the
	// loop still spends its whole content-derived press budget there (see
	// clearPressMargin, maxClearPresses below; colab-fleet#129 replaced
	// what used to be a flat clock here). AFTER movement, an unchanged
	// capture is a press that did nothing, and repeating it for the rest of
	// the budget is not buying more evidence, it is more destructive
	// keystrokes aimed at text nobody has re-read. Three is enough to
	// distinguish "stopped" from "one slow repaint" without costing the
	// caller most of the budget finding out.
	stallPresses = 3

	// clearPressMargin is how many presses beyond composerVisualLines'
	// count a clear pass is given before the composer having never moved at
	// all counts as evidence rather than as "the pane has not repainted
	// yet" (colab-fleet#129). composerVisualLines is a count of what is
	// ON SCREEN right now, not a guarantee that C-u maps onto it one for
	// one — this margin is the acknowledgment that the mapping is measured,
	// not proven exact (see composerVisualLines' own doc comment), without
	// being large enough to matter for how long a genuinely stuck composer
	// takes to be reported as such.
	clearPressMargin = 5

	// maxClearPresses is the hard ceiling on how many C-u presses one clear
	// pass will ever spend, regardless of how large composerVisualLines
	// reports the composer to be. #129 is explicit that a human paste is
	// not bounded by what the API's own input cap would allow, so an
	// expectation derived from content has no natural ceiling of its own —
	// this is the bound this driver still needs regardless (§4.4: "a
	// driver that can block without a bound is a specification violation").
	// Sized comfortably above #129's own field case (~80 rows) so that case
	// is not the thing this limits; a composer that genuinely exceeds it
	// still gets an honest "made progress, ran out of budget" report
	// (discardIncomplete's damaged branch) rather than either a silent
	// truncation or an unbounded loop — the same outcome a composer that
	// stalls for any other reason already produces, so a caller does not
	// need to know which of the two happened to react correctly: re-read
	// and, if there is more to clear, ask again.
	maxClearPresses = 120

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

	// promptPollInterval is how often readiness is checked. This is not the
	// polling §5.5 forbids: that rule is about callers learning of state
	// changes, and this is one driver waiting for a process it just started.
	promptPollInterval = 1500 * time.Millisecond
	// sessionGoneConfirmations is how many CONSECUTIVE polls must find a
	// session absent from the enumeration before settleNewSession treats it
	// as gone rather than as a listing race. A single miss is not enough
	// evidence on its own — "a pane can vanish between listing and capture"
	// is already a documented, transient shape elsewhere in this file — so
	// this asks for two in a row (colab-fleet #125's own bound: the session's
	// own lifetime, not a guessed duration) before giving up on delivery.
	sessionGoneConfirmations = 2

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

	// futileClearRetention is how long a "this exact residue would not
	// move" record (#87) is trusted. Same reasoning as strandedRetention:
	// kept forever it stops being evidence about a composer the caller can
	// still see, since anything could have happened to the pane by then.
	futileClearRetention = 30 * time.Minute

	// deliveryMarkRetention is how long a delivery mark (#111) is trusted as
	// the denominator for `turns`. Deliberately much longer than
	// strandedRetention: a stranded-delivery record stops being useful once
	// the delivery it describes could no longer plausibly still be pending,
	// but `turns` is useful for the ENTIRE life of a dispatched worker — a
	// caller may reasonably check back hours later. Swept the same way, on
	// the same "kept forever it stops being evidence about anything in
	// particular" reasoning, just on a longer clock.
	deliveryMarkRetention = 24 * time.Hour
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
	// quotaSinceObserved is true when quota.Since came from the runtime's
	// own record of the refusal (#56) rather than from this driver's first
	// sighting of the notice on screen. Not part of fleet.QuotaBlock's wire
	// shape — §2.3 documents Since as a timestamp for humans and callers to
	// read, not a provenance channel, and the honest label belongs in
	// SessionState.Evidence (quotaBlockedState) the same way every other
	// "how do we know this" note in this driver already lives in prose
	// rather than in a new structured field.
	quotaSinceObserved bool

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

	// delivered remembers, per session, the most recent delivery THIS DRIVER
	// made into that session's composer — the denominator colab-fleet #111's
	// `turns` is counted relative to. Written once, at the moment Send's own
	// paste-buffer call succeeds (before Submit is even checked), so every
	// downstream outcome of that delivery — queued, stranded, confirmed —
	// shares one mark. A resume (opts.ResumeIfStranded) does NOT write a new
	// one: it finishes the SAME delivery, so the denominator must not move.
	//
	// Durable when a state store is configured, same reasoning as stranded:
	// a dispatched worker is meant to be checked on across a service
	// restart, and `turns` going silently absent on every session the moment
	// this machine redeploys would defeat the field's own purpose. See
	// noteDelivery/deliveryMarkFor and deliveryMarkRetention.
	delivered map[string]deliveryMark

	// resumeIntents remembers, per session, the conversation id a create
	// asked the runtime to resume — the durable note #72 needs to say
	// whether that was honoured, once the session's own conversation
	// resolves. Same shape as stranded, for the same reasons: durable when
	// a state store is configured, in memory otherwise, keyed on session
	// id with cwd carried for corroboration (§5.4). See resumeintent.go.
	resumeIntents map[string]resumeIntentRecord

	// createRecords remembers, per session, what a create asked to pin, ask
	// for a runtime surface, and carry as a prompt — the durable notes #84,
	// #85 and #86 need to answer what was APPLIED rather than only what was
	// REQUESTED, once each of those can be told. Same shape as
	// resumeIntents, for the same reasons. See createrecord.go.
	createRecords map[string]createRecord

	// environments remembers what each created session's process received
	// (see environment.go). In memory only, for the reason stated on
	// Environment.
	environments map[string]fleet.SessionEnvironment

	// futile remembers, per session, that Discard's clear loop already
	// spent a full pass against a specific composer residue and produced
	// no movement at all (#87). In memory only, like environments: this is
	// transient evidence about one clear attempt, not a fact the service
	// owes a restart — a restart re-earns it in the time one ordinary pass
	// takes, exactly as a genuinely first-time-frozen composer would. See
	// noteFutile/futileClearAttempts/forgetFutile below.
	futile map[string]futileClear

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

	// trustSeed pre-answers the runtime's folder-trust question for every
	// directory under a configured root — see internal/trustseed and #47.
	// Nil means unconfigured, the same off-by-default contract as
	// credentialPath: a driver built for a test never touches a real state
	// file merely because it was constructed. See WithTrustSeed.
	trustSeed *trustseed.Seeder

	// sessionEnv is this machine's declared identity for its sessions —
	// colab-fleet issue #94. Nil/empty means unconfigured, the same
	// off-by-default contract as credentialPath and trustSeed: a driver
	// built for a test never merges configuration into a caller's env
	// merely because it was constructed. See WithSessionEnv and
	// sessionenv.go's provisionSessionEnv.
	sessionEnv []SessionEnvEntry

	// psBin and psRun are colab-fleet #116's own exec seam, deliberately
	// separate from bin/run rather than reusing them. Those name and run the
	// multiplexer specifically (execFunc's own doc comment); psRun queries
	// the OS process table for a PID this driver already resolved from the
	// multiplexer, an unrelated external program with its own argv shape. A
	// shared field would make a test double built for one silently answer
	// for the other. See processidentity.go.
	psBin string
	psRun execFunc

	// inboxResolver and inboxDial are colab-fleet #119's own seam. Nil
	// means unconfigured — the same off-by-default contract as
	// credentialPath and trustSeed: a driver built for a test never
	// attempts an inbox delivery merely because it was constructed, and
	// Send behaves exactly as it did before #119 until a composition root
	// opts in. See inbox.go.
	inboxResolver InboxResolver
	inboxDial     inboxDialFunc
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

// WithTrustSeed enables #47's directory-trust seeding: statePath is the
// runtime's own state file (in practice, the same path WithCredentialPath
// points at — two options rather than one reused field, so credential
// generation stays a bare stat with no read of the file's contents, exactly
// as its own doc comment promises, regardless of whether this feature is
// on), home is the operator's home directory, and roots are the configured
// roots seeding is scoped to — machine-local configuration, like
// FLEET_PEERS' addresses, never committed to this repository.
//
// Off by default for the same reason as WithRecordRoot and
// WithCredentialPath: an empty statePath or a nil/empty roots list leaves
// trustSeed nil, and every method on internal/trustseed.Seeder is a no-op on
// a nil receiver, so Create never has to branch on whether this was
// configured.
func WithTrustSeed(statePath, home string, roots []string) Option {
	return func(d *Driver) { d.trustSeed = trustseed.New(statePath, home, roots) }
}

// WithSessionEnv declares this machine's identity for its sessions —
// colab-fleet issue #94. entries is expected to have already passed
// ValidateSessionEnv; this option does no validation of its own; the same
// division main.go already keeps for TrustRoots (validated once at startup,
// wired here without re-checking).
//
// Off by default for the same reason as WithRecordRoot, WithCredentialPath
// and WithTrustSeed: a driver built for a test must not merge configuration
// into a caller's env merely because it was constructed. An empty or nil
// list leaves provisionSessionEnv a no-op (see sessionenv.go).
func WithSessionEnv(entries []SessionEnvEntry) Option {
	return func(d *Driver) { d.sessionEnv = entries }
}

// TrustSeedResult passes through internal/trustseed.Result so a caller
// outside this package (cmd/colab-fleetd's startup-and-interval maintainer)
// never has to import internal/trustseed itself.
type TrustSeedResult = trustseed.Result

// SeedTrustRoots runs one pass of #47's trust seeding — see
// internal/trustseed.Seeder.SeedAll. Meant to be called once at startup and
// again on an interval; a Driver with WithTrustSeed unconfigured returns a
// zero Result and a nil error, doing nothing.
func (d *Driver) SeedTrustRoots() (TrustSeedResult, error) {
	return d.trustSeed.SeedAll()
}

// withExec injects a fake multiplexer. Unexported: tests only.
func withExec(f execFunc) Option { return func(d *Driver) { d.run = f } }

// WithPSBinary sets the process-table query executable colab-fleet #116's
// process-identity resolution shells out to. Default "/bin/ps" — an
// absolute path, not a bare name, for the reason session-identity.md's
// "Two traps this feature inherits" section documents for this driver's own
// shell-outs: a created session's login-shell wrap gives it a real PATH, but
// this call runs outside that wrap, on the same clean four-entry search path
// as everything else this daemon shells out on directly.
func WithPSBinary(path string) Option {
	return func(d *Driver) {
		if path != "" {
			d.psBin = path
		}
	}
}

// withPSExec injects a fake process-table query. Unexported: tests only —
// the same shape as withExec, and deliberately not the same field (see
// Driver.psRun's own doc comment).
func withPSExec(f execFunc) Option { return func(d *Driver) { d.psRun = f } }

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
		psBin:        "/bin/ps",
		psRun:        runReal,
		inboxDial:    dialInboxReal,
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
	d.loadDelivery()
	d.loadResumeIntents()
	d.loadCreateRecords()
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
// DeliversRawKeys is true. This driver owns a real pane (keys.go implements
// driver.KeySender against it) and produces the SessionState.ScreenDigest
// that corroborates each key. This is the one capability whose truth this
// driver's whole substrate is built from — a terminal multiplexer session IS
// a screen — so unlike ObservesState there is no substrate-fidelity reason
// to report anything but true here.
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
		ObservesState:   false,
		DeliversRawKeys: true,
		// This driver reads the runtime's own control-channel status label
		// off the pane footer (controlchannel.go). Declared rather than
		// assumed, so a nil ControlChannel is answerable: on this driver it
		// means the runtime rendered no label, not that nobody looked.
		ObservesControlChannel: true,
		// #85: this driver latches Session.RuntimeSurface off the same
		// footer label ObservesControlChannel already reads, once
		// corroborated — see surface.go's runtimeSurfaceFor.
		ReportsRuntimeSurface: true,
		ConfirmsDelivery:      false,
		SupportsResume:        true,
		// #122: true only once a composition root has actually wired
		// WithInboxResolver — set once at construction, never mutated
		// after, so reading it here needs no lock, the same as
		// d.deadline just below. A nil resolver (every test in this
		// package that does not pass one, and any consumer that has not
		// wired one) reports false, matching sendViaInbox's own
		// first-line check (inbox.go) exactly rather than approximating it.
		DeliversToInbox: d.inboxResolver != nil,
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
// for this driver's own registry; #47's trust-seed counts (see
// internal/trustseed) are merged in under their own names when that feature
// is configured, rather than exposed through a second driver — one map, one
// reader, and the two registries' names do not collide because trustseed's
// are all prefixed "trust_seed.".
func (d *Driver) Counters() map[string]int64 {
	out := d.counters.Snapshot()
	for k, v := range d.trustSeed.Counters() {
		out[k] = v
	}
	return out
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
			Quota: quotaOnly(d.quotaBlock()),
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

	// Loaded once and shared by noteSessionSet and identityDrift both
	// (colab-fleet #96/#97), rather than reading the store twice per
	// listing. Nil when unconfigured — every reader downstream already
	// treats a nil/empty map as "nothing asserted", the same honest default
	// every other durable record in this driver uses.
	var priorRecords map[string]sessionRecord
	if d.store != nil {
		priorRecords = d.loadRecords()
	}
	drift := identityDrift(rows, priorRecords)
	driftBySession := make(map[string]nameDrift, len(drift))
	for _, nd := range drift {
		driftBySession[nd.live.session] = nd
	}
	// colab-fleet #102: a second, independent index over the same prior
	// records, for identityAssertionFor below. Not threaded through
	// identityDrift itself — that function's output also drives
	// reassertNames, and TestIdentityReassertStopsOnceContested /
	// TestReassertRefusesWhenTheNameIsTaken cover that path unchanged; this
	// keeps it that way rather than reshaping it to serve a second reader.
	assertedByRun := indexByPaneCreated(priorRecords)

	d.noteSessionSet(rows, priorRecords)

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
		// Published so a caller can quote it back on a raw key (keys.go). It
		// is already computed for the classifier's own use; empty when the
		// capture failed, which correctly leaves that session unkeyable rather
		// than keyable against a screen nobody read.
		st.ScreenDigest = digest
		// colab-fleet #97: this read agreed with a rename that did not
		// hold. Say so in the read itself, not only in the repair
		// attempted below (after the lock) — a caller reading THIS
		// response must not see it agree silently the way #97's own
		// measurement found it doing.
		if nd, ok := driftBySession[r.session]; ok {
			st.Evidence += "; " + driftSentence(nd.want, r.session)
		}
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
		// colab-fleet #102: the same fact the evidence sentence above
		// carries, machine-readable. Populated HERE — at response-build
		// time, from what THIS read observed — not from reassertNames'
		// repair below, which runs after this response is built and lands
		// on the caller's NEXT poll, not this one.
		s.IdentityAssertion = identityAssertionFor(r, priorRecords, assertedByRun)
		// #84/#85/#86: this session's own create record, if one is still on
		// file — see createrecord.go. Absent means either nothing was
		// requested that this record would carry, or the record already
		// expired; pinOutcomeFor/promptDeliveryFor/runtimeSurfaceFor tell
		// those apart from their own carried/requested flags, never the
		// presence of the record itself.
		if cr, ok := d.createRecordForLocked(r.session, r.cwd); ok {
			s.Pins = pinOutcomeFor(cr)
			s.PromptDelivery = promptDeliveryFor(cr)
			// #85: the runtime's own footer, already classified into st a
			// few lines up, is the corroboration a dictated identifier
			// needs before RuntimeSurface may claim Known: true (§5.7 —
			// publishing an uncorroborated identifier as a fact is #84's
			// defect in a second field). Latched, never unset: identity,
			// not liveness — a channel that later reads Failed keeps its
			// address and reports the failure through state.controlChannel,
			// which is the field for it.
			if st.ControlChannel != nil && st.ControlChannel.State == fleet.ControlChannelActive && !cr.SurfaceSeen {
				d.noteSurfaceSeenLocked(r.session)
				cr.SurfaceSeen = true
			}
			s.RuntimeSurface = runtimeSurfaceFor(cr, r.session)
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

	// colab-fleet #97: put back every name this driver asserted and the
	// runtime no longer carries — whether the rename never reached the
	// runtime, or reached it and a second actor on the machine later undid
	// it; either way the record, not the last read, is what this driver
	// trusts. After the lock, like the conversation lookups below: each
	// repair is a real multiplexer call, and nothing here may run under the
	// lock that guards every session's observed state.
	d.reassertNames(ctx, rows, drift)

	// Locate each session's record in the runtime's own store. This is the
	// only source on this path that is not the runtime describing itself
	// (conversation.go says why that matters), and it is also the only one
	// that can answer "I looked and could not tell" — which is a different
	// answer from the absent field a driver with no store leaves behind.
	for _, p := range pending {
		conv := d.conversations.lookup(p.key, p.cwd, p.name, p.started)
		sessions[p.index].Conversation = conv
		// #72: a session whose CREATE asked to resume a conversation gets
		// that intent compared against what actually resolved, so a resume
		// silently downgraded to a fresh conversation is reported rather
		// than looking like an ordinary healthy start.
		if requested, ok := d.resumeIntentFor(p.name, p.cwd); ok {
			sessions[p.index].ResumeOutcome = resumeOutcomeFor(requested, conv)
		}
	}

	// #111: publish `turns` for every session carrying a live delivery
	// mark — independent of the quota/lastTurn gate just below, because
	// "did the agent run at all" is exactly the answer a QUIET session
	// needs most, unlike LastTurn/Quota which only ever upgrade something
	// the screen already flagged. Gated on a delivery mark existing, not on
	// anything the screen flagged: only a session this driver has actually
	// delivered into ever opens its record for this, so a fleet with
	// nothing dispatched through Send pays nothing extra here.
	for i := range sessions {
		sessions[i].State.Turns = d.turnsFor(sessions[i].ID, string(sessions[i].Cwd), sessions[i].Conversation)
	}

	// Ask the runtime's own record about whatever the screen already
	// flagged this cycle (#56) — a usage-limit notice, or a last turn the
	// screen read as failed. Only sessions the screen already flagged pay
	// for this: a quiet session's record is never opened, so a healthy
	// fleet costs nothing extra here. recordUnavailable (the zero value of
	// quotaVerdict when nothing resolves) means every downstream consumer
	// keeps its existing screen-derived fallback unchanged.
	var quotaRecord apiErrorFact
	var quotaVerdict recordVerdict
	for i := range sessions {
		switch {
		case sessions[i].State.Status == fleet.StatusQuotaBlocked:
			if quotaVerdict == recordUnavailable {
				if fact, verdict := d.recordFactFor(sessions[i]); verdict != recordUnavailable {
					quotaRecord, quotaVerdict = fact, verdict
				}
			}
		case sessions[i].State.LastTurn != nil:
			switch fact, verdict := d.recordFactFor(sessions[i]); verdict {
			case recordAPIError:
				sessions[i].State.LastTurn = &fleet.TurnEnd{
					Outcome:   "failed",
					Reason:    fact.reasonSentence(),
					Retryable: fact.retryable(),
				}
			case recordCleanTurn:
				// The durable record says the last turn actually
				// succeeded — the screen's "api error" match was history
				// a window scan cannot tell from the present. #56's
				// argument for Quota, arriving at LastTurn instead.
				sessions[i].State.LastTurn = nil
			}
			// recordUnavailable: keep classify.go's screen-derived
			// TurnEnd exactly as built — still the legitimate fallback
			// when no record store is configured or this session's
			// record cannot be matched.
		}
		// The control channel is orthogonal to what the session is DOING
		// (controlchannel.go's own comment says so, and this is that rule
		// again): a session can be StatusWorking with LastTurn nil and
		// still have a failed channel, so this is its own check rather
		// than another case of the switch above, which a session matching
		// both would only ever enter once.
		if ch := sessions[i].State.ControlChannel; ch != nil && ch.State == fleet.ControlChannelFailed {
			// Only a session the footer already flagged Failed pays for
			// this (#69, the same "quiet session's record is never
			// opened" discipline the switch above already holds). Reason
			// stays empty — never a guess — whenever the record cannot
			// explain why: no store, no matched conversation, or no
			// matching entry.
			if fact, ok := d.controlReasonFor(sessions[i]); ok {
				channel := *ch
				channel.Reason = fact.reasonText()
				sessions[i].State.ControlChannel = &channel
			}
		}
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
	d.noteQuotaBlock(sawLimit, hint, sawWorking, quotaRecord, quotaVerdict, now)

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
	//
	// recordCleanTurn joins sawWorking as an equally sufficient reason (#56):
	// a blocked session's OWN durable record showing its most recent turn
	// succeeded is the same kind of positive proof, from a source that
	// outlives the notice scrolling off the one pane that showed it.
	if sawWorking || quotaVerdict == recordCleanTurn {
		for i := range sessions {
			if sessions[i].State.Status != fleet.StatusQuotaBlocked {
				continue
			}
			st := sessions[i].State
			st.Status = fleet.StatusIdle
			st.Quota = nil
			if quotaVerdict == recordCleanTurn {
				st.Evidence = "a limit notice is on screen, but the runtime's own record shows " +
					"a later turn on this account already succeeded, so the notice is history"
			} else {
				st.Evidence = "a limit notice is on screen, but another session on this account is working now, so the notice is history"
			}
			sessions[i].State = st
		}
	}

	// q is read once and reused below for the source's own Quota field
	// (#10) — the same remembered fact, at both grains, from a single
	// lock acquisition rather than two.
	q, sinceObserved := d.quotaBlock()
	if q != nil {
		for i := range sessions {
			sessions[i].State = quotaBlockedState(sessions[i].State, q, sinceObserved)
		}
	}

	// Every block carries a real since. The per-session path builds its
	// QuotaBlock in classify, which has no clock, so it left the zero time —
	// which serialises as year 1 and is worse than absent: a caller computing
	// "blocked for how long" gets two millennia.
	//
	// This is exactly the session whose OWN screen showed the notice
	// directly (quotaBlockedState's rewrite only touches idle/unknown/
	// starting, by design — a session already reporting quota_blocked keeps
	// classify.go's own evidence phrase). q's own Since/sinceObserved — the
	// account-level fact noteQuotaBlock just finished maintaining, whether
	// from THIS cycle's record or restored from a restart — is the
	// authoritative answer (#56) and applies here first; the status's own
	// since (this driver's first observed TRANSITION into the status, still
	// a sighting rather than the refusal) and the read time are fallbacks
	// for when there is no account-level block to consult at all.
	for i := range sessions {
		st := sessions[i].State.Quota
		if st == nil || !st.Since.IsZero() {
			continue
		}
		blocked := *st
		recordConfirmed := false
		switch {
		case q != nil && !q.Since.IsZero():
			blocked.Since = q.Since
			recordConfirmed = sinceObserved
			if q.ResetHint != "" && blocked.ResetHint == "" {
				blocked.ResetHint = q.ResetHint
			}
		case sessions[i].State.Since != nil && !sessions[i].State.Since.IsZero():
			blocked.Since = *sessions[i].State.Since
		default:
			blocked.Since = now
		}
		sessions[i].State.Quota = &blocked
		if recordConfirmed {
			sessions[i].State.Evidence += "; since is the runtime's own record of the refusal"
		} else {
			sessions[i].State.Evidence += "; since is when this driver first observed the notice, " +
				"not when the refusal happened — the runtime's own record could not confirm it"
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

// maxNameReasserts bounds how many times reassertNames will put an asserted
// name back after finding the runtime disagreeing with it (colab-fleet
// #97). A repair already proven not to hold twice is not attempted a third
// time — discardProvenFutile's rule (this file, the composer-clear case)
// applied to identity: an unbounded loop against a second actor on the
// machine that keeps reverting a name is a rename war, not a fix.
const maxNameReasserts = 2

// Counter names for identity repair (colab-fleet #96/#97) — see counterSet's
// own doc comment on why these are a registry entry rather than a new field.
const (
	// counterIdentityReasserted counts every time List successfully put an
	// asserted name back after finding the runtime disagreeing with it.
	counterIdentityReasserted = "identity.reasserted"
	// counterIdentityReassertFailed counts a reassert attempt whose own
	// rename-session call failed.
	counterIdentityReassertFailed = "identity.reassert_failed"
	// counterIdentityContested counts a drift entry List declined to
	// repair: the wanted name is itself live under a different session, or
	// maxNameReasserts is already spent.
	counterIdentityContested = "identity.contested"
)

// reassertNames puts back every name in drift — colab-fleet #97: a rename
// this driver recorded and the runtime no longer carries, whether because
// it never reached the runtime or reached it and was later undone by a
// second actor on the machine. Called from List, after its own
// d.mu.Unlock(): each repair is a real multiplexer call, and none of them
// may run under the lock that guards every session's observed state.
func (d *Driver) reassertNames(ctx context.Context, rows []paneRow, drift []nameDrift) {
	if len(drift) == 0 {
		return
	}
	liveNames := make(map[string]bool, len(rows))
	for _, r := range rows {
		liveNames[r.session] = true
	}
	for _, nd := range drift {
		if liveNames[nd.want] {
			// Another live session already carries the name this driver
			// wants to restore. Refuse rather than let the multiplexer
			// decide — the same rule Rename itself applies at request
			// time (§ above) — and stop trying: a name genuinely taken by
			// someone else does not become free by retrying.
			d.recordContested(nd.live.session, nd.rec)
			d.counters.incr(counterIdentityContested)
			continue
		}
		if nd.rec.Reasserts >= maxNameReasserts {
			d.counters.incr(counterIdentityContested)
			continue
		}
		if _, err := d.run(ctx, d.bin, "rename-session", "-t", "="+nd.live.session, nd.want); err != nil {
			d.recordReassertAttempt(nd.live.session, nd.rec, false)
			d.counters.incr(counterIdentityReassertFailed)
			continue
		}
		d.mu.Lock()
		if o, ok := d.observed[nd.live.session]; ok {
			d.observed[nd.want] = o
			delete(d.observed, nd.live.session)
		}
		d.mu.Unlock()
		d.recordReassertAttempt(nd.live.session, nd.rec, true)
		d.counters.incr(counterIdentityReasserted)
	}
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
		st.ScreenDigest = digest                           // see List's stamp of the same field
		d.observed[r.session] = observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			sinceRestored: carried,
		}
		d.mu.Unlock()
		// Same record upgrade List applies to LastTurn (#56) — State has no
		// pre-resolved Conversation to reuse (it returns a bare
		// SessionState, not a Session), so this does its own lookup; that
		// lookup memoises successes in conversationStore, so a session List
		// already resolved this cycle costs a map read here, not a rescan.
		st = d.upgradeLastTurnFromRecord(st, r.cwd, r.session, r.created, r.paneID)
		// Same record upgrade List applies to ControlChannel.Reason (#69),
		// same reason State does its own lookup rather than reusing a
		// resolved Conversation.
		st = d.upgradeControlChannelFromRecord(st, r.cwd, r.session, r.created, r.paneID)
		// #111: same split as the two upgrades just above — List resolves
		// `turns` from its own pre-resolved Conversation, State does its own
		// lookup.
		st = d.upgradeTurnsFromRecord(st, r.cwd, r.session, r.created, r.paneID)
		// Same rewrite List applies, generalised to a one-session read (#10)
		// — see quotaBlockedState's own comment for why a session's own
		// state must not be reported as an unqualified "starting"/"idle"/
		// "unknown" while this machine's account is known to be refusing
		// work. This is the read Create's own HTTP handler makes to build
		// the state it hands back in a 201 — without this, a create
		// reported nothing and the fact was swallowed until a later poll.
		q, sinceObserved := d.quotaBlock()
		return quotaBlockedState(st, q, sinceObserved), nil
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
	// #53: fail closed on text this runtime reads as its own syntax rather
	// than a message, BEFORE anything else. This is a decision about the
	// BYTES, not about the moment — it must not depend on which session
	// exists, what its screen shows, or whether the substrate is reachable
	// at all, so it runs ahead of every one of those and the substrate is
	// never touched for text that was always going to be refused. See
	// inputguard.go for the pattern list and why it belongs to this driver.
	if reason, refused := refuseAsRuntimeSyntax(text); refused {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
	}

	// colab-fleet #112: ResumeIfStranded asks to finish the delivery already
	// sitting in the composer; ReplaceIfStranded asks to throw it away and
	// deliver this call's text instead. Both at once is a contradiction, not
	// an ambiguity to resolve by picking one silently — refused before
	// anything else runs, the same way #53's guard above decides on the
	// bytes alone before looking at session state.
	if opts.ResumeIfStranded && opts.ReplaceIfStranded {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "resumeIfStranded and replaceIfStranded were both set — the first asks " +
				"to finish the delivery already in the composer, the second asks to " +
				"discard it and deliver this text instead; set at most one",
		}, nil
	}

	// colab-fleet #119: capability-detected fast path over a target
	// session's own inbox, tried before anything below touches the pane.
	// inboxEligible excludes every shape (!Submit, ResumeIfStranded,
	// ReplaceIfStranded) that names a pane-composer concept the inbox path
	// has none of — those calls fall straight through to the pane path
	// unchanged, same as ever. sendViaInbox manages its own bounded
	// context internally (mirroring ResolveProcessIdentity/
	// VerifyProcessIdentity, which it calls); it does not use the `ctx`
	// this function bounds just below, because it must run — and
	// possibly return — before that bound exists.
	if inboxEligible(opts) {
		if receipt, ok, err := d.sendViaInbox(ctx, ref, text); err != nil {
			return fleet.DeliveryReceipt{}, err
		} else if ok {
			return receipt, nil
		}
		// ok=false: no inbox capability for this target — fall through to
		// the pane path below, unchanged.
	}

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
		// colab-fleet #64: no composer painted is one fact with (at least) two
		// causes, and the old wording asserted one of them as if it were
		// established — "still starting or is not listening" — when the
		// actual observation is only "no composer". A runtime showing a
		// full-screen interface with no composer of its own (a dialog, for
		// one) paints exactly this way too, and reads as "possibly broken"
		// through a message that guessed wrong.
		//
		// `young` is the same discriminator classify.go already uses for the
		// identical ambiguity (starting vs. unknown) — reused here rather
		// than invented, so the two places that hit this shape stay
		// consistent rather than developing their own private judgment calls.
		young := d.now().Sub(target.created) < startingWindow
		if young {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is not able to receive input yet: no composer has been " +
					"painted, and the session is young enough to still be starting. " +
					"Delivering now would render the text and lose the submit. Wait for " +
					"the session to report idle, then send again",
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is not able to receive input yet: no composer has been " +
				"painted, and the session is old enough that this is unlikely to be " +
				"ordinary startup. That could still mean the runtime is slow to start, " +
				"or it could mean the runtime is showing a full-screen interface with " +
				"no composer of its own — a dialog, for one — which this driver cannot " +
				"tell apart from here. Delivering now would render the text and lose " +
				"the submit. If the session does not settle to idle on its own, keys() " +
				"can still reach the screen directly (deliversRawKeys: true)",
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
			// #101: this branch only ever runs when the composer is already
			// holding OUR OWN pending delivery (strandedMatches, just above).
			// opts.Submit is not re-checked here: resume is a completion of a
			// submit already requested by the call that stranded this text,
			// not a fresh request that could reasonably ask to land-without-
			// submitting.
			//
			// #109 corrects the rest of this paragraph as it used to read: it
			// claimed "there is nothing left to land, only to submit", treating
			// strandedMatches as proof the paste had already FINISHED landing.
			// It has not — strandedMatches corroborates WHOSE delivery this is,
			// never whether it settled. See the `landed` check a few lines down
			// for what replaced that claim.
			//
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
			//
			// #101: attribute whatever collapsed-paste marker is currently
			// sitting in the composer to OUR text, using an empty `before` so
			// any marker already present counts as "gained" — there is no
			// fresh paste here to distinguish it from, the paste already
			// happened on the attempt that stranded it. This is the same
			// attribution confirmLanded already performs for a fresh paste;
			// reused here for a paste that landed earlier.
			//
			// #109: `landed` IS now checked, and this reverses #101's own
			// reasoning for ignoring it. composerText (above) proves the
			// composer holds SOMETHING, not that it holds a SETTLED, complete
			// copy of our text — a very large multi-line paste can still be
			// mid-collapse, or the composer can hold two ambiguous markers
			// (an unrelated stranding alongside ours), and #101's comment
			// waved both off as "a failed re-match here only degrades atCount
			// to 0, which confirmSubmitted already treats as fall back to the
			// composer-empty check alone" — true of the OUTCOME confirmSubmitted
			// reports, but that reasoning only covers whether the SUBMIT is
			// confirmed, not whether what gets submitted is complete. Pressing
			// the wake key regardless of `landed` submits whatever is
			// currently sitting in the composer even when this driver has NO
			// attributable evidence it is our full delivery — measured live as
			// a truncated, tail-only fragment reaching the agent (#109): the
			// resume path was completing an unconfirmed, possibly-partial
			// landing instead of redoing it. `landed=false` now refuses to
			// press submit at all, the same discipline the first-attempt path
			// (below) already applies to identical evidence — the record is
			// kept either way, so a later resume gets another chance once the
			// paste has actually settled.
			key, atCount, landed := d.confirmLanded(ctx, target.paneID, text, map[pasteKey]int{})
			if !landed {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason: d.withRestartNoteReason(ref.ID, "resumed a delivery this driver had "+
						"stranded earlier, but the composer could not be confirmed to hold a "+
						"complete, attributable copy of it this time either — pressing submit now "+
						"risks completing a partial paste instead of the full one; the record is "+
						"kept — retry the same send with resumeIfStranded again"),
				}, nil
			}
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "Space", "C-m"); err != nil {
				return fleet.DeliveryReceipt{}, fmt.Errorf("send: submitting stranded text: %w", err)
			}
			// #101: this used to report `submitted` — the strongest outcome in
			// the enum — and call forgetStranded immediately afterwards,
			// discarding this driver's own record of the text on the strength
			// of a keystroke nobody checked. Getting `unknown` on a resume
			// left the caller exactly where it started, unable to tell a
			// transient artefact from the strand this driver already counts.
			//
			// CORROBORATING WHICH TEXT IS OURS still goes through this
			// driver's own record (strandedMatches, above) rather than
			// re-reading the screen — #49 still holds, a collapsed multi-line
			// paste cannot be compared byte for byte. WHETHER THE SUBMIT
			// REGISTERED is a different question, and it is answered the same
			// evidence-based way the first-attempt path already answers it
			// below: the composer emptying, or this delivery's own attributed
			// marker clearing. Delivering a receipt this driver did not earn
			// is the exact conflation delivery.go's OutcomeSubmitted doc
			// forbids.
			if !d.confirmSubmitted(ctx, target.paneID, key, atCount) {
				// The record is KEPT, deliberately unlike the old behaviour: a
				// swallowed keystroke here must leave something for a third
				// attempt to resume, not discard the only trace of the text on
				// a keystroke nobody checked.
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason: d.withRestartNoteReason(ref.ID, "resumed a delivery this driver had "+
						"stranded earlier, but the submit could not be confirmed this time "+
						"either; the record is kept — retry the same send with "+
						"resumeIfStranded again"),
				}, nil
			}
			d.forgetStranded(ref.ID)
			return fleet.DeliveryReceipt{
				// Queued, not submitted — matching the first-attempt path's
				// own outcome for the identical evidence (§4.3's
				// ConfirmsDelivery stays false for the same reason on both
				// paths): a confirmed submit says the bytes left this
				// driver's hands, not that the agent consumed them.
				Outcome: fleet.OutcomeQueued,
				Reason: "resumed a delivery this driver had stranded earlier, and the submit " +
					"registered this time; agent receipt is not observable on this substrate",
			}, nil
		}

		// colab-fleet #112: the resume branch just above only ever fires for
		// an EXACT match (strandedMatches) with ResumeIfStranded set. Every
		// other shape used to fall straight through to one refusal claiming
		// "text a human typed" — even when this driver's OWN record said
		// otherwise. Consult that record now, for the three cases that
		// refusal was collapsing into one.
		if record, hasRecord := d.strandedRecordFor(ref.ID, target.cwd); hasRecord {
			if opts.ReplaceIfStranded {
				expectedLines, _ := composerVisualLines(screenNow)
				receipt, cleared, err := d.tryReplaceStranded(ctx, ref, target, record, pending, expectedLines)
				if err != nil {
					return fleet.DeliveryReceipt{}, err
				}
				if !cleared {
					return receipt, nil
				}
				// Cleared: the stranded record is already forgotten inside
				// tryReplaceStranded. Fall through — deliberately NOT a
				// return — to the ordinary delivery path below, which pastes
				// and confirms THIS call's text exactly as it would for a
				// composer that was never busy, and writes a fresh delivery
				// mark for #111 in the process.
			} else if record.Text == text {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "composer holds a delivery this driver made into this session and " +
						"could not confirm submitted; it is this service's own text, not a " +
						"person's draft. Resend the same text with resumeIfStranded to finish it",
				}, nil
			} else {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "composer holds a delivery this driver made into this session and " +
						"could not confirm submitted — this driver's own record says the text " +
						"there is its own, not a person's draft — but the text being sent now " +
						"is different. resumeIfStranded finishes the ORIGINAL delivery; " +
						"replaceIfStranded discards it and delivers this text instead; or " +
						"read the session and discard the composer first",
				}, nil
			}
		} else {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "composer holds unsent input; delivering would concatenate " +
					"with text a human typed and has not submitted (§2.4)",
			}, nil
		}
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

	// colab-fleet #111: this is the single write site for a delivery mark —
	// the moment "a delivery was made into this composer" becomes true.
	// Every outcome below (queued unsubmitted, stranded-unknown, confirmed)
	// shares this one mark, and a resume completing an EARLIER delivery
	// never reaches this line at all, so `turns` never resets under it.
	d.noteDelivery(ref.ID, target.cwd)

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
		d.noteStranded(ref.ID, target.cwd, text, d.currentComposerDigest(ctx, target.paneID))
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: d.withRestartNoteReason(ref.ID, "text was delivered to the composer but "+
				"did not render in time to be confirmed landed, and no single new paste "+
				"marker could be attributed to this delivery alone; it may be sitting there "+
				"unsent — retry the same send with resumeIfStranded to submit it"),
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
		d.noteStranded(ref.ID, target.cwd, text, d.currentComposerDigest(ctx, target.paneID))
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: d.withRestartNoteReason(ref.ID, "the text landed and was attributed to "+
				"this delivery, and a submit was issued, but this delivery's own block did "+
				"not clear and the composer did not empty — the submit did not register for "+
				"it. It is sitting there unsent; retry the same send with resumeIfStranded "+
				"to submit it"),
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

	sc := newScreen(captures[live.paneID])
	pending, _ := composerText(sc)
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

	// #87: this EXACT residue may already be proven to resist a clear pass —
	// a prior call spent a full, content-sized budget (clearComposer's own
	// expectedLines+clearPressMargin, capped at maxClearPresses) pressing
	// C-u against it and it never moved. If so, do not press again: a
	// second identical pass against text nobody has re-read is not
	// gathering more evidence, it is destructive keystrokes spent to learn
	// what the first pass already learned.
	//
	// #129 asked whether this still holds now that a real human's
	// persistence was observed clearing a composer this exact refusal had
	// given up on, and the answer is: refusing here is unchanged, but the
	// PASS this now guards is not the one that field case indicted. That
	// pass used to be bounded by a flat 3-second clock regardless of what
	// the composer held — a paste sized like #129's own field case could
	// not have finished inside it at any machine speed, which is a
	// different failure than "pressing again would not help." Now that a
	// pass is sized to the composer's own row count instead, a residue that
	// still has not moved after that many presses is evidence a human's
	// persistence never actually contradicted: nothing here claims a human
	// pressing MORE than a content-sized pass already did would fail too,
	// only that this driver pressing the SAME text again, having already
	// spent that pass, would not learn anything new. Refuse outright,
	// before touching the pane.
	if attempts := d.futileClearAttempts(ref.ID, live.cwd, expectDigest); attempts > 0 {
		return fleet.Ack{}, d.withRestartNote(ref.ID, discardProvenFutile(attempts))
	}

	expectedLines, _ := composerVisualLines(sc)
	left, _, cleared, err := d.clearComposer(ctx, live.paneID, ref.ID, live.cwd, pending, expectedLines)
	if err != nil {
		return fleet.Ack{}, fmt.Errorf("discard: %w", err)
	}
	if cleared {
		return fleet.Ack{Accepted: true}, nil
	}
	return fleet.Ack{}, d.withRestartNote(ref.ID, discardIncomplete(pending, left))
}

// clearComposer walks a composer's unsent text backward with repeated C-u
// presses until it empties or the pass proves futile — the mechanism both
// Discard and colab-fleet #112's replace-stranded path need, extracted here
// so the two callers cannot drift apart on #87's stall/futility semantics.
// Discard is the ORIGINAL of this code.
//
// pending is the composer text the CALLER has already corroborated —
// Discard's digest check, or #112's ComposerDigest match — BEFORE calling
// this. It presses keys unconditionally and trusts that corroboration
// already happened; it does not repeat it.
//
// expectedLines is composerVisualLines' count for that SAME corroborated
// screen, computed by the caller because it already has the screen this
// pending came from — see composerVisualLines for why an on-screen row
// count, not a duration, is what a press budget should be sized to.
//
// # Why a press count now, not colab-fleet#129's retired promptClearWindow
//
// C-u clears the line the cursor sits on: readline's unix-line-discard,
// killing from the cursor back to the start of the CURRENT line, not the
// whole buffer. A composer holding one short line empties in one press,
// measured directly (C-a C-k and Escape were tried too; C-u alone was
// enough for a single line). A composer spanning several rows does not —
// one press clears the row the cursor is on and leaves every row above it
// standing, so a single un-repeated press against a multi-row paste can
// only ever get partway there (issue #32's field case: 209 characters,
// four on-screen rows, corrupted by the un-repeated fix of the day). So
// this presses the same key again on every iteration the composer is still
// non-empty, walking it backward one row at a time — the same thing an
// operator clearing a stuck multi-line prompt by hand would do.
//
// The bound on how many times to do that used to be a flat 3-second clock
// (promptClearWindow), sized against #87's stuck-composer case and never
// checked against the opposite one: #129 measured a real paste (~6.6 KB,
// on the order of eighty on-screen rows) whose OWN required press count —
// roughly one per row — could not fit inside that budget regardless of
// machine speed. A duration is the wrong unit for work that scales with
// row count; expectedLines+clearPressMargin is presses, the unit the work
// is actually measured in, capped at maxClearPresses so the pass still
// terminates for content with no natural size limit (see that constant).
//
// #87's early exit is UNCHANGED by any of this: once movement has actually
// been observed, stallPresses further presses that change nothing are no
// longer "still walking it backward" — they are evidence the pass has
// stopped working, and pressing MORE just makes a bigger dent for no
// further gain. That check still fires before the press budget is
// necessarily exhausted, exactly as before; only what bounds the case where
// it never fires has changed.
//
// Verification stays in the loop: a keypress that did not register looks
// exactly like one that did, the same reason Send confirms before
// submitting.
//
// left is what remains (empty on success). moved reports whether the pass
// observed ANY movement at all — the caller's own signal for
// noteFutile/its own message, exactly as Discard used it before extraction.
// cleared is true only once the composer read back genuinely empty. err is
// non-nil only for a failed keystroke or capture call — "ran out of budget
// without emptying" is a normal outcome (cleared=false, err=nil), reported
// differently by each of the two callers.
func (d *Driver) clearComposer(ctx context.Context, paneID, id, cwd, pending string, expectedLines int) (left string, moved, cleared bool, err error) {
	presses := expectedLines + clearPressMargin
	if presses < 1 {
		presses = 1
	}
	if presses > maxClearPresses {
		presses = maxClearPresses
	}
	left = pending
	stall := 0
	for pressN := 0; pressN < presses; pressN++ {
		if _, runErr := d.run(ctx, d.bin, "send-keys", "-t", paneID, "C-u"); runErr != nil {
			return left, moved, false, runErr
		}
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			got, _ := composerText(sc)
			if got == "" {
				d.forgetFutile(id)
				return "", moved, true, nil
			}
			if got != left {
				moved = true
				stall = 0
			} else if moved {
				stall++
			}
			left = got
		}
		if moved && stall >= stallPresses {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if pressN == presses-1 {
			break
		}
		select {
		case <-ctx.Done():
			return left, moved, false, ctx.Err()
		case <-time.After(promptClearInterval):
		}
	}

	if !moved {
		// First time this exact residue has been seen not to move — record
		// it so a caller that retries against the SAME residue is refused
		// (futileClearAttempts) before spending another full pass on it.
		d.noteFutile(id, cwd, screenDigest(pending))
	}
	return left, moved, false, nil
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
//
// # #87: "unchanged" stopped meaning "safe AND worth retrying"
//
// This function only ever sees the FIRST time a given residue is observed
// not to move — Discard's futileClearAttempts check above refuses a repeat
// before this is even reached, so "safe to retry" here is no longer the
// lie it used to become on the second, third, fourth identical call. See
// discardProvenFutile for what a caller gets once retrying stops being
// useful, and this call's own doc comment for why the two must not share a
// message.
func discardIncomplete(before, after string) error {
	if after == before {
		return fmt.Errorf(
			"%w: discard: the clear keystroke did not register; the composer is "+
				"unchanged from what was read, so retrying once more with the same "+
				"digest is safe — if it is still unchanged after that retry, stop and "+
				"re-read rather than retrying again",
			ErrAmbiguousTarget)
	}
	return fmt.Errorf(
		"%w: discard: the clear keystroke ran but did not finish; the composer now "+
			"holds neither the original text nor nothing (found digest %s) — it is "+
			"damaged, not merely unclear, so re-read it before doing anything else "+
			"rather than retrying blind",
		ErrAmbiguousTarget, screenDigest(after))
}

// discardProvenFutile reports the state discardIncomplete's "unchanged"
// branch cannot: this is not the first pass against this residue, it is at
// least the second, and the prior pass already spent a full, content-sized
// press budget (clearComposer's expectedLines+clearPressMargin, capped at
// maxClearPresses — colab-fleet#129) pressing C-u against it without moving
// it at all (#87).
//
// The Issue this closes described exactly the failure this guards against:
// a caller that followed "retrying with the same digest is safe" to the
// letter, four times, and made zero progress each time — the advice was
// true in the narrow sense (nothing was destroyed) and false in the sense
// that mattered (retrying was never going to help). This message stops
// making that promise once the evidence for it is gone, and — unlike the
// "safe to retry" wording — deliberately contains neither "safe" nor
// "unchanged", so a caller pattern-matching on either of those substrings
// to decide whether to retry sees a different answer here, not a repeat of
// the first one.
//
// It names the one escape hatch this driver can actually offer: `keys`
// refuses outright while the composer holds text (see keys.go), and this
// file's own history already measured Escape as not helping here (Discard's
// C-u comment: "C-a C-k and Escape were tried too"). What is left, and what
// this says, is the session-level operation that is guaranteed to work
// regardless of what state the composer is stuck in: close it.
func discardProvenFutile(attempts int) error {
	return fmt.Errorf(
		"%w: discard: a full clear pass already left this exact composer text "+
			"unmoved %d time(s); pressing again is not expected to do anything "+
			"different, so this call made no attempt — re-read the composer before "+
			"deciding anything, and if it genuinely must go, close the session "+
			"(DELETE /v1/machines/{machine}/sessions/{id}) rather than retrying the "+
			"same digest indefinitely",
		ErrAmbiguousTarget, attempts)
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

	// colab-fleet #97: write the durable half. The multiplexer call above
	// just demonstrated the rename reaches the runtime — that has never
	// been in question — but nothing until now recorded that this driver
	// EXPECTS the session to be named `to`, so nothing has ever put it back
	// if a second actor on the machine undoes it later. This is what
	// List's identityDrift/reassertNames read.
	d.noteRenamed(ref.ID, to, live.cwd, live.paneID, live.created)

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
	// #111: a destroyed session's `turns` denominator describes a delivery
	// into a composer that no longer exists — forget it the same way and
	// for the same reason as the stranded record just above.
	d.forgetDelivery(ref.ID)
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
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.Session{}, errors.New("create: idempotency key is required (§10)")
	}
	// A completed key returns what it produced; a pending one means this
	// driver was interrupted mid-create and must find out what happened
	// before doing anything (§10, see idempotency.go).
	if ref, rec, found := d.idem.lookup(key); found {
		if rec.Phase == idemComplete {
			cr, ok := d.createRecordFor(ref.ID, string(spec.Cwd))
			pins, surface, prompt := sessionFactsFor(cr, ok, ref.ID)
			return fleet.Session{
				SessionRef: ref, Cwd: spec.Cwd,
				Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
				IdentityAssertion: d.identityAssertionForCreate(ref.ID),
			}, nil
		}
		if adopted, ok := d.resolvePending(ctx, key, rec); ok {
			cr, found := d.createRecordFor(adopted.ID, string(spec.Cwd))
			pins, surface, prompt := sessionFactsFor(cr, found, adopted.ID)
			return fleet.Session{
				SessionRef: adopted, Cwd: spec.Cwd,
				Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
				IdentityAssertion: d.identityAssertionForCreate(adopted.ID),
			}, nil
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
	name, markerApplied, ok := d.resolveName(ctx, requested, spec.Marker)
	if !ok {
		return fleet.Session{}, fmt.Errorf(
			"create: could not derive a free session name from %q; either it sanitizes "+
				"to nothing, or too many sessions already carry it", requested)
	}
	if spec.Cwd == "" {
		return fleet.Session{}, errors.New("create: cwd is required")
	}

	contextFile := string(spec.ContextRef)
	if contextFile != "" && !filepath.IsAbs(contextFile) {
		return fleet.Session{}, fmt.Errorf("create: contextRef must be absolute, got %q", contextFile)
	}
	// colab-fleet issue #94: fold this machine's declared identity into the
	// caller's env BEFORE any of the validation below, so a configured value
	// is checked by the same bound as a caller's own (see sessionenv.go's
	// readSessionEnvFile) and a bareExec driver refuses a configured value
	// exactly as it already refuses a caller-supplied one. Must run here,
	// inside the local driver's own Create — never in the HTTP handler — so
	// a create this machine only relays onward never reads this machine's
	// files (see sessionenv.go's package doc for why).
	mergedEnv, err := d.provisionSessionEnv(spec)
	if err != nil {
		return fleet.Session{}, err
	}
	spec.Env = mergedEnv
	if err := validateEnv(spec.Env); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	// Refuse rather than start a session missing what the caller asked for. The
	// bare-exec shape has no shell to apply an environment file in, so a create
	// carrying variables cannot be honoured there — and a session that comes up
	// without the identity its supervisor gave it looks perfectly healthy and
	// fails later, somewhere else, which is the failure mode this whole area
	// keeps producing. This also catches a value sessionEnv contributed above:
	// bareExec has no out-of-band channel for it either.
	if len(spec.Env) > 0 && d.bareExec {
		return fleet.Session{}, errors.New(
			"create: this driver is configured without the login-shell wrap, so it has " +
				"no out-of-band channel for env; refusing rather than starting a session without it")
	}
	if spec.PermissionMode != "" && spec.PermissionMode != fleet.PermissionModeBypass {
		return fleet.Session{}, fmt.Errorf(
			"create: unknown permissionMode %q (this runtime has one: %q)",
			spec.PermissionMode, fleet.PermissionModeBypass)
	}
	if err := validateMcpConfig(spec.McpConfig); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	if spec.Resume != "" && !safeArgvValue(spec.Resume) {
		return fleet.Session{}, fmt.Errorf(
			"create: resume %q would be read as a flag by the agent, not as a conversation id",
			spec.Resume)
	}
	// colab-fleet #84: Agent/Model/Effort get the same guard Resume already has,
	// four lines above. Before this, a value beginning with "-" silently failed
	// safeArgvValue inside claudeCodeCommand, the flag was never appended, and
	// the create response still echoed the REQUESTED value back — telling a
	// caller a pin was applied when the session was running whatever the
	// runtime defaults to. Refusing here, before any argv is built, is §2.1's
	// own rule: "a driver that cannot honour [a hint] must say so at creation
	// ... rather than silently substituting a default."
	for _, pin := range []struct{ name, value string }{
		{"agent", string(spec.Agent)},
		{"model", spec.Model},
		{"effort", spec.Effort},
	} {
		if pin.value != "" && !safeArgvValue(pin.value) {
			return fleet.Session{}, &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: fmt.Sprintf(
					"create: %s %q would be read as a flag by the agent, not as a value; "+
						"refusing rather than starting a session with the pin silently dropped (§2.1)",
					pin.name, pin.value),
				Machine: d.machine,
			}
		}
	}
	for _, k := range spec.Consents {
		if _, ok := consentableKinds[k]; !ok {
			return fleet.Session{}, fmt.Errorf(
				"create: %q is not a consentable question — see the driver's note on "+
					"why some boot questions have no safe affirmative option", k)
		}
	}

	// Intent first. A crash between here and the session starting leaves a
	// pending record, which the next attempt resolves by looking rather
	// than guessing.
	if err := d.idem.reserve(key, name, string(spec.Cwd)); err != nil {
		return fleet.Session{}, fmt.Errorf("create: recording intent: %w", err)
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
		return fleet.Session{}, fmt.Errorf("create: staging env: %w", err)
	}
	recordPath := ""
	if !d.bareExec {
		recordPath = d.envRecordPath()
		argv = loginWrap(d.loginShell(), recordPath, envPath, argv)
	}

	// #47, point 5: seed this session's own working directory before the
	// process that would ask about it is even started, closing the same
	// race a periodic-only pass leaves open for a worktree younger than the
	// interval. Best-effort by design — a refusal, a lost race, or the
	// feature being unconfigured must never fail a create it is only trying
	// to help; d.trustSeed is nil-safe (see WithTrustSeed) and the error, if
	// any, is already counted inside it. This session simply meets its
	// Consents path below, exactly as it would have without this.
	if err := d.trustSeed.SeedPath(string(spec.Cwd)); err != nil {
		log.Printf("tmux: trust-seed: %v", err)
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
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	if envPath != "" {
		// The wrapper unlinks it the moment it has read it. This is the case
		// where it never does — the shell died, the agent binary was missing —
		// and a file of values must not outlive the session it was staged for.
		go d.sweepStagedEnv(envPath)
	}

	ref := fleet.SessionRef{Machine: d.machine, ID: name, Name: name}
	if err := d.idem.complete(key, ref); err != nil {
		return fleet.Session{}, fmt.Errorf("create: recording result: %w", err)
	}
	if spec.Resume != "" {
		// #72: recorded now, before there is any way yet to tell whether
		// the runtime actually honoured it — see resumeintent.go.
		d.noteResumeIntent(name, string(spec.Cwd), string(spec.Resume))
	}
	// #84/#85/#86: recorded now, before there is any way yet to tell what
	// became of the pin, the surface, or the prompt — see createrecord.go.
	// The same record List reads back on every later listing, so the 201
	// body built below and the first 200 body are computed from one fact,
	// never two that can drift apart.
	d.noteCreateRecord(name, string(spec.Cwd), spec)
	// colab-fleet #96/#97: recorded now too, for the same "before there is
	// any way to tell what became of it" reason as the create record just
	// above — the marker fact #96 needs, and the identity #97's List/
	// reassertNames will have something to put back if the runtime ever
	// disagrees with it. Cwd/Pane/Created are filled in by noteSessionSet
	// on this session's first List (see its own "stub" branch); this call
	// only knows the name and the marker decision.
	d.noteAssertedName(name, string(spec.Cwd), name, sanitizeName(spec.Marker), markerApplied)

	if recordPath != "" {
		go d.captureEnvironment(name, recordPath)
	}
	if spec.Prompt != "" || spec.TrustCwd {
		go d.settleNewSession(req, ref, built)
	}
	rec, found := d.createRecordFor(name, string(spec.Cwd))
	pins, surface, prompt := sessionFactsFor(rec, found, name)
	// colab-fleet #102: the identity this call just asserted, read back from
	// the same durable record noteAssertedName just wrote above — honestly
	// unresolved, since nothing has read this session back yet and this
	// cannot claim the runtime carries it (that claim is List's to make, on
	// a later read). Same "one fact, not two that can drift" property
	// noteCreateRecord's own comment states for pins/surface/prompt above,
	// and shared with the idempotent-replay returns near the top of this
	// function via identityAssertionForCreate.
	return fleet.Session{
		SessionRef: ref, Cwd: spec.Cwd,
		Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
		IdentityAssertion: d.identityAssertionForCreate(name),
	}, nil
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
	// # Resuming pins the bridge the transcript remembers (#48)
	//
	// `--resume` and `--remote-control` interact in a way that only shows up
	// after something invalidates control channels fleet-wide. The transcript
	// records the channel id the session was bound to; resuming it makes the
	// runtime retry THAT id rather than mint a new one. If the id was orphaned
	// — a multiplexer death, a machine restart, anything that ends the old
	// worker — the retry can fail, and a session that resumed its conversation
	// perfectly comes back unreachable from outside.
	//
	// Measured: 63 sessions lost at once, 67 rebuilt with resume plus a
	// remote-control binding, 37 with no live channel. Retrying by hand
	// recovered 25 of them; 12 were refused permanently, the server having
	// archived the session — and for those the only way to mint a new channel
	// is to start WITHOUT `--resume`, which costs the conversation.
	//
	// Nothing here can prevent that; the trade is the caller's. What this
	// driver now does is stop hiding it: the runtime's own view of the channel
	// is reported as SessionState.ControlChannel (controlchannel.go), so a
	// supervisor can find the affected sessions by reading state instead of
	// grepping panes.
	//
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
	for _, path := range spec.McpConfig {
		// Repeated rather than joined: the flag takes one path per occurrence,
		// and a joined list would be handed to the runtime as a single
		// filename containing a separator — a failure that surfaces as a
		// session missing its tools rather than as an error anyone can read.
		if safeArgvValue(string(path)) {
			argv = append(argv, "--mcp-config", string(path))
		}
	}
	if contextFile != "" {
		argv = append(argv, "--append-system-prompt-file", contextFile)
	}
	return argv
}

// validateMcpConfig refuses a create rather than starting a session that will
// come up without the tools it was asked for.
//
// # Why an unreadable path is a refusal and not a warning
//
// The runtime starts, fails to load the file, and presents a session that looks
// perfectly healthy: it lists, it reads, it accepts input. What it cannot do is
// the work it was created for, and nothing about it says so — the same shape as
// a session started without the environment holding its credentials, which is
// already refused here in the same words. A 201 for a session that is quietly
// not what was asked for is worse than an error at the call site.
//
// Contents are deliberately not read. A driver that parsed these would be
// deciding what a session may talk to, which is a supervisor's judgement (§1).
func validateMcpConfig(paths []fleet.AbsolutePath) error {
	for _, p := range paths {
		path := string(p)
		if path == "" {
			return errors.New("mcpConfig contains an empty path")
		}
		if !safeArgvValue(path) {
			return fmt.Errorf("mcpConfig %q would be read as a flag by the agent, not as a path", path)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("mcpConfig %q must be absolute; this service does not "+
				"resolve a path against a working directory it does not share", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("mcpConfig %q cannot be read here (%v); refusing rather than "+
				"starting a session that will come up without the tools it was created for", path, err)
		}
		_ = f.Close()
	}
	return nil
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

	screenNow := newScreen(captures[target.paneID])
	before := parsePrompt(screenNow)
	if before == nil {
		// colab-fleet #64: this refusal fires whenever the screen has no
		// structured prompt this driver recognises — which is right and
		// common (nothing is being asked) but was worded as though that
		// were the only possibility. A full-screen interface with no
		// composer either paints exactly the same way, and there "a
		// keypress would be consumed by whatever it is doing instead" is
		// false: what it is doing is waiting for that keypress.
		//
		// A composer being present settles it: the runtime is doing
		// something ordinary (idle, or a human mid-message), definitely
		// not blocked on an unrecognised full-screen prompt, since that
		// shape has no composer of its own to paint.
		if _, hasComposer := composerText(screenNow); hasComposer {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is not waiting on a prompt; a keypress would be " +
					"consumed by whatever it is doing instead",
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "no recognised prompt is on screen, and no composer either. If " +
				"the runtime is not asking anything, this refusal is the correct " +
				"one. If it is — a full-screen interface this driver did not " +
				"recognise as a structured prompt — respond() cannot answer it: " +
				"there is no option list or nonce here to answer against. keys() " +
				"can still reach the screen directly (deliversRawKeys: true)",
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
// So delivery happens after Create returns, and only once the interface is
// ready to receive. Failure is not silent: the prompt is simply absent, and
// the session's state says what it is doing instead.
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
// So a prompt this routine may not answer is now a reason to keep waiting, not
// a reason to stop.
//
// # colab-fleet #125: bounded by the session's own lifetime, not a guessed duration
//
// This used to give up after a fixed 90s window, on the theory that a session
// still not ready by then had probably lost its chance. #125 measured the cost
// of that theory being wrong: a session parked on a dialog is answerable in
// ninety seconds or in ten minutes, entirely depending on when a human notices
// it — a duration this service cannot predict and has no business guessing at.
// Racing an arbitrary timer against an unknown human response time discards the
// caller's entire instruction on exactly the sessions most likely to still be
// alive and worth delivering to.
//
// So there is no timer here at all. This loop tries as hard as possible: it
// keeps polling for as long as the session itself exists, and stops only on
// one of three real events — delivered, refused, or the session itself is
// gone (promptReadiness's own `present` signal, confirmed over
// sessionGoneConfirmations consecutive polls so a single listing race is not
// mistaken for a closed session). "The session's own lifetime" is the bound;
// nothing here waits longer than the thing it is waiting on.
//
// # colab-fleet #125: an answer to WHY, available DURING the wait
//
// A design that merely retries harder while staying silent trades one
// invisible failure for another — a session that quietly did nothing becomes a
// service that quietly waits, and a human staring at either gets the same
// nothing. So every poll that finds the session still not ready records ITS
// OWN reason on the create record (notePromptPending, below the switch) —
// "still starting", "parked on a folder-trust dialog awaiting a keypress",
// "the composer already holds other text" — not only a static "pending" flag.
// A caller reading the session mid-wait sees a diagnosis, not a mystery; #86's
// terminal-outcome guarantee (never `null`, always a real value once delivery
// is abandoned) is unchanged and sits alongside it, not instead of it.
//
// # colab-fleet #126: that reason is now a class too, not only prose
//
// The prose above is exactly what a human reads; it is not something a caller
// can branch on. promptReadiness's readinessCheck.waitingOn carries the SAME
// classification the prose is built from — a dialog is fleet.WaitingPrompt, a
// composer holding other text is fleet.WaitingUnsentInput, no composer
// painted at all is fleet.WaitingStarting — and rides along on the identical
// call to notePromptPending, so PromptDelivery.WaitingOn and Evidence can
// never disagree about the same wait.
func (d *Driver) settleNewSession(req fleet.Request, ref fleet.SessionRef, spec fleet.SessionSpec) {
	ctx := context.Background()

	// One consent, spent once — per kind, because a session can meet more than
	// one boot question on the way up. A question re-read on the next poll,
	// because the keypress has not repainted yet, must not be answered twice:
	// the second digit lands in whatever screen replaced it.
	answered := map[fleet.PromptKind]bool{}
	consecutiveGone := 0
	lastEvidence := ""
	for {
		check := d.promptReadiness(ctx, ref.ID)

		if check.checked && !check.present {
			consecutiveGone++
		} else {
			consecutiveGone = 0
		}
		if consecutiveGone >= sessionGoneConfirmations {
			// #125: the session this delivery targeted is gone, so nothing
			// will ever become ready to receive it — that is a real, terminal
			// answer, not a reason to keep polling a session that no longer
			// exists. #86's own rule still applies: resolved as unknown, never
			// left unresolved, because nothing DECLINED this delivery, the
			// target simply stopped existing first.
			if spec.Prompt != "" {
				d.counters.incr(counterInitialPromptSessionGone)
				d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown,
					"the session no longer exists; it ended before this driver "+
						"could deliver the prompt it was created with")
			}
			return
		}

		blocking := check.blocking
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
		case check.ready:
			if spec.Prompt != "" {
				d.deliverInitialPrompt(ctx, req, ref, spec.Prompt)
			}
			return
		default:
			// Still waiting. #125's live half: record WHY, not only THAT —
			// only on change, so a long wait costs one write per state
			// transition, not one every promptPollInterval.
			if spec.Prompt != "" && check.reason != "" && check.reason != lastEvidence {
				d.notePromptPending(ref.ID, check.waitingOn, check.reason)
				lastEvidence = check.reason
			}
		}
		time.Sleep(promptPollInterval)
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
	if err != nil {
		// A transport-level error Send itself already wraps and named. #86:
		// resolved as unknown, not refused — nothing declined this delivery,
		// the attempt itself failed.
		d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown, err.Error())
		return
	}
	if receipt.Outcome != fleet.OutcomeUnknown {
		// Delivered and confirmed, or refused outright — not this call's
		// problem to retry into. #86: resolved with the receipt's own
		// outcome and reason, the same pair send() itself would have
		// answered with had anyone been waiting on this specific call.
		evidence := receipt.Reason
		if evidence == "" {
			evidence = "the driver confirmed the agent received the create-time prompt"
		}
		d.notePromptDelivered(ref.ID, receipt.Outcome, evidence)
		return
	}

	d.counters.incr(counterInitialPromptRetried)
	retry, err := d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	// #101: the resume path used to report `submitted` unconditionally, which
	// is why this compared against it. It now confirms the submit the same
	// way the first attempt does and reports `queued` on the same evidence
	// (§4.3 — this substrate cannot observe agent receipt on either path).
	// Comparing against `queued` here keeps this call site in step with that
	// contract instead of silently degrading to "always retry failed" the
	// moment resume stopped over-claiming.
	if err == nil && retry.Outcome == fleet.OutcomeQueued {
		// #86: delivered on the retry — a different fact from an ordinary
		// first-attempt submission, worth saying so in the evidence rather
		// than reporting it identically.
		d.notePromptDelivered(ref.ID, fleet.OutcomeQueued,
			"delivered on the second attempt; the first could not be confirmed in time")
		return
	}

	// Still sitting there after the one retry the measured pattern earns it.
	// The text and the record of it are exactly where an ordinary stranded
	// send leaves them (composer, d.stranded) — nothing here is lost, only
	// unannounced, which this closes.
	d.counters.incr(counterInitialPromptStranded)
	log.Printf("tmux: initial prompt still unsent after one retry session=%s machine=%s",
		ref.ID, d.machine)
	// #86: resolved as unknown — the text reached the composer and could
	// not be confirmed submitted after one retry; it is sitting there
	// unsent, exactly as the log line above and d.stranded already record,
	// now also readable from the create response without a log to grep.
	d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown,
		"the text reached the composer and could not be confirmed submitted after one "+
			"retry; it is sitting there unsent — see resumeIfStranded")
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

// readinessCheck is promptReadiness's full answer. #125 asks for two things a
// bare (ready, blocking) pair cannot carry: whether the session is even still
// there to wait on, and a live, human-readable reason for whatever state was
// found — settleNewSession's own diagnosis of "still starting" vs "parked on
// a dialog" vs "the composer holds something else" comes from here, not from
// re-deriving it against the raw screen a second time.
type readinessCheck struct {
	// ready means the composer exists and is empty: the interface has
	// painted, and nothing is already sitting in it.
	ready bool
	// blocking is the question the session is parked on, when one is on
	// screen — nil otherwise. Returned as a value rather than folded into
	// `reason` because settleNewSession's own consent logic needs the
	// structured Kind/Options/Nonce, not prose.
	blocking *fleet.SessionPrompt
	// present reports whether this session was found at all in the
	// enumeration this check ran, INCLUDING a pane whose process has already
	// exited (tmux's own dead flag) — both mean nothing will ever become
	// ready here. Only meaningful when checked is true.
	present bool
	// checked reports whether the enumeration itself succeeded. False means
	// this check learned nothing — a transient listing failure, not evidence
	// the session is gone (see the "pane can vanish between listing and
	// capture" caution elsewhere in this file) — and present must not be
	// read in that case.
	checked bool
	// reason is prose for a human, populated whenever checked is true. Never
	// parsed (§2.3) — the same discipline every other Evidence field in this
	// package holds itself to.
	reason string
	// waitingOn (colab-fleet #126) is the machine-readable class for reason,
	// computed by the SAME branch that produces the prose so the two can
	// never disagree — settleNewSession passes it straight through to
	// notePromptPending. Empty on `ready` (nothing to wait on any more) and
	// on a check this driver could not classify further than "checked but
	// unhelpful" (the session was not visible, or its process had already
	// exited) — unclassified there, never a guess.
	waitingOn fleet.WaitingReason
}

func (d *Driver) promptReadiness(ctx context.Context, id string) readinessCheck {
	callCtx, cancel := d.bounded(ctx)
	defer cancel()
	rows, captures, err := d.enumerate(callCtx)
	if err != nil {
		return readinessCheck{}
	}
	for _, r := range rows {
		if r.session != id {
			continue
		}
		if r.dead {
			return readinessCheck{checked: true,
				reason: "the session's process has already exited"}
		}
		sc := newScreen(captures[r.paneID])
		if p := parsePrompt(sc); p != nil {
			p.Kind = classifyPromptKind(p)
			reason := "parked on a prompt this driver does not recognise; " +
				"a human may need to look at the session directly"
			if p.Kind != "" {
				reason = fmt.Sprintf("parked on a %q dialog awaiting a keypress", string(p.Kind))
			}
			if p.Question != "" {
				reason += fmt.Sprintf(" (%q)", p.Question)
			}
			return readinessCheck{checked: true, present: true, blocking: p, reason: reason,
				waitingOn: fleet.WaitingPrompt}
		}
		text, found := composerText(sc)
		if !found {
			return readinessCheck{checked: true, present: true,
				reason:    "still starting: the interface has not painted a composer yet",
				waitingOn: fleet.WaitingStarting}
		}
		if text != "" {
			return readinessCheck{checked: true, present: true,
				reason: "the composer already holds other text; waiting for it to " +
					"clear before this prompt can be placed",
				waitingOn: fleet.WaitingUnsentInput}
		}
		return readinessCheck{checked: true, present: true, ready: true,
			reason: "the composer is empty and ready"}
	}
	return readinessCheck{checked: true,
		reason: "the session is not visible to this driver right now"}
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
// Called with what the current read saw: whether any session's SCREEN showed
// a limit notice (sawLimit/hint — upgrade-only per #54: this is what may
// promote the account INTO a block, never what clears one) and whether any
// session was observed working (sawWorking).
//
// record and verdict are #56's structured source: the runtime's own record
// for whichever session the current read found already reporting
// quota_blocked, when one could be resolved. Three things follow from it —
//
//   - recordAPIError with category "rate_limit" corrects Since to the
//     refusal's own timestamp, and ResetHint to the record's own words,
//     rather than trusting the screen's window-scraped copy of the same
//     sentence.
//   - recordCleanTurn is durable, positive proof the account is not
//     refusing work — the same authority sawWorking already has, from a
//     source that survives the notice scrolling off the one pane that
//     showed it. It clears the block alongside sawWorking, never in place
//     of it: a driver with no record store configured must keep exactly
//     today's behaviour.
//   - recordUnavailable changes nothing here; the caller falls back to
//     first-sighting semantics and says so in SessionState.Evidence
//     (quotaBlockedState), never presenting one as the other.
func (d *Driver) noteQuotaBlock(sawLimit bool, hint string, sawWorking bool, record apiErrorFact, verdict recordVerdict, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case sawWorking, verdict == recordCleanTurn:
		if d.quota != nil {
			d.quota = nil
			d.quotaSinceObserved = false
			d.saveQuotaLocked()
		}
	case sawLimit && d.quota == nil:
		since, sinceObserved, resetHint := now, false, hint
		if verdict == recordAPIError && record.category == "rate_limit" {
			since, sinceObserved = record.at, true
			if h := record.resetHintText(); h != "" {
				resetHint = h
			}
		}
		d.quota = &fleet.QuotaBlock{Since: since, ResetHint: resetHint}
		d.quotaSinceObserved = sinceObserved
		d.saveQuotaLocked()
	case sawLimit && verdict == recordAPIError && record.category == "rate_limit" && !d.quotaSinceObserved:
		// A block already exists from an earlier cycle's first sighting,
		// and the record has only now resolved (a conversation lookup can
		// legitimately lag a cycle, or the block was carried in from a
		// restart before a record was ever consulted). Upgrade Since in
		// place rather than waiting for the block to clear and re-enter.
		d.quota.Since = record.at
		d.quotaSinceObserved = true
		if h := record.resetHintText(); h != "" && d.quota.ResetHint == "" {
			d.quota.ResetHint = h
		}
		d.saveQuotaLocked()
	case sawLimit && hint != "" && d.quota.ResetHint == "":
		// A later notice may carry a reset time the first one did not.
		d.quota.ResetHint = hint
		d.saveQuotaLocked()
	}
}

// quotaBlock reports the remembered account block, if any, and whether its
// Since is the runtime's own record of the refusal rather than this
// driver's first sighting of the notice (#56) — see quotaBlockedState for
// where that distinction becomes something a caller can read.
func (d *Driver) quotaBlock() (*fleet.QuotaBlock, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quota == nil {
		return nil, false
	}
	q := *d.quota
	return &q, d.quotaSinceObserved
}

// quotaOnly discards quotaBlock's sinceObserved half for call sites that
// only need the wire fact — fleet.SourceStatus.Quota carries no evidence
// string of its own to put the distinction in (unlike a session's own
// SessionState, which quotaBlockedState annotates).
func quotaOnly(q *fleet.QuotaBlock, _ bool) *fleet.QuotaBlock { return q }

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
//
// sinceObserved says which source q.Since came from (#56's quotaBlock
// accessor) and is spelled out in Evidence rather than added as a new field
// on the wire QuotaBlock — the same "say so in evidence, do not add a
// silent field" rule this issue was written to enforce.
func quotaBlockedState(st fleet.SessionState, q *fleet.QuotaBlock, sinceObserved bool) fleet.SessionState {
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
	if sinceObserved {
		st.Evidence += "; since is the runtime's own record of the refusal"
	} else {
		st.Evidence += "; since is when this driver first observed the notice, " +
			"not when the refusal happened — the runtime's own record could not confirm it"
	}
	if q.ResetHint != "" {
		st.Evidence += " (reported reset: " + q.ResetHint + ")"
	}
	return st
}

// quotaPersisted is what this driver actually writes to the state store —
// fleet.QuotaBlock, embedded rather than nested, plus the one bit #56 needs
// that has no home on that wire type (see quotaBlockedState's comment on
// why it stays out of the wire shape).
//
// Embedded, specifically, rather than a nested `block` field: this key
// already holds a bare QuotaBlock on any instance that persisted one before
// #56, and encoding/json has no notion of a schema migration — a nested
// field would decode an old file's top-level `since`/`resetHint` into
// nothing, silently dropping an in-force block on the first restart after
// this upgrade (found and rejected in review, not deployed and then
// noticed). Embedded, `since` and `resetHint` stay exactly where an old
// file already has them; `sinceObserved` is simply absent on a file no
// version before #56 ever wrote, and decodes to its correct, honest
// default: false, "not record-confirmed" — true of every block this driver
// had ever persisted before this field existed.
type quotaPersisted struct {
	fleet.QuotaBlock
	SinceObserved bool `json:"sinceObserved,omitempty"`
}

func (d *Driver) saveQuotaLocked() {
	if d.store == nil {
		return
	}
	if d.quota == nil {
		_ = d.store.Save("quota", quotaPersisted{})
		return
	}
	_ = d.store.Save("quota", quotaPersisted{QuotaBlock: *d.quota, SinceObserved: d.quotaSinceObserved})
}

func (d *Driver) loadQuota() {
	if d.store == nil {
		return
	}
	var p quotaPersisted
	if found, err := d.store.Load("quota", &p); err == nil && found && !p.Since.IsZero() {
		block := p.QuotaBlock
		d.quota = &block
		d.quotaSinceObserved = p.SinceObserved
	}
}

// strandedRecord is what noteStranded persists: the text this driver
// delivered and could not confirm, and enough beside it (§5.4) to tell a
// live session from one that merely recycled the same id.
type strandedRecord struct {
	Text string    `json:"text"`
	Cwd  string    `json:"cwd"`
	At   time.Time `json:"at"`

	// ComposerDigest (colab-fleet #112) fingerprints the composer's own
	// content at the moment this record was made — screenDigest of the SAME
	// text composerText() would read back, not of Text itself, because a
	// multi-line paste renders as a collapsed marker rather than the literal
	// bytes (F49) and Text/the rendered composer can legitimately differ.
	//
	// This is what makes the #112 replace path safe: a later attempt to
	// clear this composer and deliver something else may proceed ONLY when
	// the composer's CURRENT digest still matches this one, which proves
	// nothing has been typed there since this driver made this record. A
	// mismatch — or an empty digest, from a record made before this field
	// existed — degrades to the honest refusal naming `discard`, never a
	// guess. Empty on a record from before colab-fleet #112.
	ComposerDigest string `json:"composerDigest,omitempty"`
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
//
// composerDigest is screenDigest of the composer's CURRENT rendered content
// at the moment of this call — see strandedRecord.ComposerDigest for why
// this is not simply screenDigest(text).
func (d *Driver) noteStranded(id, cwd, text, composerDigest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stranded == nil {
		d.stranded = map[string]strandedRecord{}
	}
	d.stranded[id] = strandedRecord{Text: text, Cwd: cwd, At: d.now(), ComposerDigest: composerDigest}
	d.saveStrandedLocked()
}

// strandedRecordFor reports the stranded record for this session, if a live
// one exists — the same corroboration and retention discipline
// strandedMatches applies (§5.4: id + cwd, not id alone; strandedRetention),
// without requiring the text to match. strandedMatches itself is kept
// exactly as it was for the resume path (colab-fleet #112's plan: reuse the
// existing exact-match call there rather than re-deriving its equality from
// this accessor); this is for the three cases the resume path does not
// cover — same text without ResumeIfStranded, different text, and (from the
// replace path) whatever text is there now.
func (d *Driver) strandedRecordFor(id, cwd string) (strandedRecord, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepStrandedLocked()
	prior, ok := d.stranded[id]
	if !ok || prior.Cwd != cwd {
		return strandedRecord{}, false
	}
	return prior, true
}

// currentComposerDigest re-captures one pane and fingerprints whatever its
// composer currently holds — screenDigest of the same text composerText()
// would read back, the digest strandedRecord.ComposerDigest and #112's
// replace path both compare against. Empty when the capture fails or the
// composer reads empty; a caller treats that the same way an absent
// ComposerDigest is already treated elsewhere — degrade to the honest
// answer, never guess.
func (d *Driver) currentComposerDigest(ctx context.Context, paneID string) string {
	sc, ok := d.captureForClassify(ctx, paneID)
	if !ok {
		return ""
	}
	pending, ok := composerText(sc)
	if !ok || pending == "" {
		return ""
	}
	return screenDigest(pending)
}

// tryReplaceStranded is colab-fleet #112's opt-in door out of the busy-
// composer refusal: clear a composer this driver's own record says IT
// stranded, then let the caller's ORIGINAL Send fall through to deliver
// different text in its place.
//
// Safety rests entirely on record.ComposerDigest matching the composer's
// CURRENT content (pending, already read by the caller before this is
// called) — proof that nothing has been typed there since this driver made
// the record. A record with no digest (predates colab-fleet #112) or a
// digest that no longer matches is degraded to an honest refusal naming
// `discard`, never guessed past; see driver.SendOptions.ReplaceIfStranded
// for why this can never be inferred from anything less.
//
// cleared=true means the composer is now empty and the stranded record has
// already been forgotten — the caller falls through to the ordinary
// delivery path with the NEW text. cleared=false means the returned receipt
// IS Send's answer, unchanged. err is non-nil only for a failed multiplexer
// call, never for an honest "did not clear" (that is receipt, not err).
//
// expectedLines is composerVisualLines' count for the same screen pending
// was read from — the caller already has that screen, see clearComposer's
// own doc comment for why this is what a press budget is sized to now
// (colab-fleet#129).
func (d *Driver) tryReplaceStranded(ctx context.Context, ref fleet.SessionRef, target *paneRow, record strandedRecord, pending string, expectedLines int) (receipt fleet.DeliveryReceipt, cleared bool, err error) {
	digest := screenDigest(pending)

	// #87: refuse outright, before pressing anything, if a full,
	// content-sized pass against this EXACT residue already proved it will
	// not move — the same discipline Discard itself applies before ever
	// touching the pane, and Discard's own doc comment is where the #129
	// reasoning for why this still holds is recorded.
	if attempts := d.futileClearAttempts(ref.ID, target.cwd, digest); attempts > 0 {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "a previous attempt to clear this exact composer residue made no " +
				"progress at all (#87); refusing to press the same keys into a composer " +
				"already proven not to move. Read the session and discard it directly " +
				"instead, or wait for the residue to change",
		}, false, nil
	}

	if record.ComposerDigest == "" || record.ComposerDigest != digest {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "composer holds a delivery this driver made into this session, but its " +
				"content has changed since that delivery was recorded (or the record " +
				"predates this check) and this driver cannot confirm it is still only its " +
				"own text — refusing to guess. Read the session and discard the composer " +
				"(discard?expect=<composerDigest>) before sending again",
		}, false, nil
	}

	left, moved, didClear, err := d.clearComposer(ctx, target.paneID, ref.ID, target.cwd, pending, expectedLines)
	if err != nil {
		return fleet.DeliveryReceipt{}, false, err
	}
	if !didClear {
		reason := "attempted to clear this driver's own stranded delivery to make room " +
			"for the replacement text, but the composer did not fully empty"
		if !moved {
			reason += " and made no progress at all — a further replaceIfStranded attempt " +
				"is refused (#87) until the residue changes; discard the session directly " +
				"instead"
		} else {
			reason += fmt.Sprintf("; %d character(s) remain. The stranded record is kept, "+
				"but its digest no longer matches this composer, so a retry will be told to "+
				"discard first rather than clear again — read the session and discard the "+
				"composer directly", len(left))
		}
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, false, nil
	}

	d.forgetStranded(ref.ID)
	return fleet.DeliveryReceipt{}, true, nil
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

// futileClear is what noteFutile records: a composer residue Discard's
// clear loop already spent one full pass on and could not move (#87).
//
// Cwd travels with it for the same reason it does on strandedRecord (§5.4):
// an id is recyclable, and matching on id alone would let a record made for
// one session's composer be misread as describing an unrelated session
// that later reused the same id. ResidueDigest is what makes the match
// exact rather than merely "this id had trouble once" — a caller's own
// progress (a NEW residue, a new digest) must never be blocked by a record
// that describes a DIFFERENT, earlier piece of text.
type futileClear struct {
	Cwd           string
	ResidueDigest string
	Attempts      int
	At            time.Time
}

// noteFutile records that a clear pass against this exact residue produced
// no movement at all. Called only from Discard's own "unchanged" branch.
//
// Attempts counts consecutive passes against the SAME (cwd, residue) pair;
// a new residue (the composer changed, however slightly) or a different cwd
// (the id was recycled) starts back at 1 rather than accumulating, because
// neither describes the state this attempt just observed.
func (d *Driver) noteFutile(id, cwd, residueDigest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.futile == nil {
		d.futile = map[string]futileClear{}
	}
	attempts := 1
	if prior, ok := d.futile[id]; ok && prior.Cwd == cwd && prior.ResidueDigest == residueDigest {
		attempts = prior.Attempts + 1
	}
	d.futile[id] = futileClear{Cwd: cwd, ResidueDigest: residueDigest, Attempts: attempts, At: d.now()}
}

// futileClearAttempts reports how many consecutive times a pass against
// this EXACT residue has already produced zero movement — 0 means no
// matching record, i.e. Discard has not yet spent a pass on this text.
//
// Corroborated on cwd, not id alone (§5.4, same rule strandedMatches
// applies): a record for a recycled id describes a different session's
// composer and must not gate this one. A record older than
// futileClearRetention is treated as absent, the same reasoning
// sweepStrandedLocked already applies to stranded records.
func (d *Driver) futileClearAttempts(id, cwd, residueDigest string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepFutileLocked()
	prior, ok := d.futile[id]
	if !ok || prior.Cwd != cwd || prior.ResidueDigest != residueDigest {
		return 0
	}
	return prior.Attempts
}

// forgetFutile drops any futile-clear record for id. Called once Discard
// actually empties the composer — evidence that whatever was stopping a
// prior pass no longer applies, so a future stall against a NEW residue
// deserves a fresh full pass, not a record left over from a different one.
func (d *Driver) forgetFutile(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.futile, id)
}

// sweepFutileLocked drops records older than futileClearRetention. Caller
// holds d.mu. Cheap and unconditional, same shape as sweepStrandedLocked:
// at most one entry per session with a clear pass genuinely still stuck.
func (d *Driver) sweepFutileLocked() {
	if len(d.futile) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.futile {
		if now.Sub(rec.At) > futileClearRetention {
			delete(d.futile, id)
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

// deliveryMark is what noteDelivery persists: the moment of this driver's
// most recent delivery into one session's composer, and a memo of the last
// turn count successfully computed from it (colab-fleet #111).
type deliveryMark struct {
	// Cwd corroborates the same way every other durable record here does
	// (§5.4) — an id alone is recyclable.
	Cwd string `json:"cwd"`
	// At is the delivery this mark's count is "since". Never moved by a
	// resume finishing the SAME delivery — only a fresh paste starts a new
	// mark.
	At time.Time `json:"at"`

	// Count and Size are a memo-and-latch pair, not raw state: Count is the
	// last successfully computed turn count, Size is the runtime record's
	// file size at the moment that count was computed. A later read whose
	// record size is UNCHANGED reuses Count instead of re-parsing up to
	// 256KiB; a later read that cannot resolve the record at all (a
	// transient stat/open failure) reports this Count rather than flapping
	// `turns` to absent for a reason that has nothing to do with whether the
	// session is alive. Zero Size means no count has ever been computed for
	// this mark yet — the honest starting state, not "zero turns".
	Count int   `json:"count"`
	Size  int64 `json:"size"`
}

// deliveryFile is the durable document — one entry per session with a
// remembered delivery. Its own file, same reasoning strandedFile already
// gives: a create key, a stranded delivery and a delivery mark are three
// different concerns with three different shapes and three different
// lifetimes.
type deliveryFile struct {
	Records map[string]deliveryMark `json:"records"`
}

const deliveryFileName = "delivery-mark"

// noteDelivery records that this driver just pasted text into a session's
// composer — see the field's own doc comment on Driver.delivered for why
// this is the single write site every downstream outcome shares, and why a
// resume must never call this.
func (d *Driver) noteDelivery(id, cwd string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delivered == nil {
		d.delivered = map[string]deliveryMark{}
	}
	d.delivered[id] = deliveryMark{Cwd: cwd, At: d.now()}
	d.saveDeliveryLocked()
}

// deliveryMarkFor reports the live delivery mark for a session, if any —
// the same id+cwd corroboration and retention discipline as
// strandedRecordFor, on deliveryMarkRetention's longer clock.
func (d *Driver) deliveryMarkFor(id, cwd string) (deliveryMark, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepDeliveryLocked()
	prior, ok := d.delivered[id]
	if !ok || prior.Cwd != cwd {
		return deliveryMark{}, false
	}
	return prior, true
}

// updateDeliveryMarkCount refreshes a mark's memoised count after a
// successful turnsSince read — see deliveryMark.Count/Size. A no-op if the
// mark has since been forgotten or replaced by a new delivery (its Cwd or At
// would then no longer match what the caller resolved the count against);
// silently doing nothing in that case is correct, not a bug swallowed — the
// caller's OWN read already has its answer, this only refreshes the cache
// for the NEXT one.
func (d *Driver) updateDeliveryMarkCount(id, cwd string, at time.Time, count int, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prior, ok := d.delivered[id]
	if !ok || prior.Cwd != cwd || !prior.At.Equal(at) {
		return
	}
	prior.Count = count
	prior.Size = size
	d.delivered[id] = prior
	d.saveDeliveryLocked()
}

// forgetDelivery drops a session's delivery mark. Called from Close (#111,
// mirroring forgetStranded): a destroyed session's `turns` denominator is
// gone with it, the same way its composer is.
func (d *Driver) forgetDelivery(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.delivered, id)
	d.saveDeliveryLocked()
}

// sweepDeliveryLocked drops marks older than deliveryMarkRetention. Caller
// holds d.mu. Same shape as sweepStrandedLocked, longer clock.
func (d *Driver) sweepDeliveryLocked() {
	if len(d.delivered) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.delivered {
		if now.Sub(rec.At) > deliveryMarkRetention {
			delete(d.delivered, id)
		}
	}
}

func (d *Driver) saveDeliveryLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(deliveryFileName, deliveryFile{Records: d.delivered})
}

// loadDelivery restores delivery marks at startup, sweeping anything already
// past deliveryMarkRetention — same "sweep on load" shape as loadStranded.
func (d *Driver) loadDelivery() {
	if d.store == nil {
		return
	}
	var f deliveryFile
	found, err := d.store.Load(deliveryFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivered = f.Records
	d.sweepDeliveryLocked()
	d.saveDeliveryLocked()
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

// restoredWaitingInputSince reports whether the CURRENT waiting_input status
// for id is known — either from this instance's own in-memory observation,
// or from a record persisted to disk before this instance started — to
// predate this service's current process, and since when.
//
// This is the exact "restored" fact stampSinceLocked already computes for a
// State/List read's Evidence line (see that function, immediately below):
// colab-fleet #124's own field report quoted that evidence verbatim —
// "unchanged for 49m0s (age carried from before this service restarted)".
// What #124 found missing is that Discard's OWN failure messages
// (discardIncomplete, discardProvenFutile) never carried this same fact, so
// an operator seeing a 409 had to separately call State(), notice the
// phrase, and do the correlation by hand — which is exactly what #124's
// report spent two follow-up comments doing. This is factored out as its
// own method (rather than inlined at Discard's call sites) so it reads the
// SAME two sources stampSinceLocked reads, in the same order, and can never
// silently drift into a second, slightly different definition of
// "restored".
//
// Locks d.mu itself — unlike stampSinceLocked, which assumes the caller
// already holds it (it runs inside State/List's own locked section).
// Discard does not hold d.mu at its call site, so this manages its own.
func (d *Driver) restoredWaitingInputSince(id string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if prior, ok := d.observed[id]; ok && prior.status == fleet.StatusWaitingInput {
		return prior.statusSince, prior.sinceRestored
	}
	if rec, ok := d.persistedRecord(id); ok &&
		rec.Status == string(fleet.StatusWaitingInput) && !rec.StatusSince.IsZero() {
		return rec.StatusSince, true
	}
	return time.Time{}, false
}

// restartNote reports restoredWaitingInputSince's fact as a ready-to-append
// clause, and ok=false when it does not apply to id.
//
// The phrase deliberately reuses stampSinceLocked's own wording ("carried
// from before this service restarted") verbatim rather than inventing new
// prose, so an operator who has already seen that phrase on a State() read
// recognizes it immediately here.
//
// Factored out of what used to be withRestartNote's own body so colab-fleet
// #131 can reuse the fact on Send's OutcomeUnknown receipts (see
// withRestartNoteReason) without also carrying Discard's remedy clause below
// — that clause is specific to a stuck composer C-u cannot move; Send's own
// Reason strings already say how to retry, and appending Discard's advice
// there would be wrong for what the caller is looking at.
func (d *Driver) restartNote(id string) (string, bool) {
	since, restored := d.restoredWaitingInputSince(id)
	if !restored {
		return "", false
	}
	return fmt.Sprintf("this composer's unsent-input status was already "+
		"holding before this service's current process started (age carried "+
		"from before this service restarted, since %s)",
		since.Format(time.RFC3339)), true
}

// withRestartNote appends restartNote's fact, plus Discard's own remedy
// clause, to err's message when it applies to id, and returns err unchanged
// otherwise (including when err is nil). Wrapped with %w so
// errors.Is(_, ErrAmbiguousTarget) still holds on the result — every caller
// of Discard's error checks that kind, and appending a note by plain string
// concatenation instead would silently break that check for exactly the
// sessions this note is meant to help.
func (d *Driver) withRestartNote(id string, err error) error {
	if err == nil {
		return err
	}
	note, ok := d.restartNote(id)
	if !ok {
		return err
	}
	return fmt.Errorf("%w; %s — closing the session is the one remedy known "+
		"to work against a residue in that condition", err, note)
}

// withRestartNoteReason appends restartNote's fact to reason when it applies
// to id, and returns reason unchanged otherwise. colab-fleet #131: the same
// correlation Discard's 409s carry via withRestartNote, reused on Send's own
// OutcomeUnknown receipts — a caller retrying a swallowed submit or an
// unconfirmed paste should not have to cross-reference a separate State()
// read to learn the session's waiting_input status predates this service's
// current process. Unlike withRestartNote this never wraps an error — every
// Send call site below already returns (fleet.DeliveryReceipt, nil) on this
// path, so there is no error kind to preserve.
func (d *Driver) withRestartNoteReason(id, reason string) string {
	note, ok := d.restartNote(id)
	if !ok {
		return reason
	}
	return reason + "; " + note
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
//
// colab-fleet #104: which branch fired, and how long this call waited before
// it did, are both recorded on the way out — see counters.go's
// counterSubmitConfirmed* / counterSubmitConfirmLatency* doc comments for why.
// Recorded here, at the one place both are known, rather than pushed onto
// the callers: there are two of them (an ordinary send and the
// resumeIfStranded path), and duplicating this at each would risk exactly
// the drift #104 is trying to make observable.
func (d *Driver) confirmSubmitted(ctx context.Context, paneID string, key pasteKey, atCount int) bool {
	start := d.now()
	deadline := start.Add(submitConfirmWindow)
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			if text, found := composerText(sc); found && text == "" {
				d.recordConfirmed(counterSubmitConfirmedByComposerEmpty, d.now().Sub(start))
				return true
			}
		}
		if atCount > 0 && d.paintedMarkers(ctx, paneID)[key] < atCount {
			d.recordConfirmed(counterSubmitConfirmedByMarkerCleared, d.now().Sub(start))
			return true
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			d.counters.incr(counterSubmitConfirmTimeout)
			return false
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterSubmitConfirmTimeout)
			return false
		case <-time.After(submitConfirmInterval):
		}
	}
}

// recordConfirmed increments the counter naming which signal decided a
// confirmSubmitted call, plus the latency bucket it took to decide it.
// Factored out only because confirmSubmitted now has two call-site returns
// that both need it; it carries no logic of its own beyond confirmLatencyBucket.
func (d *Driver) recordConfirmed(bySignal string, elapsed time.Duration) {
	d.counters.incr(bySignal)
	d.counters.incr(confirmLatencyBucket(elapsed))
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
