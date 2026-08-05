# Client guide — using colab-fleet from your own program

For someone **writing a client**: a supervisor, a dashboard, a CLI, a script.

If you want to know *why* the API is shaped this way, read
[`spec/session-abstraction.md`](spec/session-abstraction.md). This document
only tells you what to call and what you must handle. Where the two disagree,
the spec wins and this file is the bug.

Every response below is copied from a running service, with names and paths
replaced.

---

## 1. The mental model, in one picture

```
your program ──HTTP──▶ colab-fleetd on YOUR machine ──HTTP──▶ colab-fleetd on another machine
                                                    ──HTTP──▶ ... and another
```

**You talk to one service: the one on your own machine.** You never address a
peer directly. Ask for `scope=fleet` and your local service fans out, applies
deadlines, carries your identity, and reports which machines answered.

This is the single most important thing to get right, because the wrong version
looks like it works:

> **Do not keep a list of machines and call each one.** You would be
> reimplementing federation, deadlines, partial-failure reporting and
> authorization — badly, and in a place where adding a machine means changing
> your code instead of one config file.

There is no client library and none is needed. Every call is JSON over HTTP,
and the event stream is Server-Sent Events. In Node, that is `fetch` with no
dependencies.

## 2. Connecting

| what | value |
|---|---|
| Address | operational config, not a constant — read it from your own config |
| Auth | `Authorization: Bearer <token>`, required on **every** request |
| Content type | `application/json` |

There is no unauthenticated mode. Not on loopback, not in development.

**Get your own principal.** Do not reuse the token a machine uses to talk to
its peers. Ask the operator to add you to the service's principal table with
the grants you need:

```json
{ "name": "my-supervisor", "token": "…", "grants": ["read"] }
```

Grants are per verb, so you can be given exactly what you need:

| grant | lets you |
|---|---|
| `read` | list, read a session, subscribe to events |
| `create` | start sessions on this machine |
| `send` | deliver input, and answer prompts |
| `interrupt` | interrupt a running turn |
| `close` | destroy sessions |
| `relay` | have any of the above forwarded to a **peer** |

`relay` is separate on purpose: "may mutate sessions here" and "may ask another
machine to mutate its own" are different questions, and a hardened host can
still be a full-featured client.

Every response carries `Fleet-Clock` (RFC3339, the answering machine's clock).
Use it to compute skew rather than assuming clocks agree.

## 3. Finding out what is there

Three endpoints answer "what am I talking to", and a client that skips them
ends up hardcoding answers that differ per machine.

```
GET /v1/health     → epoch, cursor, startedAt, build, drivers
GET /v1/machines   → which machines this service can see, and which one is you
GET /v1/runtimes   → what each machine's driver can actually DO
```

`/v1/health`'s `drivers` is an array of the same `{machine, runtime,
capabilities}` rows `/v1/runtimes` returns, for this machine only. If you want
capabilities, ask `/v1/runtimes` — it covers peers too.

`/v1/machines` tells you who is out there, and **which one you are**:

```json
{ "items": [ { "machine": "aurora",   "self": true,  "status": "ok", "observedAt": "…" },
             { "machine": "borealis", "self": false, "status": "ok", "observedAt": "…" } ],
  "sources": [ … ], "complete": true }
```

**`self` is the field you need before you attach anything.** A session on the
`self` machine is reachable from the terminal you are already in; a session on
any other machine needs whatever remoting you use. Without checking it, a
client either SSHes to its own host for every local session, or tries to run a
remote machine's attach command locally. Both look like they work until they
do not.

**`/v1/health` is your liveness check** — it is the question "is the session
layer up", which is not the same question as "are there sessions". It also
carries `build`, a version-control stamp of the running code:

```json
"build": { "known": true, "revision": "0f0d390…", "modified": false, "go": "go1.26.5" }
```

Show it somewhere. Two machines running different builds is a normal condition
during a rollout and an invisible one otherwise — it has already cost one
debugging session where the symptom made no sense against the source. When
comparing builds, treat `known: false` or `modified: true` as **unverifiable**,
never as equal.

**`/v1/runtimes` you MUST consult before relying on a capability**, and degrade
rather than assume when one is missing:

```json
"capabilities": { "observesState": true, "confirmsDelivery": true,
                  "supportsResume": true,
                  "supportsPin": { "model": true, "effort": true, "agent": true },
                  "source": "observed", "deadlineMs": 5000 }
