# ADR: a delivery confirmed without a started turn is not forgotten

**Issue:** #240
**Status:** decided

## Context

A message sent to a session by another driver was found sitting **unsent** in
the receiver's composer — five times on one machine in one morning, including
a coordinating session whose stuck input stalled a whole lane for half an
hour. In every case the sender had been told `queued`, had no reason to
retry, and the driver refused every attempt to finish the delivery ("the
composer holds text this driver has no record of placing"); a third party
recovered each one by reading the screen and sending `replaceIfStranded` with
the composer's digest.

The issue was filed as "a session with no turns yet", and a later comment
corrected it: the sessions had been running for tens of minutes. `turns: 0`
was real but meant "nothing completed since the last delivery" (#111), not
"just created". The common factor was a message arriving while the receiver
was busy.

Two earlier fixes cover neighbouring hops and left this one open: #25 refuses
a send to a pane with no composer yet, and #112 trusts the driver's record
when one exists. Neither helps when there is no record, and there is none
because the driver deliberately deletes it.

## What the code does, and which hop was open

On the terminal path every failure after the paste — the text not landing,
the submit failing, the submit not registering — writes a stranded record and
returns `unknown`, so the sender is told. The one success outcome is
`queued`, after which the record is forgotten, and no tombstone is left for it
because "that text is gone".

That premise holds for a submit confirmed by a turn: a user entry or a command
entry in the runtime's own transcript. It does **not** hold for the other
evidence the driver accepts:

- a transcript `enqueue` — the runtime took the text into its queue because a
  turn was running; nothing has started on it;
- the composer reading empty (or the delivery's paste marker clearing) on a
  screen, when the transcript stays silent.

Both show the text left the composer. A runtime that queued it can return it
(a queued message handed back after the running turn was interrupted or
failed). Nothing in the driver then remembered it, so the text looked like a
person's draft and was refused, and the sender had no reason to look.

**Not measured:** what the runtime does to the queue in each case, and which
of the two weak signals produced the five incidents. The change below is
correct under either, and adds the counters that settle it.

## Decision

1. **Keep a provisional entry when the confirmation is weak.** A confirmation
   that does not show a turn started (`confirmSubmittedFromSourceTurn`
   reporting `turnProven == false`) records a *provisional tombstone*: the
   labelled text, the working directory, and the transcript position taken
   before the submit. It is the same record the draft rule already uses for a
   lapsed strand (#180 M2), so it proves the composer's text is the driver's
   own under the same two tests and acts through the same doors
   (`resumeIfStranded`, `replaceIfStranded`) with no new way to clear a
   composer. One entry per (directory, text) per session; the existing cap and
   24-hour retention apply; it persists with the other tombstones.
2. **Proof requires the text not to have run.** When a transcript was
   resolved, a provisional entry proves nothing once a user turn carrying the
   same text appears after the recorded position: the runtime ran it, and the
   same words in the composer are a person's or a recall from history.
3. **Say so on the receiving side.** `SessionState.strandedDelivery` is true
   only with `waitingOn: unsent-input` while the driver's own memory (a live
   record or a tombstone) proves the composer's text is its own delivery. It
   is part of `MateriallyDiffers`, so a subscriber hears about it. The text
   itself is never published.
4. **Say so on the sending side.** A `queued` receipt resting on a weak
   confirmation names what the sender will see if the text is handed back and
   what to do about it; the busy-composer refusal says whose text is there.
5. **Count what is left unexplained.** `submit_confirm.by_enqueue`,
   `stranded.provisional_kept`, `stranded.provisional_handed_back`, and
   `stranded.unexplained_labelled_composer` — an unsent composer opening with
   the driver's sender label that nothing it remembers explains (logged with
   whether a record exists under another working directory). The last is
   diagnostic only and never proof. If it fires after this change, another hop
   is open.

## Why surface the strand, not submit it

The issue allowed either: land the text, or make the strand visible to the
sender. Landing would need the driver to press submit unattended, later. The
hand-back this change guards follows a stopped turn — an interrupt or a
failure — and re-submitting would override a stop the driver cannot attribute.
A send also cannot stay open across a busy turn of unbounded length (§4.4). The
draft rule and the stranded-delivery machinery already finish a strand when the
sender asks; the minimal fix is to stop discarding what they need. Landing
remains open as a follow-up, to be decided on the counters above.

## Not covered

- A delivery through an optional delivery module or the inbox lane writes no
  composer record on its own path; a module confirming a queue entry is the
  same shape and is not changed here.
- A hand-back rendered as a collapsed paste marker, or several queued messages
  returned joined, will not match the recorded text; they stay refused (fail
  safe) and are counted as unexplained when they carry the sender label.
