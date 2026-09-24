# ADR: a delivery-module seam, and terminal path v2 as the built-in module

**Issue:** #180
**Status:** decided

## Context

A live test on real sessions of the agent runtime measured `POST …/input`
delivering only 42 of 91 sends cleanly. Six root causes were each reproduced:

1. **The landed check missed wrapped text.** The runtime word-wraps its own
   composer. The last-24-byte needle could never match text that straddled a
   wrap, so sends of 77–98, 151–168, … bytes stranded, and retries failed the
   same way.
2. **Pastes were not bracketed.** A multi-line paste lost its first Enter,
   and a short multi-line text submitted early.
3. **Long pastes were truncated** above about 1 KB, and the receipt still
   said queued.
4. **Concurrent sends merged** into one turn.
5. **The stranded-record lifecycle was unsafe.** Resume and replace could
   submit or wipe a person's draft.
6. **Confirmation read the screen,** even though the runtime's own transcript
   records every accepted message within about half a second.

A prototype ("terminal path v2") fixed all six. Three rounds of review then
found 18 further defects in it: 2 high, 9 medium and 7 low. This ADR records
the shape that shipped and the decisions each finding forced.

## Decision

### A delivery-module seam

`internal/delivery` defines `Module`: `Name`, `Deliver`, `ReservedEnv`.

- **Driver versus module.** A local driver keeps every decision about the
  request: the per-session lock, sanitising, the runtime-syntax guard,
  contradictory flags, the inbox fast path and the sender label. The module
  owns the mechanism: how text reaches the session, how that is confirmed,
  and what is recorded when it strands.
- **Callers see nothing of it.** Request bodies, outcomes and receipts are
  unchanged. The only new fields are optional: `expect`, and `route`, which
  the prototype introduced.
- **One module ships:** the tmux driver's built-in terminal path. A later
  module is a new implementation, not a change here. Discovery, a
  configuration switch and fallback between modules are deliberately absent:
  designing them against one implementation would be guessing.
- **Reserved environment names.** A module may reserve names it must be the
  sole setter of. Session create refuses caller env naming one, as a 400
  naming the variable, and startup refuses a configured `sessionEnv` entry
  naming one. The built-in module reserves none.
- **Counters.** Every send is counted exactly once under `delivery.<module>.`.
  The counters cover the receipt outcome, a finer class (`delivered`,
  `resumed`, `refused`, `refused.draft_kept`, `refused.busy`, `stranded`,
  `unknown`), and the confirmation signal (`confirmed.transcript` or
  `confirmed.screen`). Discards are counted too. A send with no class lands
  in `unclassified`, which a test pins at zero.

### The draft rule

The service never clears or submits text sitting in a composer unless **(a)**
its own record proves the text is its own stranded delivery, or **(b)** the
caller passes the composer's current digest as `expect`. Otherwise it refuses
and the text stays. A flag on the request is a wish, never proof.

- **What counts as the service's own record:**
  - the digest it took at strand time;
  - the recorded text, read back row by row;
  - for a paste the runtime collapsed to `[Pasted text #N +L lines]`, the
    marker it saw the paste land as (H1).
- **Tombstones.** A record that lapses or is replaced leaves a tombstone:
  the same text and digest, kept 24 hours (8 per session) and used only as
  proof (a). That keeps #135's case working — a retry that outlives the
  30-minute record still clears the service's own text — without #135's
  unconditional clear (M2).
- **`replaceIfStranded`** used to clear any composer on the send grant
  alone. It now needs proof like everything else (M9).

### One composer text model

- **Row structure only.** Everything read back from a composer ignores what
  the rendering decided: row breaks, continuation indent, composing form. It
  keeps what the text itself says.
- **Collapsed digest.** The digest collapses whitespace runs rather than
  deleting them, so `rm -rf /tmp/build` and `rm -rf / tmp/build` are
  different (L4). Every comparison also accepts the digest form the previous
  build persisted, so stranded records and caller-held digests survive the
  upgrade (L1).
- **Resize tolerance.** A wrap at a space joins back identically. A wrap
  inside an over-long token changes the digest; for that case, a resume
  whose recorded text still matches the composer row by row proceeds anyway.

### Signals read only the composer

- **Landed signals** — region match, collapsed-paste markers, the tail
  needle and the clipped tail — read the composer's fenced rows and nothing
  else. Text below the closing rule, or in the transcript above it, never
  confirms a delivery (H2, L2).
