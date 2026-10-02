# Whether a programmatic `/rename` makes the runtime write a fresh custom-title is now measured by a compat check, not this doc

**Issue:** #222 (tmux: bring the runtime's own title along on a rename), fix
built on #227 (split from #223 item 4)

⚠️ **This is a per-build fact, not a repo fact.** The finding below explains
WHY nobody had ever looked and what "unmeasured" used to mean; it is not a
substitute for running the check against the candidate a reader actually
cares about. A runtime release can change this from one build to the next,
the same way it can change any of the other things `muster compat`
exists to catch (see `docs/compat.md`).

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

## The fix — built on #227, the `H-RENAME` compat check

A compat check modeled on the existing `E-NAME` check, as this doc originally
recommended: `internal/drivers/tmux/compat_rename.go` boots a real Claude Code
session (the same trusted session `E-NAME` itself boots), drives it through a
real programmatic rename — the driver's own `Rename` then its own `SyncTitle`,
the exact two calls and order the service makes — and keeps reading the
transcript for `compatRenameWait` (30s), far past `SyncTitle`'s own four-second
`submitConfirmWindow`, specifically so a runtime that is merely SLOW to write
the custom-title is not reported the same as one that never does. Run
`muster compat --claude <candidate> --only H-RENAME` (or a full run) to
get the answer for a given build; its `detail` names the latency when a
custom-title did follow, or says plainly that it did not within that window.

It is `warn`-gated on purpose: `SyncTitle` already degrades honestly to
`pending` when this has not happened, so nothing downstream is broken by
either answer — the gate reflects that this is a *finding*, not a correctness
requirement this driver can enforce on the runtime.

## Where the facts this repo has measured live

- `internal/drivers/tmux/terminalpath2_transcript.go`'s `localCommandLine` doc
  comment — the local_command echo/result shape, 34 samples, human-typed
  (#187).
- `internal/drivers/tmux/compat_send_checks.go`'s `E-NAME` check — the FIRST
  custom-title, at boot, matches the `-n` boot name. Says nothing about a
  later rename.
- `internal/drivers/tmux/compat_rename.go`'s `H-RENAME` check — whether, and
  after how long, a LATER, programmatic rename gets one too. This is the
  measurement that used to be missing; its report is the live answer, not a
  line in this file.
