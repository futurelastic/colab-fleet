# ADR 165 — The applied marker is a read-only field on the session, not a label

**Status:** accepted (2026-09-14)
**Issue:** colab-fleet #165 · builds on #153, #96, #90

## Context

A session-type marker existed only as a suffix on the session name. A consumer
grouping sessions by type had to run a suffix test on the name — the test #90
and #96 recorded as ambiguous when a marker is drawn from the same alphabet as
the name body. #153 added caller-supplied labels and listed "the marker as a
label the driver sets" as a follow-up, which forced a question ADR 153 had left
closed: may the service own any key in the label namespace?

## Options

- **A — reserved label key.** The service writes `marker` into the label map at
  create. No new field, and rename carries it for free. It is the first
  exception to ADR 153's rule that the namespace is entirely the caller's:
  callers may already use that key, and the label-write path would need a
  refusal for it.
- **B — dedicated read-only field.** A `marker` field on the session, set once
  when the marker is applied and never writable through the label endpoints.
  One more field in the spec and on the relay path; ADR 153 stays intact.

## Decision

**B.** The marker is a fact the service applied, not caller metadata. A field
keeps label ownership unambiguous, needs no reserved-key refusal, and cannot
collide with a key a caller already uses.

The terminal driver already records its marker decision at the instant it makes
it (#96), so the field publishes a recorded fact rather than re-deriving one
from the name.

## Consequences

- **The published value lives in its own record field.** The existing
  marker-decision fields answer a naming question about one string, whether
  this driver appended the marker, and a rename clears them because the caller
  dictated the new string. The published marker answers what kind of session
  the run is, which a rename does not change. Merging the two would either lose
  the type on rename or corrupt the next create's naming decision, so the
  record keeps them apart.
- **Value rule:** the marker the create asked for, when the resolved name ends
  in it, whether the driver appended it or the name already carried it. Empty
  when the name kept a *different* marker (markers are never stacked): that
  create applied nothing, and must not claim it did.
- **Older records** that predate the field still publish when they can do so
  exactly: a record saying the driver appended the marker has not been renamed
  since, so the marker is on the name.
- **Empty is never "untyped".** It means no record: no marker asked for, a
  driver that does not record markers, or a session older than the field.
- **The relay needs no special handling.** A peer's session decodes into the
  same type, so the field crosses the federation like any other.