```

Read `source` too. `assumed` means **nobody has confirmed these** — they are a
conservative floor for a peer that has not answered yet, not that peer's real
answer. Treating `assumed` as fact is how a briefly unreachable machine becomes
a permanently incapable one in your UI.

## 4. Reading the fleet

```
GET /v1/sessions?scope=fleet
```

Optional filters: `machine`, `status`, `agent`, `cwdPrefix`.

```json
{
  "items": [
    {
      "machine": "aurora",
      "id": "alpha💬",
      "name": "alpha💬",
      "runtime": "claude-code-tmux",
      "cwd": "/work/alpha",
      "startedAt": "2026-08-01T23:14:01+07:00",
      "attach": {
        "kind": "multiplexer",
        "target": "alpha💬",
        "command": ["/opt/homebrew/bin/tmux", "attach-session", "-t", "alpha💬"],
        "readOnly": ["/opt/homebrew/bin/tmux", "attach-session", "-r", "-t", "alpha💬"],
        "shared": true
      },
      "state": {
        "status": "idle",
        "confidence": "inferred",
        "evidence": "spinner line in finished form; composer empty",
        "since": "2026-08-05T10:12:28+07:00"
      }
    }
  ],
  "sources": [
    { "machine": "aurora",   "status": "ok", "count": 25, "observedAt": "…" },
    { "machine": "borealis", "status": "ok", "count": 68, "observedAt": "…" }
  ],
  "complete": true
}
```

**This costs one round trip and, on the multiplexer driver, a constant number
of subprocess spawns regardless of session count.** You are not being charged
per session, so do not build a per-session read loop to "avoid a big response".

### You MUST read `sources` and `complete`

`items` is not the whole answer. A machine that did not respond contributes a
`SourceStatus` — it never silently drops out of `items`.

**This applies to every plural response, not just this one.** `/v1/sessions`,
`/v1/machines` and `/v1/runtimes` all return the same envelope, and all three
can be partial for the same reason.

```json
{ "machine": "borealis", "status": "unreachable", "error": "no answer within 3s" }
```

`complete: false` means at least one machine failed to answer, and **you are
looking at a partial fleet**. A UI that renders `items` and ignores this will
tell someone their sessions are gone when the truth is that a machine is
unreachable. If you implement one rule from this document, implement that one.

`status` values: `ok`, `degraded`, `unreachable`.

**To find a session when you only know its id, list and scan.** There is no
id filter, and you do not need one: a fleet-wide listing is a single round trip
that already contains every session, so scanning `items` is the intended route
rather than a workaround. The single-session endpoint (§5) needs a machine,
which is exactly what you do not have yet.

**Ids are opaque and hostile to naive formatting.** They routinely contain
spaces and emoji. Never build a space- or tab-delimited line out of them, never
`cut`/`awk` such a line back apart, and always percent-encode an id when you put
it in a URL path — on *every* endpoint, not just the ones whose examples happen
to show it.

**"Does session X exist?" has three answers, not two.** If you did not find it
and `complete` is true, it does not exist. If you did not find it and
`complete` is false, **you do not know** — it may be sitting on the machine
that failed to answer. Any function of yours that returns a plain yes/no is
throwing that third case away, so make it return the third case, or make it
loud. Collapsing "I could not see it" into "it is gone" is how a supervisor
starts recreating work that is already running.

### Session state

| field | meaning |
|---|---|
| `status` | `starting`, `idle`, `working`, `waiting_input`, `dead`, `unknown` |
| `confidence` | `observed` (measured) or `inferred` (deduced from a screen) |
| `evidence` | human-readable reason — show it, do not parse it |
| `since` | when this status was **first observed to hold** |
| `prompt` | present when the session is blocked on a question (§6) |

Two rules that will save you an incident:

- **`idle` does not mean the last turn succeeded.** Check `state.lastTurn`: a
  session whose turn died on a transient error looks exactly like one that
  finished — same empty composer, same settled status line. If `lastTurn.outcome`
  is `failed` and `retryable` is true, sending anything resumes it; no human is
  needed. Ignore this and abandoned work sits looking available.
- **`unknown` is not `dead`.** It means the service could not determine the
  state. Never clean up, kill or restart on `unknown`.
- **`since` is your stall detector, and it needs no probe.** `waiting_input`
  held for 12 seconds is a human typing; held for 14 hours it is a session
  nobody is coming back to. Do not type into a pane to find out — that is
  exactly the invasive test this field exists to replace.

## 5. Reading one session

```
GET /v1/machines/{machine}/sessions/{id}
```

Returns the same `Session` shape (not wrapped in an envelope). Use it when you
already know the id; use the listing when you want many, since a listing costs
about the same as a single read.

**404 and 504 mean opposite things and must never be conflated.**

| you get | it means | do |
|---|---|---|
| `404 not_found` | the machine answered; there is no such session, and there never was | treat as gone |
| `200` with `state.status: "dead"` | the machine had this session and it has ended | treat as finished |
| `504 unreachable` | **the machine did not answer. Nothing is known.** | retry; never report the work as gone |

A client that treats 504 as 404 will confidently report live work as lost.

## 6. Answering a question the agent is blocked on

When a session is `waiting_input` because a menu is on screen, `state.prompt`
is populated:

```json
"prompt": {
  "kind": "folder-trust",
  "question": "Do you trust the files in this folder?",
  "options": ["1. Yes, I trust this folder", "2. No, continue without these"],
  "selected": 1,
  "nonce": "b6f0…"
}
```

Answer it:

```
POST /v1/machines/{machine}/sessions/{id}/respond
{ "choice": 1, "nonce": "b6f0…" }
```

- `nonce` covers the question and its options. If the screen changed since you
  read it, your answer is refused rather than applied to a different question.
  **Always send it** — it is optional only for a human answering something they
  are looking at this second.
- `choice` is 1-based. Omit it to accept the highlighted default, and `cancel:
  true` dismisses instead of answering.
- `kind` names the question when the service recognises it — `resume-chooser`,
  `folder-trust`, `tool-permission`, `bypass-permissions`. Filter on it if you
  automate answers, so you only ever answer questions you know. **An absent
  kind is not permission**: it means the service did not recognise the prompt,
  and answering it blind is how an automation kills a session.
- **Never blindly accept the default.** A real prompt in the wild highlights
  `No, exit` — a client that reflexively confirms would kill the session it was
  trying to start. That is why `options` and `selected` are both on the wire.

The receipt tells you what happened: `submitted` means the prompt cleared;
`refused` means the session was not at a prompt (a keypress sent to a busy
session is swallowed by whatever it is doing); `unknown` means the answer went
in but the prompt did not clear — re-read rather than retry blindly.

## 7. Driving a session

```
POST   /v1/machines/{machine}/sessions/{id}/input       { "text": "...", "submit": true }
POST   /v1/machines/{machine}/sessions/{id}/interrupt
DELETE /v1/machines/{machine}/sessions/{id}?startedAt=<from the read>
POST   /v1/machines/{machine}/sessions                  (create; see below)
```

**`input` can legitimately refuse.** If a human has typed something into the
composer and not sent it, delivering more text would corrupt their line, so the
service refuses and tells you. That refusal is a feature; do not retry through
it. Check the receipt rather than assuming success.

**Delivery is confirmed before submit.** If the text cannot be confirmed on
screen, you get an `unknown` outcome naming the stranded text instead of a
cheerful success — because "sent" and "landed" are different claims.

**`rename` changes the id.** `POST …/sessions/{id}/rename` with `{"name":"…"}`,
and carry `?startedAt=` exactly as you would for a delete — renaming the wrong
session does not fail loudly, it succeeds and leaves that session named after
somebody else's work. Afterwards, **the id you hold is stale**: use the new one,
and if you are subscribed, re-key on `session.renamed` rather than concluding
the old id died.

**`DELETE` should carry `?startedAt=`** from the session you read. Ids are
recyclable; without corroboration you may destroy a *different* session that
inherited the id. A mismatch answers `409 conflict`, which means your belief is
stale — re-read and decide, do not retry.

Send the timestamp exactly as the read returned it, URL-encoded (it contains
`:` and `+`):

```
DELETE /v1/machines/aurora/sessions/alpha?startedAt=2026-08-01T23%3A14%3A01%2B07%3A00
```

Omitting it is allowed and gives you a weaker guarantee — the driver
corroborates against its own sightings instead of against what *you* saw — and
the refusal tells you which one you got.

### Creating a session

```
POST /v1/machines/{machine}/sessions
Idempotency-Key: 4f1c9e2a-…          ← REQUIRED

