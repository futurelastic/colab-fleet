# Whether a programmatic `/rename` makes the runtime write a fresh custom-title is unmeasured

**Issue:** #222 (tmux: bring the runtime's own title along on a rename)

## What is NOT known

Colab-fleet#187 measured, against real transcripts (runtime builds 2.1.273
through 2.1.281, 34 commands), that a session-management command the runtime
runs itself — `/rename` among them — writes a `system`/`subtype:"local_command"`
echo-then-result pair. That measurement says nothing about whether the runtime
ALSO writes a fresh `"type":"custom-title"` entry as a consequence, or how long
after the local_command pair it would appear, or whether a busy runtime defers
it behind whatever else it is doing.

Every one of #187's 34 samples came from a HUMAN typing `/rename` at the
keyboard. #222's `SyncTitle` (`internal/drivers/tmux/titlesync.go`) delivers the
identical command PROGRAMMATICALLY, through the ordinary composer-paste
pipeline, unlabelled. If the runtime's own code path treats those two
differently — plausible, since one arrives as literal keystrokes and the other
as a bracketed paste — the custom-title write could behave differently too, and
nothing in this repo has ever looked.

## What this means for a reader debugging a `pending` rename

`SyncTitle` degrades HONESTLY when it cannot confirm: if no `custom-title` entry
follows the local_command confirmation within the confirmation window
(`submitConfirmWindow`, 4s), it reports `TitlePending` rather than guessing
either `synced` or `failed`, and increments `title_sync.command_without_title`
so the RATE is at least visible. If that counter is nonzero at any real scale in
production, the honest next step is a live measurement — not a code change
guessing at the runtime's behavior from here.

## The recommended fix — not built in #222

A compat check modeled on the existing `E-NAME` check
(`internal/drivers/tmux/compat_send_checks.go`): boot a real Claude Code
session, send a programmatic `/rename <new>-x`, and assert whether a matching
`custom-title` entry appears afterwards and at what latency. `E-NAME` already
proves the FIRST custom-title (at boot) matches the `-n` value; this would be
its sibling for a LATER rename. Filed as part of futurelastic/colab-fleet#223.

## Where the two facts this repo HAS measured live

- `internal/drivers/tmux/terminalpath2_transcript.go`'s `localCommandLine` doc
  comment — the local_command echo/result shape, 34 samples, human-typed.
- `internal/drivers/tmux/compat_send_checks.go`'s `E-NAME` check — the FIRST
  custom-title, at boot, matches the `-n` boot name. Says nothing about a later
  rename.
