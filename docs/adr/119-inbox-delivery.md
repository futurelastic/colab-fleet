# ADR: deliver the final hop over a session's own inbox, capability-detected,
# never in place of the pane path

**Issue:** #119 (unblocked by #117's ruling and #116's shipped identity
primitive; #115 is the discovery note; #118 spiked the namespace; #120, the
reply-path question, is unresolved and out of scope here)
**Status:** decided

## Context

#115 measured that a plain process — not a session, not any part of the
runtime — can deliver a message into a live session's own inbox and have it
arrive as a genuine user turn, on both machines this fleet runs on. That is
strictly better than the terminal-surface path this driver already had:
Send's own doc comment in tmux.go spends hundreds of lines proving a paste
landed and a submit registered, because the composer can silently drop
either. Delivering over the inbox removes the composer from the path
entirely — the failure mode is not detected-and-retried, per the issue body,
it stops existing.

Three things stood between that measurement and shipping it:

- **#117** — whether this service may hold a per-session credential at all.
  Ruled 2026-08-26: yes, full grant, every session on the machine. Not
  narrowed to sessions this service itself started.
- **#116** — resolving a session to its process authoritatively, verified
  immediately before a write, refusing rather than guessing. Shipped
  (`1970c7f`). Filed after a relay that resolved a session name by walking
  the process table delivered cleanly to the WRONG session — a misroute
  that reported success at every layer.
- **#120** — whether a reply nominated to a non-runtime endpoint actually
  arrives. Still open. #119 is briefed to assume one-way delivery only, and
  this change does not touch reply routing at all.

## Decision

Send tries an inbox delivery first, capability-detected per target, and
falls back to the existing pane path — unchanged, byte for byte — whenever
that capability is absent. Four pieces:

1. **`internal/inboxclient`** — the wire protocol only (#115's own
   framing: newline-delimited JSON, an auth line carrying the per-session
   token, a message line, one response line). No path knowledge, no
   `fleet` import, tested entirely over `net.Pipe`.
2. **`fleet.Outcome` gains five values** (`delivered`, `held`, `denied`,
   `expired`, `dropped`) rather than reusing the existing four. `refused`
   IS reused — same word, same shape — but the other five have no honest
   match among `submitted`/`queued`/`refused`/`unknown`, and #117's ruling
   was explicit: "do not flatten `held` or `refused` into a generic
   success... because current callers expect three values." Collapsing
   any of the five into an existing value would be exactly that.
3. **`internal/drivers/tmux/inbox.go`** — `InboxResolver`, an injected
   function from `ProcessIdentity` to `(InboxAddress, ok, err)`. See
   "Why a resolver function, not a path convention" below.
4. **`Send` calls it first**, gated by `inboxEligible` (below), before the
   pane-path enumerate/readiness logic runs at all.

### Why a resolver function, not a path convention (the load-bearing choice)

`internal/probe`'s own #118 spike test already set the precedent this
decision follows: its doc comment states outright that a committed test
hardcoding the real namespace's path or naming convention would "leak an
implementation detail of a system this repo is not allowed to name," and
commits only the generalized shape ("a directory, one file per address")
instead. This repo is PUBLIC and Tier 1 for privacy (CLAUDE.local.md); that
constraint does not relax for a *feature* instead of a *test*.

So neither where a session's inbox socket lives, nor where its credential
comes from, is knowledge this file can hold — not as a hardcoded path, not
as a documented naming convention, not even as a private helper only this
package calls. `InboxResolver` is the seam that knowledge crosses: the
composition root (`cmd/colab-fleetd`) supplies a real implementation built
from machine-local configuration this repository never commits, the same
shape `WithTrustSeed`'s roots and `FLEET_PEERS`'s addresses already use. A
`Driver` built with no resolver — every existing test, and any consumer
that hasn't wired one — never touches an inbox at all.

### `inboxEligible`: three flags name a pane concept the inbox has none of

`opts.Submit == false` asks to land text in a composer without submitting —
there is no composer on the inbox path to land text in. `ResumeIfStranded`
and `ReplaceIfStranded` both name finishing or discarding a delivery that
stranded in a PANE composer earlier; the inbox path cannot have stranded
anything (that is the entire property #115's issue advertises: the failure
mode stops existing, not "is retried better"). A caller setting any of
these three is explicitly asking for the pane, so `Send` skips the inbox
attempt entirely for that call rather than let the inbox path reinterpret a
pane-shaped request.

