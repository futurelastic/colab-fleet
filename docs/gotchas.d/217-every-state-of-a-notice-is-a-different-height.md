# Every state of a notice is a different height, and a recogniser written for one reads the rest as nothing

**Issue:** #217 (tmux: the feedback-draft card's other states)

## What broke

#215 recognised the runtime's feedback-draft card by its key row and made it a
prompt on the short pane it hides the composer on. The card is one box that
changes its *status text* as it is answered, and every other state of it was still
read as no card at all. Measured on real panes, 80 by 24, with a seven-row card:

- the **send confirmation** and the **send error** wrap at the box width, so each
  is a row taller than the key row. The composer's opening rule moves to the
  *last* row and its `❯` row falls off the bottom too;
- **sending** is one row and looks like the key row's geometry, but is not
  something to answer;
- `1` opens a **full-screen panel** that replaces the composer entirely;
- the **question** about turning drafts off is a plain row, not a box.

Read through the driver, each was answered wrongly: as `idle` with the composer
empty (the false idle #215 fixed for the key row, until #216 stopped the
finished-spinner branch claiming a composer it had not found) and since then as
`unknown` whose evidence calls it "a full-screen interface" with no composer of its
own to paint. Sends were refused as if the runtime were starting or showing a
full-screen interface, and `keys(Enter)` on the panel's editor — whose highlighted
row sends the draft with the conversation attached — was delivered (measured on
trunk after #216).

## What the evidence said that the source did not

- The states were read out of the runtime's binary in #215 and could not be
  captured then. The binary holds the *words*, not the layout: the wrapping, the
  row a key hint ends up on, and which rows a state takes are only on a screen. A
  scratch session on its own socket, asked to call the draft-feedback tool, made
  the card appear on demand; a local tunnelling proxy that could hold or refuse
  connections reached the sending and error states without a submit leaving the
  machine (the proxy's log shows the request arriving and stopping).
- **Escape on the card dismisses the card, not the draft.** #215 and the Issue
  said the draft. The draft's file and the footer's count are unchanged, and
  `/feedback` still lists it. Escape with text in the composer does nothing to the
  card. The premise behind guarding `keys(Escape)` was weaker than it looked.
- A box's *status* is the runtime's and its *body* is the agent's, and the
  agent's rows are never the last rows of the box. Reading the status from the
  bottom, as a whole text, means the agent cannot write one, and a wrapped status
  needs no special case.

## The trap in the redactor this turned up

`RedactCapture` kept `⏺ SendFeedback(… for …)` whole. `statusLine` is a *shape*: a
symbol, a capital, and " for " or an ellipsis anywhere after it. A tool call is
drawn behind the response bullet as `⏺ Name(arguments)`, and one whose arguments
said "for" matched, so the arguments of an agent's tool call reached a corpus
screen destined for a public repository. The redactor now takes the response
bullet before it asks whether a line is a status line: the bullet is not one of the
spinner's frames. The lesson is the one the corpus README already states — a
redactor that keeps by shape keeps what the shape happens to fit — and the check
that caught it was reading the redacted screen, not trusting the fixed-point test,
which a leaked line passes as easily as a clean one.

## What reaching them cost, so the next capture does not repeat it

The runtime keeps **at most ten** queued drafts and evicts the oldest when an
eleventh arrives, without asking. A capture session that drafts seven reports
displaced two of the operator's own unreviewed drafts, silently, mid-run. The
drafts directory was backed up first and restored after, which is the only reason
that was noticed and undone: copy it before drafting anything, and diff it after.
Drafts also belong to whatever account the session runs under, so a scratch
session shows *other sessions'* draft titles on the panel's list — which is why
the redactor keeps the panel's frame and nothing inside it.

## Rule

Capture every state of a notice before recognising any of them, and read the
whole screen the tail of a state lands on, not the row you expect it in. When a
state cannot be captured without a real, external effect, find the seam that
stops the effect and leaves the screen (here, the network), and check the seam
before pressing the key.
