# HTTP API — specification

Concrete wire protocol for the model in
[`session-abstraction.md`](session-abstraction.md). Where the two disagree, the
abstraction wins and this document is the bug.

**Status:** implemented and running, across two machines. The surface is not
frozen — it has changed several times under contact with a real driver, and it
will change again — but it is no longer aspirational: every endpoint below is
served, and the federated ones have been exercised peer to peer.

---

## 1. Conventions

- JSON in, JSON out. `Content-Type: application/json`.
- Timestamps are RFC 3339 with offset, in the **stamping machine's** clock
  (§11). Every response carries `Fleet-Clock: <rfc3339>` so callers can compute
  skew.
- Sessions are addressed positionally — `/machines/{machine}/sessions/{id}` —
  rather than by a composite identifier in one path segment. There is no
  fleet-wide id to encode (§7.1), and the hierarchy makes the machine scope
  visible at every call site.
- A call that resolves a local session driver carries `Fleet-Runtime:
  <runtime-id>`, and `Fleet-Runtime-Resolution: default` when a configured
  default runtime was the tiebreak (session-abstraction.md §7.1a). See §3.3's
  `rename` entry for the full rule.

## 2. Error model

```json
{
  "error": {
    "kind": "not_found",
    "message": "no session with that id",
    "machine": "<machine-id>",
    "retryable": false
  }
}
```

| `kind` | HTTP | Meaning |
|---|---|---|
| `invalid` | 400 | Malformed request |
| `unauthorized` | 401 / 403 | Caller not permitted for this verb on this machine |
| `not_found` | 404 | The machine answered, and there is no such session |
| `conflict` | 409 | Idempotency key reused with a different body; or a destructive request whose `startedAt` disagrees with the live session (§5.4) — the request is well formed, the caller's belief is stale |
| `unsupported` | 501 | Driver lacks the capability (§4.3 of the model) |
| `unreachable` | 504 | **The machine did not answer.** Nothing is known. |

**`not_found` and `unreachable` must never be conflated.** One means the fleet
knows the session does not exist; the other means the fleet knows nothing at
all. This is §5.7 expressed at the wire, and it is the single most important
line in this document: a client that treats 504 as 404 will confidently report
work as gone while it is running fine on an unreachable host.

## 3. Endpoints

### 3.1 Service and topology

```
GET /v1/health
→ 200 { "epoch": "...", "cursor": 12904, "startedAt": "...",
        "build": { "known": true, "revision": "...", "modified": false,
                   "time": "...", "go": "go1.26.5" },
        "drivers": [...] }

GET /v1/machines
→ 200 { "items": [ { "machine": "...", "self": true, "status": "ok",
                     "observedAt": "..." } ],
        "sources": [...], "complete": true }

GET /v1/runtimes
→ 200 { "items": [ { "machine": "...", "runtime": "...",
                     "capabilities": { "observesState": true,
                                       "deliversRawKeys": true,
                                       "confirmsDelivery": true,
                                       "supportsResume": false,
                                       "supportsPin": { "model": true,
                                                        "effort": false,
                                                        "agent": true },
                                       "source": "observed",
                                       "observedAt": "..." } } ],
        "sources": [...], "complete": true }

GET /v1/whoami[?machine=<id>]
→ 200 { "principal": "...", "machine": "...", "grants": ["read", "send"],
        "source": "observed" }
```

