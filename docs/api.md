# API reference

Every endpoint, at a glance. This is a **reference**, not the specification —
[`spec/api-http.md`](spec/api-http.md) is normative and wins any disagreement.
If you are writing a client and want a walkthrough rather than a lookup table,
read [`client-guide.md`](client-guide.md) first and come back here.

- **Base path:** `/v1`
- **Content type:** `application/json` on every request and response except the
  SSE stream.
- **Addressing:** a session is `(machine, id)`. There is no fleet-wide id.

---

## Conventions that apply everywhere

**Authentication.** `Authorization: Bearer <token>` on **every** route, with no
exemptions and no unauthenticated mode — not on loopback, not in development.
A service with no token configured refuses to start rather than falling back to
open.

**Grants.** With a principal table configured, each caller is a named identity
with its own credential and its own list of grants: `read`, `create`, `send`,
`interrupt`, `close`, `rename`, `discard`, `keys`, `relay`. Every grant defaults
to denied. Without a principal table the service runs in single-token mode and
two booleans stand in: mutations against local sessions, and relaying mutations
to a peer.

**Relaying.** A mutation aimed at a machine other than the one you are talking
to requires the `relay` grant on the service you called *and* the verb grant on
the machine that actually performs it. Those are two separate refusals and you
will meet them one at a time.

> A read aimed at a peer — a fleet-scoped listing, or a path naming another
> machine — needs only `read` today, on the same principal table, whether the
> target is local or a peer. Whether it should *also* cost `relay`, the way a
> relayed mutation does, is an open question rather than an oversight — see
> the known gaps below.

**Deadlines.** `Fleet-Deadline-Ms: <ms>` on any request. A caller may only
shorten a driver's declared deadline, never extend it.

**Corroboration.** Any session-addressed operation accepts `?startedAt=` — the
value from a prior read. A destructive operation uses it to refuse acting on a
session that has been replaced since you looked.

**Runtime disambiguation.** `?runtime=` on any session-addressed operation, and
`"runtime"` in the create body, picks between several local drivers on one
machine.

**Scope.** `?scope=fleet` (the default) or `?scope=local`. Fleet fans out to
every configured peer, exactly one hop — peers never recurse.

---

## At a glance

| Method | Path | Does | Grant | Relays |
|---|---|---|---|---|
| `GET` | `/v1/health` | Build stamp, uptime, drivers, current event cursor | — ⚠️ | no |
| `GET` | `/v1/machines` | Known machines and whether they answered | — ⚠️ | always |
| `GET` | `/v1/runtimes` | Drivers present and the capabilities they declare | — ⚠️ | always |
| `GET` | `/v1/sessions` | List sessions, filtered | — ⚠️ | `scope` |
| `GET` | `/v1/sessions/watch` | Long-poll the event feed | — ⚠️ | `scope` |
| `GET` | `/v1/events` | Same feed as SSE | — ⚠️ | `scope` |
| `POST` | `/v1/machines/{machine}/sessions` | Start a session | `create` | yes |
| `GET` | `/v1/machines/{machine}/sessions/{id}` | Read one session | — ⚠️ | yes |
| `GET` | `…/{id}/environment` | What environment the process actually got | — ⚠️ | yes |
| `POST` | `…/{id}/input` | Deliver text to the composer | `send` | yes |
| `POST` | `…/{id}/respond` | Answer a prompt the session is blocked on | `send` | yes |
| `POST` | `…/{id}/keys` | Deliver one raw key to the screen | `keys` | yes |
| `POST` | `…/{id}/interrupt` | The equivalent of Ctrl-C | `interrupt` | yes |
| `POST` | `…/{id}/discard` | Clear unsent composer text without sending it | `discard` | yes |
| `POST` | `…/{id}/rename` | Change the session's id | `rename` | yes |
| `DELETE` | `/v1/machines/{machine}/sessions/{id}` | Destroy the session | `close` | yes |

⚠️ = the specification requires `read`; the implementation does not check it yet.

---

## Reads

### `GET /v1/health`

Liveness and identity. Returns `{epoch, cursor, startedAt, build, drivers,
counters}`. The `build` is a version-control stamp: an unknown or
locally-modified build never compares equal to anything, so "we disagree" stays
distinguishable from "we are different vintages".

### `GET /v1/machines`

`{items: [{machine, self, status, observedAt}], sources, complete}`. Always
probes peers; there is no `scope` here.

### `GET /v1/runtimes`

`{items: [{machine, runtime, capabilities}], sources, complete}`. Consult this
before relying on a capability — a driver that cannot do something says so here
rather than failing at the call.

