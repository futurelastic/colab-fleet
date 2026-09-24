# ADR 187 — A slash command the runtime runs itself is confirmed by its `local_command` entries

**Status:** accepted (2026-09-25)
**Issue:** colab-fleet #187 · builds on #180 (M3, slash-command confirmation)

## Context

#180 M3 made a sent slash command confirmable from the session's transcript, on
the strength of one entry shape: a `"type":"user"` entry whose text is wrapped in
`<command-name>…</command-name>`. Its fixtures were synthetic.

The live gate for #180 sent `/rename <the session's own name>` through `/input`.
The receipt was accurate — `queued` — but it arrived only after the whole 4 s
confirmation window, by the screen fallback. The transcript signal had stayed
silent, because the runtime had written no `<command-name>` user entry for that
command. Every session-management command that behaves this way pays the full
window, and each of them is a send a caller is waiting on.

## What the runtime writes, measured

Read off real transcripts written by runtime builds 2.1.273 through 2.1.281: 34
commands, 17 distinct names. Two shapes exist, by command:

| | entries, in order |
|---|---|
| **user-entry form** (M3) | a meta user entry (`local-command-caveat`) → a user entry carrying `<command-name>` → a `system`/`local_command` **result** |
| **local-command form** (this ADR) | a `system`/`local_command` **echo** → a `system`/`local_command` **result** → a meta user entry (`system-reminder`) |

- The **echo** has `content` holding the same `<command-name>` / `<command-message>` /
  `<command-args>` markup, leading slash included, and no `commandRun`.
- The **result** has `content` holding `<local-command-stdout>…` and
  `commandRun: {"command": "rename", "args": "…"}` — the name **without** its
  slash. Its `args` equalled the echo's in all 34 pairs, and it followed the echo
  immediately in all 34.
- Both carry `isMeta: false`, so the meta filter that keeps a reminder from being
  a turn does not touch them.
- Every measured command wrote a result, whichever form it took. Only the
  local-command form wrote an echo.

## Decision

**A `system`/`local_command` entry that names a command is a command candidate**,
read by the same path as M3's user entry and matched by the same rule
(`commandMatches`: the same command name, and the same arguments when the entry
recorded any). It reads the command from `commandRun` when present and from the
echo's `<command-name>` markup otherwise.

**Both entries count.** The echo is written at dispatch, so a command that takes
seconds to run confirms then, not at completion. The result is the one entry every
measured command wrote, so a build that drops an earlier entry costs no
confirmation. The result's `stdout` is never read.

Everything else stays as it was. A candidate that names some other command is
silence, never "a different turn" — a different turn stops the screen fallback,
and another command running in the same moment is no reason to stop waiting for
this one. Entries before the send's own offset are never read, so an identical
earlier `/rename` cannot confirm this one. The outcome is still `queued`, never
`submitted`; nothing about that changes.

## Alternatives considered

**Confirm on any `local_command` entry after the offset.** Cheapest, and wrong: it
is not evidence about this send. A different command the runtime ran in the same
window would confirm a submit that never registered.

**The echo only, or the result only.** Each covers the measured `/rename` case.
Neither covers both forms: the result alone is late for a slow command, and the
echo alone is absent from the user-entry form.

**Read the result's `stdout` for the effect** (the new name, for `/rename`).
Rejected: it is the command's output, free text that differs per command and per
build, and the name and arguments are already structured.

## Consequences

- The measured case moves from the full window to roughly the runtime's own write
  latency. The tests that pin it fail against the old code, and the send test takes
  the whole 4 s there.
- **The shape is not checked by `colab-fleetd compat`.** That is by the
  maintainers' ruling (`docs/compat.md`, *Not checked*: local-command entries), not
  an oversight here. The cost of the shape changing is the slow path again, never
  a wrong answer: a candidate that no longer parses is silence, and the screen
  fallback is unchanged.
- **Not measured:** whether a command that starts a new conversation record (such
  as `/clear`) writes its entries to the record the driver is polling. If it does
  not, it stays on the screen fallback exactly as before.
- The fixtures in `slashcommand_test.go` have the measured shape and synthetic
  values. A real transcript is somebody's conversation and is not committed.