**`/v1/whoami` reports the presented credential's own grants, and nothing
about any other principal** (colab-fleet #106, session-abstraction.md §7.7).
Unlike every other read route, it does not require the `read` grant — see §5
— because a principal holding no grants at all is exactly the caller who
needs it most. `machine` defaults to this service's own id; naming a peer
always answers `source: "assumed"`, `grants: []`, the same conservative-floor
meaning `/v1/runtimes` already gives an unreached peer's capabilities —
reused rather than reinvented, because it is the identical "nobody has told
me anything" fact. This service has no mechanism to learn what a peer has
granted a credential (unlike a peer's driver capabilities, which are probed
and cached), so a peer's real answer must be read from that machine directly.

Clients **must** consult `/v1/runtimes` before relying on a capability, and
degrade rather than assume. A driver never emulates (§5.6).

They must also consult `source`. `assumed` means nobody has confirmed these
values and they are a conservative floor — reading them as the runtime's answer
is how a temporarily unreachable peer becomes a permanently incapable one.

**`build` identifies the code, and `known: false` is not a match.** Two
services that disagree may be two services running different vintages, and
those need opposite responses: one is a bug, the other is a deploy. A client
comparing builds **must** treat an unstamped or `modified` build as
*unverifiable* rather than as equal — an unmodified pair of equal revisions is
the only comparison that means anything, and the failure this field exists to
catch is precisely a confident conclusion drawn from an absent measurement.

### 3.2 Sessions

```
GET /v1/sessions?scope=fleet|local&machine=&status=&agent=&cwdPrefix=
→ 200 Collection<Session>
```

Returns the envelope of §9 — `items`, `sources`, `complete` — never a bare
array. An unreachable machine appears in `sources` with `status: "unreachable"`;
it never contributes silence to `items`.

**`scope` defaults to `fleet` for clients and MUST be `local` for proxied
calls** (§13.1). A service querying a peer always asks `scope=local`; a peer
receiving `scope=local` answers for itself and never forwards. This is what
keeps fan-out one hop deep, and it is the only thing preventing two
mutually-configured peers from querying each other forever.

A `scope=local` response carries exactly one `SourceStatus`. The proxying
service **adopts** that record rather than synthesizing a fresh one (§13.2) — a
peer that answers promptly while reporting itself `degraded` must not be
relayed as `ok`.

**The envelope may also carry `feed`, and that is how a snapshot becomes a
mirror:**

```json
{ "items": [...], "sources": [...], "complete": true,
  "feed": { "cursor": 12904, "epoch": "..." } }
```

`feed` is where this snapshot sits in the event sequence (§4). A client lists
once, then watches from that cursor, and never polls again.

The cursor is read **before** the enumeration begins, deliberately. A snapshot
may therefore already contain changes newer than the cursor it carries, and
replaying from that cursor re-applies them. That overlap is the point: applying
an event twice to a mirror keyed by session id changes nothing, while missing
one leaves a mirror that is wrong permanently and cannot tell. The design
produces the recoverable failure, the same way §7.3 insists a gap be announced
rather than skipped.

**`feed` is ABSENT when the cursor is not a resume point**, and absent is a real
answer (§5.7), not zero. A service advances its sequence only while it is
actually observing a driver; with nothing subscribed the cursor is frozen while
the fleet keeps moving, so a number handed out then would look resumable and
would silently skip everything before the first subscription. A client that
finds no `feed` has its ordering backwards: **watch first, list second.**

### 3.3 Deadlines

Every request carries an effective deadline:

```
Fleet-Deadline-Ms: 3000        (request header, optional)
```

A caller may shorten a driver's declared `deadlineMs`, never extend it. On
expiry the service returns the envelope with that source marked
`unreachable`, `error` naming the elapsed time — **not** an open connection and
not a 5xx for the whole call. One unresponsive peer degrades an envelope; it
never fails a fleet-wide query.

Absent the header, the driver's declared `deadlineMs` applies. There is no
configuration in which a call has no deadline: measured against a stopped peer,
an undeadlined request was still blocked after seven seconds with no result,
and no mainstream HTTP client defaults to protecting you from that.

```
POST /v1/machines/{machine}/sessions
Idempotency-Key: <caller-supplied, required>
{ "runtime": "...", "cwd": "/abs/path", "agent": "...", "model": "...",
  "effort": "...", "name": "...", "marker": "...", "remoteControl": true,
  "prompt": "...", "contextRef": "/abs/path", "trustCwd": false,
  "env": {"NAME": "value"}, "resume": "<conversation id>",
  "permissionMode": "bypass", "consents": ["folder-trust"],
  "mcpConfig": ["/abs/servers.json"] }

→ 201 { "machine": "...", "id": "...", "name": "...", "runtime": "...",
        "runtimeSurface": {"known": null, "evidence": "..."},
        "promptDelivery": {"outcome": null, "evidence": "..."},
        "identityAssertion": {"asserted": "...", "drifted": null, "evidence": "..."},
        "state": {...} }
→ 200 (same body) if the key was already seen — the existing session
→ 409 if the key was seen with a different body
```

`Idempotency-Key` is **required, not optional**. A create without one is
rejected with `invalid`. The rationale is §10: a timed-out federated create
that gets retried produces two agents writing to the same working directory,
and the caller cannot detect it afterwards.

**`prompt` is capped at 1024 bytes.** Over that, the create is rejected
outright — `invalid` (400) naming the limit and the caller's actual size —
instead of being accepted and left to strand in the composer with no
delivery receipt to explain why (colab-fleet #114, #110, #112: the creation
path measurably strands even shorter prompts than `input` does). This is a
conservative default, not the bisected true failure boundary, which #114
leaves as open work. A caller with more to send should write it somewhere
the agent can read deliberately and pass only a short pointer here — the
same workaround #112 already adopted ad hoc.

**The driver that served the create builds this response, not the service
layer relaying the caller's own request back at it** (colab-fleet #84, #85,
#86) — `agent`/`model` on it are what the runtime is actually using, when the
driver can tell, never an echo of what was requested; the requested values
live in `pins` alongside whether they were honoured. The three paragraphs
below are one recurring shape, not three unrelated notes: a 201 is a receipt
for the CREATE call succeeding, never proof that everything the create asked
for was applied.

**A 201 for a `resume` create is not proof the resume was honoured** — a
concurrent burst can have the runtime silently start a fresh conversation
instead, with no refusal and no degraded status (colab-fleet #72). Poll the
session afterward and read `resumeOutcome` (§2.10) once its `conversation`
resolves; do not infer success from the create response alone.

**A 201 for a create that asked to pin `agent`/`model`/`effort` is not proof
the pin was applied** — a value this driver cannot pass through safely is
refused outright (`invalid`, naming the field), but a value that *reaches*
the runtime intact can still be defaulted or ignored there with nothing
reported back except by reading `pins` (colab-fleet #84).

**A 201 for a create that requested remote control is not proof a
runtime-hosted surface exists yet** — the runtime registers it, if it does at
all, asynchronously after the process starts, so `runtimeSurface` (§2.13) is
legitimately `known: null` on the create response and may take a read or two
to resolve. `known: false` is different and final: the create opted out
(`remoteControl: false`), or the runtime declined, and a caller polling for
one may stop (colab-fleet #85).

**A 201 for a create that carried a `prompt` is not a delivery receipt for
it** — the prompt is sent after the process starts, so the 201 is written
before delivery can be known. Read `promptDelivery` (session-abstraction.md
§2.11): `outcome: null` means still in flight, and is never evidence the
prompt was lost — a session polled moments later reading `idle` with
"composer empty, no turn yet" looked identical, before colab-fleet #111, to
one that never had a prompt at all (colab-fleet #86). Read `state.turns`
alongside it now: `turns: 0` is what a session that never took a turn looks
like, and a nonzero count is proof of the opposite — the prompt was received
and at least one turn against it has already completed. Do not re-send
through `input` while `promptDelivery` still holds.

**A 201's `name` is what this machine asserted, not proof the runtime still
carries it.** Nothing has read the session back yet, so `identityAssertion`
(§2.14) legitimately reads `drifted: null` on the create response — the same
"asserted, not yet corroborated" state a rename can also produce, and the
one whose absence a prose-only sentence used to leave undetectable
(colab-fleet #97, #102).

**`runtime` on the response is the runtime that actually served this
create** — not an echo of the request body's own `runtime`, which is
commonly absent (session-abstraction.md §7.1a: a caller with one local
runtime, or one relying on the configured default, sends no hint at all).
See `Fleet-Runtime` above for the same fact carried on every other
session-addressed call, not only `create`.

`contextRef` is a path. Inline context is not accepted, and context never
reaches a command line (§5.3).

**`name` is a request, not the id.** The driver owns the naming rules
(session-abstraction.md §2.1) and may sanitize the name, number it against
sessions already live, or append `marker`. A marker is carried, never
stacked, whatever alphabet it is drawn from — a name that already ends in
the one sent keeps the one it has, so a caller resuming an already-marked
name may send the same `marker` again without growing it (colab-fleet #88).
**Read the returned `id`** — it is
the resolved string, and it is what every later call must address. A caller
that assumes the name it sent is the id it got will address the wrong session
the first time two carry the same name.

**`remoteControl` omitted is not `false`.** Omitted means "whatever a
first-class session on this substrate gets". Send `false` only to deliberately
create a session that cannot be reached remotely.

**`trustCwd` is consent to one question about the `cwd` in this same request**:
the runtime's folder-trust dialog, which on some runtimes stands between a new
session and doing anything at all. With it, the driver answers that dialog by
locating the option that grants trust and choosing it by index — never by
accepting the highlighted default, which on a neighbouring boot screen means
`No, exit` — and answers nothing if the wording is ambiguous. Absent means the
driver answers nothing, which is what every caller written before this field
already gets.

It requires the **`send` grant in addition to `create`** (§6): answering a
dialog is a keypress, it is the same blast radius as `respond`, and folding it
into `create` would make this route a second, unreviewed way to drive a
session. On a relayed create the check belongs to the peer serving it, against
the same credential — §13's "proxying does not launder authorization".

**`consents` generalises `trustCwd`**, which remains valid and means exactly
`["folder-trust"]`. Each entry is a `PromptKind` the driver can RECOGNISE; the
affirmative option is then found by reading the runtime's own option text, and an
unrecognised or ambiguously-worded screen is answered not at all. Not every kind
is consentable: `resume-chooser` is refused, because its options are summaries of
prior conversations and nothing in them identifies the one the caller named —
consent there would be a coin flip, and losing it resumes a stranger's work. Same
`send` grant as `trustCwd`.

**`env` is delivered out of band, never on a command line.** Values are staged in
a 0600 file the session reads and unlinks; nothing reaches an argv, because the
payload likeliest to be a credential must not be the one exception to §5.3. Names
must look like variable names, and a value may not contain a newline or NUL — the
staging format is line-oriented, and a newline would arrive as a second variable
invented out of value content. Violations are `invalid`, never truncated. A driver
with no out-of-band channel refuses the create rather than starting a session
missing its identity.

**`resume` continues a prior conversation.** Not to be confused with
`supportsResume` in `/v1/runtimes`, which answers whether sessions survive a
service restart — a different question with an unfortunately similar name. A
resumed session commonly meets the resume chooser; see `consents` for why that
one is yours to answer.

**`permissionMode` requests a non-default permission posture.** One value:
`bypass`. It requires the **`send` grant** too — a session in that mode acts
without asking, and between "may start a session" and "may start a session that
needs no permission for anything", the second is plainly the larger authority.
An unrecognised value is refused rather than passed through.

**`mcpConfig` names tool-server configuration files, by PATH.** Each entry is
absolute, and the flag is emitted once per entry rather than joined — a joined
list reaches the runtime as a single filename containing a separator, which
surfaces as a session missing its tools rather than as an error anybody can
read.

Paths, never inline configuration, and for a sharper reason than `contextRef`'s:
these files commonly hold the credentials their servers authenticate with, so
inline content would put a secret in an argv every process table on the machine
can read. A caller holding one in memory writes it to a 0600 file and names the
file — the same move `env` already forces.

A path the driver cannot read is a **refusal, not a start**. The runtime would
come up, fail to load it, and present a session that lists, reads and accepts
input while being unable to do the work it was created for — the same failure
`env` is already refused for, in the same words. A 201 for a session that is
quietly not what was asked for is worse than an error at the call site. On a
relayed create the paths belong to the PEER's filesystem and travel verbatim;
the machine serving the create is the one that checks them.

Nothing here reads, merges, validates or interprets the contents. A service that
parsed these would have begun to hold opinions about what a session may talk to,
which is a supervisor's judgement and §6's non-goal.

It requires the **`send` grant in addition to `create`**, like `permissionMode`
and for the same shape of reason: these configurations name servers the session
will LAUNCH, and between "may start a session" and "may start a session that
also starts these", the second is plainly the larger authority. The refusal names
the field, so a caller fixes one line rather than re-reading its whole request.

Caller-supplied values that land in the agent's argv (`agent`, `model`, `effort`,
`resume`, `mcpConfig`) may not begin with `-`: the CLI would read them as flags,
which would turn a create grant into "run the agent with arguments of my
choosing". That guard is what a caller with no field for its tool servers used to
hit while smuggling the flag through a pin.

```
GET /v1/machines/{machine}/sessions/{id}/environment?runtime=
→ 200 { "known": true, "shell": "...", "login": true, "interactive": true,
        "names": ["..."], "path": ["...", "..."],
        "serviceNames": ["..."], "servicePath": ["..."],
        "capturedAt": "..." }
→ 200 { "known": false, "reason": "..." }   ← a real answer, not an error
→ 501 unsupported, if the runtime cannot report one
```

What environment a session's process actually received. **`names` carries
variable names and never values** — the environment in question is the one
holding credentials, and a read that returned them would be a worse defect than
any it diagnoses. `path` is the one value present, because a search path is not
a secret and is the drift this endpoint was added for.

`serviceNames`/`servicePath` are the same enumeration for the **service's own**
process, so a reader can see what the session's startup contributed rather than
only what it ended up with. An empty difference is the interesting case: it
means the startup files added nothing, which means an agent needing credentials
will start normally and fail at its first tool call.

`known: false` is an ordinary 200 (§5.7). "The session had no environment" and
"we never found out" are opposite answers, and a driver must not collapse them.

```
GET /v1/machines/{machine}/sessions/{id}?runtime=
→ 200 { "machine": "...", "id": "...", "name": "...", "runtime": "...",
        "cwd": "...", "agent": "...", "model": "...", "startedAt": "...",
        "attach": { "kind": "multiplexer", "target": "...",
                    "command": ["...", "..."], "readOnly": ["...", "..."],
                    "shared": true },
        "conversation": { "known": true, "id": "...",
                          "source": "derived", "evidence": "..." },
        "resumeOutcome": { "requested": "...", "honoured": false,
                           "evidence": "..." },
        "identityAssertion": { "asserted": "...", "drifted": false,
                               "evidence": "..." },
        "state": { "status": "working", "confidence": "inferred",
                   "evidence": "...", "since": "..." } }
```

`attach` (§2.8) is how a **human's** terminal reaches the session. `command` is
argv to run *on that session's machine*; the client composes any remoteness
itself, because this service knows which machine it is and not how you reach
it. Prefer `readOnly` whenever the user asked to watch rather than to take
over — the two are different attachments, and offering the wrong one shares a
live keyboard with a running agent. Absent means the driver has no answer,
which is a real answer (§5.7).

`conversation` (§2.9) names the record the **runtime** keeps of this session's
conversation — the one source here that is not the process describing itself.
It has three states and they are not interchangeable: the field **absent** means
nobody looked, `known: false` means somebody looked and could not tell (an
ordinary 200, with the evidence saying why), and `known: true` carries the
identifier plus a `source` saying whether it was matched or dictated. A caller
that reads the absent field as "this session has no record" has turned a driver
without a record store into a finding about somebody's session.

`resumeOutcome` (§2.10) is present only when this session's `create` set
`resume`, and says whether that was actually honoured — never assume it from
a create that merely returned 201, because a resume can be silently ignored
under load and the create still succeeds on a fresh conversation (colab-fleet
#72). Absent means no resume was requested; `honoured` absent (with
`evidence`) means the session's own `conversation` has not resolved yet, not
a "no"; `honoured: true`/`false` is the verdict once it has.

`identityAssertion` (§2.14) says what identity **this machine** last asserted
for the session, and whether the runtime still carries it — machine-readable,
where before this fact only reached a caller as prose inside `state.evidence`
(colab-fleet #97, #102). Absent means this machine asserted no identity for
this session at all (adopted or foreign); `drifted` absent means an identity
was asserted but not yet corroborated against a live read; `drifted: false`
means the runtime carries it as of this read; `drifted: true` carries
`carried`, naming what it holds instead. The repair, when this machine
attempts one, lands on the *next* read, not this one — a single
`drifted: true` is not a permanent condition.

```
POST /v1/machines/{machine}/sessions/{id}/input
{ "text": "...", "submit": true, "resumeIfStranded": false, "replaceIfStranded": false }
```

`resumeIfStranded` completes a delivery that returned `unknown` — the text
reached the composer and could not be confirmed. The service submits it only if
its own record says that text is what it delivered there; text it did not place
is never submitted. Send the same text: this finishes one delivery rather than
starting another.

**`replaceIfStranded` (colab-fleet #112) is the door out when the caller wants
DIFFERENT text instead of finishing the old delivery.** `resumeIfStranded`
only ever completes the delivery already sitting in the composer — a caller
that wants to send something else has no use for it, and until #112 had no
other way in either: the busy-composer refusal below applied even though the
service's own record showed the "human typed" attribution was wrong. With
`replaceIfStranded` set, and only when the service's own record shows the
composer holds a delivery **this service itself placed** there (never on the
strength of any other evidence — a human may have attached and typed since,
and the service cannot compare pasted bytes to rule that out), it clears that
text and delivers the new text in its place. Both flags set at once is a
contradiction — asking to finish the old delivery and replace it in the same
call — and is refused outright rather than picking one silently.

A refusal that reaches this path distinguishes three cases a caller could not
tell apart before #112, all previously collapsed into one message asserting a
human was typing:

- **the service's own record shows this composer holds a delivery it placed,
  and the new text is identical to it** — resend with `resumeIfStranded` to
  finish that same delivery, not `replaceIfStranded`.
- **the service's own record shows this composer holds a delivery it placed,
  and the new text is different** — this is the case that used to have no
  answer. `resumeIfStranded` would finish the OLD delivery; `replaceIfStranded`
  discards it and delivers the new text; `discard` (below) clears it without
  delivering anything.
- **no matching record exists** — the original, unchanged answer: the
  composer may hold a person's own unsent draft, and this service will not
  guess otherwise. Neither flag helps here; read the session and `discard`
  with the composer's digest, or wait.

```
POST /v1/machines/{machine}/sessions/{id}/discard?expect=<composerDigest>&startedAt=
→ 202 { "accepted": true }
→ 409 if the digest does not match what is there now, or none was supplied
→ 409 also if the clear could not be confirmed to have finished — the message
  says which of three things happened: the composer is unchanged and this is
  the first time (safe to retry with the same digest), the composer is
  unchanged and a PRIOR full clear pass already proved retrying does nothing
  (do not retry — see below), or the composer is now damaged (re-read before
  doing anything else; do not retry blind)
```

Removes unsent composer text without submitting it. `expect` is
`state.composerDigest` from a read, sent as the **query parameter** shown above
— not a JSON body field, even though `composerDigest` is also the name of a
field in the read response that produced it. It is **required when there is
text**: this deletes somebody's typing, and a caller that has not seen the
current text has no business removing it. An already-empty composer returns
202, so a retry after a timeout is safe.

A driver that cannot confirm its own clear keystroke finished reports that as
409 too, never 400: the request was well formed, and a keystroke failing to
land is not the caller's mistake to fix by resending the same bytes. Three
outcomes share that 409, and need three different next steps:

- **unchanged, first pass** — the composer reads exactly what the caller
  already corroborated. Nothing was destroyed, so retrying with the same
  digest is exactly as safe as the first attempt was.
- **unchanged, proven futile** — a *prior* call already spent a full pass
  against this exact residue and it did not move. This driver does not press
  again against text already proven not to respond; it refuses before
  touching the pane at all, and the message deliberately does not repeat
  "safe to retry" — repeating it was #87's failure mode, a caller that
  followed that advice to the letter, four times, and made zero progress
  each time. The message instead names the one operation guaranteed to work
  regardless of what state the composer is stuck in: `DELETE
  /v1/machines/{machine}/sessions/{id}`.
- **damaged** — the keystroke registered PARTIALLY: some of the text is
  gone, none of it cleanly, and the composer now holds neither what the
  caller saw nor nothing — worse than either extreme, and not safe to retry
  blind. The message carries the residue's current digest, so the caller's
  next legal call needs no extra re-read to learn it.

A single call also stops pressing early once it has clear evidence a pass has
stalled — movement observed, then several presses in a row that changed
nothing — rather than spending the rest of its window on keystrokes already
proven to do nothing against text nobody has re-read.

```
POST /v1/machines/{machine}/sessions/{id}/rename?startedAt=&runtime=
{ "name": "new-name" }
→ 202 { "accepted": true }
→ 409 if startedAt disagrees, or the new name is already in use here
```

**Renaming changes the `id`**, not a label beside it — on a substrate where the
id is the name an operator sees, anything less renames the session in this API
and leaves their terminal saying the old thing. Send `?startedAt=` for the same
reason `DELETE` wants it: acting on the wrong session here succeeds *silently*
and leaves it wearing somebody else's name.

Subscribers receive `session.renamed` carrying `from` and `to`. A client
filtering by id **must** re-key on it, or it stops matching a session that is
still alive — and cannot tell that from the session having died. **This event
fires more than once per rename** — §4's event-plane section covers what the
`corroboration` field on each one means, and why waiting for the second is
worth doing before treating a rename as durable.

`runtime` is an **optional** query parameter on every single-session endpoint
(`GET`, `input`, `respond`, `discard`, `rename`, `keys`, `interrupt`,
`DELETE`) and on `POST …/sessions` (`create`, in the JSON body as
`"runtime"`). A session `id` is scoped to `(machine, runtime)` — not to
`machine` alone (session-abstraction.md §2.2) — so two runtimes on one
machine may legally reuse an id, which this URL cannot otherwise
disambiguate.

**When `runtime` is supplied, it is used outright** — the caller named its
own runtime and nothing here second-guesses it. Omitted, and the machine
runs exactly one local runtime, resolution is unambiguous and `runtime` is
never required; that was every machine's whole history until a second
runtime existed to register.

**Omitted with more than one local runtime registered** resolves
existence-first, THEN a configured default as tiebreak
(session-abstraction.md §7.1a, colab-fleet issue #60):

- a nonempty `id` (every endpoint but `create`) is checked against every
  registered runtime's own record of what it has ever had. Exactly one
  affirms it → that runtime, full stop, even against a machine configured
  with a different default — a default must never steer an id that plainly
  belongs elsewhere into a false `not_found`. More than one affirms it, or
  the check cannot be completed for all of them → `invalid` (400) naming the
  runtimes involved; never `not_found`, which would assert something untrue
  about the fleet.
- a genuine miss — `create`'s own case, and what a nonempty `id` reaches once
  every runtime has affirmatively confirmed absence — resolves to the
  machine's configured **default runtime**, when one is configured.
  Unconfigured, this is the ambiguity's older shape: `invalid` (400) naming
  it, exactly as before a default runtime could be configured at all.

**Every 2xx and every error response from a call that reached this
resolution carries `Fleet-Runtime: <runtime-id>`**, naming the runtime that
actually served the request — the one piece of information a bare-id call
against more than one runtime previously had no way to surface. **It also
carries `Fleet-Runtime-Resolution: default` when, and only when, the
configured default runtime was the tiebreak.** Its absence is itself
informative: every other resolution — an explicit `runtime`, the sole
registered driver, or an existence match — is exactly as trustworthy as a
caller naming its own runtime, because each is either the caller's own word
or a fact this machine just confirmed by asking. Only the default is a
genuine guess wearing a configuration's authority, and a caller in a
position to care (retrying a destructive call, auditing a surprising
`not_found`) can tell the two apart without reading a log
(session-abstraction.md §5.7, §7.1a guardrail 2).

**A proxied call to a peer machine carries neither header.** Resolution
never reaches this machine's local runtimes at all for a peer-addressed
call — see session-abstraction.md §13.1 and §7.1a's federation note — so
there is no local runtime id to report and the configured default plays no
part in it.

> Origin: session-abstraction.md Appendix A, F1; the default-runtime
> tiebreak and its headers: colab-fleet issue #60.

```
POST /v1/machines/{machine}/sessions/{id}/input?runtime=
{ "text": "...", "submit": true }

→ 200 { "outcome": "submitted" | "queued" | "refused" | "unknown",
        "reason": "prompt holds unsent input" }
```

**A driver that delivers over a target session's own inbox instead of the
terminal surface (colab-fleet #119) can additionally answer `delivered` |
`held` | `denied` | `expired` | `dropped`** — session-abstraction.md §2.4 has
the full vocabulary and why those five are not folded into the four above.
This is capability-detected per target, never a caller choice: the same call
shape, the same endpoint, a richer answer only when that surface was actually
used for this delivery.

**`text` is capped at 1024 bytes, the same limit `prompt` carries on
create** (colab-fleet #114). Over that, the call is rejected outright —
`invalid` (400) naming the limit and the caller's actual size — before any
driver is even resolved, rather than reaching the composer and stranding
there with no exit but `resumeIfStranded` or destroying the session. Chunk
longer content instead: several calls, each comfortably under the limit —
the mitigation #112 already verified end to end.

`submitted` is genuinely part of this type — it is what a driver reporting
`confirmsDelivery: true` on `/v1/runtimes` would return here — but no driver
in this fleet currently declares that capability, so `input` cannot actually
produce it today; a confirmed submission reports `queued` instead. api.md's
known-gaps section tracks this alongside the equivalent, opposite-direction
gap already recorded for `respond`.

**A refusal is `200`, not an HTTP error.** Refusal is an expected domain
outcome carrying structured information, not a fault. Mapping it to 4xx would
train clients to treat it as an exception and retry — which is precisely the
behaviour the refusal exists to prevent. HTTP errors here mean the driver could
not be reached or the caller is not permitted; they never describe what the
driver decided.

A refusal here may also describe the **text**, not the session: a driver may
refuse caller text its own runtime would read as something other than a
message, before looking at session state at all — session-abstraction.md §3,
colab-fleet issue #53. That refusal happens whether or not the addressed
session exists, and it is never a candidate for `resumeIfStranded`: the text
never reached a composer to strand.

```
POST /v1/machines/{machine}/sessions/{id}/respond?runtime=
{ "choice": 1, "nonce": "<SessionPrompt.nonce>" }
                         // or {"nonce": "..."} to accept the highlighted option
                         // or {"cancel": true, "nonce": "..."} to dismiss

→ 200 { "outcome": "queued" | "refused", "reason": "..." }
```

`state.waitingOn` discriminates `waiting_input`, which carries two situations
needing opposite handling: `prompt` (answer it) and `unsent-input` (do not send
to it). Only the first has a `prompt` to branch on, so without this field they
are separable only by reading `evidence` — prose explicitly not to be parsed.
Absent means unclassified (§5.7), not "no reason".

A session out of quota is **not** one of these: it reports `quota_blocked`
(§2.3), because nothing a caller sends will unblock it.

`state.lastTurn` — when present — says how the most recent turn **ended**:
`{"outcome":"failed","reason":"…","retryable":true}`. It exists because a turn
that died and a turn that finished leave the same screen: an error, a settled
status line, an empty composer. Both are honestly `idle`, and a supervisor that
cannot tell them apart silently abandons the work.

Absent means the screen said nothing about it — **not** that the turn
succeeded (§5.7). `retryable` is the runtime's own word for the failure, not
our judgement of its error code: when true, sending anything resumes the
session and no human is required.

`state.controlChannel` — when present — is what the RUNTIME says about its own
remote-control connection: `active`, `connecting`, `reconnecting` or `failed`.
It is the runtime describing itself, not a claim about whatever is at the far
end, and this service still does not model bridges.

It exists because `failed` is otherwise invisible here. A session whose control
channel is dead raises no prompt, blocks nothing and changes no status — it sits
at an empty composer with a healthy status line and is, through every other
field, an ordinary live session. Measured: 37 of 67 sessions came back from a
fleet-wide recovery in that state, and the only way to find them was to read
pane text.

**Absent is not `active`** (§5.7). A runtime with no such channel reports
nothing, and so does a driver that cannot look; `observesControlChannel` in
`/v1/runtimes` is what separates those, and an unreached peer reports it
`assumed` rather than `false`. It never changes `status`: a session nothing
outside can reach is still running and still able to work.

A change here fires `session.state` on the event stream (§4) like any other
material change — which is the point, since nothing else about the session
moves when a channel drops.

`state.controlChannel.reason` — when present — is why a `failed` channel
failed, in the runtime's own words, sourced from its own durable record
rather than from a screen (colab-fleet #69): `{"state":"failed","reason":"Remote
Control disconnected — this session was ended or archived from another
device or app (code 4090)"}`. It carries a close code when the runtime put
one in the sentence, but classifies nothing — no field here says whether a
retry helps; that mapping was never measured (#65) and this endpoint does
not guess it. Absent whenever `state` is not `failed`, or is `failed` but no
record, no readable one, or no matching entry can explain why — the same
§5.7 discipline `controlChannel` itself already applies one field up.

`state.prompt.kind` — when present — names what is being asked
(`resume-chooser`, `folder-trust`, `settings-trust`, `tool-permission`).
`bypass-permissions` is deliberately absent from what CLASSIFICATION can produce:
its options are generic and its identifying words sit in the question, which this
service does not read. See the client guide.
It is **advisory and fails to absent**: an unrecognised prompt carries no kind.

A client may auto-answer a kind it knows. It must **never** treat an absent
kind as safe: a real prompt in this fleet highlights `No, exit`, so answering
what you cannot read eventually kills the session you meant to rescue. The
service deliberately does not choose for you — deciding what to answer is a
supervisor's judgement, and a session service that made it would have become
one.

**Send `nonce`.** It is `SessionPrompt.nonce` from the state you read, and it
is the whole of the protection: a caller reads a prompt, shows it to a human,
and answers a minute later — by which time the session may be showing a
DIFFERENT question in the same place, and an answer submitted by index would be
applied to it silently. With the nonce that becomes a refusal.

It is optional only for a human answering something they are looking at right
now. An automated caller that omits it is choosing to answer whatever happens
to be on screen; a driver that answers unchecked must say so in the receipt.

Answers a prompt the session is blocked on. Refused as an ordinary 200 when the
session is not at a prompt — a keypress delivered to a session that is not
asking anything is consumed by whatever it is doing.

This is not a flag on `input`, because `input` must guarantee it never produces
a keystroke: a message containing `C-c` must not interrupt the session
receiving it (§3 of the abstraction).

```
POST /v1/machines/{machine}/sessions/{id}/keys?expect=<screenDigest>&startedAt=&runtime=
{ "key": "Down" }

→ 200 { "outcome": "submitted" | "refused" | "unknown", "reason": "..." }
→ 400 if `key` is outside the vocabulary below
→ 409 if `expect` does not match the screen now, or none was supplied
→ 501 if the driver cannot deliver a key event (§4.3 `deliversRawKeys: false`)
```

Delivers ONE raw key event to a session's screen. It exists for the full-screen
dialogs a driver does not recognise — navigated with arrow keys, confirmed with
a bare Enter — which `respond` cannot answer and `input` must never learn to.

**It is not a flag on `respond`.** `respond` refuses whenever the driver sees no
prompt, and that refusal is the whole of its safety: a keypress delivered to a
session that is not asking anything is consumed invisibly by whatever it is
doing. The screens this route exists for are exactly the unrecognised ones, so
folding it in would mean relaxing that check for the case it was written to
exclude. This route pays for its own safety instead.

**`expect` is `state.screenDigest`, and it replaces the nonce.** There is no
`prompt` on an unrecognised screen and therefore no `SessionPrompt.nonce`, so
the caller quotes back a fingerprint of the screen it read — the same discipline
`discard` uses with `composerDigest` and `DELETE` with `startedAt`. A screen
that has moved on is `409`: well-formed request, stale belief. It is
**required**; a caller that has not read the screen has no business pressing
Enter on it.

`state.screenDigest` is new and is published by any driver that can produce one.
It is a fingerprint and never the text: the pane holds a conversation, and a
read that returned it would make every listing a transcript leak. It is not
comparable across drivers or across restarts — quote it back, never compute one.

**Vocabulary, closed:** `Up` `Down` `Left` `Right` `Enter` `Escape` — move,
accept, dismiss. Anything else is `invalid`, rejected before any driver is
consulted. Absent by design: every character key, which is `input`'s job and
whose guarantee is that a message never becomes a keystroke; and every control
key — `C-c` is `interrupt`, `C-u` is `discard` — each of which has corroboration
and confirmation a blind keypress cannot offer. An endpoint accepting arbitrary
key names would quietly become a second, unreviewed way to do everything else
here.

**One key per request**, and no sequence field. After the first key the screen
is different, so every later key in a batch would be delivered against a digest
describing something that no longer exists — reintroducing exactly what `expect`
prevents. `Down Down Enter` is three requests with a read between each. That is
the honest price of three corroborated keypresses.

**Two refusals hold regardless of the digest**, as ordinary 200 outcomes:

- the composer holds unsent text — `Enter` would submit somebody's half-typed
  message, which is the harm `send` already refuses to cause;
- the session is at a prompt the driver DID recognise — answer it through
  `respond`, which verifies a nonce and can say which option it chose. Falling
  back to a blind arrow key is a downgrade dressed as a capability.

**`submitted` means the screen changed under the key.** A key a dialog swallows
leaves the session exactly as stuck as before, so an unchanged screen is
reported `unknown` with the reason saying so — never `submitted`. A legitimate
no-op (`Down` at the bottom of a list) reports the same way, because from
outside the dialog the two are the same observation and inventing a distinction
would mean claiming to know what the dialog is.

It requires its own **`keys` grant** (§6), not `send`. `respond` shares `send`
on a same-blast-radius argument that does not survive here: `respond` is gated
by a recognised prompt and this deliberately is not, so an operator may permit
one and withhold the other. Absent means denied, so no existing principal gains
it by upgrading — which means a fresh deployment cannot press a key until an
operator explicitly grants it, on purpose, not as an oversight (colab-fleet
#68). `deliversRawKeys: true` on a runtime is a statement about the DRIVER;
whether any caller may reach it is a separate, orthogonal fact this grant
alone controls, and a capability that reads as present while every caller is
refused looks identical to the endpoint not existing.

**A federated keypress needs two grants, at two different machines, discovered
in the wrong order if you only read the refusal you hit first (#68).** The
machine that will actually run the key needs `keys`; the machine relaying the
request there needs `relay` — the ordinary grant §13 already requires for any
proxied mutation, nothing special to `keys`. A caller debugging the relayed
path who fixes the `keys` refusal first (because that is the grant named in
the first 401) will find the call still refused, now naming `relay` — not a
second bug, the other half of the same requirement.

```
POST   /v1/machines/{machine}/sessions/{id}/interrupt?runtime=   → 202
DELETE /v1/machines/{machine}/sessions/{id}?runtime=              → 202
```

Both are `202 Accepted`: they express intent, and confirmation arrives as a
state change on the event stream. A driver may not be able to promise
synchronous completion, and pretending otherwise would be emulation.

## 4. Events

```
GET /v1/events?cursor=<last-seen>&epoch=<last-seen>&session=&cwdPrefix=
Accept: text/event-stream
```

`session` may be repeated to name several sessions. Selectors narrow and
compose with AND. Naming is not sugar over `cwdPrefix`: a substrate may charge
per watched session, in which case a caller that can only describe what it
wants pays for every match (§5.5).

Server-sent events. Every event carries `cursor`, `epoch`, and the `machine` it
originated from — including events proxied from peers (§13).

Each frame carries the kind twice, deliberately:

```
id: 41
event: session.state
data: {"cursor":41,"epoch":"...","machine":"...","kind":"session.state","payload":{...}}
```

An event relayed from a peer additionally carries `origin`:

```
data: {"cursor":41,"epoch":"<this service>","machine":"<peer>","kind":"session.state",
       "origin":{"cursor":7,"epoch":"<the peer>"},"payload":{...}}
```

`cursor` and `epoch` always belong to the service being talked to, so
resumption is never ambiguous; `origin` preserves the peer's own coordinates so
a caller that later talks to that peer directly can resume there. Proxied
subscriptions ask the peer for `scope=local` (§13.1).

`event:` lets a browser `EventSource` listen by kind; the `kind` property lets
every other client read the stream as framed JSON without parsing SSE; and
`id:` makes a reconnecting browser send `Last-Event-ID` on its own, so
resumption needs no client code. The server honours that header when no
`cursor` parameter is given.

| Event | Payload |
|---|---|
| `session.created` | full session |
| `session.state` | ref + `SessionState` — fired on any **material** change, not only a change of `status` |
| `session.closed` | ref + final state |
| `session.renamed` | `{ "machine", "from", "to", "startedAt"?, "corroboration" }` — a session's **id** changed (session-abstraction.md §3's `rename`); a subscriber filtering by id must re-key on `to` or it silently stops matching a session that is still alive |
| `source.status` | a machine's reachability changed |
| `machine.quota` | `{ "machine", "blocked": bool, "quota"? }` — this machine's **account** started or stopped refusing work |
| `machine.account` | `{ "machine", "generation" }` — this machine's local **credential material** changed |
| `control.resync` | `{ "reason": "epoch_changed" \| "cursor_expired" }` |

`source.status` exists so a client learns a peer went away as an **event**,
rather than inferring it from data that stopped arriving. Inferring absence
from silence is the failure mode this whole specification is organised against.

**`session.state` fires on any material change**, which is every structured
field a caller branches on: `status`, `confidence`, `waitingOn`,
`composerDigest`, the prompt (its options, highlight and **nonce**), `quota`,
`lastTurn`, `turns`, `credentialGeneration`. It began firing on `status` alone, and
everything else then moved underneath a silent feed — including the nonce,
whose entire job is to make an answer submitted against a replaced question
refusable. A feed that under-reports does not merely go stale; it manufactures
the failure `respond` was built to refuse.

Two things are deliberately **not** material, and a client should know it.
`evidence` is prose for humans that §2.3 forbids parsing, and the runtime
repaints it continuously — treating it as a change would emit an event per
keystroke while saying nothing anyone may act on. `since` on its own is a
driver re-stamping when it first observed a status, not the session doing
something different. So a mirror's `evidence` is as fresh as the last material
change; re-read the session when you want the current prose.

**`session.renamed` fires more than once for the same rename** (colab-fleet
#103). The first is always `"corroboration": "accepted"`, published the
instant `POST …/rename` returns `202` — the same fact this event always
carried, named honestly now as provisional rather than left to be read as a
durability claim it never was. A rename that reverts (§3.3's own measured
case: a `202`, a correct read for roughly half an hour, then a silent revert,
with nothing on the stream saying so for that whole window) used to leave a
subscriber holding a name that had stopped being true with no way to learn it.
It cannot anymore: exactly one further `session.renamed`, for the same `from`
and `to`, always follows — never silently omitted — carrying one of:

- **`"corroborated"`** — a later, independent observation found the new id
  still resolving, with no sign of a revert.
- **`"contested"`** — the new id stopped resolving **and** the old id's
  identity came back, matched by `startedAt` rather than by name alone
  (§5.4) — the exact shape #97 measured.
- **`"unconfirmed"`** — this service cannot say either way: the new id
  stopped resolving without the old id's identity reappearing to corroborate
  a revert (as consistent with an ordinary `DELETE` of the freshly-renamed
  session as with an unattributable revert), or the stream watching for it had
  a gap of its own (a `source.status` degradation, a `control.resync`)
  somewhere in the window. Not a claim the rename held, and not a claim it
  reverted — said plainly rather than by omission.

A client that only ever acts on the first `session.renamed` it sees for a
given `to` gets exactly today's behaviour. One that wants to know whether a
rename actually held waits for the second.

`machine.quota` is the only event whose subject is not a session, and the only
one a scheduler should act on by **not** doing something. It fires once at the
transition, carries the reset time when the runtime printed one, and is
announced to a subscriber that connects while a block is already in force —
joining late must not mean learning nothing.

The alternative, measured: an account hit its weekly limit and 48 autopilot
sessions each discovered it separately, by being dispatched work and stalling.
Every discovery cost a session that had already been sent. There is no earlier
signal available — the runtime prints no warning before it refuses, so the
first refusal is the notice.

`machine.account` is the sibling of `machine.quota` for a different
account-level fact (#12): the local credential material itself changed, so
every session started before that moment is bound to an identity that is no
longer the one in force. It fires once at the transition. Unlike
`machine.quota` it is **not** re-announced to a subscriber that joins after
the fact — every machine has some generation the moment a credential store
exists, so there is no "already in force and worth repeating" case the way a
block has; a joining subscriber instead reads `generation` directly off each
session (`SessionState.CredentialGeneration`).

`generation` is an identity marker, not a health claim: it says which
credential this machine now has, never that any particular session's binding
to it still answers. This layer **reports the transition only** — a rebind is
a supervisor's operation, layered on top, not something this event triggers or
performs.

On `control.resync` the client refetches state and resubscribes. The service
never resumes silently from an arbitrary point (§7.3) — an announced gap is
recoverable, a silent one is not.

`control.resync` carries one of three reasons, and they are three different
statements about whose view is stale:

| `reason` | what happened |
|---|---|
| `epoch_changed` | you hold another instance's cursors |
| `cursor_expired` | your cursor is older than what is retained — or newer than anything this service has stamped, which it equally cannot supply |
| `feed_gap` | **the sequence is intact and this service stopped watching.** Its subscription to a driver dropped and was re-established, so changes in between were never stamped at all |

`feed_gap` is not a politer `cursor_expired`. One says the caller fell behind;
the other is this service admitting the hole is its own, and telling a caller
its cursor is too old when the cursor is perfectly current sends it hunting a
slowness problem it does not have. The action is the same for all three —
refetch and resubscribe — which is why a client that already handles
`control.resync` needs no new code, only a better log line.

**`source.status` also reports the feed itself.** A driver subscription that
fails or ends is announced `degraded` and retried; when it comes back, `ok`
arrives with a `feed_gap` resync beside it. Both edges, on the transition only.
The alternative was measured and is the worst shape available: the pump gave up
after a single failure, and every subscriber then held a stream that was open,
healthy-looking, and permanently empty. A machine that is momentarily empty
reaches that state on an ordinary path — the first driver's control mode has no
unattached form, so with no sessions there is nothing to attach to — which
means a client subscribing to an idle machine was never told about the first
session it started. A subscriber told it is deaf can re-list; one told nothing
cannot tell deaf from quiet.

### 4.1 The same feed as ordinary request/response

```
GET /v1/sessions/watch?since=<cursor>&epoch=<epoch>&wait=<ms>
                      &scope=fleet|local&session=&cwdPrefix=

→ 200 { "cursor": 12931, "epoch": "...", "events": [ {...}, {...} ] }
```

A long poll over the same hub, the same sequence, and the same envelope: each
entry of `events` is exactly what an SSE frame's `data:` line carries. This is a
**transport, not a second event model** — nothing is expressible in one and not
the other, because the moment they diverge there are two answers to one
question. Use it when you want a request you can retry, log and reason about,
and when your client must survive its own restart without a stream-reconnect
state machine.

- Returns as soon as at least one selected event exists, or at `wait` with
  `events: []`. Default `wait` is 25s, capped at 60s, and a shorter
  `Fleet-Deadline-Ms` wins — §3.3's rule that a caller may shorten and never
  extend.
- **`cursor` in the response is what to send as the next `since`**: the last
  event in the batch, or — for an empty batch — exactly what you sent. It is
  deliberately NOT the service's current cursor, which advances for every
  subscriber while your filter selects for you; handing it back would advance
  you past events you never saw.
- `since` omitted means **from now**. Never "from the beginning": the oldest
  entry in the retained window is an arbitrary point, and resuming from an
  arbitrary point is §7.3's silent gap.
- `epoch` omitted alongside a `since` means "the instance I was already talking
  to", the same reading the stream gives a browser's `Last-Event-ID`. If that
  assertion is wrong you get `epoch_changed` rather than a bad resume.
- **A stale cursor is a `200`, with `control.resync` in the batch** — the same
  rule as a refused `input` (§3.3). The request was well formed; what the
  service has to say is domain information to act on, not a fault to retry.
  After a resync the response's `cursor` is the one you sent, unchanged: a
  resync is not a position to resume from, so re-list and take the next cursor
  from the listing's `feed`.
- Requires the `read` grant (§6) and grants nothing further.

**Building a mirror**, in full:

```
1. GET /v1/sessions/watch?wait=0            → cursor C, epoch E   (arms the feed)
2. GET /v1/sessions                         → items, and a feed position ≥ C
3. loop: GET /v1/sessions/watch?since=C&epoch=E
         apply events in order; C := response cursor
4. on control.resync (any reason)           → back to 2
```

Step 1 before step 2 is not decoration. The service watches only while somebody
is subscribed, and the first watch is what makes the sequence live; a listing
taken before it carries no `feed` at all, which is the service telling you the
ordering is wrong rather than handing you a cursor that would skip.

## 5. Authorization

- Every request carries `Authorization: Bearer <token>`.
- **There is no unauthenticated mode.** A service that can start processes and
  read paths is a remote-execution surface whatever its intent, so there is no
  configuration in which authentication is off — not for loopback, not for
  development.
- Permissions are **per verb, per machine**. `list` and `state` may be granted
  broadly; `create`, `input`, `interrupt`, `close`, `rename`, `discard` and
  `keys` are granted per peer and default to denied. That default is deliberate
  and applies to a fresh deployment as much as an established one — no grant is
  implied by anything else, including a runtime advertising the capability the
  grant gates (§3, `keys`; colab-fleet #68).
- Each caller presents its own credential and holds per-verb grants (§6).
- **`GET /v1/whoami` is the one read exempt from needing the `read` grant
  itself** (§3.1, session-abstraction.md §7.7, colab-fleet #106). It reports
  only the presented credential's own grants, never another principal's, so
  the risk `read` gates elsewhere — reading someone else's data — does not
  apply; and gating it on `read` would make it unusable by the principal who
  needs it most, the one holding none.
- When proxying (§13), the relaying service authenticates as **itself** with the
  credential it holds on that peer, and asserts the original principal in
  `Fleet-On-Behalf-Of`. A caller's own credential is not meaningful on another
  machine once credentials are per peer, so authority travels as identity plus
  assertion; the peer trusts the assertion as far as it trusts the relay, and a
  relay never obtains more than it was granted. A peer authorizes the principal who initiated the request. A
  service that substituted its own identity would make every machine a confused
  deputy for every other.
- **A read that reaches beyond this machine — `scope=fleet`, or a path naming a
  specific peer — needs only the `read` grant, never `relay`.** This is a
  deliberate asymmetry with the proxying rule above, not an oversight: a
  relayed mutation changes state on a machine the caller is not talking to,
  while a relayed read does not, so requiring one grant for both would treat
  reaching and changing as the same act. The symmetric rule was considered and
  not taken — at least one principal this fleet is observed through holds
  `read` without `relay` today, and folding `relay` into the read check would
  have refused that call silently the moment it landed, with no error a caller
  could act on. A third, narrower grant for cross-machine reach alone would be
  the most precise separation and was not rejected on its merits; it costs a
  new grant in the model plus a migration for every existing principal, which
  is not worth buying against a distinction nothing has yet been harmed by
  (colab-fleet #81).
- Every remote-originated mutation is logged: actor, verb, target, outcome.

## 6. What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, claims, or work
planning (§1 non-goals). If such an endpoint ever looks necessary, the
supervisor is asking the wrong service, or this service has begun to grow into
a second supervisor.

No endpoint returns a session's screen text, transcript, or other content the
session itself produced, and none stores a result on a session's behalf
(session-abstraction.md §5.8, colab-fleet #82). This is a different kind of
absence from the paragraph above — not a domain this service doesn't
understand, but a data class it declines to carry regardless of domain. A
dispatched agent's answer is delivered by the agent, to a reply address the
caller supplied at dispatch, over `input` — never read back through this API.
