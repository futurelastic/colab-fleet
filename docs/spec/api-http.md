# HTTP API — specification

Concrete wire protocol for the model in
[`session-abstraction.md`](session-abstraction.md). Where the two disagree, the
abstraction wins and this document is the bug.

**Status:** draft, unimplemented. The `/v1` prefix is aspirational — the surface
is not stable and will change without ceremony until a driver has proven it.

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
→ 200 { "epoch": "...", "cursor": 12904, "startedAt": "...", "drivers": [...] }

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
                                                        "agent": true } } } ],
        "sources": [...], "complete": true }
```

Clients **must** consult `/v1/runtimes` before relying on a capability, and
degrade rather than assume. A driver never emulates (§5.6).

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
        "state": { "status": "working", "confidence": "inferred",
                   "evidence": "...", "since": "..." } }
```

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
POST   /v1/machines/{machine}/sessions/{id}/interrupt?runtime=   → 202
DELETE /v1/machines/{machine}/sessions/{id}?runtime=              → 202
```

Both are `202 Accepted`: they express intent, and confirmation arrives as a
state change on the event stream. A driver may not be able to promise
synchronous completion, and pretending otherwise would be emulation.

## 4. Events

```
GET /v1/events?cursor=<last-seen>&epoch=<last-seen>
Accept: text/event-stream
```

Server-sent events. Every event carries `cursor`, `epoch`, and the `machine` it
originated from — including events proxied from peers (§13).

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
- When proxying (§13), a service presents the **original caller's** authority,
  not its own. A peer authorizes the principal who initiated the request. A
  service that substituted its own identity would make every machine a confused
  deputy for every other.
- Every remote-originated mutation is logged: actor, verb, target, outcome.

## 6. What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, claims, or work
planning (§1 non-goals). If such an endpoint ever looks necessary, the
supervisor is asking the wrong service, or this service has begun to grow into
a second supervisor.
