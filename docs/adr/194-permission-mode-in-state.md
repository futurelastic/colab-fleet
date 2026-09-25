# ADR: `state.permissionMode` — the session's current mode, read off the runtime's own indicator row

**Issue:** #194
**Status:** decided — a maintainer confirms or reshapes; the reshaping below is listed so it can be

## Context

#188 made `BTab` (Shift+Tab) a `keys` key, so a client can press the runtime's mode
cycle. It cannot tell which mode the session is in afterwards: `state` published
fingerprints of the screen and no mode, and the receipt for a press says only that the
screen changed under it. A busy session repaints for other reasons, and the runtime's
cycle order decides where a press lands. The "press, re-read, repeat until the mode I
want is showing" loop that ADR 188 left to the client had nothing to re-read, so a client
with a mode control still needed its own handle on the terminal multiplexer — the thing
`keys` exists to make unnecessary.

## Decision

`SessionState` gains `permissionMode`, published **when the driver can classify it, and
only then**: a closed set of six words, `default` · `acceptEdits` · `plan` · `auto` ·
`bypass` · `unknown`, with a strict decoder like `status`.

- **Absent** means nothing was read: the driver does not look
  (`DriverCapabilities.observesPermissionMode`, which an unreached peer reports
  `assumed`), no fenced composer was on screen (a dialog owns it), or nothing is painted
  under the composer yet. A client reads again.
- **`unknown`** means the indicator area *was* read and named no mode — wording this
  build does not know, a hint painted in its place, or two different modes at once. A
  client cycling toward a target **stops**: it cannot say what the next press does.
- It is a material change for the event plane (`session.state` fires when it changes) and
  it never touches `status`.

### Where it is read from

The row under the composer's closing fence, and nothing else. That is chrome the runtime
redraws and nothing the agent prints can reach — the same region and the same reason as
`controlChannel`. A matcher that scanned the whole screen would let a session whose
transcript says `plan mode on` classify itself as being in plan mode, and a client that
stops cycling at `plan` would stop on a lie.

The anchor is the fence-checked composer locator, not "everything after the last `❯`". The
menu-selection glyph is the same character, so the looser rule would read a dialog's own
lines as an indicator area and report `unknown` while a dialog owns the screen; and a
multi-line draft — text somebody typed, above the closing rule — would be part of what
is read. Both are tests.

### What was measured (and where the request's proposal was reshaped)

The five named modes were read off a real runtime build (2.1.282) in a private multiplexer
server by pressing the cycle key through every stop and reading the row after each press:

| Mode | First footer row, verbatim |
|---|---|
| default | `⏸ manual mode on · ? for shortcuts · ← for agents` |
| acceptEdits | `⏵⏵ accept edits on (shift+tab to cycle) · ← for agents` |
| plan | `⏸ plan mode on (shift+tab to cycle) · ← for agents` |
| auto | `⏵⏵ auto mode on (shift+tab to cycle) · ← for agents` |
| bypass | `⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents` |

The cycle, in a session launched with bypass on a model that offers auto: bypass → auto →
default → acceptEdits → plan → bypass. Launched without, on a model that does not offer
auto: default → acceptEdits → plan → default. A build or model that does not offer a mode
skips it, so **a client must read after every press and never count them.**

Four things in that table differ from what the repository had assumed or the request
proposed:

1. **`bypass` is a member.** The request's list was `default`, `acceptEdits`, `plan`,
   `auto` plus unknown. `bypass` is the mode most of an unattended fleet runs in, and a set
   without it would report `unknown` for exactly the sessions a mode control exists to
   reach. It is spelled as `SessionSpec.permissionMode` spells it (`PermissionModeBypass`,
   one constant), so a session created with it reads back as it.
2. **The default mode is not a bare `? for shortcuts`.** It says `manual mode on`. The
   synthetic composer used in the driver's tests modelled it the other way; it now paints
   the measured rows.
