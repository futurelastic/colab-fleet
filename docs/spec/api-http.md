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
                                       "confirmsDelivery": true,
                                       "supportsResume": false,
                                       "supportsPin": { "model": true,
                                                        "effort": false,
                                                        "agent": true },
                                       "source": "observed",
                                       "observedAt": "..." } } ],
        "sources": [...], "complete": true }
```

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
  "effort": "...", "name": "...", "prompt": "...", "contextRef": "/abs/path" }

→ 201 { "machine": "...", "id": "...", "name": "...", "state": {...} }
→ 200 (same body) if the key was already seen — the existing session
→ 409 if the key was seen with a different body
```

`Idempotency-Key` is **required, not optional**. A create without one is
rejected with `invalid`. The rationale is §10: a timed-out federated create
that gets retried produces two agents writing to the same working directory,
and the caller cannot detect it afterwards.

`contextRef` is a path. Inline context is not accepted, and context never
reaches a command line (§5.3).

```
GET /v1/machines/{machine}/sessions/{id}?runtime=
→ 200 { "machine": "...", "id": "...", "name": "...", "runtime": "...",
        "cwd": "...", "agent": "...", "model": "...", "startedAt": "...",
        "attach": { "kind": "multiplexer", "target": "...",
                    "command": ["...", "..."], "readOnly": ["...", "..."],
                    "shared": true },
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

`runtime` is an **optional** query parameter on every single-session endpoint
(`GET`, `input`, `interrupt`, `DELETE`). A session `id` is scoped to
`(machine, runtime)` — not to `machine` alone (session-abstraction.md §2.2) —
so two runtimes on one machine may legally reuse an id, which this URL cannot
otherwise disambiguate.

It is required only when the addressed machine runs more than one runtime and
the bare id is ambiguous between them. A service that can resolve the id
unambiguously must not require it; that is the common case, one runtime per
machine. An ambiguous request with `runtime` omitted is `invalid` (400) naming
the ambiguity — never `not_found`, which would assert something untrue about
the fleet.

> Origin: session-abstraction.md Appendix A, F1.

```
POST /v1/machines/{machine}/sessions/{id}/input?runtime=
{ "text": "...", "submit": true }

→ 200 { "outcome": "submitted" | "queued" | "refused" | "unknown",
        "reason": "prompt holds unsent input" }
```

**A refusal is `200`, not an HTTP error.** Refusal is an expected domain
outcome carrying structured information, not a fault. Mapping it to 4xx would
train clients to treat it as an exception and retry — which is precisely the
behaviour the refusal exists to prevent. HTTP errors here mean the driver could
not be reached or the caller is not permitted; they never describe what the
driver decided.

```
POST /v1/machines/{machine}/sessions/{id}/respond?runtime=
{ "choice": 1, "nonce": "<SessionPrompt.nonce>" }
                         // or {"nonce": "..."} to accept the highlighted option
                         // or {"cancel": true, "nonce": "..."} to dismiss

→ 200 { "outcome": "queued" | "refused", "reason": "..." }
```

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
| `session.state` | ref + `SessionState` |
| `session.closed` | ref + final state |
| `source.status` | a machine's reachability changed |
| `control.resync` | `{ "reason": "epoch_changed" \| "cursor_expired" }` |

`source.status` exists so a client learns a peer went away as an **event**,
rather than inferring it from data that stopped arriving. Inferring absence
from silence is the failure mode this whole specification is organised against.

On `control.resync` the client refetches state and resubscribes. The service
never resumes silently from an arbitrary point (§7.3) — an announced gap is
recoverable, a silent one is not.

## 5. Authorization

- Every request carries `Authorization: Bearer <token>`.
- **There is no unauthenticated mode.** A service that can start processes and
  read paths is a remote-execution surface whatever its intent, so there is no
  configuration in which authentication is off — not for loopback, not for
  development.
- Permissions are **per verb, per machine**. `list` and `state` may be granted
  broadly; `create`, `input`, `interrupt` and `close` are granted per peer and
  default to denied.
- Each caller presents its own credential and holds per-verb grants (§6).
- When proxying (§13), the relaying service authenticates as **itself** with the
  credential it holds on that peer, and asserts the original principal in
  `Fleet-On-Behalf-Of`. A caller's own credential is not meaningful on another
  machine once credentials are per peer, so authority travels as identity plus
  assertion; the peer trusts the assertion as far as it trusts the relay, and a
  relay never obtains more than it was granted. A peer authorizes the principal who initiated the request. A
  service that substituted its own identity would make every machine a confused
  deputy for every other.
- Every remote-originated mutation is logged: actor, verb, target, outcome.

## 6. What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, claims, or work
planning (§1 non-goals). If such an endpoint ever looks necessary, the
supervisor is asking the wrong service, or this service has begun to grow into
a second supervisor.
