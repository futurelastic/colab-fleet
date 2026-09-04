# ADR: the inbox index binds each record to one process run, and never holds a credential copy

**Issue:** #146 (extends #122's `newFileInboxResolver`, which wires #119's
delivery path; the writer that populates this index remains machine-local
operator work outside this repository)
**Status:** decided

## Context

`newFileInboxResolver` answers one keyed lookup: "does an entry exist for
this exact pid" against `<dir>/<pid>.json`. The record it read was:

```go
type inboxIndexEntry struct {
	Network string `json:"network"`
	Socket  string `json:"socket"`
	Token   string `json:"token"`
}
```

Keyed by pid alone, with nothing that binds a record to the process it
describes — the identical hazard ADR 116 named for this driver's own
`ProcessIdentity` (`ResolveProcessIdentity`/`VerifyProcessIdentity`), one
layer up, at an index a separate, machine-local writer populates instead of
this driver's own `ps` query.

#146 measured the actual failure shape rather than the one first assumed.
The address half of a stale entry **self-corrects** on a recycled pid: both
the index and the runtime it describes are keyed by the same pid, so a
resolver reading a stale row's socket still, coincidentally, names *a*
reachable inbox — just not the one the row's author meant. The credential
half does not self-correct: a stale entry hands **the previous occupant's
token to the current occupant's socket**, which #117's grant to hold a
per-session token never authorised.

## Decision

Two changes to the record shape, both closing the same class of bug from
different angles, plus one resolver-behaviour rule tying them together.

### 1. `StartedAt` binds the record to one process run

```go
type inboxIndexEntry struct {
	Network   string `json:"network"`
	Socket    string `json:"socket"`
	TokenPath string `json:"token_path"`
	StartedAt string `json:"started_at"`
}
```

`StartedAt` carries the same textual form `ResolveProcessIdentity` already
produces from `ps -o lstart=` — exported as `tmux.ParseProcessStartTime` so
the comparison parses the identical layout instead of duplicating the format
string. The resolver parses it and compares with `time.Equal` against the
freshly-resolved `identity.StartedAt`: **a field copy, compared exactly, no
tolerance.** The runtime side of this comparison is already produced in this
exact form by code that exists; whoever populates the index is expected to
copy that value verbatim, not reformat or re-derive it.

**On mismatch, this is capability-absent, not a refusal** — the resolver
returns `ok=false` with an error describing the recycled pid.
`sendViaInbox` already treats a resolver error identically to `ok=false`
(see `inbox.go`'s `InboxResolver` doc comment): both fall through to the
pane path, and neither ever produces `fleet.OutcomeRefused`. That refusal
stays reserved for `VerifyProcessIdentity` failing immediately before the
write — a fact about the *target*. A stale index row is a fact about *this
service's own plumbing*, and ADR 119's seam is exactly what keeps those two
kinds of fact from being reported in the same vocabulary.

### 2. `TokenPath` is a locator, never a copy

`Token` (a value) becomes `TokenPath` (a locator). The resolver reads the
index entry, and — only once the start-time check above has passed — reads
the credential fresh from wherever `TokenPath` points, at resolve time, in
the same call. This is the second reasoning line the original issue named
independently of the first: `InboxAddress.Token`'s own doc comment already
says "never cached by this driver", and `newFileInboxResolver`'s own doc
comment already gives "read fresh on every call" as its reason. A value
copied into a second directory is a cache with a longer lifetime than the
thing it caches — exactly the shape both existing comments already argue
against, just not yet applied to this second file.

The property this buys, independent of requirement 1: if whatever populates
`TokenPath`'s target keys it the same way the address is keyed (by the
current occupant of a pid, not a snapshot taken earlier), then reading it
fresh means a recycled pid can never produce a stale-token disclosure at
all, with or without the `StartedAt` check catching it first. Two
independent defences against the same class, deliberately not collapsed
into one: `StartedAt` protects every field the index holds (including a
future one), while the locator protects the credential specifically, by
construction rather than by validation.

### Why both, not one alone

`StartedAt` alone still lets a stale-but-plausible entry hand out a stale
token in the (very small) window where a recycled pid's process happens to
have started at a timestamp indistinguishable at `ps -o lstart=`'s
second-granularity from the one the entry names — a hazard the issue's own
research explicitly did not need to accept once a cheaper fix (never copying
the credential at all) was available. The locator alone, without
`StartedAt`, would still let a live-but-genuinely-stale entry serve the
*wrong address* to a correctly-keyed identity, which is not the credential
disclosure this issue was filed over but is still worth closing given
`StartedAt` is nearly free once the identity comparison already exists for
`ProcessIdentity` elsewhere in this driver.

## What this deliberately does not do

- **No socket/credential discovery code in this repository, still.** ADR
  119's line holds: this file defines the shape of two locators (a JSON
  index, and now a second file each entry names), never the real paths or
  naming convention either config lives under on a real machine.
- **No change to the writer.** Scheduling, wiring, and the real credential
  store's location remain machine-local operator work, exactly as #122's
  original doc comment already scoped them. This ADR changes the contract
  the writer must emit; it is not the writer.
- **No tolerance window on the `StartedAt` comparison.** The issue's own
  research closed this as an open question: the value being compared is a
  field copy of something already produced in the exact form needed, so
  introducing a tolerance would be solving a problem that measurement showed
  does not exist.

## Alternatives considered

**Verify the credential itself over the wire (a challenge-response against
the dialed socket) instead of trusting the index's `StartedAt` field.**
Stronger in principle, same shape ADR 116 rejected for the process-identity
question it answers: #119's inbox protocol has no such handshake today, and
building one to answer an identity question this driver can already answer
cheaper (a field comparison, no round trip) would be solving #146 by
reopening #117/#120's still-narrow scope.

**Keep `Token` as a value but shorten its assumed lifetime with a
`written_at` timestamp and a staleness threshold.** Rejected: a threshold is
a guess about how fast a writer can catch up after a pid recycles, and
guessing is precisely what #116's own discipline ("refuse rather than
guess") already rejects for this class of problem. The locator removes the
guess entirely rather than tuning it.

## Consequences

- The machine-local writer that populates `FLEET_INBOX_INDEX` must now emit
  `started_at` (the exact `ps -o lstart=` text, e.g. from wherever it
  already tracks a session's process) and `token_path` (a second location,
  read fresh) instead of `token`. That writer does not exist in this
  repository yet; this ADR fixes the contract it must meet before it is
  built, per the issue's own "why now."
- `newFileInboxResolver` now performs two filesystem reads per resolve
  instead of one whenever an index entry exists — accepted deliberately, in
  exchange for the stale-credential class no longer existing structurally.
- A start-time mismatch is observable (an error is returned) without ever
  being reported as the target session refusing — an operator watching logs
  sees "the index was stale for this call," never "session X refused
  delivery," which stays true to what the fact actually is.
