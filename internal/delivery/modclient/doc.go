// Package modclient is the service's side of an optional external delivery
// module: it starts the module's helper program, speaks the module protocol to
// it over the program's stdio, keeps it supervised, and turns everything that can
// go wrong into an error a caller can act on.
//
// It knows nothing about sessions, tmux, routing or receipts. It is a typed,
// concurrent request/response client over one child process, plus the pure
// helpers around it (discovery, the child's environment, the hello check, the
// line reader). What to DO with a module — when to offer it a message, when to
// fall back to the built-in path — is the caller's, because only the caller
// knows what a message is. The one thing this package never does is decide
// that a request can be repeated somewhere else; it says whether it can.
//
// # The protocol, in one screen
//
// A module is an executable started as `<path> serve` with stdin and stdout
// piped. Everything is newline-delimited UTF-8 JSON, one object per line.
//
//	← {"event":"hello","module":"…","protocol":1,"version":"…","ops":[…],"reservedEnvPrefixes":[…]}
//	→ {"id":"7","op":"send","args":{…}}
//	← {"id":"7","ok":true,"result":{…}}
//	← {"id":"7","ok":false,"error":{"code":"not-live","message":"…","retryable":true}}
//
// The module says hello first, within HelloTimeout. The service then sends
// requests carrying an id; the module serves them CONCURRENTLY and answers each
// by id, in any order. There are six operations — prepare-launch, attach, send,
// confirm, close, health — and a module that does not advertise all six is
// refused. A negative attach probe is a result (live:false), not an error. The
// types in protocol.go are the wire contract; unknown fields are ignored in both
// directions, so either side can grow without breaking the other.
//
// stderr is the module's diagnostic channel. It is logged, one line at a time,
// truncated and quoted (%q) so that nothing in it can forge a log line, and it is
// never parsed.
//
// # Life cycle
//
// One supervisor goroutine owns the child. It starts the process, reads the
// first line, validates it (ParseHello), probes health once, and only then
// declares the module available and calls OnReady. From then on:
//
//   - the child exiting, or its pipe breaking, makes the module unavailable,
//     fails every request still pending on it, and restarts it after an
//     exponential backoff (BackoffMin doubling to BackoffMax; a child that stays
//     up for BackoffResetAfter earns a fresh start at BackoffMin);
//   - a REFUSED hello (wrong protocol, a missing operation, invalid JSON, an
//     oversize or silent first line, a malformed reserved prefix) kills the child
//     and disables the module until the daemon restarts, with the reason logged
//     once. A refusal is deterministic — the same binary says the same thing
//     next time — so retrying would only be noise. A child that dies BEFORE
//     saying hello is not a refusal: it is a crash, and is restarted like one;
//   - Stop closes the child's stdin (the module drops its connections but keeps
//     its on-disk state, so the next start can re-attach), waits ShutdownGrace
//     for it to exit, then kills its process group.
//
// # Generations, and why request ids never repeat
//
// Every accepted hello starts a new generation (Generation is 0, then 1, 2, …).
// A lane attached in one generation does not exist in the next, which is why
// OnReady carries the generation: the caller re-attaches its lanes from there.
// OnReady fires once per generation, the first time that generation passes a
// health probe — not again when the same child merely recovers from a bad probe.
//
// Request ids come from one counter that is never reset, so an answer from a
// dead child can never be mistaken for the answer to a request made to its
// successor. Each child also has its own pending table and one reader goroutine,
// and a response whose generation is not the current one is dropped, so the
// separation does not rest on the ids alone.
//
// # Not sent, versus lost — the distinction this package exists to keep
//
// Every failure a caller can see is one of these, and callers must treat them
// differently:
//
//   - ErrNotSent: no byte of the request reached the module — it was refused
//     locally (no connected child, the caller's context was already done), or
//     it was still queued when the caller gave up, or the pipe was already
//     broken. Nothing happened on the module's side. Falling back to another
//     delivery path is SAFE.
//   - ErrLost: at least one byte was written and no usable answer came back — a
//     deadline, the child dying, an answer that broke the protocol. The module
//     MAY have acted. The outcome is unknown and the request must NEVER be
//     resent, replayed or repeated on another path: the message may already be
//     on its way, and a second copy is worse than a slow answer.
//   - *Error: the module answered ok:false. That is a complete answer — the
//     request reached the module and was decided — so it is neither of the above.
//     Its Code is one of the protocol's stable codes; its Message is prose and is
//     untrusted.
//
// A request is "written" from the moment its frame is handed to the pipe, which
// is deliberately conservative: if the writer is inside Write when the caller
// gives up, some bytes may have gone, so that is ErrLost. Only a frame that
// certainly never started (still in the writer's queue, or skipped by it because
// its caller had left) is ErrNotSent. A module's deadline miss additionally
// satisfies errors.Is(err, ErrDeadline); the caller's own deadline or
// cancellation is reported as the context's error, wrapped the same way.
//
// # Deadlines, suspicion and health
//
// The client enforces its own per-request deadline (the module is not trusted to
// answer in time): prepare-launch Deadlines.Prepare, attach TimeoutMs plus
// AttachExtra (Default when TimeoutMs is 0), everything else Default — with send
// and confirm extended to cover the module-side wait they were asked for. A miss
// counts as deadline_missed, marks the module Suspect (so Usable turns false at
// once) and triggers an immediate health probe; a passing probe clears the
// suspicion. Health runs once at start and every HealthInterval. A probe that
// answers ok:false makes the module unavailable at once but keeps the child, which
// says it is alive and may recover. A probe that errors is one strike; two in a
// row make the module unavailable, and if both were deadline misses the child is
// killed so the restart path runs. A failed probe is retried after
// HealthRetryAfter, not a whole interval later.
//
// # What was left open, and decided here
//
// The specification fixes the protocol and the counters; these are the details it
// leaves to this package, decided and recorded so the integration can rely on
// them.
//
//   - Operations are attempted whenever a hello-accepted child is connected,
//     whatever the state. Usable is POLICY for callers deciding whether to OFFER a
//     module a message; the operations themselves are not gated on it, so a
//     lane can still be closed on a module that is no longer trusted with a send.
//     With no connected child every operation fails ErrNotSent.
//   - OnReady is once per generation (see above). The first health probe that
//     fails does not fire it; the first later one that passes does.
//   - A health answer refreshes what a hello carries: the reserved prefixes (each
//     validated, invalid ones dropped, only when the field is present) and the
//     version. A per-lane Health query (LaneKey set) is returned to the caller but
//     does not replace LastHealth or move the state machine.
//   - Reserved prefixes are retained after the child exits, so a guard built on
//     them does not lapse while the module restarts.
//   - A request that would exceed the frame limit (MaxLineBytes) is refused
//     locally with an *Error of code too-large, unsent, rather than written.
//   - A result must be a JSON object. An absent, null or scalar result — or a
//     laneKey / sendId that could not be echoed back safely, or an env entry with
//     an invalid name — is an unusable answer: it satisfies ErrBadResponse and
//     ErrLost together, because the request did reach the module. A null result
//     in particular must never decode to a zero value a caller could read as
//     "nothing was written".
//   - A laneKey is opaque. A key with path separators is accepted verbatim; the
//     rule that no path is ever built from it belongs to the caller. Control
//     characters and keys over 128 bytes are refused.
//   - A hello reserving more than 64 prefixes is refused; retained lists of
//     operations, prefixes and text fields are capped, and control characters in
//     any module-supplied text become '?'.
//   - The retained JSON blobs of a health result (supportedClaude, lanes,
//     counters) are dropped when over 64 KiB.
//   - stderr logging is capped at 200 lines per child and 512 bytes per line.
//   - After Stop the state is StateDisabled ("stopped"); a refused module keeps
//     its refusal as the reason.
//   - Discovery uses Lstat: a symbolic link is not followed and so is not a
//     module. The modules directory's own permissions are not judged.
//   - ChildEnv cannot tell an unset variable from an empty one (a getenv
//     function reports both as ""), so an empty forwarded variable is absent. A
//     forwarded name in the FLEET_ namespace (any case) is dropped and reported,
//     including FLEET_STATE_DIR, which comes only from the state directory
//     argument.
//   - Config.ShutdownGrace and Config.HealthRetryAfter are additions to the
//     brief, so the tests can use short values.
//
// # Security posture
//
// The module is a same-user local child and every byte it sends is untrusted.
// Lines are capped at MaxLineBytes and an oversize line is drained without being
// buffered; unknown fields are ignored; every retained string is size-capped and
// stripped of control characters; a response never chooses a file to open — the
// transcript path a send returns is carried, and this package never opens it;
// nothing a module says is ever placed on a command line or executed. The child's
// environment is built from scratch (ChildEnv) and its working directory is the
// system temporary directory, so a secret in the daemon's environment reaches it
// only if an operator named it, and the FLEET_ namespace never does.
//
// # Testing
//
// The tests drive a fake module (package modtest) in two forms that share one
// executor: in-process over in-memory pipes, for speed and determinism, and as a
// real process — the test binary re-executed through a shell wrapper — to
// exercise the real launcher's pipes, process group, minimal environment, stderr
// and kill. TestMain in this package calls modtest.MaybeServe for the second form.
package modclient
