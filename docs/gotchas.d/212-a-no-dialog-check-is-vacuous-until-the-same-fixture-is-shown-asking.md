# A "no dialog appeared" check proves nothing until the same fixture is shown asking

**Issue:** #212 (compat: pin the external-imports dialog and the keys the seeder writes)

## What was asked

`colab-fleetd compat` pinned the folder-trust question twice: `C1` (a seeded
directory reaches its composer with no dialog) and `F-TRUST` (an unseeded one shows
the dialog, classified). The external-imports question the service now seeds and
answers (#211) had neither. The obvious fix — give `C1`'s directory an import of a
file from outside it — is where the trap is.

## The trap

`C1` passing on that directory says only that **no dialog showed**. That is also what
you see if the fixture never made the runtime ask in the first place (the import is
not "outside" by the runtime's reckoning, the syntax is not one it follows, the path
has a character it trips on — directory `a` is deliberately built with a dot, an
underscore, a space and a non-ASCII letter). A check that can only pass by *absence*
is vacuous until the same fixture is shown to produce the presence.

So the two halves are one design, not two checks that happen to sit together:

- `F-IMPORTS` — a directory with the identical fixture, trusted but **not**
  import-approved, shows the dialog.
- `C1` — the seeded directory, with the same fixture, does not.

The pair is the only thing that makes the "does not" mean anything, so neither is to
be dropped or gated apart from the other without the reason being written down.

## Why the harness cannot get the second directory through the seeder

The driver's seeder writes the trust answer and the imports answer together, on
purpose — one statement of the operator's policy covers both. A directory it seeds can
therefore never show the imports question. The harness needs a directory that is
trusted and not import-approved, so it writes the trust answer itself, through the
seeder's own writer with its key set narrowed to that one
(`trustseed.NewTrustOnly`) — never by hand-editing the runtime's state file, which is
the race that writer exists to survive — into a directory placed outside the
driver's seeding root so the driver's own seeder refuses it.

## Measured, on a real build (2.1.283)

- With the fixture in place: `C1` passes, `F-IMPORTS` passes (unnumbered menu, the
  decline highlighted, one affirmative option).
- Negative control on `C1`: a build of the service whose seeder writes the imports
  keys under other names → `C1` **fails**, says the directory *still asks about that
  import*, and names both keys.
- Negative control on `F-IMPORTS`: the same directory seeded with both answers → the
  session reaches its composer and `F-IMPORTS` **fails** with "no dialog appeared".
- A full run leaves three trust entries in the runtime's state file (it was 876 project
  entries before and 879 after), not the "one per run" `docs/compat.md` used to say: one
  for each trusted throwaway directory a session was launched in.

## The other thing this turned up

A dialog the classifier dispatches on must also survive `RedactCapture`, or the pack
`--pack` writes holds a state nobody can replay: the redactor keeps an option row only
when it is on `knownOptionPhrases`, a list kept apart from the classifier's words.
#211 classified the two imports options and did not add them there, so the pack's
imports dialog came out as `[redacted]` rows. Adding a classified option means adding
it to both.