### `GET /v1/sessions`

Filters: `status`, `agent`, `cwdPrefix`, and `scope`.

Returns `{items, sources, complete, feed?}`.

> **You must read `sources` and `complete`.** A fleet list where one machine did
> not answer is still a `200`. `complete: false` means the list is partial, and
> `sources` says which machine failed you. Treating a partial list as the whole
> fleet is how a session gets declared gone when its machine was merely
> unreachable.

`feed: {cursor, epoch}` appears **only** once something is subscribed to the
feed. Its absence is the service telling you that you are doing the sequence
backwards — see *Events* below.

### `GET /v1/machines/{machine}/sessions/{id}`

One session, in full. See *The session object*.

### `GET /v1/machines/{machine}/sessions/{id}/environment`

What environment variables and `PATH` the session's process actually received —
names only, never values. `{known: false}` is an ordinary `200`, not an error.
This exists because a session that inherits the wrong `PATH` fails in a way
nothing else in the API can explain.

---

## Writes

### `POST /v1/machines/{machine}/sessions` — create

`Idempotency-Key` header is **required**. Body:

```json
{
  "runtime": "", "cwd": "/abs/path", "agent": "", "model": "", "effort": "",
  "name": "", "prompt": "", "contextRef": "/abs/path", "marker": "",
  "remoteControl": true, "trustCwd": false, "env": {}, "resume": "",
  "permissionMode": "", "consents": [], "mcpConfig": []
}
```

`201` with the session. Four fields — `trustCwd`, `consents`, `permissionMode`,
`mcpConfig` — additionally require the `send` grant on top of `create`, because
each one hands the new session authority its creator would otherwise have to
grant interactively.

### `POST …/{id}/input` — send text

```json
{ "text": "…", "submit": true, "resumeIfStranded": false }
```

Returns `200` with a **delivery receipt** — always `200`, even on refusal.

| `outcome` | Meaning | What to do |
|---|---|---|
| `submitted` | The agent received it | Done |
| `queued` | Accepted, submission unconfirmed | Done |
| `refused` | The driver actively declined; `reason` says why | Read the reason — this is information, not a fault |
| `unknown` | Sent, outcome unverifiable — **the text may be sitting unsent** | Retry with `resumeIfStranded: true` |

`resumeIfStranded` completes a delivery the service itself attempted and lost
confirmation of. It only ever resubmits text the service's own record says it
placed there — never text a human typed.

> A `POST` to `/input` is not the same thing as an instruction delivered. If you
> write one client rule from this document, make it: read the outcome.

### `POST …/{id}/respond` — answer a blocked session

```json
{ "choice": 2, "cancel": false, "nonce": "…" }
```

`choice` is **1-based**, matching the order of `prompt.options`; `0` accepts
whatever is currently highlighted. Returns the same delivery receipt as `input`.

The `nonce` comes from `state.prompt.nonce` on the session you just read, and
changes whenever the prompt changes. Send it always. If it no longer matches,
the driver refuses rather than applying your answer by index to a question that
has changed underneath you — which is the entire reason it exists.

`respond` refuses when it sees no prompt it recognises. That refusal is its
safety property, and it is why raw keys are a separate endpoint rather than a
flag here.

### `POST …/{id}/keys` — one raw key

```json
{ "key": "Down" }
```

One of `Up`, `Down`, `Left`, `Right`, `Enter`, `Escape` — anything else is a
`400` that names the valid set. Requires `?expect=<screenDigest>` from a prior
read; a stale digest is a `409`. For full-screen dialogs `respond` cannot
classify. Its own grant, deliberately not folded into `send`.

### `POST …/{id}/discard`

Clears unsent composer text without submitting it. Requires
`?expect=<composerDigest>` when the composer is non-empty. `202`.

### `POST …/{id}/rename`

```json
{ "name": "new-id" }
```

Changes the session's **id**, not a display label. Announced as
`session.renamed` so subscribers can re-key. `202`.

### `POST …/{id}/interrupt` and `DELETE …/{id}`

Both express intent and return `202`. Confirmation arrives on the event stream,
not in the response.

---

## The session object

```json
{
  "machine": "machine-b", "id": "s42", "name": "…",
  "runtime": "tmux", "cwd": "/abs/path", "agent": "…", "model": "…",
  "startedAt": "…", "attach": {…}, "conversation": {…}, "resumeOutcome": {…},
  "state": {
    "status": "waiting_input",
    "confidence": "observed",
    "evidence": "prose — display it, never parse it",
    "since": "…",
    "prompt": { "question": "…", "options": ["…"], "selected": 1, "kind": "tool-permission", "nonce": "…" },
    "waitingOn": "prompt",
    "composerDigest": "…", "screenDigest": "…",
    "quota": { "since": "…", "resetHint": "…" },
    "lastTurn": { "outcome": "failed", "reason": "…", "retryable": true },
    "controlChannel": { "state": "active", "reason": "" }
  }
}
```