{ "cwd":  "/work/alpha",             ← required, absolute
  "name": "alpha",                   ← what you want it called; a REQUEST, not a guarantee
  "runtime": "claude-code-tmux",     ← optional; omitted means the machine's only runtime
  "agent": "…", "model": "…", "effort": "…",   ← optional hints (see below)
  "prompt": "first instruction",     ← optional, delivered once the agent is ready
  "contextRef": "/abs/path" }        ← optional, a PATH — never inline content
```

```json
→ 201 { "machine": "aurora", "id": "alpha", "name": "alpha",
        "runtime": "claude-code-tmux", "cwd": "/work/alpha",
        "state": { "status": "starting", "confidence": "inferred", … } }
```

**Read `id` out of the response and key everything afterwards on it.** Do not
assume it equals the `name` you asked for. On the multiplexer driver they
happen to coincide today; that is a property of one driver, not of the API, and
a client that hardcodes the equality breaks on the first driver that sanitises
or de-duplicates names.

Fields the body does **not** have: `machine` (it is in the path) and `id` (it
is the server's answer, not your input — sending one is ignored, not honoured).

`agent`, `model` and `effort` are **hints**. A driver that cannot pin one says
so rather than silently substituting a default — check `supportsPin` in
`/v1/runtimes` before relying on any of them.

**The `Idempotency-Key` header is required**, and a create without one is
rejected with `invalid` before any driver is consulted. A federated create that
times out and gets retried without one produces two agents in the same working
directory, and nothing afterwards can detect it. Same key + same body returns
the original session; same key + different body is a `409`.

On `409`, the guide's advice is "re-read and decide", and the decision is
genuinely yours — but it is a narrow one. The conflict means the session at
that id is not the session you looked at. Re-read it: if the working directory
and start time describe something you did not mean to touch, **stop** and
surface it, because the alternative is destroying a stranger's work. Do not
loop retrying with a fresh `startedAt` each time; that turns a safety
mechanism into a slower way of doing the dangerous thing.

Deleting a session already reported `dead` is harmless — the driver is
reconciling something it has already lost, and you get the same answer. Skip it
if you like; do not build logic that depends on the distinction.

`state.status` on a fresh session is usually `starting` — the agent is not
ready yet. If you passed a `prompt`, the service delivers it once the runtime
is up; you do not need to wait and send it yourself.

Pass context by path (`contextRef`), never inline. Prompts and context never
reach a command line.

## 8. Events — subscribe, do not poll

```
GET /v1/events
Accept: text/event-stream
```

Filters: `session` (repeatable), `cwdPrefix`, plus `cursor` and `epoch` to
resume.

```
id: 41
event: session.state
data: {"cursor":41,"epoch":"…","machine":"aurora","kind":"session.state","payload":{…}}
```

The kind appears twice on purpose: `event:` lets a browser `EventSource`
listen by type, while `kind` inside `data` lets every other client treat the
stream as framed JSON.

**Resuming.** Send back the last `cursor` and `epoch` you saw. If your cursor
is too old, or the service restarted (different `epoch`), you receive
`control.resync` — which means *your view is stale, refetch the list*. It never
resumes you silently from an arbitrary point, because a subscriber that
believes it has complete history and does not is unrecoverable.

A browser gets this free: `id:` makes `EventSource` send `Last-Event-ID` on
reconnect, and the server honours it.

### What produces an event, and what does not

This is the part to get right before you build a UI on it.

| happens | event? |
|---|---|
| a session appears | `session.created` |
| a session ends | `session.closed` |
| a session's **status** changes (idle → working, → waiting_input) | `session.state` |
| the agent prints output, the spinner ticks, the screen scrolls | **no** |
| a machine becomes unreachable | `source.status` |

Events fire on **transitions**, not on content. A session that stays `working`
for twenty minutes produces one event at the start, not a stream — which is
what you want, and also means **a quiet stream is normal**. Do not treat
silence as a broken connection; on a fleet of ~100 sessions, minutes can pass
with nothing to say.

### What subscribing costs

Measured on a machine with 25 live sessions, subscribing with no filter:

| | |
|---|---|
| helper processes | up to 17 (capped at 16, plus one for lifecycle) |
| their total memory | ~17 MB (~1 MB each) |
| their CPU when idle | 0% |
| released when you disconnect | yes, immediately |

**The service caps this at 16 helpers per subscription**, and says so in its
log. The cap costs you nothing: notifications are triggers, not data, so any
one of them makes the service re-read *every* session — watching a subset still
detects changes fleet-wide. What degrades slightly is how quickly a change is
noticed on a session nobody is watching.

The cap exists because this cost is not paid by you. Each helper holds a
connection to a multiplexer server that other tools on that machine share, and
an uncapped subscription once exhausted one: every new connection refused,
including a human's terminal, while all 69 sessions were alive. Name the
sessions you care about when you can — but you can no longer bring a machine
down by not doing so.

**Stop your subscription when you are done.** A stream that outlives the
process that opened it keeps its helpers open on every machine in the fleet.
That is what caused the incident above.

One documented limitation: on a machine with **no sessions at all**, a
subscription cannot be opened, because this substrate has no unattached form to
listen on. The first session's creation is therefore missed. Re-subscribe after
a create, or list once when your stream opens.

## 9. Attaching a human's terminal

The service will not attach anything for you — attaching gives a terminal to a
*person*, and no person is on the other end of an HTTP request. Instead every
session carries a hint:

```json
"command":  ["/opt/homebrew/bin/tmux", "attach-session", "-t", "alpha💬"],
"readOnly": ["/opt/homebrew/bin/tmux", "attach-session", "-r", "-t", "alpha💬"],
"shared": true
```

- It is **argv**, not a shell string. Session ids contain emoji and spaces.
- It runs **on that session's machine**. Compare `session.machine` against the
  `self` entry from `/v1/machines`: if they match, run the argv directly; if
  they do not, wrap it in your own remoting. The service knows which machine it
  is, not how you reach it.
- **`attach` may be absent entirely.** That means the driver has no interactive
  attachment to offer — not that you should invent one. Show the session as
  unattachable rather than guessing a command.
- The binary path is that machine's own. Do not hardcode one: in a live
  two-machine fleet the paths differ, because the hosts are different
  architectures.
- **Offer `readOnly` for "watch".** The read-write attachment shares a real
  keyboard with a running agent.

## 10. Errors

```json
{ "error": { "kind": "unauthorized", "message": "principal ops does not hold the create grant (§6)",
             "machine": "aurora", "retryable": false } }
