# A bound written for the transcript cut a question off once nothing above it could be transcript

**Issue:** #220 (a wrapped question kept only its last three rows, so a long one was
reported starting mid-sentence)

## What broke

`parseMenuShape` kept the last three rows above the options as `prompt.question`: "the
last couple of lines before the options are the ask; everything above is transcript".
That is true of a menu with no header, whose rows above can be anything the agent
printed. It stopped being true the moment the dialog's header row (tab bar or chip) was
found: the loop drops everything before the header, so every row between it and the
options **is** the question, and the bound only cut it.

Measured on synthetic screens on trunk after #219: a 30-row wrapped question reported
rows 28–30, a 4-row one lost its first row, and the review screen of a multi-question
dialog reported the tail of its answers list without its heading. In a real multiplexer,
through the driver's own capture, a 10-row wrapped question reported rows 8–10 before
the change and 1–10 after (`TestLiveWrappedMultiSelectIsReadAndAnswered`).

Three fixtures had pinned the cut as if it were intent — the `wantQuestion` of three
#219 cases, and the corpus case for the same layout, whose `for` text said "the question
is the wrapped block's last three rows". A test that asserts the bound cannot tell a
deliberate bound from an inherited one; the bound's own comment ("everything above is
transcript") was the only place the reason lived, and it named a region the header had
already excluded.

## What was decided (the issue's open question)

- **Keep the whole block once the header is found** — a person answering a question
  from a client cannot answer one that starts mid-sentence, and the same cut hid the
  heading of the review screen. A menu with no header keeps three rows as before
  (`unanchoredQuestionRows`).
- **Cap it at 32 rows** (`maxQuestionRows`, the same bound as the option count). The
  screen bounds the question already, but a pane an agent writes to can be as tall as
  the agent likes, and the text is reported to every reader of the state, digested into
  the nonce and quoted in refusals. Nothing measured needs more than a pane's worth; the
  number is one constant.
- **When the cap bites, keep the rows nearest the options.** The ask sits at the end of
  a question, and the review screen is recognised by the line that closes its question
  (`reviewScreenPrompt`), so the tail is what has to survive. Keeping the head would have
  silently broken that recognition on a long review.

## Stability — what `question` and `nonce` were required to keep

Both digest the question, so it has to read the same on every capture of the same
screen. It does: the window reaches the dialog's opening rule and no further
(`dialogTop`, #219), so the block between the header and the options does not depend on
how much history the capture carries above the dialog. Pinned by
`TestTheQuestionDoesNotDependOnTheHistoryAboveTheDialog` (0 to 40 rows of history, whole
and capped). A capture that itself cuts the dialog off above its header finds none and
reads three rows, as it always did.

## The rule it left

- **A bound that exists because a region MIGHT hold something foreign should be lifted
  where structure proves it does not.** The three-row bound was right for the case it was
  written for and wrong for the case #219 made reachable; the header row was the evidence
  the bound had been waiting for.
- **Test at the height that crosses the bound, not only the shape that carries it** — the
  same lesson as #219, one field over. Every question fixture fitted in three rows.
- When a fix changes what a field holds, the digests over it change with it: a review
  screen's nonce now covers the answers it lists, so a review of different answers is a
  different prompt. That is the safer direction; it is not new information a client has to
  handle, since the nonce was already opaque.

## Not changed here

- The unnumbered-menu recogniser reads its own question, three rows above a footer-found
  block with no header. It has no anchor to widen from.