**`status`** — `starting`, `working`, `waiting_input`, `idle`, `quota_blocked`,
`dead`, `unknown`. A closed set with a strict decoder: an unrecognised value is
a decode error, never a silent default.

**`confidence`** — `observed` (read from a structured API) or `inferred`
(deduced from a screen). It survives a relay rather than being flattened, so a
proxied answer never looks more certain than the original.

**`waitingOn`** — `prompt` (a dialog is attached) or `unsent-input` (the
composer holds text nobody submitted; do not send to it).

**Absence is not failure.** A `null` is the service saying nobody looked, which
is a different fact from a negative answer. `conversation: null` means nothing
resolved it, not that there is no conversation. This distinction is the
invariant the rest of the design serves.

---

## Events

Two transports, one feed.

- **`GET /v1/events`** — SSE. Frames are `id: <cursor>`, `event: <kind>`,
  `data: <envelope>`. Resume with `?cursor=&epoch=`, or the `Last-Event-ID`
  header on a browser reconnect.
- **`GET /v1/sessions/watch`** — long poll, for clients that would rather retry
  a request than hold a stream. `?since=&epoch=&wait=` (default 25s, max 60s).
  Returns a batch, not one event per poll.

Filters on both: `session` (repeatable — name the ones you care about),
`cwdPrefix`, `scope`.

**Kinds:** `session.created`, `session.state`, `session.closed`,
`session.renamed`, `source.status`, `machine.quota`, `machine.account`,
`control.resync`.

**Cursor and epoch.** The epoch identifies a service *instance*; a restart gets
a new one. The cursor is monotonic within an epoch. A relayed event keeps the
originating machine's own cursor and epoch in `origin`, and takes the relaying
service's cursor for local ordering — so resumption is never ambiguous about
whose sequence you are holding.

**Resync** arrives in-band as a `control.resync` event, never as an error, with
one of three reasons: `epoch_changed` (you hold another instance's cursors),
`cursor_expired` (older than retained), `feed_gap` (the sequence is intact but
*this* service's own subscription dropped and reconnected — a different party is
at fault). All three prescribe the same recovery.

**Build a mirror in this order.** Getting it wrong is the most common client
bug:

1. `GET /v1/sessions/watch?wait=0` — **arm the feed first.** The service only
   advances its sequence while something is subscribed.
2. `GET /v1/sessions` — take the snapshot *and* the `feed{cursor, epoch}`.
3. Loop `watch?since=&epoch=` (or hold the SSE stream).
4. On any resync, go back to step 2.

Listing before ever watching returns no `feed` at all. That absence is the
answer, not a degraded response.

---

## Errors

```json
{ "error": { "kind": "not_found", "message": "…", "machine": "machine-b", "retryable": false } }
```

| `kind` | HTTP | Meaning |
|---|---|---|
| `invalid` | 400 | Malformed request |
| `unauthorized` | 401 | Caller not permitted |
| `not_found` | 404 | The machine answered; there is no such session |
| `conflict` | 409 | Well-formed, but your belief is stale |
| `unsupported` | 501 | The driver cannot do this |
| `unreachable` | 504 | The machine did not answer at all |

`not_found` and `unreachable` must never be conflated. One is an answer; the
other is the absence of one.

---

## What this API deliberately lacks

No endpoint exposes version control, worktrees, issues, work claims, or
planning. If you need one of those here, either the caller is asking the wrong
service, or this service has begun growing into a second supervisor.

> **colab-fleet knows a session has a working directory.
> It does not know what a worktree is.**

---

## Known gaps between this document and the code

Recorded rather than smoothed over, because a reference that quietly disagrees
with the implementation is worse than one that admits where it does.

- **Whether a peer-targeted read should also require `relay`** is undecided
  (filed separately from the fix that closed the underlying gap). Today a read
  needs only `read`, local or relayed, the same principal table either way.
- **`GET /v1/sessions?machine=`** appears in the specification. The filter has no
  such field; the parameter is silently ignored. Use `scope` and filter
  client-side.
- **`respond`'s outcome vocabulary** is documented in the specification as
  `queued | refused`. It shares its type with `input` and can also return
  `submitted` or `unknown`.
- **`/v1/machines` and `/v1/runtimes` take no `scope`.** Every other plural
  endpoint does.