- **Marker counts** before and after the paste come from the same capture
  shape (L2).
- **A clipped composer.** A fresh send whose composer grew past the visible
  top confirms when its visible rows render a substantial tail of the text.
  A resume never accepts a clipped composer (#149, decided here).
- **The dead `confirmLanded` is deleted.** Its #143/#145 tests now exercise
  the live check (M5).

### The last look before Enter

One fresh capture must positively show a composer holding this delivery, by
the same evidence the landed check accepted. "No recognised menu" is not
enough, because a half-painted dialog has no menu yet (M6).

Before the paste, and again at that last look, the pane's foreground process
must be the runtime (H2):

- **A shell name refuses.** The multiplexer's `pane_current_command` is the
  foreground process's name. A live runtime pane reports its own process
  title, never a shell name.
- **A shell leading the foreground group refuses,** from ps over the pane's
  terminal.
- **A matching record is positive identity.** A foreground process whose
  per-process session record matches the session's working directory
  proves the runtime is in front.

A missing record does not refuse by itself. Not every runtime build writes
one, and a false refusal of a healthy session is its own failure. The
measured case — a shell under a composer frame left by an exited runtime —
is closed by the negative checks, and independently by the composer-only
signals above.

### Resume

- **A paste that never rendered.** The record notes when nothing rendered
  and no Enter was pressed. For that class only, a resume that finds the
  composer empty, with no transcript evidence of acceptance, pastes again
  through every check a fresh send makes. Without an Enter, the runtime
  cannot have taken the text as a turn (M1).
- **Slash commands.** The runtime records an accepted slash command as a
  `<command-name>` entry. That entry confirms the command sent and is never
  "a different turn". A genuinely different turn gets one screen check, so
  the receipt says whether the composer emptied (M3).

### Locks and dialogs

- **Dialogs.** The landed check stops at the first capture that shows a
  dialog.
- **Respond priority.** A respond pre-empts a send holding the session lock
  at the send's safe points, which lie before Enter, never after it. The
  pre-empted send keeps its record and says why (M4).
- **Busy refusals.** Every refusal of a lock the caller's deadline ran out
  on opens with `composer busy, retryable: `. It is still `refused`, because
  nothing was done; the prefix lets a consumer recognise it as transient.

### Guards

- **Invisible characters.** The sanitiser drops C1 controls; U+009B spells
  the paste-end sequence in its 8-bit form. The runtime-syntax guard skips
  everything that renders as nothing before judging the first visible
  character (M7).
- **Leading `/` (L6).** A leading `/` is a command to the runtime and is
  refused, with two exceptions:
  - `/rename`, `/rc` and `/remote-control` are delivered for any caller,
    because existing consumers send them;
  - any command is delivered for a human relay.
- **Terminal-route label (M8).** `route: "terminal"` without the human-relay
  grant needs a label that prints; a `{}` label no longer counts.
- **Trusted relay assertions (M8, L3).**
  - **The trust rule:** relay assertions (`Fleet-On-Behalf-Of`, and the new
    `Fleet-Human-Relay`) are honoured only from a principal that is one of
    this service's configured peers, or from any caller when no principal
    table is configured.
  - **Across a peer:** the human-relay fact now crosses a peer relay under
    that trust rule.
- **Session-record ids.** Only a UUID-shaped session id from the runtime's
  per-process record is accepted, and no id resolves a path outside the
  record root (L5).
- **Arrow keys.** Keys refuses arrow keys on an empty composer with no
  dialog showing (L7).

## Consequences and limits

- **Residual window before Enter.** A window of a few tens of milliseconds
  remains between the last capture and the Enter keystroke. It is documented,
  not closed.
- **Composing-form coverage.** Normalisation to a composed form covers Latin
  and Vietnamese only. Text in other scripts sent decomposed does not
  region-match; it falls to the transcript signal or strands honestly. The
  standard library has no normaliser, and a dependency was not taken.
- **Caller digests across a resize.** An `expect` read before a resize that
  re-broke an over-long token is refused. That is a safe refusal, not harm.
- **Retryable busy refusals.** Consumers that treat every `refused` as final
  still need to learn the busy prefix.
- **Slash-command allowlist.** The allowlist is one constant.
