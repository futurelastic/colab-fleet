# ADR 217 — The feedback card's other states are recognised, not answered

**Status:** accepted (2026-09-26)
**Issue:** colab-fleet #217 · builds on #215 (ADR 215), #134

## Context

ADR 215 made the runtime's feedback-draft card a prompt while it hides the
composer, and left every other state of it unrecognised because they had been read
out of the runtime's own binary and never seen on a screen: the review view, the
send confirmation, the error row, the question about turning drafts off, and the
sending and sent lines. It also left two open questions: what each state should
report, and whether `keys(Escape)` should be allowed to reach the card.

Recognising a state by its structure needs its structure, so this work started by
getting the states onto a screen.

### How they were captured

A scratch session on its own multiplexer socket was asked to call the runtime's
draft-feedback tool, which writes a local draft and shows the card. Every screen
below is a real capture of an 80-column, 24-row pane, the size a created session
gets. Nothing was sent to anyone:

- the send confirmation was reached by pressing `2` once and left with Escape;
- the **sending** and **error** states were reached with the session's traffic
  routed through a local tunnelling proxy that could be switched to *hold* or
  *refuse* new connections. With it holding, the send request arrived at the proxy
  and stopped there (the proxy's log shows it); switched to refuse, the runtime
  reported the failure. The confirmation key was pressed only after the switch,
  and only after a probe request confirmed the proxy was holding.
- the **sent** line was not captured: it needs a real submit, and nothing here can
  stand in for one. It stays unrecognised.

### What was measured

- **One box, five texts.** The card is one bordered box: the agent's title, a
  quoted preview of its details, and then *a status text* that changes with the
  state. The key row is the first of five: `1 to review · 2 to send · 0 to
  dismiss` (with ` · +N more queued` after it when more drafts wait); the
  confirmation, `Send without reviewing (full draft + env, no transcript)? 2 to
  send · Esc to back`; `Sending…`; and the error, `✘ Couldn't send feedback
  (<reason>). The draft is still queued. Try again later.` with the key hint
  `1 to review & retry · Esc to dismiss` as the tail of the same wrapped text.
  The reason was measured once (`couldn't reach the service`).
- **The confirmation and the error wrap.** At 80 columns each is two rows, one
  more than the key row. On a 24-row pane with a seven-row card that pushes the
  composer's opening rule to the **last** row: even its `❯` row is off the
  bottom, and the panel below replaces it outright. Read through the driver
  before this change, these screens were answered wrongly: as `idle` with the
  composer empty (the false idle ADR 215 fixed for the key row alone) until #216
  stopped the finished-spinner branch claiming a composer it had not found, and
  since then as `unknown` whose evidence calls the session "a full-screen
  interface [that] has none of its own to paint", which describes neither the card
  nor the panel. Every send was refused with wording that blames startup or a
  full-screen interface, and `keys(Enter)` on the panel's editor was delivered
  (measured on trunk after #216): Enter there sends the draft.
- **The question is not boxed.** `Turn off Claude-drafted feedback? 0 to turn off ·
  Esc to keep` is a plain row above the composer's fence. It appeared after
  dismissing with Escape (twice in three tries) and not after dismissing with `0`
  (once in one). It hides nothing: the composer beneath it read whole.
- **`1` opens a full-screen panel**, the runtime's `/feedback` list: a heavy
  rule across the pane, the title `Feedback drafts`, this session's drafts and
  other sessions', and `Enter to review · d to discard · Esc to close`. Enter on a
  draft opens an editor whose highlighted `Send feedback` row **sends** it, with
  the conversation attached by default. The panel replaces the composer outright.
  Escape backs out (editor to list, list to composer). The card is gone afterwards;
  the draft stays queued.
- **Escape dismisses the card, and only the card.** With the composer empty it
  removes the card; the draft's file, and the footer's count, do not change, and
  `/feedback` still lists it. With text in the composer it does nothing to the
  card. On the confirmation it steps back to the key row; on the error it
  dismisses. This corrects the premise the Issue and ADR 215 carried, that Escape
  dismisses the *draft*.
- **The card is non-modal in every state** (typed text stays, the card stays).

## Decision

**Recognise every measured state by its exact text; answer only the one already
answerable.** The key row over an empty, visible composer stays the only prompt.
Every other state is one a person answers at the terminal, and reads as what it is.

1. **One recogniser for the box family.** The status text is the shortest run of
   rows at the bottom of the box that, joined, is exactly one of the four texts
   above. Nothing above it is read, so the agent's words cannot forge a status,
   and a status that wraps is one text. A box whose tail is none of the four is not
   a card. The error is matched by its fixed head and its fixed tail with a
   parenthesised reason between them.
2. **Over a composer that cannot be read as a whole, the confirmation, sending
   and error read `unknown`, never `idle`, with the state named.** Not
   `waiting_input`: there is nothing a caller can answer, and the spec's
   `waitingOn` vocabulary says what `waiting_input` promises. The evidence and every
   refusal (`send`, `keys`, `discard`, `respond`) name the state and what settles it.
   Over a composer that reads whole they are idle, as the key row is.
3. **The key row with its `❯` row off the bottom is also `unknown`.** Whether the
   hidden composer holds text cannot be read, and a digit would be appended to it:
   the same reason ADR 215 refused the key row over typed text. The key row is a
   prompt only when the `❯` row is visible, empty and last.
4. **The confirmation is not answerable through `respond`, on purpose.** It is
   the runtime's own second guard on sending the user's data, and it exists to
   need a second, deliberate act by a person. A client that carries the first
   click (`respond(send)`) has reached the confirmation; a person gives the second.
   In practice it is also almost always over a hidden `❯` row, so an answer could
   not be read back.
5. **The panel reads `unknown`, naming it, and refuses every delivery.** It is a
   person's editing surface: a key that is harmless in a composer is not harmless
   here (Enter opens a draft or sends it; `d` discards one). **Escape alone is
   accepted through `keys`**, because it decides nothing and is the way out, and
   the receipt says what it did. Arrow keys and Enter are refused.
6. **The question is recognised only to guard `0`.** It hides nothing, so the
   session stays idle and takes messages. A message that is only `0` would answer
   it and turn the feature off, so `send` refuses a lone digit while it (or any
   card state) is on screen. It is not a prompt and has no answer path.
7. **`keys(Escape)` is not refused, and `interrupt` is not guarded.**
   - Escape only dismisses a notice, reversibly: the draft stays queued. The
     caller has to have read the screen (the digest it quotes) to press it, and
     Escape is documented as the escape hatch. Refusing it would also trap the
     turn-off question (`Esc to keep` is the safe answer) and the confirmation
     (`Esc to back`).
   - Over a card this driver cannot read past, `keys` accepts Escape in the three
     states it measurably leaves — the key row (whose `❯` row is off the bottom, so
     it is not a prompt), the confirmation and the error — and refuses it while
     sending (what Escape does to a request in flight is not known). Enter and the
     arrows are refused there: Enter could submit text nobody read.
   - Every accepted Escape's receipt says what Escape does on that screen.
   - `interrupt` is unchanged. A stop must not be refused by a notice, the card is
     hidden while a turn runs, and the worst case is a dismissed notice.
8. **`respond` receipts name what the answer led to**, read from the screen the
   card left on, and word an unread outcome as an expectation.

## Alternatives rejected

- **Make the confirmation and the error answerable** (`[send, back]`,
  `[review & retry, dismiss]`). Designed and then dropped on the geometry: on a
  24-row pane both wrap and hide the `❯` row, and an answer to a card whose
  composer row cannot be seen is exactly what ADR 215 refuses. The machinery would
  be dead on the panes fleets create, and live only on a pane one row taller than
  the card, where the composer reads whole and nothing needs answering.
- **`waiting_input` with no prompt for the panel and confirmation.** It is the
  existing shape for unsent text, but `waitingOn` exists to say *why*, and adding
  a value for a state no client can act on grows the vocabulary for nothing.
  `unknown` with named evidence is what ADR 215 already did for the card over typed
  text, and it costs no new API surface.
- **A prompt of kind `feedback-review` for the panel, with one option (`close`).**
  It would let a client close a panel it opened. It would also put a key in a
  client's hand on a screen where the next key can send the user's conversation,
  and Escape through `keys` already leaves the panel.
- **Refuse `keys(Escape)` while the card is up.** See decision 7.
- **A separate kind per state.** Five kinds for states only one of which is a
  prompt.

## Consequences

- A session in any recognised state reads honestly and refuses by name instead of
  reading idle and refusing as if it were starting. A supervisor that pings such a
  session is told what it is waiting for.
- `respond(send)` on the key row lands on a confirmation this driver names, and
  from there only a person moves it. That is the runtime's design, made visible.
- **The sent line is still unrecognised.** It is a success that settles by
  itself. If it ever hides the composer for long enough to matter, it reads as
  idle with the composer absent, as the other states did before this.
- Card boxes are recognised by exact wording, so a runtime that rewords one falls
  back to unrecognised (idle over a readable composer, the old false idle over an
  unreadable one). `compat`'s `F-FEEDBACK` check reads the candidate for the
  literals the recogniser matches whole; the key row's own words are composed at
  run time and cannot be searched.
- The turn-off question's appearance after an Escape is inconsistent (twice in
  three, never after `0`). The rule is not known, and nothing here depends on it.
- `RedactCapture` gained the status rows, the panel's frame and the question as
  kept vocabulary. A send error's reason is kept only when measured, and otherwise
  masked character for character so the screen still reads as the error. It also
  no longer keeps a tool-call line as if it were the spinner line — see
  `docs/gotchas.d/217-every-state-of-a-notice-is-a-different-height.md`.

## Reopen when

A state is captured that is not one of the five box texts, the panel, or the
question (in particular the sent line); the runtime makes the confirmation
something other than a second guard; or `composerSpan` reads a bottom-clipped
composer as clipped (#216), at which point the key row over a hidden `❯` row may
be answerable and decision 3 is worth asking again.
