# ADR: `BTab` (Shift+Tab) joins the `keys` vocabulary under the existing `keys` grant

**Issue:** #188
**Status:** decided — ruled by the maintainer on the Issue (option A)

## Context

A client wanted to change a **running** session's permission mode from a
mouse-driven control, with nobody at the terminal. On the runtime this service
drives, only one of the modes has a typed command, and it is one-way; every
other mode is reached by cycling with Shift+Tab. `POST …/keys` accepted exactly
`Up`, `Down`, `Left`, `Right`, `Enter`, `Escape`, and `SessionSpec.permissionMode`
covers create time only — so through this API a live session could not be moved
between modes at all, and the client would have kept a direct handle on the
terminal multiplexer to do it.

The request has a security edge that the ordinary keys do not. Moving a session
to a looser mode (accept edits, auto) **escalates** what the agent inside may do
unattended; moving to plan or default de-escalates. Which grant may do that is an
authority decision, so it was put to the maintainer rather than left to whoever
implemented it.

## Decision

**Add `BTab` to the closed key vocabulary, under the existing `keys` grant.**
Same `?expect=` corroboration, same refusals, same confirm-by-repaint as every
other key; delivered to the multiplexer as its own name for Shift+Tab.

The consequence is accepted and written down where a reader will meet it: **any
principal holding `keys` can escalate any session it can reach.** That is stated
in `docs/api.md`, in `docs/spec/api-http.md` (§3 `keys` and §5), in
`docs/client-guide.md`, in `docs/install.md` next to where the grant is handed
out, and at `GrantKeys`'s declaration in `internal/service/auth.go` and the key
vocabulary in `keys.go`.

How `BTab` differs from the keys it sits beside, in the driver:

- It is **not** refused on an idle, empty composer. The arrow keys are (#180 L7:
  there they drive the runtime's own interface); an idle composer is the one
  place `BTab` is for.
- It still takes the composer lock like every key but `Escape`, and is refused
  for every reason the others are: no or stale `expect`, unsent text in the
  composer, a composer taller than the capture window, a prompt `respond` can
  answer. In particular a mode can never be reached by pressing `BTab` at a
  prompt the classifier recognises.
- It is **not a mode setter.** `submitted` means the screen changed under the
  key, exactly as for the others. This service does not read the mode indicator
  and `state` publishes no permission mode, so the receipt cannot say which mode
  the session is in afterwards, and no wording anywhere claims it can.

## Alternatives considered

**B — a first-class set-mode operation under a grant of its own.** The service
would read the current mode, press until it reaches the target, and refuse when
it cannot confirm — the same confirm-or-refuse stance as `input` — and would
publish the mode in `state`. `keys` would stay unable to escalate. It is the
tighter shape and the one triage recommended. Not taken: it needs a mode
classifier for a screen this driver does not parse today, a new operation, and a
new grant, to buy a separation nobody has yet needed; and A does not foreclose
it.

**C — B, with separate grants for escalating and de-escalating.** Tightest, and
one more grant nothing has asked for. Not taken for the same reason, one step
further out.

## Consequences

- **`keys` is now the grant that can escalate.** An operator who wanted the arrow
  keys without that cannot have it from this version. A principal that holds
  `keys` today gains the ability on upgrade — the one place in this repo where
  "absent means denied, so nothing changes on upgrade" is not the whole story —
  so whoever deploys the build that carries this should know it before granting.
- **The client's loop is real work.** "Press until the mode I want is showing"
  needs the mode to be *readable*, and through this API it is not: the client
  must learn the mode from a source of its own. Exposing the current mode in
  `state` (the machine-local index already carries a permission-mode class, #148,
  and the footer shows the mode) is the natural follow-up and is what B would
  have included; it is filed separately rather than smuggled in here.
  **Update:** that follow-up is #194 — [ADR 194](194-permission-mode-in-state.md).
  `state.permissionMode` now publishes the mode, read from the footer; the index
  class was measured and is not a source. The statements above that this service
  "does not read the mode indicator" are true of the decision as made here and
  are superseded by that ADR for the read side; this ADR's decision, the grant,
  is unchanged.
- **Federation:** a service that predates this answers `400` naming the keys it
  does deliver, and across a peer relay the machine that runs the session is the
  one that decides, so a caller can meet that `400` from the far end.
- **Revisit** if a principal ever needs de-escalation without escalation, or if
  the mode becomes readable and a set-mode operation starts to look cheap: that is
  the trigger for B or C, not a reason to reopen this on its own.
