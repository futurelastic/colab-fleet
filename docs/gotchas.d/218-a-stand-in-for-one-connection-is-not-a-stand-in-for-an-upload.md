# A stand-in that can hold or refuse a connection is not a stand-in for a multi-step upload

**Issue:** #218 (tmux: the feedback card's sent line, and the rule behind the turn-off question)

## What was ruled

#218 was ruled: reach the sent line by answering the send with a success shape
from the same local proxy #217 used for the sending and error states — nothing
leaves the machine — and park the line unrecognised, as #217 already documents,
if the runtime refuses the locally issued certificate.

## Why that proxy doesn't carry over as-is

The sending and error states never needed the proxy to *complete* anything: one
held a connection open, the other refused it, and each produced the right screen
without the proxy ever having to answer as the real service would (#217's own
account: "the proxy's log shows the request arriving and stopping"). A success
shape is a different ask — it has to be a convincing final answer, not just an
open or closed door.

Read out of the runtime's own binary, the same way the other four status texts
were before #217 confirmed their geometry on a real pane: the successful send's
status text is the single word **"Sent"** — the fifth of the box's five texts
(ADR 217 calls it "one box, five texts" and lists four; the fifth is this one).
That part carries over cleanly and is recorded here.

What doesn't carry over is the shape of the exchange leading up to it. The
strings around the send-confirmation and error texts sit in a small,
self-contained cluster; the strings around the successful path sit in a larger
one that names a bundling/zipping step ahead of any upload. A single
held-or-refused connection answers one request; a bundling step implies more
than one exchange before there is anything to answer "success" to. A stand-in
that only answers the one request it happens to see is not the same guarantee
as "nothing leaves the machine" — an earlier leg of a multi-step submission
could reach the real service even while the proxy correctly holds or forges the
last one.

## Rule

**"The proxy already reached two of this box's states" is not evidence it can
reach a third whose shape hasn't been checked.** Sending and error needed only a
connection to hold or refuse; nothing here shows the successful path is the same
shape, and building a stand-in on that assumption is exactly the thing
`docs/gotchas.d/217-every-state-of-a-notice-is-a-different-height.md` already
warns about — capture every state before recognising it — now applied to the
stand-in that would feed the capture, not only to the recogniser it would feed
afterward.

## Disposition

Parked, per the ruling's own fallback: the sent line stays unrecognised, as ADR
217 documents. The literal text is recorded here so the next attempt doesn't
need to re-derive it; what still needs a real pane is its geometry (row count,
whether it wraps, whether a queued suffix can follow it) and a stand-in that can
honestly answer every leg of the exchange, not just the last one, before
"nothing leaves the machine" is safe to assert again.

The turn-off question's decline rule (the issue's other half) was also checked
against a bounded read of the build: no persisted counter or threshold surfaced
this way. Nothing in the driver depends on it — the question is recognised and
guarded wherever it appears, regardless of why it appears — so this stays open
only as a "nice to predict" question, not a blocker, exactly as ADR 217 already
says.