```

| kind | HTTP | meaning |
|---|---|---|
| `invalid` | 400 | malformed request |
| `unauthorized` | 401/403 | you lack the grant for this verb on this machine |
| `not_found` | 404 | the machine answered; no such session |
| `conflict` | 409 | idempotency key reused differently, or `startedAt` disagrees |
| `unsupported` | 501 | the driver cannot do this |
| `unreachable` | 504 | **the machine did not answer** |

`message` names the grant you were missing, which makes a permissions problem a
one-line fix rather than a guess.

## 11. Deadlines

Send `Fleet-Deadline-Ms: 3000` to shorten a call. You can shorten a driver's
declared deadline, never extend it.

A peer that misses its deadline **degrades the envelope** — that machine is
marked `unreachable` in `sources` — rather than failing your whole query. One
slow machine never takes down a fleet-wide read.

## 12. When the service itself is down

Your program must distinguish "no sessions" from "I cannot see any sessions".

If the connection fails, show *"session layer unreachable"* and keep whatever
you last knew, marked stale. **Never render an empty list.** An empty list is a
claim that there are no sessions, and a supervisor that makes that claim about
a machine full of live agents will act on it.

This is the same rule the service applies to itself (`sources`, `unknown`,
504), pointed back at you — and it is the one clients get wrong.

## 13. A minimal client

Node 18+, no dependencies:

```js
const BASE = process.env.FLEET_URL;                 // your machine's service
const TOKEN = await readFile(process.env.FLEET_TOKEN_FILE, "utf8");

