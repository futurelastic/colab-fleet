# A burst of arrow presses survives a plain menu — and does not survive a press that moves focus

**Issue:** #205 (settles the question #204 left open)

## What was asked

#204 found that beside a preview pane several keys in one `send-keys` reach
the runtime as one, and left this open: `walkHighlight` (#171) moves an
unnumbered menu's highlight with all of its arrows in one call, and had only
been measured moving it one row. Does a longer walk come up short?

## What was measured

Live, on one runtime build (2.1.282), in throwaway sessions on a private
multiplexer server, arrows and Escape only — never a confirm key. The machine
was under heavy load (load average about 100), so a timing race had every
chance to show.

| layout | keys in ONE call | trials | result |
|---|---|---|---|
| the unnumbered trust menu (two rows, wraps) | `Down Up`, `Down`×2, ×3, ×4, ×5, `Up`, `Up`×3, `Down Up Down` | 6 each, 48 in all | every one ended on the row that all presses applied gives |
| a numbered picker, eight rows | `Down`×3, `Down`×7, then `Up`×4 | 3 each | rows 4, 8, 4 — every press applied |
| a settings list, burst begins **inside** the list | `Down`×3, `Up`×2 | 3 each | every press applied |
| the same list, burst begins on the **search box** above it | `Down`×3, `Down`×5 | 3 each | the highlight moved **one** row: the first press moved focus into the list, the rest were lost |

Single keys were the control throughout: one press, one row; three separate
calls even 0.1 s apart also moved three.

The two-row menu cannot count presses (two states, so a result only tells the
parity of what was applied). Its 48 results rule out "only the first", "only
the last" and "none" for every length tried; the eight-row picker is what
counts.

## What it means

- **On the menu `walkHighlight` serves, a burst is not lost.** So the answer
  to #205 is that no change to how the keys are sent was called for. No
  unnumbered menu of three or more rows exists in this build to measure
  directly, and the trust menu has two rows, so a real walk there is one key.
- **Loss shows where a press in the burst changes which control has focus, or
  the layout under it** — the preview pane (#204), the search box handing
  focus to the list (this). The rest of the burst is gone. That the later
  presses go to the control being replaced is an inference from the pattern,
  not something read from the runtime.
- **#204's rule stands as the default:** on a layout nobody measured, one key
  per call and a read before the next. `walkHighlight` is a measured
  exception, not a licence; its read-back stays, and a test now pins it
  against a menu that keeps only one key of a burst
  (`TestRespondConfirmsNothingWhenABurstOfArrowsLosesPresses`).

## Two traps in the measuring

- **A shell that does not split a variable sends ONE argument.** In zsh,
  `tmux send-keys -t pane $keys` with `keys="Down Down Down"` typed the words
  as text; the menu ignored them and the run read as "presses lost" — ten
  trials of wrong evidence. Run the loop under `sh`, or spell the key names
  out, and keep a single-key control in the same script: if it does not move
  the highlight, the harness is the fault.
- **A throwaway session shares the operator's home.** One probing session sent
  Enter into a settings list to see what it did, and that toggled a real
  setting. Keep exploratory probing to arrows and Escape, and never send
  Enter or Space to a menu whose rows you have not read.