### The two-call identity discipline is not relaxed by #117's grant

`sendViaInbox` calls `ResolveProcessIdentity` once, then `inboxResolver`,
then `VerifyProcessIdentity` again immediately before dialing — the same
resolve-then-verify shape #116 was built for, applied here rather than
merely available to be applied. #117's own ruling says this plainly: "This
ruling makes that check more load-bearing, not less." A resolver error or a
capability-absent answer is NOT a refusal (§ below); a verification
failure IS one, always, with no pane fallback — falling back there would
be delivering on the exact best guess #116 exists to forbid, just reached
through a different surface underneath the same call.

### Resolver error and capability-absent are the same outcome, deliberately

`InboxResolver` returning `(_, false, nil)` and returning `(_, false, err)`
are both treated as "no usable capability for this call" by `sendViaInbox`,
never surfaced to `Send`'s caller as a refusal. A resolver-side failure
(its own credential store unreadable, say) is a fact about THIS SERVICE's
plumbing, not about the target session; reporting it as a refusal would
misattribute a local problem to the session being sent to. The honest
response to half a capability is the same as to none of it: fall back to
the pane, which has no dependency on whatever the resolver could not
answer.

## What this deliberately does not do

- **No reply path.** #120 is unresolved; this change is Send only, and
  nothing here binds a reply address or attempts to receive one.
- **No scope narrowing beyond what #117 ruled.** The full grant covers
  every session on the machine; enforcement is structural rather than an
  extra check — `ResolveProcessIdentity` only ever resolves sessions this
  driver's own `enumerate()` already tracks, so there is no broader set to
  accidentally reach.
- **No change to the pane path's own behavior, wording, or test suite.**
  Every existing Send test in this package passes unmodified — see the
  full suite result recorded on the issue at wrap — which is the direct
  evidence for "off by default" being true rather than aspirational.
- **No socket/credential discovery code in this repository.** See "Why a
  resolver function" above; that is not an omission to fill in later, it
  is the shape the privacy constraint requires permanently.

## Alternatives considered

**Reuse `OutcomeRefused`/`OutcomeUnknown` for the whole inbox vocabulary
rather than adding five values.** Rejected: it is precisely the flattening
#117's ruling names and forbids. A caller receiving `held` mapped to
`refused` cannot tell "a human is reviewing this" from "the driver declined
before trying," and #117's own text calls that specific conflation out by
name.

**A hardcoded path convention (e.g. `<root>/<pid>.sock`), matching #118's
generalized description of the real namespace.** Rejected: #118's own test
generalizes the shape as something safe to commit, not the literal
convention as a green light to hardcode it into a shipped feature. A
resolver function costs one type and one option; a hardcoded convention
costs nothing until the real namespace's naming ever changes underneath it
with no signal at build time — the exact fragility #119's own issue body
already names as the reason the pane path stays a fallback.

## Consequences

- A composition root that wants #119's behavior live wires
  `WithInboxResolver` with a real implementation reading machine-local
  configuration; nothing in `cmd/colab-fleetd` does this yet as of this
  change (out of scope for #119's own issue, which asked for the delivery
  change itself, not composition-root wiring).

  **Addendum, colab-fleet #122:** that gap was not hypothetical — it shipped
  and deployed exactly as described, and stayed unreachable because nothing
  outside this file's own tests ever called `WithInboxResolver`. #122 closed
  it with `cmd/colab-fleetd/inboxresolver.go`: a resolver reading one JSON
  file per pid from an operator-supplied directory (`FLEET_INBOX_INDEX`),
  and `DriverCapabilities.deliversToInbox` (`capabilities.go`, surfaced on
  `GET /v1/runtimes`) so an operator can confirm the wiring is live without
  reading a receipt's wording — the same "a caller should be able to ask"
  argument #121 already made for `build`. Neither addition weakens this
  ADR's own "no socket/credential discovery code in this repository" line
  above: the resolver never lists a directory or guesses a name from a
  pid, it performs one keyed lookup against an index whose location an
  operator supplies and whose shape this repository invents for itself,
  not the real runtime's own convention.
- `fleet.Outcome`'s wire vocabulary grew from four values to nine;
  `docs/spec/session-abstraction.md` §2.4 and `docs/spec/api-http.md`'s
  `input` example are both updated to state the five new values and why
  they are not folded into the existing four.
