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
`interrupt`, `close`, `rename`, `discard`, `keys`, `label`, `relay`. Every grant defaults
to denied. Without a principal table the service runs in single-token mode and
two booleans stand in: mutations against local sessions, and relaying mutations
to a peer.

**Relaying.** A mutation aimed at a machine other than the one you are talking
to requires the `relay` grant on the service you called *and* the verb grant on
the machine that actually performs it. Those are two separate refusals and you
will meet them one at a time.

> A read aimed at a peer — a fleet-scoped listing, or a path naming another
> machine — needs only `read`, on the same principal table, whether the target
> is local or a peer. It does **not** also cost `relay`, unlike a relayed
> mutation. Ruled deliberately, not left as an oversight: a relayed mutation
> changes state on a machine the caller is not talking to, a relayed read does
> not, and requiring the same grant for both would treat reaching and changing
> as one act. The symmetric rule was measured and rejected — at least one
> principal this fleet is observed through holds `read` without `relay`, and
> that call would have broken silently the moment `relay` was required for it
> too. A separate, narrower cross-machine-reach grant would be the most
> precise separation and was not rejected on its merits — it costs a new
> grant plus a migration for every existing principal, which is not worth
> buying against a distinction nothing has yet been harmed by (colab-fleet
> #81).

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
| `POST` | `…/{id}/labels` | Set or delete caller-supplied labels | `label` | yes |
| `DELETE` | `/v1/machines/{machine}/sessions/{id}` | Destroy the session | `close` | yes |

⚠️ = the specification requires `read`; the implementation does not check it yet.

---

## Reads

### `GET /v1/health`

Liveness and identity. Returns `{epoch, cursor, startedAt, build,
maxInputBytes, labels, drivers, counters}`. `labels` is the bounds this machine
enforces on session labels, `{maxKeys, maxKeyBytes, maxValueBytes}`; a service
that omits it predates labels. The `build` is a version-control stamp: an unknown or
locally-modified build never compares equal to anything, so "we disagree" stays
distinguishable from "we are different vintages".

`build` fields: `known`, `revision` (a commit sha), `modified`, `time`, `go`,
and `version`.

**`build.version` is the release the running code descends from** — the output
of `git describe --tags` at the built commit, stamped at link time by
`scripts/deploy.sh` (#161). It is what a client checks to enforce a minimum
supported service version; `revision` cannot do that, because a sha is not
ordered.

| Value | Means |
|---|---|
| `"v0.1.0"` | built exactly at release `v0.1.0` |
| `"v0.1.0-2-g3ce7e27"` | two commits **after** `v0.1.0`, at commit `3ce7e27` |
| `"v0.1.0-2-g3ce7e27-dirty"` | as above, plus uncommitted changes (`modified: true`) |
| `null` | not stamped — a plain `go build`, or no release tag reachable |
| absent | the service predates this field |

Comparing against a floor `vX.Y.Z`: strip a trailing `-dirty`, then a
trailing `-<digits>-g<hex>` group; what remains is the release tag, compared as
semver. A stripped `-N-g<sha>` means "after that release", so it satisfies a
floor equal to its tag. Strip from the end rather than splitting on the first
`-`, so a pre-release tag (`v0.2.0-rc.1-3-gabc1234`) keeps its own hyphen. **`null` and absent are
"cannot verify", never "too old" and never "new enough"** — refuse with a message
saying the service did not report a version, which is a different problem to
solve than a version that is too low. A version never participates in build
equality: two builds at one clean revision are the same code whatever their
stamps say.

### `GET /v1/machines`

`{items: [{machine, self, status, observedAt}], sources, complete}`. Always
probes peers; there is no `scope` here.

### `GET /v1/runtimes`

`{items: [{machine, runtime, capabilities}], sources, complete}`. Consult this
before relying on a capability — a driver that cannot do something says so here
rather than failing at the call.

### `GET /v1/sessions`

Filters: `status`, `agent`, `cwdPrefix`, `label`, and `scope`.

`label=key:value` keeps sessions carrying that exact pair; repeat it and every
pair must match. It is the answer to "is any machine already working on X" —
put X in a label at create and ask for it here, instead of encoding it in the
name. A peer too old to apply the filter shows up in `sources` as `degraded`
with no items, so the list is `complete: false` rather than silently wrong.

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
  "permissionMode": "", "consents": [], "mcpConfig": [], "labels": {}
}
```

`201` with the session. Four fields — `trustCwd`, `consents`, `permissionMode`,
`mcpConfig` — additionally require the `send` grant on top of `create`, because
each one hands the new session authority its creator would otherwise have to
grant interactively.

`labels` is a map of up to 16 caller facts about the session — keys 1–128 bytes
without `:`, values up to 128 bytes. Opaque to the service: it stores them and
filters on them, and never interprets them. Over the bounds is a `400` naming
the limit. Sending them needs only `create`. Relayed to a peer that predates
labels, the create is refused `unsupported` before anything is started there.

### `POST …/{id}/input` — send text

```json
{ "text": "…", "submit": true, "resumeIfStranded": false,
  "from": { "agent": "…", "session": "…", "relayOfHuman": false } }
```

Returns `200` with a **delivery receipt** — always `200`, even on refusal.

`from` (optional) labels the message with who it comes from, so the receiving
session sees `agent · session · machine` instead of an anonymous peer. Leave it
out and the message is unlabelled, exactly as before.

- **`agent` and `session` are your own statement.** The service carries them
  but cannot verify them — under a shared token nothing tells one caller from
  another. Do not treat a label as proof of who sent something.
- **`machine` is not yours to set.** The service stamps the machine where your
  request entered the fleet and ignores any value you send. If it cannot
  establish one across a relay, it leaves the machine out.
- **`relayOfHuman: true` adds one line of text and nothing else.** The line says
  the sender *states* it is relaying an instruction from the human operator. It
  is unverified, it grants nothing, and no permission, policy or routing
  decision reads it.
- On the inbox path the label goes in the envelope's sender-name field, and a
  name the service cannot guarantee intact is dropped — never the message. On
  the terminal path it goes on as the first line of the text, as
  `[from: …]`.
- A `resumeIfStranded` retry must repeat the same `from` as well as the same
  text: on the terminal path the stranded text includes the label line.

| `outcome` | Meaning | What to do |
|---|---|---|
| `submitted` | The agent received it — reachable only when the driver's `confirmsDelivery` capability is `true` (see below) | Done |
| `queued` | Accepted, submission unconfirmed | Done |
| `refused` | The driver actively declined; `reason` says why | Read the reason — this is information, not a fault |
| `unknown` | Sent, outcome unverifiable — **the text may be sitting unsent** | Retry with `resumeIfStranded: true` |

**`submitted` is not one of the outcomes `input` can return today.** Every driver
in this fleet reports `confirmsDelivery: false` on `/v1/runtimes` — none can
distinguish "the agent received it" from "the runtime accepted it" — so a
confirmed delivery through `input`, including a confirmed `resumeIfStranded`
retry, reports `queued` instead. `submitted` stays real: it is what `respond`
and `keys()` report for a keystroke that visibly changed the screen, which is
evidence `input`'s own confirmation (the composer emptying) does not have. A
client that keys on `submitted` from `input`, or loops until it sees one,
waits forever — check `confirmsDelivery` before writing that client rule, not
this table alone.

`resumeIfStranded` completes a delivery the service itself attempted and lost
confirmation of. It only ever resubmits text the service's own record says it
placed there — never text a human typed.

`resumeIfStranded` and `replaceIfStranded` also clear a composer the service
holds **no** record for at all (colab-fleet #135), instead of dead-ending at
the busy-composer refusal — both flags already declare "deliver this text
regardless of what's stuck in the composer", so the driver folds `discard`'s
own read-then-clear corroboration into this one call rather than making the
caller do it by hand across three round trips (read → discard → resend). It
never resubmits the foreign text itself — only ever THIS call's own — and a
bare `input` with neither flag set keeps refusing exactly as before.

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
`400` that names the valid set. Requires `?expect=<digest>` from a prior read; a
stale digest is a `409`. Which digest depends on what the composer holds **right
now**, decided before the check runs: `?expect=<composerDigest>` when the
composer holds unsent text (the same value a read publishes as
`state.composerDigest`, and the same one `discard` corroborates against) or
`?expect=<screenDigest>` when it is empty (`state.screenDigest`). Sending the
wrong one back is indistinguishable from a genuine race — both fail the same
`409` — so read `state` immediately beforehand and use whichever digest it
reports for the composer's current state, not whichever one you last happened to
have. For full-screen dialogs `respond` cannot classify. Its own grant,
deliberately not folded into `send`.

### `POST …/{id}/discard`

Clears unsent composer text without submitting it. Requires
`?expect=<composerDigest>` when the composer is non-empty. `202`.

If a prior call already came back `409` naming this exact residue
proven-futile, retry with `&force=true` on the SAME `?expect=` — this reaches
for a stronger clear mechanism than the ordinary pass. `force` never relaxes
`expect`; it has no effect before a prior call has actually proven this
residue futile. `DELETE …/{id}` still works but should not be needed for a
stuck composer alone (colab-fleet#136).

### `POST …/{id}/rename`

```json
{ "name": "new-id" }
```

Changes the session's **id**, not a display label. Announced as
`session.renamed` so subscribers can re-key. `202`.

### `POST …/{id}/labels`

```json
{ "labels": { "issue": "153", "old": null } }
```

Merges: a string sets a key, `null` deletes it, anything not named stays. The
bounds apply to the result. Send `?startedAt=` so a recycled id is refused
`409` instead of silently labelled. Needs the `label` grant — not `rename`.
`200` with the whole session. Announced as `session.labels` with the complete
map. Labels follow a rename and are forgotten when the session closes.

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
  "labels": { "issue": "153" },
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

**`labels`** — always present, `{}` when there are none. A session read
through a peer on an older build has no `labels` key at all.

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
`session.renamed`, `session.labels`, `source.status`, `machine.quota`, `machine.account`,
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

Nor does any endpoint return a session's screen text, transcript, or other
content the session itself produced — no "give me the result" route, and
nothing stores one on a session's behalf (colab-fleet #82). A dispatched
agent's answer travels the way its input did: the caller names a reply
address at dispatch time, and the worker delivers its answer there itself.

---

## Known gaps between this document and the code

Recorded rather than smoothed over, because a reference that quietly disagrees
with the implementation is worse than one that admits where it does.

- **`GET /v1/sessions?machine=`** appears in the specification. The filter has no
  such field; the parameter is silently ignored. Use `scope` and filter
  client-side.
- **`respond`'s outcome vocabulary** is documented in the specification as
  `queued | refused`. It shares its type with `input` and can also return
  `submitted` or `unknown`.
- **`input`'s outcome vocabulary** is documented in the specification as
  including `submitted`, and the type genuinely allows it — but no driver in
  this fleet currently reports `confirmsDelivery: true`, so `input` cannot
  actually return it today. A confirmed submission, including a confirmed
  `resumeIfStranded` retry, reports `queued` instead. `submitted` remains
  reachable through `respond` and `keys()`.
- **`input`'s inbox outcomes are documented as five values; four of them cannot
  occur.** Only `delivered` is reachable. `held`, `denied`, `expired` and
  `dropped` would all require reading a receipt the protocol routes to a reply
  address this service does not hold (#120). `held` is the consequential one:
  #148 measured a receiver holding 206 messages for a human who never came and
  then dropping them, while this endpoint answered `delivered` every time. That
  is addressed at the source — a send this service cannot attest now declines
  the inbox path and falls back to the terminal one — rather than by producing
  `held`, so `delivered` is honest but still means "the attested bytes reached
  the socket", never "the model saw the turn".
- **`deliversToInbox: true` does not imply any send will use the inbox.** Since
  #148 a send also needs the target's permission-mode class from the
  machine-local index; without it every send falls back while this flag still
  reads true. On a machine whose index writer does not emit that field yet, that
  is every send.
- **`/v1/machines` and `/v1/runtimes` take no `scope`.** Every other plural
  endpoint does.