const call = async (path, init = {}) => {
  const res = await fetch(BASE + path, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN.trim()}`,
               "Content-Type": "application/json", ...init.headers },
  });
  if (!res.ok) throw Object.assign(new Error(res.statusText), { body: await res.json() });
  return res.json();
};

// Reads — and note that `complete` is checked, not just `items`.
const { items, sources, complete } = await call("/v1/sessions?scope=fleet");
if (!complete) {
  const down = sources.filter(s => s.status !== "ok").map(s => s.machine);
  console.warn("partial view; unreachable:", down.join(", "));
}

// Answering a prompt, with the nonce that makes it safe.
const s = items.find(x => x.state.prompt);
if (s) {
  await call(`/v1/machines/${s.machine}/sessions/${encodeURIComponent(s.id)}/respond`,
    { method: "POST",
      body: JSON.stringify({ choice: 1, nonce: s.state.prompt.nonce }) });
}

// Events — a stream, not a poll.
const res = await fetch(`${BASE}/v1/events`, {
  headers: { Authorization: `Bearer ${TOKEN.trim()}`, Accept: "text/event-stream" },
});
for await (const chunk of res.body.pipeThrough(new TextDecoderStream())) {
  for (const line of chunk.split("\n")) {
    if (!line.startsWith("data: ")) continue;
    const ev = JSON.parse(line.slice(6));
    if (ev.kind === "control.resync") await refetchEverything();
    else apply(ev);
  }
}
```

(The SSE parser above is deliberately naive — a real one buffers across chunk
boundaries.)

## 14. Checklist before you ship a client

- [ ] Reads `complete` and `sources`, and shows a partial fleet as partial
- [ ] Never treats `504` as `404`
- [ ] Never acts destructively on `unknown`
- [ ] Sends `Idempotency-Key` on every create
- [ ] Sends `?startedAt=` on every delete
- [ ] Sends `nonce` on every respond, and never blind-accepts a default
- [ ] Subscribes rather than polls, and handles `control.resync`
- [ ] Talks only to its own machine's service
- [ ] Shows "unreachable", never an empty list, when the service is down
- [ ] Keys on `id` from the server, never on the `name` it asked for
- [ ] Checks `self` from `/v1/machines` before attaching, so local sessions do not take a remote path
- [ ] Treats "not found in a partial view" as unknown, not as absent
- [ ] Has its own principal and its own token