3. **The mode is named by its wording, not its glyph or its hints.** `⏸`/`⏵⏵`, the
   `(shift+tab to cycle)` hint and `← for agents` move freely — the hint is absent in the
   default mode altogether. Labels match as lower-case phrases anywhere on an indicator row.
4. **Two of the five wordings are not literal strings in the runtime binary**
   (`manual mode on`, `bypass permissions on` are composed at run time; the other three are
   searchable). That decides the shape of the compat check below.

### The machine-local index is not a source

The request named two candidate sources to measure before choosing. The index's
permission-mode class (#148) was measured and set aside on structure, not preference: it
has **two values** (`bypass` or `prompting`), so it cannot tell four prompting modes
apart; and it is written **once, when the session is launched**, so it goes stale on the
first press of the key this field exists to serve. It answers a different question — what
posture a message must be attested against — and stays the source for that.

### `colab-fleetd compat`: `F-MODE`

A candidate build whose indicator wording changes must not silently start reporting
`unknown` in production, so the compat report gains `F-MODE`. Because two wordings cannot
be searched in the binary, it reads what a live session shows: the harness already boots a
session in the default mode and one in bypass, and the check asserts the driver's own read
of each is `default` and `bypass`. The other three modes cannot be entered without pressing
keys in a session other checks share, so their wording is checked statically, the way the
remote-control labels are.

**Gate: `warn`.** A candidate that rewords the indicator still delivers, confirms and
classifies dialogs exactly as before; only this read degrades. `warn` puts a failing row in
the report before a fleet takes the build without rejecting one for a cosmetic loss. This
is a judgement, not a rule: the gate is data, not shape (`docs/compat.md`, *Schema*), so a
maintainer who decides a mode control is load-bearing changes one word, with no schema bump.

## Alternatives considered

**Absent only, no `unknown`** — the `controlChannel` shape, where nil covers everything.
Not taken: the request asked for an explicit unknown, and the distinction earns its keep
here. A client looping on the field must tell "read again" (nothing painted, a dialog
owns the screen) from "stop, the indicator is there and unreadable" (the build changed).
Folded together, the second case is a loop that presses forever.

**Read the mode from the index.** Set aside above.

**A lenient decoder that maps an unrecognised value to `unknown`.** Would make a newer
peer's sixth mode harmless to an older service. Not taken: it contradicts the discipline
`status`, `confidence` and `controlChannel.state` already hold, and a client that branches
on six names silently falling through for a seventh is what a strict decoder exists to
stop. The consequence is stated below.

**A set-mode operation (options B/C on #188).** Out of scope, as the request says. Reading
the mode is what makes either cheap enough to reconsider; deciding whether to build one is
a separate ruling, and this ADR does not pre-empt it.

## Consequences

- **A client can now build a mode control without a second handle on the terminal.** The
  loop is in `docs/client-guide.md` (*Changing a session's permission mode*): read; stop on
  a match; on absent read again; on `unknown` stop; otherwise press once with a fresh
  digest; bound it at one lap.
- **Adding a member later is a coordinated upgrade,** like adding a `status`: a service on
  the old build decoding a peer's new word refuses the whole state. The alternative was
  rejected above; whoever adds a mode should expect this and say so in the release notes.
- **The reader is tied to a runtime build's wording**, which is why it has a compat check
  and why the redactor keeps the mode rows whole (a committed real capture of each mode is
  in the classifier corpus, redacted by the sanctioned tool, so the fixtures exercise the
  row rather than a hand-written imitation).
- **`auto` is not offered on every model.** A session on a model that does not offer it
  never lands there; the runtime paints an "unavailable" notice **above** the composer, and
  a test pins that this notice is not read as the mode.
- **Read-only, no new grant.** The field is one of six fixed words carrying no
  conversation and no screen text, so it rides the ordinary `state` read; it does not reopen
  the rule that `state` publishes fingerprints of the screen and never the screen.
